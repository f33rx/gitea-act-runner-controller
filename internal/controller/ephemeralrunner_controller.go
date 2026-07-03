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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
	"github.com/f33rx/gitea-act-runner-controller/internal/metrics"
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
//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=gitearunnersets,verbs=get;list;watch
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

	// ADR 0008: stuck-vs-slow detection. Re-fetch to see PhaseStartTime as just written
	// by updateRunnerStatusFromPod (which operates on its own copy, not this one).
	latest := &giteaactionsv1alpha1.EphemeralRunner{}
	if err := r.Get(ctx, req.NamespacedName, latest); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		log.Error(err, "failed to refetch runner for timeout check")
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	if latest.Status.Phase == giteaactionsv1alpha1.EphemeralRunnerRunning {
		r.recordLogProgress(ctx, latest)
	}
	if timedOut, reason := r.checkTimeout(latest); timedOut {
		log.Info("runner timed out, deleting", "runner", latest.Name, "reason", reason)
		// ADR 0010: counted by which checkTimeout branch fired, using the same
		// phase discriminator checkTimeout itself switches on.
		if latest.Status.Phase == giteaactionsv1alpha1.EphemeralRunnerRunning {
			metrics.RunnerStalledTotal.WithLabelValues(latest.Spec.GiteaRunnerSetName, latest.Namespace).Inc()
		} else {
			metrics.RunnerPendingTimeoutTotal.WithLabelValues(latest.Spec.GiteaRunnerSetName, latest.Namespace).Inc()
		}
		if err := r.Delete(ctx, latest); err != nil && !apierrors.IsNotFound(err) {
			log.Error(err, "failed to delete timed-out runner")
			return ctrl.Result{Requeue: true}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
}

// checkTimeout implements ADR 0008 Decisions 3-4: a Running runner whose container log
// has not grown for StallWindow is presumed stuck (fail-and-teardown, no retry -- the
// job may already be claimed); a Pending runner with no phase change for PendingTimeout
// never claimed a job (pre-claim failure -- safe to delete and let the
// EphemeralRunnerSet recreate it, which is this ADR's retry-with-backoff path). Both are
// opt-in: a nil window/timeout on the runner (no GiteaRunnerSet override and no manager
// default) disables that check.
func (r *EphemeralRunnerReconciler) checkTimeout(runner *giteaactionsv1alpha1.EphemeralRunner) (bool, string) {
	switch runner.Status.Phase {
	case giteaactionsv1alpha1.EphemeralRunnerRunning:
		if runner.Spec.StallWindow == nil {
			return false, ""
		}
		// LastProgressTime (log-growth liveness) is the primary signal; recordLogProgress
		// keeps it moving whenever the container log grows. If log-checking is disabled
		// or has not observed anything yet, fall back to PhaseStartTime so stall
		// detection still degrades to the coarser pod-phase-only signal instead of
		// silently never firing.
		anchor := runner.Status.LastProgressTime
		if anchor == nil {
			anchor = runner.Status.PhaseStartTime
		}
		if anchor == nil {
			return false, ""
		}
		elapsed := time.Since(anchor.Time)
		if elapsed >= runner.Spec.StallWindow.Duration {
			return true, fmt.Sprintf("stalled: no log progress for %s (window %s)", elapsed.Round(time.Second), runner.Spec.StallWindow.Duration)
		}
	case giteaactionsv1alpha1.EphemeralRunnerPending:
		if runner.Spec.PendingTimeout == nil || runner.Status.PhaseStartTime == nil {
			return false, ""
		}
		elapsed := time.Since(runner.Status.PhaseStartTime.Time)
		if elapsed >= runner.Spec.PendingTimeout.Duration {
			return true, fmt.Sprintf("pending timeout: no progress for %s (timeout %s)", elapsed.Round(time.Second), runner.Spec.PendingTimeout.Duration)
		}
	}
	return false, ""
}

// recordLogProgress implements ADR 0008 Decision 2's heartbeat: find the Gitea job this
// runner claimed and compare its job-log Content-Length against the last observed size.
// Any growth means act_runner is actively streaming step output to Gitea and the job is
// presumed alive, resetting the stall clock. act_runner ships step output to Gitea via
// its own UpdateLog/gRPC protocol independent of the runner container's stdout, so this
// reads Gitea's job-log endpoint rather than kubectl logs. Errors (credential lookup,
// API failures, no matching job yet) are logged and swallowed -- a transient failure
// must not itself look like a stall; checkTimeout's PhaseStartTime fallback covers the
// gap.
func (r *EphemeralRunnerReconciler) recordLogProgress(ctx context.Context, runner *giteaactionsv1alpha1.EphemeralRunner) {
	log := log.FromContext(ctx)

	giteaClient, err := r.giteaClientForRunner(ctx, runner)
	if err != nil {
		log.V(1).Info("failed to build Gitea client for stall liveness check", "runner", runner.Name, "error", err.Error())
		return
	}

	jobs, err := giteaClient.ListOrgInProgressJobs(runner.Spec.OrgName)
	if err != nil {
		log.V(1).Info("failed to list in-progress jobs for stall liveness check", "runner", runner.Name, "error", err.Error())
		return
	}

	var jobURL string
	for _, job := range jobs {
		if job.RunnerID == runner.Status.RunnerID {
			jobURL = job.URL
			break
		}
	}
	if jobURL == "" {
		// The runner hasn't (yet) claimed a job Gitea reports as in-progress -- nothing
		// to measure growth against this reconcile.
		return
	}

	size, err := giteaClient.JobLogSize(jobURL)
	if err != nil {
		log.V(1).Info("failed to read job log size for stall liveness check", "runner", runner.Name, "error", err.Error())
		return
	}
	if size <= runner.Status.LastJobLogSize {
		return
	}

	now := metav1.Now()
	for i := 0; i < 3; i++ {
		latest := &giteaactionsv1alpha1.EphemeralRunner{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: runner.Namespace, Name: runner.Name}, latest); err != nil {
			log.Error(err, "failed to refetch runner for log-progress update")
			return
		}
		latest.Status.LastProgressTime = &now
		latest.Status.LastJobLogSize = size
		if err := r.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) && i < 2 {
				continue
			}
			log.Error(err, "failed to update runner log-progress status")
			return
		}
		runner.Status.LastProgressTime = &now
		runner.Status.LastJobLogSize = size
		return
	}
}

