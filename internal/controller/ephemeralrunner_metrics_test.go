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
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/metrics"
)

// ADR 0010: a Pod transitioning to Running must count exactly one job-started event,
// labeled by the runner's GiteaRunnerSet and namespace.
func TestUpdateRunnerStatusFromPod_CountsJobStarted(t *testing.T) {
	scheme := newTestScheme(t)
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-started", Namespace: "gitea-runners"},
		Spec:       giteaactionsv1alpha1.EphemeralRunnerSpec{GiteaRunnerSetName: "started-set"},
		Status:     giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerPending},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(runner).WithObjects(runner).Build()
	r := &EphemeralRunnerReconciler{Client: c, Scheme: scheme}

	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodRunning}}
	r.updateRunnerStatusFromPod(context.Background(), runner, pod)

	if got := testutil.ToFloat64(metrics.JobStartedTotal.WithLabelValues("started-set", "gitea-runners")); got != 1 {
		t.Errorf("job_started_total = %v, want 1", got)
	}
}

// A Pod transitioning to Succeeded/Failed must count exactly one job-completed event,
// labeled by result.
func TestUpdateRunnerStatusFromPod_CountsJobCompleted(t *testing.T) {
	for _, tc := range []struct {
		name       string
		podPhase   corev1.PodPhase
		wantResult string
	}{
		{"succeeded", corev1.PodSucceeded, "succeeded"},
		{"failed", corev1.PodFailed, "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newTestScheme(t)
			runner := &giteaactionsv1alpha1.EphemeralRunner{
				ObjectMeta: metav1.ObjectMeta{Name: "runner-" + tc.name, Namespace: "gitea-runners"},
				Spec:       giteaactionsv1alpha1.EphemeralRunnerSpec{GiteaRunnerSetName: "completed-set-" + tc.name},
				Status:     giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerRunning},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(runner).WithObjects(runner).Build()
			r := &EphemeralRunnerReconciler{Client: c, Scheme: scheme}

			pod := &corev1.Pod{Status: corev1.PodStatus{Phase: tc.podPhase}}
			r.updateRunnerStatusFromPod(context.Background(), runner, pod)

			if got := testutil.ToFloat64(metrics.JobCompletedTotal.WithLabelValues("completed-set-"+tc.name, "gitea-runners", tc.wantResult)); got != 1 {
				t.Errorf("job_completed_total{result=%s} = %v, want 1", tc.wantResult, got)
			}
		})
	}
}

// A Reason-only update (no phase change) must not double-count -- the counters are
// keyed to the same phaseChanged signal that gates PhaseStartTime (ADR 0008).
func TestUpdateRunnerStatusFromPod_ReasonOnlyChangeDoesNotCount(t *testing.T) {
	scheme := newTestScheme(t)
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-reason-only", Namespace: "gitea-runners"},
		Spec:       giteaactionsv1alpha1.EphemeralRunnerSpec{GiteaRunnerSetName: "reason-only-set"},
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:  giteaactionsv1alpha1.EphemeralRunnerPending,
			Reason: "Pod is pending: some other message",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(runner).WithObjects(runner).Build()
	r := &EphemeralRunnerReconciler{Client: c, Scheme: scheme}

	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodPending, Reason: "different reason"}}
	r.updateRunnerStatusFromPod(context.Background(), runner, pod)

	if got := testutil.ToFloat64(metrics.JobStartedTotal.WithLabelValues("reason-only-set", "gitea-runners")); got != 0 {
		t.Errorf("job_started_total = %v, want 0 (phase did not change to Running)", got)
	}
}

// A hard-cap (activeDeadlineSeconds) kill arrives as PodFailed with reason
// DeadlineExceeded. It must be distinguishable from an ordinary job failure, otherwise
// the cap cannot be tuned from metrics (ADR 0008 open question 1).
func TestUpdateRunnerStatusFromPod_LabelsDeadlineExceeded(t *testing.T) {
	scheme := newTestScheme(t)
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-dl", Namespace: "gitea-runners"},
		Spec:       giteaactionsv1alpha1.EphemeralRunnerSpec{GiteaRunnerSetName: "dl-set"},
		Status:     giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerRunning},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(runner).WithObjects(runner).Build()
	r := &EphemeralRunnerReconciler{Client: c, Scheme: scheme}

	pod := &corev1.Pod{Status: corev1.PodStatus{Phase: corev1.PodFailed, Reason: deadlineExceededReason}}
	r.updateRunnerStatusFromPod(context.Background(), runner, pod)

	if got := testutil.ToFloat64(metrics.JobCompletedTotal.WithLabelValues("dl-set", "gitea-runners", resultDeadlineExceeded)); got != 1 {
		t.Errorf("completed_total{result=deadline_exceeded} = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.JobCompletedTotal.WithLabelValues("dl-set", "gitea-runners", resultFailed)); got != 0 {
		t.Errorf("completed_total{result=failed} = %v, want 0", got)
	}
}

