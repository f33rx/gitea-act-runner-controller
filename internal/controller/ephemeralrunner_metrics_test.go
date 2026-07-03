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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
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
