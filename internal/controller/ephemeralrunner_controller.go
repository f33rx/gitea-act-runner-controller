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
	"errors"
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
	sigsjson "sigs.k8s.io/json"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
	"github.com/f33rx/gitea-act-runner-controller/internal/metrics"
)

const (
	finalizerEphemeralRunner = "giteaactions.blackrabbitpursuits.com/ephemeral-runner"
	secretSuffixToken        = "-token"
	envGiteaToken            = "GITEA_TOKEN"
	envGiteaServerURL        = "GITEA_SERVER_URL"
	envGiteaEphemeral        = "GITEA_RUNNER_EPHEMERAL"
	envGiteaRunnerName       = "GITEA_RUNNER_NAME"

	// job_completed_total result labels (ADR 0010). Kept coarse and closed-set so
	// cardinality stays bounded.
	resultSucceeded        = "succeeded"
	resultFailed           = "failed"
	resultStalled          = "stalled"
	resultDeadlineExceeded = "deadline_exceeded"

	// The kubelet's pod.status.reason for an activeDeadlineSeconds kill.
	deadlineExceededReason = "DeadlineExceeded"
	envGiteaRunnerOrgName  = "GITEA_RUNNER_ORG_NAME"
)

// DefaultRunnerServiceAccountName matches config/samples and the e2e harness so a
// bare binary keeps working.
const DefaultRunnerServiceAccountName = "gitea-runner"

// DefaultRunnerImage is the runner container image when neither the pod template nor
// --default-runner-image names one.
const DefaultRunnerImage = "ghcr.io/f33rx/gitea-act-runner:latest"

// EphemeralRunnerReconciler reconciles an EphemeralRunner object.
type EphemeralRunnerReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// TeardownCredential locates the org-write Secret the finalizer uses to
	// deregister runners. Zero value falls back to the Default* constants.
	TeardownCredential types.NamespacedName
	// RunnerServiceAccountName is set on runner Pods whose template names none. Empty
	// falls back to DefaultRunnerServiceAccountName.
	RunnerServiceAccountName string
	// RunnerImage is used when the template's runner container has no image. Empty
	// falls back to DefaultRunnerImage.
	RunnerImage string
	// RunnerResources fills in resources the template's runner container leaves unset.
	RunnerResources corev1.ResourceRequirements
}

func (r *EphemeralRunnerReconciler) runnerServiceAccountName() string {
	if r.RunnerServiceAccountName != "" {
		return r.RunnerServiceAccountName
	}
	return DefaultRunnerServiceAccountName
}

func (r *EphemeralRunnerReconciler) runnerImage() string {
	if r.RunnerImage != "" {
		return r.RunnerImage
	}
	return DefaultRunnerImage
}