// A pod deleted out from under a Running runner is recreated by Reconcile. The phase
// must not rewind to Pending: the replacement pod reaching Running would then look like
// a second job start and inflate started_total, while the first job's outcome is never
// counted at all.
func TestReconcile_PodRecreationDoesNotRewindRunningPhase(t *testing.T) {
	scheme := newTestScheme(t)
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-recreate", Namespace: "gitea-runners"},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			GiteaRunnerSetName: "recreate-set",
			RegistrationToken:  "tok",
		},
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerRunning},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(runner).WithObjects(runner).Build()
	r := &EphemeralRunnerReconciler{Client: c, Scheme: scheme}

	key := types.NamespacedName{Namespace: runner.Namespace, Name: runner.Name}
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		p := &corev1.Pod{}
		if err := c.Get(context.Background(), key, p); err == nil {
			break
		}
	}

	got := &giteaactionsv1alpha1.EphemeralRunner{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get runner: %v", err)
	}
	if got.Status.Phase != giteaactionsv1alpha1.EphemeralRunnerRunning {
		t.Fatalf("phase = %q after pod recreation, want Running (rewinding double-counts started_total)", got.Status.Phase)
	}
}

// A stall kill deletes the runner outright, so it never transitions through a terminal
// phase and updateRunnerStatusFromPod never counts it. The completion must be counted at
// the kill site instead, or started_total stays permanently ahead of completed_total.
func TestReconcile_StallKillCountsCompletion(t *testing.T) {
	scheme := newTestScheme(t)
	stale := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "runner-stall",
			Namespace:  "gitea-runners",
			Finalizers: []string{finalizerEphemeralRunner},
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			GiteaRunnerSetName: "stall-set",
			OrgName:            "org",
			RegistrationToken:  "tok",
			// The liveness read will fail against the unreachable stub URL below, which
			// is a hard error, so Reconcile would fail open and skip the stall check. A
			// nil window instead leaves checkTimeout on its PhaseStartTime fallback,
			// which is already past due -- the stall-kill path this test targets.
			StallWindow: &metav1.Duration{Duration: 30 * time.Second},
		},
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:          giteaactionsv1alpha1.EphemeralRunnerRunning,
			PodName:        "runner-stall",
			PhaseStartTime: &stale,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-stall", Namespace: "gitea-runners"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	// recordLogProgress builds a Gitea client from the parent set + secret. Point it at a
	// stub that returns an empty in-progress job list: no matching job means it returns
	// nil (not an error), so Reconcile proceeds to checkTimeout instead of failing open.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jobs":[],"total_count":0}`))
	}))
	defer srv.Close()
	grs := &giteaactionsv1alpha1.GiteaRunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "stall-set", Namespace: "gitea-runners"},
		Spec: giteaactionsv1alpha1.GiteaRunnerSetSpec{
			GiteaConfigURL:       srv.URL,
			GiteaConfigSecretRef: giteaactionsv1alpha1.SecretKeySelector{Name: "creds", Key: "token"},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "gitea-runners"},
		Data:       map[string][]byte{"token": []byte("tok")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(runner).
		WithObjects(runner, pod, grs, secret).Build()
	r := &EphemeralRunnerReconciler{Client: c, Scheme: scheme}

	before := testutil.ToFloat64(metrics.JobCompletedTotal.WithLabelValues("stall-set", "gitea-runners", resultStalled))
	key := types.NamespacedName{Namespace: runner.Namespace, Name: runner.Name}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	after := testutil.ToFloat64(metrics.JobCompletedTotal.WithLabelValues("stall-set", "gitea-runners", resultStalled))

	if after != before+1 {
		t.Fatalf("completed_total{result=stalled} = %v, want %v", after, before+1)
	}
	if got := testutil.ToFloat64(metrics.RunnerStalledTotal.WithLabelValues("stall-set", "gitea-runners")); got < 1 {
		t.Errorf("runner_stalled_total = %v, want >= 1", got)
	}
}
