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

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
)

const (
	finalizerEphemeralRunner = "giteaactions.blackrabbit.dev/ephemeral-runner"
	secretSuffixToken        = "-token"
	envGiteaToken            = "GITEA_TOKEN"
	envGiteaServerURL        = "GITEA_SERVER_URL"
	envGiteaEphemeral        = "GITEA_RUNNER_EPHEMERAL"
	envGiteaRunnerOrgName    = "GITEA_RUNNER_ORG_NAME"
)

// EphemeralRunnerReconciler reconciles an EphemeralRunner object.
type EphemeralRunnerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=ephemeralrunners,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=ephemeralrunners/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=ephemeralrunners/finalizers,verbs=update
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete

// Reconcile implements reconciliation for EphemeralRunner.
func (r *EphemeralRunnerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	runner := &giteaactionsv1alpha1.EphemeralRunner{}
	if err := r.Get(ctx, req.NamespacedName, runner); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to get EphemeralRunner")
		return ctrl.Result{}, err
	}

	// Handle deletion with finalizer.
	if runner.ObjectMeta.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, runner)
	}

	// Add finalizer if not present.
	if !controllerutil.ContainsFinalizer(runner, finalizerEphemeralRunner) {
		controllerutil.AddFinalizer(runner, finalizerEphemeralRunner)
		if err := r.Update(ctx, runner); err != nil {
			log.Error(err, "failed to add finalizer")
			return ctrl.Result{}, err
		}
	}

	// Ensure the per-pod registration token Secret exists.
	secret := &corev1.Secret{}
	secretName := types.NamespacedName{
		Namespace: runner.Namespace,
		Name:      runner.Name + secretSuffixToken,
	}
	if err := r.Get(ctx, secretName, secret); err != nil {
		if apierrors.IsNotFound(err) {
			// Create the Secret with the registration token.
			secret = r.constructTokenSecret(runner)
			if err := controllerutil.SetControllerReference(runner, secret, r.Scheme); err != nil {
				log.Error(err, "failed to set controller reference on Secret")
				return ctrl.Result{}, err
			}
			if err := r.Create(ctx, secret); err != nil {
				log.Error(err, "failed to create token Secret")
				return ctrl.Result{}, err
			}
			log.Info("created token Secret", "secret", secretName)
		} else {
			log.Error(err, "failed to get token Secret")
			return ctrl.Result{}, err
		}
	}

	// Ensure the Pod exists.
	pod := &corev1.Pod{}
	podName := types.NamespacedName{
		Namespace: runner.Namespace,
		Name:      runner.Name,
	}
	if err := r.Get(ctx, podName, pod); err != nil {
		if apierrors.IsNotFound(err) {
			// Create the Pod.
			pod = r.constructPod(ctx, runner, secret)
			if err := controllerutil.SetControllerReference(runner, pod, r.Scheme); err != nil {
				log.Error(err, "failed to set controller reference on Pod")
				return ctrl.Result{}, err
			}
			if err := r.Create(ctx, pod); err != nil {
				log.Error(err, "failed to create Pod")
				return ctrl.Result{}, err
			}
			log.Info("created Pod", "pod", podName)
			runner.Status.PodName = pod.Name
			runner.Status.Phase = giteaactionsv1alpha1.EphemeralRunnerPending
			runner.Status.Reason = "Pod created"
			if err := r.Status().Update(ctx, runner); err != nil {
				log.Error(err, "failed to update EphemeralRunner status after pod creation")
				return ctrl.Result{}, err
			}
			return ctrl.Result{Requeue: true}, nil
		}
		log.Error(err, "failed to get Pod")
		return ctrl.Result{}, err
	}

	// Update status based on Pod phase.
	r.updateRunnerStatusFromPod(ctx, runner, pod)

	// Auto-teardown: if pod has finished (Succeeded or Failed), delete the EphemeralRunner
	// to trigger the finalizer and clean up the Gitea row.
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		log.Info("pod finished, initiating teardown", "pod", pod.Name, "phase", pod.Status.Phase)
		if err := r.Delete(ctx, runner); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete runner for auto-teardown")
			return ctrl.Result{Requeue: true}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 10}, nil
}