//+kubebuilder:rbac:groups=giteaactions.blackrabbitpursuits.com,resources=ephemeralrunners,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups=giteaactions.blackrabbitpursuits.com,resources=ephemeralrunners/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=giteaactions.blackrabbitpursuits.com,resources=ephemeralrunners/finalizers,verbs=update
//+kubebuilder:rbac:groups=giteaactions.blackrabbitpursuits.com,resources=gitearunnersets,verbs=get;list;watch
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
			pod, err = r.constructPod(ctx, runner, secret)
			if err != nil {
				log.Error(err, "failed to construct Pod")
				return ctrl.Result{}, err
			}
			if err := controllerutil.SetControllerReference(runner, pod, r.Scheme); err != nil {
				log.Error(err, "failed to set controller reference on Pod")
				return ctrl.Result{}, err
			}
			if err := r.Create(ctx, pod); err != nil {
				log.Error(err, "failed to create Pod")
				return ctrl.Result{}, err
			}
			log.Info("created Pod", "pod", podName)
			now := metav1.Now()
			runner.Status.PodName = pod.Name
			// Only a first-time creation starts the Pending clock. If the runner was
			// already Running its pod was deleted out from under it (node drain, manual
			// delete): rewinding the phase would make the replacement pod's transition to
			// Running look like a second job start and double-count started_total, while
			// the first job's outcome is never counted at all.
			if runner.Status.Phase == "" || runner.Status.Phase == giteaactionsv1alpha1.EphemeralRunnerPending {
				runner.Status.Phase = giteaactionsv1alpha1.EphemeralRunnerPending
				runner.Status.Reason = "Pod created"
			} else {
				runner.Status.Reason = "Pod recreated"
			}
			// ADR 0008: the Pending clock starts here. updateRunnerStatusFromPod only
			// writes on a phase/reason change, so if this write lands before the first
			// Pod event is observed the runner stays Pending with no later transition
			// and the pending timeout would never arm.
			runner.Status.PhaseStartTime = &now
			runner.Status.LastObservedTime = &now
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
	if latest.Status.Phase == giteaactionsv1alpha1.EphemeralRunnerRunning && latest.Spec.StallWindow != nil {
		if err := r.recordLogProgress(ctx, latest); err != nil {
			// Fail open: with no usable liveness signal the PhaseStartTime fallback would
			// condemn every job longer than the stall window (a credential without
			// read:repository does exactly that), and ADR 0008 prefers a missed stall
			// over killing a live job. Logged at Info so a persistent misconfiguration is
			// visible.
			log.Info("stall liveness signal unavailable, skipping stall check", "runner", latest.Name, "error", err.Error())
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
	}
	if timedOut, reason := r.checkTimeout(latest); timedOut {
		log.Info("runner timed out, deleting", "runner", latest.Name, "reason", reason)
		// ADR 0010: counted by which checkTimeout branch fired, using the same
		// phase discriminator checkTimeout itself switches on. The runner is deleted
		// here rather than transitioning through a terminal phase, so
		// updateRunnerStatusFromPod never sees it again -- count the completion here or
		// a stalled runner inflates started_total with no matching completed_total.
		if latest.Status.Phase == giteaactionsv1alpha1.EphemeralRunnerRunning {
			metrics.RunnerStalledTotal.WithLabelValues(latest.Spec.GiteaRunnerSetName, latest.Namespace).Inc()
			metrics.JobCompletedTotal.WithLabelValues(latest.Spec.GiteaRunnerSetName, latest.Namespace, resultStalled).Inc()
		} else {
			// A Pending runner never reached Running, so it was never counted as started
			// and must not be counted as completed.
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
// API failures, an unreadable log size, a status write that would not land) are
// returned so the caller skips the stall check for that cycle: a transient failure must
// not itself look like a stall. Only the "no matching in-progress job yet" case returns
// nil, leaving checkTimeout on its PhaseStartTime fallback.
func (r *EphemeralRunnerReconciler) recordLogProgress(ctx context.Context, runner *giteaactionsv1alpha1.EphemeralRunner) error {
	log := log.FromContext(ctx)

	giteaClient, err := r.giteaClientForRunner(ctx, runner)
	if err != nil {
		return fmt.Errorf("build Gitea client: %w", err)
	}

	jobs, err := giteaClient.ListOrgInProgressJobs(ctx, runner.Spec.OrgName)
	if err != nil {
		return fmt.Errorf("list in-progress jobs: %w", err)
	}

	// act_runner registers under the EphemeralRunner name (constructPod pins
	// GITEA_RUNNER_NAME), and Status.RunnerID is never populated, so the name is the
	// reliable key. Names are reused across generations and a job whose runner was torn
	// down stays in_progress until Gitea's zombie reaper runs, so among name matches take
	// the most recently started one. Parse rather than compare strings: Gitea renders
	// started_at in its configured UI timezone, whose numeric offset shifts across DST,
	// and lexical order inverts at the transition.
	var jobURL string
	var jobStartedAt time.Time
	for _, job := range jobs {
		if job.RunnerName != runner.Name {
			continue
		}
		started, err := time.Parse(time.RFC3339, job.StartedAt)
		if err != nil {
			log.V(1).Info("skipping in-progress job with unparseable started_at",
				"runner", runner.Name, "startedAt", job.StartedAt)
			continue
		}
		if jobURL == "" || started.After(jobStartedAt) {
			jobURL = job.URL
			jobStartedAt = started
		}
	}
	if jobURL == "" {
		// The runner hasn't (yet) claimed a job Gitea reports as in-progress -- nothing
		// to measure growth against this reconcile.
		return nil
	}

	size, err := giteaClient.JobLogSize(ctx, jobURL)
	if err != nil {
		return fmt.Errorf("read job log size: %w", err)
	}
	if size <= runner.Status.LastJobLogSize {
		return nil
	}

	now := metav1.Now()
	for i := 0; i < 3; i++ {
		latest := &giteaactionsv1alpha1.EphemeralRunner{}
		if err := r.Get(ctx, types.NamespacedName{Namespace: runner.Namespace, Name: runner.Name}, latest); err != nil {
			return fmt.Errorf("refetch runner for log-progress update: %w", err)
		}
		latest.Status.LastProgressTime = &now
		latest.Status.LastJobLogSize = size
		if err := r.Status().Update(ctx, latest); err != nil {
			if apierrors.IsConflict(err) && i < 2 {
				continue
			}
			// Observed growth we could not persist: report it so the caller skips the
			// stall check rather than measuring against a stale anchor.
			return fmt.Errorf("update runner log-progress status: %w", err)
		}
		runner.Status.LastProgressTime = &now
		runner.Status.LastJobLogSize = size
		return nil
	}
	return nil
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

	// Step 1: Deregister the runner from Gitea. This is the PRIMARY teardown path, and
	// the finalizer must do it rather than leave it to the sweep: once the
	// GiteaRunnerSet is gone (garc-6dx) the sweep has no org to discover.
	if runner.Status.RunnerID > 0 || runner.Spec.OrgName != "" {
		if err := r.deregisterFromGitea(ctx, runner); err != nil {
			log.Error(err, "finalizer: deregistration failed, will retry", "runner", runner.Name)
			return ctrl.Result{Requeue: true}, err
		}
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

// deregisterFromGitea removes every Gitea registration belonging to the runner using
// the org-write teardown credential. Any error means at least one registration may
// remain and the caller must retry; deregistration is idempotent, so a retry after a
// partial success is safe.
func (r *EphemeralRunnerReconciler) deregisterFromGitea(ctx context.Context, runner *giteaactionsv1alpha1.EphemeralRunner) error {
	log := log.FromContext(ctx)

	token, err := readTeardownToken(ctx, r, r.TeardownCredential)
	if err != nil {
		return err
	}
	client := gitea.NewClient(runner.Spec.GiteaConfigURL, token)

	ids, err := r.registrationsToDeregister(ctx, client, runner)
	if err != nil {
		return fmt.Errorf("resolve registration for %s: %w", runner.Name, err)
	}
	for _, id := range ids {
		if err := client.DeregisterOrgRunner(ctx, runner.Spec.OrgName, id); err != nil {
			return err
		}
		log.Info("deregistered runner from Gitea", "runner", runner.Name, "runnerId", id)
	}
	return nil
}

// registrationsToDeregister returns the Gitea runner IDs to delete: the recorded
// RunnerID when there is one, otherwise every ephemeral registration in the org
// carrying this runner's name. Status.RunnerID is never populated today (act_runner
// registers under the CR name, which constructPod pins via GITEA_RUNNER_NAME, and
// nothing reports the ID back), so the by-name path is the one that runs. Names are
// reused across generations, so a stale registration from an earlier generation is
// reclaimed along with the current one. If Status.RunnerID is ever populated it must
// come from this org's runner list: Gitea answers 404 for an ID from any other scope
// and DeregisterOrgRunner treats that as already gone.
func (r *EphemeralRunnerReconciler) registrationsToDeregister(ctx context.Context, client *gitea.Client, runner *giteaactionsv1alpha1.EphemeralRunner) ([]int64, error) {
	if runner.Status.RunnerID > 0 {
		return []int64{runner.Status.RunnerID}, nil
	}
	runners, err := client.ListOrgRunners(ctx, runner.Spec.OrgName)
	if err != nil {
		return nil, err
	}
	var ids []int64
	for _, rr := range runners {
		if rr.Ephemeral && rr.Name == runner.Name {
			ids = append(ids, rr.ID)
		}
	}
	return ids, nil
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

// runnerContainerName names the runner container garc adds to a template that has none.
const runnerContainerName = "act-runner"

// decodeRunnerTemplate strictly decodes a GiteaRunnerSet pod template. The CRD preserves
// unknown fields, so a misspelled field is only caught here. At most one regular
// container is allowed: a second one keeps the pod Running after act_runner exits, so
// teardown never fires; sidecars must be native (initContainers, restartPolicy Always).
func decodeRunnerTemplate(raw *runtime.RawExtension) (*corev1.PodTemplateSpec, error) {
	tmpl := &corev1.PodTemplateSpec{}
	if raw == nil || len(raw.Raw) == 0 {
		return tmpl, nil
	}
	strictErrs, err := sigsjson.UnmarshalStrict(raw.Raw, tmpl, sigsjson.DisallowDuplicateFields, sigsjson.DisallowUnknownFields)
	if err == nil && len(strictErrs) > 0 {
		err = errors.Join(strictErrs...)
	}
	if err != nil {
		return nil, fmt.Errorf("invalid runner pod template: %w", err)
	}
	if n := len(tmpl.Spec.Containers); n > 1 {
		return nil, fmt.Errorf("invalid runner pod template: %d containers, want at most 1 (put sidecars in initContainers with restartPolicy: Always)", n)
	}
	return tmpl, nil
}

// applyDefaultResources copies each default resource the container sets neither a
// request nor a limit for. A resource the template names is left whole: filling only
// its missing side could put a limit below the template's request.
func applyDefaultResources(dst *corev1.ResourceRequirements, def corev1.ResourceRequirements) {
	names := map[corev1.ResourceName]bool{}
	for n := range def.Requests {
		names[n] = true
	}
	for n := range def.Limits {
		names[n] = true
	}
	for n := range names {
		if _, ok := dst.Requests[n]; ok {
			continue
		}
		if _, ok := dst.Limits[n]; ok {
			continue
		}
		if q, ok := def.Requests[n]; ok {
			if dst.Requests == nil {
				dst.Requests = corev1.ResourceList{}
			}
			dst.Requests[n] = q.DeepCopy()
		}
		if q, ok := def.Limits[n]; ok {
			if dst.Limits == nil {
				dst.Limits = corev1.ResourceList{}
			}
			dst.Limits[n] = q.DeepCopy()
		}
	}
}

// constructPod builds the runner Pod from the runner's template snapshot, then forces
// the fields garc depends on: the pod is found by name and labels, restartPolicy Never
// keeps it ephemeral, and the act_runner env vars carry registration and identity.
func (r *EphemeralRunnerReconciler) constructPod(ctx context.Context, runner *giteaactionsv1alpha1.EphemeralRunner, secret *corev1.Secret) (*corev1.Pod, error) {
	log := log.FromContext(ctx)

	tmpl, err := decodeRunnerTemplate(runner.Spec.Template)
	if err != nil {
		return nil, err
	}

	labels := map[string]string{}
	for k, v := range tmpl.Labels {
		labels[k] = v
	}
	labels["app"] = "gitea-runner"
	labels["ephemeral-runner"] = runner.Name
	labels["gitearunner-set"] = runner.Spec.GiteaRunnerSetName

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
			Name:        runner.Name,
			Namespace:   runner.Namespace,
			Labels:      labels,
			Annotations: tmpl.Annotations,
		},
		Spec: tmpl.Spec,
	}
	pod.Spec.RestartPolicy = corev1.RestartPolicyNever
	// ADR 0008: the resolved hard cap, kubelet-enforced independent of any operator
	// logic. nil (unset) means no cap, matching the resolver's "no default configured"
	// case; a template value is overridden either way.
	pod.Spec.ActiveDeadlineSeconds = runner.Spec.ActiveDeadlineSeconds
	if pod.Spec.ServiceAccountName == "" {
		pod.Spec.ServiceAccountName = r.runnerServiceAccountName()
	}

	if len(pod.Spec.Containers) == 0 {
		pod.Spec.Containers = []corev1.Container{{Name: runnerContainerName}}
	}
	c := &pod.Spec.Containers[0]
	if c.Image == "" {
		c.Image = r.runnerImage()
	}
	applyDefaultResources(&c.Resources, r.RunnerResources)

	garcEnv := []corev1.EnvVar{
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
			// Pin the registered name instead of letting act_runner fall back to the pod
			// hostname: the kubelet truncates hostnames at 63 chars, and
			// recordLogProgress matches in-progress jobs by this name.
			Name:  envGiteaRunnerName,
			Value: runner.Name,
		},
		{
			Name:  "GITEA_RUNNER_LABELS",
			Value: runnerLabels,
		},
		{
			Name:  "RUNNER_CAPACITY",
			Value: "1",
		},
	}
	owned := make(map[string]bool, len(garcEnv))
	for _, e := range garcEnv {
		owned[e.Name] = true
	}
	env := make([]corev1.EnvVar, 0, len(c.Env)+len(garcEnv))
	for _, e := range c.Env {
		if !owned[e.Name] {
			env = append(env, e)
		}
	}
	env = append(env, garcEnv...)
	c.Env = env

	log.Info("constructed Pod for runner", "pod", pod.Name, "labels", labels, "image", c.Image)
	return pod, nil
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

	podReason := pod.Status.Reason

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
				metrics.JobCompletedTotal.WithLabelValues(latest.Spec.GiteaRunnerSetName, latest.Namespace, resultSucceeded).Inc()
			case giteaactionsv1alpha1.EphemeralRunnerFailed:
				// The kubelet reports an activeDeadlineSeconds kill as DeadlineExceeded
				// (ADR 0008's hard cap). Without its own label it is indistinguishable
				// from an ordinary job failure, and the cap cannot be tuned from metrics.
				result := resultFailed
				if podReason == deadlineExceededReason {
					result = resultDeadlineExceeded
				}
				metrics.JobCompletedTotal.WithLabelValues(latest.Spec.GiteaRunnerSetName, latest.Namespace, result).Inc()
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
