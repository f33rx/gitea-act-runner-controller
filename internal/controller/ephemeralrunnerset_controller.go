/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
)

// EphemeralRunnerSetReconciler reconciles an EphemeralRunnerSet object.
type EphemeralRunnerSetReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=ephemeralrunnersets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=ephemeralrunnersets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=ephemeralrunnersets/finalizers,verbs=update
//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=ephemeralrunners,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile implements reconciliation for EphemeralRunnerSet.
// It reconciles the actual EphemeralRunner count toward the desired replica count,
// creating or deleting EphemeralRunners as needed.
func (r *EphemeralRunnerSetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	ers := &giteaactionsv1alpha1.EphemeralRunnerSet{}
	if err := r.Get(ctx, req.NamespacedName, ers); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get EphemeralRunnerSet")
		return ctrl.Result{}, err
	}

	// List all EphemeralRunners owned by this EphemeralRunnerSet.
	ownedRunners := &giteaactionsv1alpha1.EphemeralRunnerList{}
	if err := r.List(ctx, ownedRunners, client.InNamespace(ers.Namespace), client.MatchingFields{
		"metadata.ownerReferences.uid": string(ers.UID),
	}); err != nil {
		log.Error(err, "failed to list EphemeralRunners for set")
		return ctrl.Result{}, err
	}

	currentCount := int32(len(ownedRunners.Items)) // #nosec G115 - len cannot exceed int32 in practice
	desiredCount := ers.Spec.Replicas

	log.V(1).Info("reconciling EphemeralRunnerSet", "namespace", ers.Namespace, "name", ers.Name,
		"desired", desiredCount, "current", currentCount, "patchID", ers.Spec.PatchID)

	// Scale up: create missing EphemeralRunners.
	if currentCount < desiredCount {
		// Read the GiteaRunnerSet to get Gitea config.
		// By convention, ERS.Name == GiteaRunnerSet.Name and same namespace.
		grs := &giteaactionsv1alpha1.GiteaRunnerSet{}
		grsKey := types.NamespacedName{
			Namespace: ers.Namespace,
			Name:      ers.Name, // ERS is named after the GiteaRunnerSet
		}
		if err := r.Get(ctx, grsKey, grs); err != nil {
			log.Error(err, "failed to get GiteaRunnerSet for EphemeralRunnerSet", "name", ers.Name)
			return ctrl.Result{Requeue: true}, err
		}

		// Get the credential Secret to fetch registration tokens.
		credSecret := &corev1.Secret{}
		credKey := types.NamespacedName{
			Namespace: ers.Namespace,
			Name:      grs.Spec.GiteaConfigSecretRef.Name,
		}
		if err := r.Get(ctx, credKey, credSecret); err != nil {
			log.Error(err, "failed to get Gitea credential secret")
			return ctrl.Result{Requeue: true}, err
		}

		token := string(credSecret.Data[grs.Spec.GiteaConfigSecretRef.Key])
		if token == "" {
			log.Error(nil, "empty token in Gitea credential secret")
			return ctrl.Result{Requeue: true}, nil
		}

		for i := currentCount; i < desiredCount; i++ {
			// Fetch a fresh registration token for this runner.
			var regToken string
			if grs.Spec.RunnerScope == "org" && grs.Spec.OrgName != "" {
				giteaClient := gitea.NewClient(grs.Spec.GiteaConfigURL, token)
				var err error
				regToken, err = giteaClient.GetOrgRegistrationToken(grs.Spec.OrgName)
				if err != nil {
					log.Error(err, "failed to fetch registration token", "index", i)
					return ctrl.Result{Requeue: true}, err
				}
			}

			runner := r.constructEphemeralRunner(grs, ers.Name, int(i), regToken)
			if err := controllerutil.SetControllerReference(ers, runner, r.Scheme); err != nil {
				log.Error(err, "failed to set owner reference on EphemeralRunner")
				return ctrl.Result{}, err
			}
			if err := r.Create(ctx, runner); err != nil {
				log.Error(err, "failed to create EphemeralRunner", "index", i)
				return ctrl.Result{Requeue: true}, err
			}
			log.Info("created EphemeralRunner", "runner", runner.Name)
		}
	}

	// Scale down: delete excess EphemeralRunners -- but NEVER a busy/claimed one.
	//
	// ADR 0007 Decision 3: "Busy runners are never killed... the operator only ever
	// deletes idle runners." A runner that has claimed its one job is mid-execution;
	// deleting it kills the job (the task is left stuck 'running' in Gitea and then
	// swept as an orphan). Busy runners self-drain: they self-exit on completion and
	// their teardown removes them. So scale-down only removes IDLE runners -- ones that
	// have not yet claimed a job -- and leaves busy ones to finish.
	//
	// We treat a runner as safe-to-delete only while it is still Pending (pod not yet
	// Running). Once the pod is Running the runner is either executing its job or about
	// to claim one on its next poll; either way we do not kill it. This makes the
	// effective floor of live pods max(desiredCount, busyCount) until they drain -- the
	// Gitea-ephemeral analogue of ARC's "decreasing desired replicas never terminates a
	// running job."
	if currentCount > desiredCount && ers.Spec.PatchID != 0 {
		toDelete := currentCount - desiredCount
		deleted := int32(0)
		for i := range ownedRunners.Items {
			if deleted >= toDelete {
				break
			}
			runner := &ownedRunners.Items[i]
			// Only delete idle (never-claimed) runners: Pending phase, or empty phase
			// with no assigned RunnerID yet. Never delete a Running/Succeeded runner.
			phase := runner.Status.Phase
			isIdle := phase == "" || phase == giteaactionsv1alpha1.EphemeralRunnerPending
			if !isIdle {
				log.V(1).Info("skipping scale-down of non-idle runner (self-drains)",
					"runner", runner.Name, "phase", phase)
				continue
			}
			if err := r.Delete(ctx, runner); err != nil {
				if apierrors.IsNotFound(err) {
					// Already gone (self-drained and GC'd between the List and this Delete);
					// that is exactly the outcome we wanted, so count it and move on.
					deleted++
					continue
				}
				log.Error(err, "failed to delete EphemeralRunner", "runner", runner.Name)
				return ctrl.Result{Requeue: true}, err
			}
			log.Info("scaled down idle EphemeralRunner", "runner", runner.Name, "phase", phase)
			deleted++
		}
		if deleted < toDelete {
			// The remainder are busy and will self-drain; requeue to trim once they do.
			log.V(1).Info("scale-down deferred for busy runners (will self-drain)",
				"requested", toDelete, "deleted", deleted, "remaining", toDelete-deleted)
		}
	}

	// Update status with current count and patchID. Retry on conflict: the listener
	// and scaling paths write to the ERS concurrently, so the copy we fetched at the
	// top of Reconcile can go stale. Refetch and re-apply the status fields each try
	// instead of failing the reconcile (which previously produced a steady stream of
	// "the object has been modified" errors).
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &giteaactionsv1alpha1.EphemeralRunnerSet{}
		if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
			return err
		}
		latest.Status.ReadyReplicas = currentCount
		latest.Status.AvailableReplicas = currentCount
		latest.Status.LastReconcilePatchID = ers.Spec.PatchID
		return r.Status().Update(ctx, latest)
	}); err != nil {
		log.Error(err, "failed to update EphemeralRunnerSet status")
		return ctrl.Result{}, err
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// constructEphemeralRunner constructs a new EphemeralRunner with Gitea config and token.
func (r *EphemeralRunnerSetReconciler) constructEphemeralRunner(
	grs *giteaactionsv1alpha1.GiteaRunnerSet,
	ersName string,
	index int,
	regToken string,
) *giteaactionsv1alpha1.EphemeralRunner {
	// Generate a unique name: {gitearunnerset-name}-runner-{index}
	name := fmt.Sprintf("%s-runner-%d", ersName, index)

	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: grs.Namespace,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			GiteaConfigURL:     grs.Spec.GiteaConfigURL,
			RegistrationToken:  regToken,
			Labels:             grs.Spec.Labels,
			RunnerScope:        grs.Spec.RunnerScope,
			OrgName:            grs.Spec.OrgName,
			GiteaRunnerSetName: ersName,
		},
	}

	return runner
}

// SetupWithManager sets up the controller with the Manager.
func (r *EphemeralRunnerSetReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Index EphemeralRunners by owner reference for faster lookups.
	if err := mgr.GetFieldIndexer().IndexField(context.Background(), &giteaactionsv1alpha1.EphemeralRunner{}, "metadata.ownerReferences.uid", func(rawObj client.Object) []string {
		runner := rawObj.(*giteaactionsv1alpha1.EphemeralRunner)
		var owners []string
		for _, ref := range runner.ObjectMeta.OwnerReferences {
			if ref.Kind == "EphemeralRunnerSet" {
				owners = append(owners, string(ref.UID))
			}
		}
		return owners
	}); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&giteaactionsv1alpha1.EphemeralRunnerSet{}).
		Owns(&giteaactionsv1alpha1.EphemeralRunner{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 5}).
		WithEventFilter(predicate.GenerationChangedPredicate{}). // Ignore status-only updates
		Complete(r)
}
