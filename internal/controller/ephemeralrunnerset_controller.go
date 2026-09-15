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

	// ADR 0008: manager-wide timeout defaults, used when a GiteaRunnerSet does not
	// override them. Zero/nil means "no default configured" for that knob (e.g. a
	// zero DefaultActiveDeadlineSeconds means no hard cap unless a set opts in).
	DefaultActiveDeadlineSeconds int64
	DefaultStallWindow           time.Duration
	DefaultPendingTimeout        time.Duration
}

//+kubebuilder:rbac:groups=giteaactions.blackrabbitpursuits.com,resources=ephemeralrunnersets,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=giteaactions.blackrabbitpursuits.com,resources=ephemeralrunnersets/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=giteaactions.blackrabbitpursuits.com,resources=ephemeralrunnersets/finalizers,verbs=update
//+kubebuilder:rbac:groups=giteaactions.blackrabbitpursuits.com,resources=ephemeralrunners,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

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
	//
	// Which runners count as idle is decided by scaleDownSafe (below); see it for the
	// two status-lag races (garc-x32, garc-nme) it guards against.
	if currentCount > desiredCount && ers.Spec.PatchID != 0 {
		toDelete := currentCount - desiredCount
		deleted := int32(0)
		for i := range ownedRunners.Items {
			if deleted >= toDelete {
				break
			}
			runner := &ownedRunners.Items[i]
			phase := runner.Status.Phase
			if safe, why := r.scaleDownSafe(ctx, runner); !safe {
				log.V(1).Info("skipping scale-down of runner", "runner", runner.Name, "phase", phase, "reason", why)
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

// registrationGracePeriod is how long after a runner is created, and separately how
// long after its pod is scheduled, scale-down leaves it alone regardless of cached phase.
const registrationGracePeriod = 15 * time.Second

// scaleDownSafe reports whether an owned runner may be deleted by scale-down, i.e. it
// has certainly not claimed a job. Two status-lag races make the cached
// EphemeralRunner phase alone insufficient:
//
//   - garc-x32 (2026-07-02): act_runner starts, registers, and claims within a few
//     seconds of pod start, faster than the runner's Status.Phase reaches Running. A
//     runner younger than registrationGracePeriod is therefore never eligible.
//   - garc-nme (2026-09-15): a runner that sat Pending (unschedulable, image pull)
//     longer than that grace is already past it when the pod finally starts; the claim
//     drops the queue, desired falls, and the still-Pending cached phase reads as idle
//     while the pod is already running the job. So the Pod itself is consulted: a pod
//     that is no longer Pending, or that was scheduled within the grace period, is
//     treated as busy. Only a pod that has not been scheduled yet, or has sat scheduled-
//     but-Pending (pulling) past the grace, is safe: act_runner has not started.
//
// The residual window is informer lag on the Pending->Running pod transition, which is
// milliseconds against the seconds act_runner needs to register and claim.
func (r *EphemeralRunnerSetReconciler) scaleDownSafe(ctx context.Context, runner *giteaactionsv1alpha1.EphemeralRunner) (bool, string) {
	if age := time.Since(runner.CreationTimestamp.Time); age < registrationGracePeriod {
		return false, "newly created (registration grace period)"
	}
	phase := runner.Status.Phase
	if phase != "" && phase != giteaactionsv1alpha1.EphemeralRunnerPending {
		return false, "non-idle (self-drains)"
	}
	pod := &corev1.Pod{}
	if err := r.Get(ctx, types.NamespacedName{Namespace: runner.Namespace, Name: runner.Name}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return true, "no pod yet"
		}
		return false, "pod lookup failed: " + err.Error()
	}
	if pod.Status.Phase != corev1.PodPending {
		return false, "pod is " + string(pod.Status.Phase) + " (self-drains)"
	}
	if pod.Spec.NodeName == "" {
		return true, "pod unscheduled"
	}
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodScheduled && c.Status == corev1.ConditionTrue {
			if since := time.Since(c.LastTransitionTime.Time); since < registrationGracePeriod {
				return false, "pod scheduled recently (registration grace period)"
			}
		}
	}
	return true, "pod scheduled but still Pending past grace"
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
			GiteaConfigURL:        grs.Spec.GiteaConfigURL,
			RegistrationToken:     regToken,
			Labels:                grs.Spec.Labels,
			RunnerScope:           grs.Spec.RunnerScope,
			OrgName:               grs.Spec.OrgName,
			GiteaRunnerSetName:    ersName,
			ActiveDeadlineSeconds: r.resolveActiveDeadlineSeconds(grs),
			StallWindow:           r.resolveStallWindow(grs),
			PendingTimeout:        r.resolvePendingTimeout(grs),
		},
	}

	return runner
}

// resolveActiveDeadlineSeconds resolves the hard-cap override per ADR 0008 Decision 6:
// the GiteaRunnerSet's own value if set, else the manager-wide default (nil if that is
// also unset, meaning no hard cap).
func (r *EphemeralRunnerSetReconciler) resolveActiveDeadlineSeconds(grs *giteaactionsv1alpha1.GiteaRunnerSet) *int64 {
	if grs.Spec.ActiveDeadlineSeconds != nil {
		return grs.Spec.ActiveDeadlineSeconds
	}
	if r.DefaultActiveDeadlineSeconds > 0 {
		v := r.DefaultActiveDeadlineSeconds
		return &v
	}
	return nil
}

// resolveStallWindow resolves the stall-detection window override per ADR 0008
// Decision 6.
func (r *EphemeralRunnerSetReconciler) resolveStallWindow(grs *giteaactionsv1alpha1.GiteaRunnerSet) *metav1.Duration {
	if grs.Spec.StallWindow != nil {
		return grs.Spec.StallWindow
	}
	if r.DefaultStallWindow > 0 {
		return &metav1.Duration{Duration: r.DefaultStallWindow}
	}
	return nil
}

// resolvePendingTimeout resolves the pre-claim (Pending) timeout override per ADR 0008
// Decision 6.
func (r *EphemeralRunnerSetReconciler) resolvePendingTimeout(grs *giteaactionsv1alpha1.GiteaRunnerSet) *metav1.Duration {
	if grs.Spec.PendingTimeout != nil {
		return grs.Spec.PendingTimeout
	}
	if r.DefaultPendingTimeout > 0 {
		return &metav1.Duration{Duration: r.DefaultPendingTimeout}
	}
	return nil
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
