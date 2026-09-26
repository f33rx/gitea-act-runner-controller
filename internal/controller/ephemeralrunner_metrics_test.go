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
	"net/http"
	"net/http/httptest"
	"strings"
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
	cases := []struct {
		name        string
		claimed     bool
		wantResult  string
		wantStalled float64
	}{
		// A claimed job whose log stopped growing past the window is a stall.
		{"claimed job with no log progress", true, resultStalled, 1},
		// garc-bds: a runner that never claimed a job is reaped by the pre-claim
		// timeout as idle, never counted as a stall.
		{"idle runner never claimed a job", false, resultIdle, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newTestScheme(t)
			stale := metav1.NewTime(time.Now().Add(-5 * time.Minute))
			set := "stall-set-" + tc.wantResult
			const logSize = 10
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
				if strings.HasSuffix(req.URL.Path, "/logs") {
					_, _ = w.Write([]byte(strings.Repeat("x", logSize)))
					return
				}
				w.Header().Set("Content-Type", "application/json")
				if !tc.claimed {
					_, _ = w.Write([]byte(`{"jobs":[],"total_count":0}`))
					return
				}
				_, _ = fmt.Fprintf(w, `{"jobs":[{"id":1,"url":"http://gitea/org/repo/actions/runs/1/jobs/0","status":"in_progress","runner_name":"runner-stall","started_at":%q}],"total_count":1}`,
					stale.UTC().Format(time.RFC3339))
			}))
			defer srv.Close()

			runner := &giteaactionsv1alpha1.EphemeralRunner{
				ObjectMeta: metav1.ObjectMeta{
					Name:       "runner-stall",
					Namespace:  "gitea-runners",
					Finalizers: []string{finalizerEphemeralRunner},
				},
				Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
					GiteaRunnerSetName: set,
					OrgName:            "org",
					RegistrationToken:  "tok",
					StallWindow:        &metav1.Duration{Duration: 30 * time.Second},
					PendingTimeout:     &metav1.Duration{Duration: 30 * time.Second},
				},
				Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
					Phase:          giteaactionsv1alpha1.EphemeralRunnerRunning,
					PodName:        "runner-stall",
					PhaseStartTime: &stale,
				},
			}
			if tc.claimed {
				runner.Status.JobRef = "http://gitea/org/repo/actions/runs/1/jobs/0"
				runner.Status.LastJobLogSize = logSize
				runner.Status.LastProgressTime = &stale
			}
			pod := &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{Name: "runner-stall", Namespace: "gitea-runners"},
				Status:     corev1.PodStatus{Phase: corev1.PodRunning},
			}
			grs := &giteaactionsv1alpha1.GiteaRunnerSet{
				ObjectMeta: metav1.ObjectMeta{Name: set, Namespace: "gitea-runners"},
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

			before := testutil.ToFloat64(metrics.JobCompletedTotal.WithLabelValues(set, "gitea-runners", tc.wantResult))
			key := types.NamespacedName{Namespace: runner.Namespace, Name: runner.Name}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}
			after := testutil.ToFloat64(metrics.JobCompletedTotal.WithLabelValues(set, "gitea-runners", tc.wantResult))
			if after != before+1 {
				t.Fatalf("completed_total{result=%s} = %v, want %v", tc.wantResult, after, before+1)
			}
			if got := testutil.ToFloat64(metrics.RunnerStalledTotal.WithLabelValues(set, "gitea-runners")); got != tc.wantStalled {
				t.Errorf("runner_stalled_total = %v, want %v", got, tc.wantStalled)
			}
		})
	}
}

// The first sighting of a runner's job is its claim: it records JobRef and starts the
// stall clock at that moment, so a runner that sat idle before claiming is not killed.
func TestReconcile_FirstSightingRecordsClaim(t *testing.T) {
	scheme := newTestScheme(t)
	idleSince := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	const jobURL = "http://gitea/org/repo/actions/runs/1/jobs/0"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if strings.HasSuffix(req.URL.Path, "/logs") {
			_, _ = w.Write([]byte("x"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"jobs":[{"id":1,"url":%q,"status":"in_progress","runner_name":"runner-claim","started_at":%q}],"total_count":1}`,
			jobURL, time.Now().UTC().Format(time.RFC3339))
	}))
	defer srv.Close()

	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-claim", Namespace: "gitea-runners", Finalizers: []string{finalizerEphemeralRunner}},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			GiteaRunnerSetName: "claim-set",
			OrgName:            "org",
			StallWindow:        &metav1.Duration{Duration: time.Minute},
			PendingTimeout:     &metav1.Duration{Duration: 10 * time.Minute},
		},
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:          giteaactionsv1alpha1.EphemeralRunnerRunning,
			PodName:        "runner-claim",
			PhaseStartTime: &idleSince,
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-claim", Namespace: "gitea-runners"},
		Status:     corev1.PodStatus{Phase: corev1.PodRunning},
	}
	grs := &giteaactionsv1alpha1.GiteaRunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "claim-set", Namespace: "gitea-runners"},
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

	key := types.NamespacedName{Namespace: runner.Namespace, Name: runner.Name}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	got := &giteaactionsv1alpha1.EphemeralRunner{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("runner was deleted, want it kept: %v", err)
	}
	if got.Status.JobRef != jobURL {
		t.Fatalf("JobRef = %q, want %q", got.Status.JobRef, jobURL)
	}
	if got.Status.LastProgressTime == nil || time.Since(got.Status.LastProgressTime.Time) > time.Minute {
		t.Fatalf("LastProgressTime = %v, want the claim time", got.Status.LastProgressTime)
	}
}