// handleDeletion handles the dual finalizer logic for runner teardown.
// Step 1: Deregister runner from Gitea (primary path for cleanup).
// Step 2: Delete pod + per-pod Secret (owner-ref GC takes care of them).
// Step 3: Remove finalizer so CR is GC'd.
func (r *EphemeralRunnerReconciler) handleDeletion(ctx context.Context, runner *giteaactionsv1alpha1.EphemeralRunner) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(runner, finalizerEphemeralRunner) {
		return ctrl.Result{}, nil
	}

	// Step 1: Deregister the runner from Gitea (org-scoped DELETE /orgs/{org}/actions/runners/{id}).
	// This is the PRIMARY teardown path (not crash-only as was assumed before).
	if runner.Status.RunnerID > 0 {
		log.Info("finalizer: deregistering runner from Gitea", "runner", runner.Name, "runnerId", runner.Status.RunnerID)

		// Read the teardown credential Secret.
		teardownSecretName := types.NamespacedName{
			Namespace: "gitea-actions-controller",
			Name:      "gitea-teardown-credential",
		}
		teardownSecret := &corev1.Secret{}
		if err := r.Get(ctx, teardownSecretName, teardownSecret); err != nil {
			log.Error(err, "failed to read teardown credential Secret", "secret", teardownSecretName)
			// If we can't read the credential, we can't deregister. Requeue to retry.
			return ctrl.Result{Requeue: true}, err
		}

		token := string(teardownSecret.Data["token"])
		if token == "" {
			log.Error(fmt.Errorf("empty token in teardown Secret"), "failed to get token")
			return ctrl.Result{Requeue: true}, fmt.Errorf("empty token in teardown Secret")
		}

		// Deregister from Gitea.
		client := gitea.NewClient(runner.Spec.GiteaConfigURL, token)
		statusCode, err := client.DeregisterOrgRunner(runner.Spec.OrgName, runner.Status.RunnerID)
		if err != nil {
			log.Error(err, "deregister API call failed")
			// Requeue on transient errors (network, etc).
			return ctrl.Result{Requeue: true}, err
		}

		if statusCode != 204 {
			log.Error(fmt.Errorf("unexpected status code"), "deregister returned non-204", "statusCode", statusCode)
			// 404 means the runner is already gone (cleanup already happened). Allow finalizer to proceed.
			// Other errors should requeue to retry.
			if statusCode != 404 {
				return ctrl.Result{Requeue: true}, fmt.Errorf("deregister returned status %d", statusCode)
			}
		}

		log.Info("successfully deregistered runner from Gitea", "statusCode", statusCode)
	}

	// Step 2: The Pod and per-pod Secret are owner-ref'd to this CR, so Kubernetes GC will handle deletion.
	// We just need to remove the finalizer to allow the CR to be GC'd.

	// Step 3: Remove the finalizer to complete teardown.
	controllerutil.RemoveFinalizer(runner, finalizerEphemeralRunner)
	if err := r.Update(ctx, runner); err != nil {
		log.Error(err, "failed to remove finalizer")
		return ctrl.Result{}, err
	}

	log.Info("finalizer complete, runner CR will be garbage collected", "runner", runner.Name)
	return ctrl.Result{}, nil
}

// constructTokenSecret creates a Secret containing the registration token.
func (r *EphemeralRunnerReconciler) constructTokenSecret(runner *giteaactionsv1alpha1.EphemeralRunner) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runner.Name + secretSuffixToken,
			Namespace: runner.Namespace,
		},
		Type: corev1.SecretTypeOpaque,
		StringData: map[string]string{
			envGiteaToken: runner.Spec.RegistrationToken,
		},
	}
}