// giteaClientForRunner looks up the runner's parent GiteaRunnerSet to obtain its
// GiteaConfigSecretRef and builds a Gitea API client from that credential, mirroring
// the read pattern already used by EphemeralRunnerSetReconciler and the listener.
func (r *EphemeralRunnerReconciler) giteaClientForRunner(ctx context.Context, runner *giteaactionsv1alpha1.EphemeralRunner) (*gitea.Client, error) {
	grs := &giteaactionsv1alpha1.GiteaRunnerSet{}
	grsKey := types.NamespacedName{Namespace: runner.Namespace, Name: runner.Spec.GiteaRunnerSetName}
	if err := r.Get(ctx, grsKey, grs); err != nil {
		return nil, fmt.Errorf("failed to get parent GiteaRunnerSet %s: %w", grsKey, err)
	}

	secret := &corev1.Secret{}
	secretKey := types.NamespacedName{Namespace: runner.Namespace, Name: grs.Spec.GiteaConfigSecretRef.Name}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		return nil, fmt.Errorf("failed to get Gitea config Secret %s: %w", secretKey, err)
	}

	token := string(secret.Data[grs.Spec.GiteaConfigSecretRef.Key])
	if token == "" {
		return nil, fmt.Errorf("empty token in Gitea config Secret %s key %s", secretKey, grs.Spec.GiteaConfigSecretRef.Key)
	}

	return gitea.NewClient(grs.Spec.GiteaConfigURL, token), nil
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
			// ADR 0008: the resolved hard cap, kubelet-enforced independent of any
			// operator logic. nil (unset) means no cap, matching the resolver's
			// "no default configured" case.
			ActiveDeadlineSeconds: runner.Spec.ActiveDeadlineSeconds,
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

		// ADR 0008: PhaseStartTime marks entry into the phase, so it only moves when the
		// phase itself changes -- a Reason-only update (e.g. a new Pending message for
		// the same underlying wait) must not reset the pending/stall clock.
		phaseChanged := latest.Status.Phase != newPhase

		// Update status on the latest version.
		latest.Status.Phase = newPhase
		latest.Status.Reason = newReason
		latest.Status.LastObservedTime = &now
		if phaseChanged || latest.Status.PhaseStartTime == nil {
			latest.Status.PhaseStartTime = &now
		}

		if err := r.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) {
				if i < 2 {
					continue // Retry on conflict
				}
			}
			log.Error(err, "failed to update EphemeralRunner status")
			return
		}
		// ADR 0010: counted once, on the attempt that actually wrote the phase change
		// (not merely computed it), so a conflict-retry never double-counts.
		if phaseChanged {
			switch newPhase {
			case giteaactionsv1alpha1.EphemeralRunnerRunning:
				metrics.JobStartedTotal.WithLabelValues(latest.Spec.GiteaRunnerSetName, latest.Namespace).Inc()
			case giteaactionsv1alpha1.EphemeralRunnerSucceeded:
				metrics.JobCompletedTotal.WithLabelValues(latest.Spec.GiteaRunnerSetName, latest.Namespace, "succeeded").Inc()
			case giteaactionsv1alpha1.EphemeralRunnerFailed:
				metrics.JobCompletedTotal.WithLabelValues(latest.Spec.GiteaRunnerSetName, latest.Namespace, "failed").Inc()
			}
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