// constructPod constructs the Pod for the runner.
func (r *EphemeralRunnerReconciler) constructPod(ctx context.Context, runner *giteaactionsv1alpha1.EphemeralRunner, secret *corev1.Secret) *corev1.Pod {
	log := log.FromContext(ctx)

	labels := map[string]string{
		"app":              "gitea-runner",
		"ephemeral-runner": runner.Name,
		"gitearunner-set":  runner.Spec.GiteaRunnerSetName,
	}

	// Build runner labels in the format label:host for act_runner.
	// Each label becomes "label:host" to use the host backend.
	runnerLabels := ""
	for i, label := range runner.Spec.Labels {
		if i > 0 {
			runnerLabels += ","
		}
		runnerLabels += label + ":host"
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      runner.Name,
			Namespace: runner.Namespace,
			Labels:    labels,
		},
		Spec: corev1.PodSpec{
			ServiceAccountName: "gitea-runner",
			RestartPolicy:      corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "act-runner",
					Image: "gitea/act_runner:0.2.13",
					Env: []corev1.EnvVar{
						{
							Name: "GITEA_RUNNER_REGISTRATION_TOKEN",
							ValueFrom: &corev1.EnvVarSource{
								SecretKeyRef: &corev1.SecretKeySelector{
									LocalObjectReference: corev1.LocalObjectReference{
										Name: secret.Name,
									},
									Key: envGiteaToken,
								},
							},
						},
						{
							Name:  "GITEA_INSTANCE_URL",
							Value: runner.Spec.GiteaConfigURL,
						},
						{
							Name:  "GITEA_RUNNER_EPHEMERAL",
							Value: "1",
						},
						{
							Name:  "GITEA_RUNNER_LABELS",
							Value: runnerLabels,
						},
						{
							Name:  "RUNNER_CAPACITY",
							Value: "1",
						},
					},
				},
			},
		},
	}

	log.Info("constructed Pod for runner", "pod", pod.Name, "labels", labels)
	return pod
}

// updateRunnerStatusFromPod updates the runner status based on the Pod phase with conflict retry.
func (r *EphemeralRunnerReconciler) updateRunnerStatusFromPod(ctx context.Context, runner *giteaactionsv1alpha1.EphemeralRunner, pod *corev1.Pod) {
	log := log.FromContext(ctx)

	now := metav1.Now()

	// Determine new status based on pod phase.
	newPhase := runner.Status.Phase
	newReason := runner.Status.Reason

	switch pod.Status.Phase {
	case corev1.PodPending:
		if runner.Status.Phase != giteaactionsv1alpha1.EphemeralRunnerPending {
			newPhase = giteaactionsv1alpha1.EphemeralRunnerPending
			newReason = fmt.Sprintf("Pod is pending: %s", pod.Status.Reason)
		}
	case corev1.PodRunning:
		if runner.Status.Phase != giteaactionsv1alpha1.EphemeralRunnerRunning {
			newPhase = giteaactionsv1alpha1.EphemeralRunnerRunning
			newReason = "Pod is running"
		}
	case corev1.PodSucceeded:
		newPhase = giteaactionsv1alpha1.EphemeralRunnerSucceeded
		newReason = "Pod completed successfully"
	case corev1.PodFailed:
		newPhase = giteaactionsv1alpha1.EphemeralRunnerFailed
		newReason = fmt.Sprintf("Pod failed: %s", pod.Status.Reason)
	default:
		newPhase = giteaactionsv1alpha1.EphemeralRunnerFailed
		newReason = fmt.Sprintf("Unknown pod phase: %s", pod.Status.Phase)
	}

	// Only update if something actually changed.
	if newPhase == runner.Status.Phase && newReason == runner.Status.Reason {
		return
	}

	// Retry on conflict: refetch and update to handle concurrent modifications.
	// Maximum 3 retries on conflict errors.
	for i := 0; i < 3; i++ {
		// Refetch the latest version before updating.
		latest := &giteaactionsv1alpha1.EphemeralRunner{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: runner.Namespace, Name: runner.Name}, latest); err != nil {
			log.Error(err, "failed to refetch runner for status update")
			return
		}

		// Update status on the latest version.
		latest.Status.Phase = newPhase
		latest.Status.Reason = newReason
		latest.Status.LastObservedTime = &now

		if err := r.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) {
				if i < 2 {
					continue // Retry on conflict
				}
			}
			log.Error(err, "failed to update EphemeralRunner status")
			return
		}
		return // Success
	}
}

// SetupWithManager sets up the controller with the Manager.
func (r *EphemeralRunnerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Create an event recorder for events.
	return ctrl.NewControllerManagedBy(mgr).
		For(&giteaactionsv1alpha1.EphemeralRunner{}).
		Owns(&corev1.Pod{}).
		Owns(&corev1.Secret{}).
		WithOptions(controller.Options{MaxConcurrentReconciles: 10}).
		WithEventFilter(predicate.GenerationChangedPredicate{}). // Ignore status-only updates
		Complete(r)
}
