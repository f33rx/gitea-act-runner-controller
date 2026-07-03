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
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/metrics"
)

// ADR 0010: recordFleetMetrics resets gauges to the current owned-runner snapshot on
// every call rather than incrementing/decrementing per-event.
func TestRecordFleetMetrics_PhaseCountsMatchOwnedRunners(t *testing.T) {
	r := &EphemeralRunnerSetReconciler{}

	ers := &giteaactionsv1alpha1.EphemeralRunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "test-set", Namespace: "gitea-runners"},
		Spec:       giteaactionsv1alpha1.EphemeralRunnerSetSpec{Replicas: 3},
		Status:     giteaactionsv1alpha1.EphemeralRunnerSetStatus{ReadyReplicas: 2, AvailableReplicas: 2},
	}
	grs := &giteaactionsv1alpha1.GiteaRunnerSet{
		Spec: giteaactionsv1alpha1.GiteaRunnerSetSpec{MinRunners: 1, MaxRunners: 5},
	}
	owned := &giteaactionsv1alpha1.EphemeralRunnerList{
		Items: []giteaactionsv1alpha1.EphemeralRunner{
			{Status: giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerRunning}},
			{Status: giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerRunning}},
			{Status: giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerPending}},
		},
	}

	r.recordFleetMetrics(ers, grs, owned)

	if got := testutil.ToFloat64(metrics.EphemeralRunnerPhaseCount.WithLabelValues("test-set", "gitea-runners", "Running")); got != 2 {
		t.Errorf("Running phase count = %v, want 2", got)
	}
	if got := testutil.ToFloat64(metrics.EphemeralRunnerPhaseCount.WithLabelValues("test-set", "gitea-runners", "Pending")); got != 1 {
		t.Errorf("Pending phase count = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.EphemeralRunnerPhaseCount.WithLabelValues("test-set", "gitea-runners", "Succeeded")); got != 0 {
		t.Errorf("Succeeded phase count = %v, want 0", got)
	}
	if got := testutil.ToFloat64(metrics.EphemeralRunnerSetDesired.WithLabelValues("test-set", "gitea-runners")); got != 3 {
		t.Errorf("desired = %v, want 3", got)
	}
	if got := testutil.ToFloat64(metrics.EphemeralRunnerSetMin.WithLabelValues("test-set", "gitea-runners")); got != 1 {
		t.Errorf("min = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.EphemeralRunnerSetMax.WithLabelValues("test-set", "gitea-runners")); got != 5 {
		t.Errorf("max = %v, want 5", got)
	}
	if got := testutil.ToFloat64(metrics.EphemeralRunnerSetReady.WithLabelValues("test-set", "gitea-runners")); got != 2 {
		t.Errorf("ready = %v, want 2", got)
	}
	if got := testutil.ToFloat64(metrics.EphemeralRunnerSetAvailable.WithLabelValues("test-set", "gitea-runners")); got != 2 {
		t.Errorf("available = %v, want 2", got)
	}
}

// A phase gauge must reflect a shrinking fleet too -- re-recording with fewer owned
// runners must lower the count, not just ratchet upward (proves reset-not-increment).
func TestRecordFleetMetrics_ResetsDownOnShrink(t *testing.T) {
	r := &EphemeralRunnerSetReconciler{}
	ers := &giteaactionsv1alpha1.EphemeralRunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "shrink-set", Namespace: "gitea-runners"},
	}
	grs := &giteaactionsv1alpha1.GiteaRunnerSet{}

	r.recordFleetMetrics(ers, grs, &giteaactionsv1alpha1.EphemeralRunnerList{
		Items: []giteaactionsv1alpha1.EphemeralRunner{
			{Status: giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerRunning}},
			{Status: giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerRunning}},
		},
	})
	if got := testutil.ToFloat64(metrics.EphemeralRunnerPhaseCount.WithLabelValues("shrink-set", "gitea-runners", "Running")); got != 2 {
		t.Fatalf("initial Running count = %v, want 2", got)
	}

	r.recordFleetMetrics(ers, grs, &giteaactionsv1alpha1.EphemeralRunnerList{
		Items: []giteaactionsv1alpha1.EphemeralRunner{
			{Status: giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerRunning}},
		},
	})
	if got := testutil.ToFloat64(metrics.EphemeralRunnerPhaseCount.WithLabelValues("shrink-set", "gitea-runners", "Running")); got != 1 {
		t.Errorf("after shrink Running count = %v, want 1 (gauge must reset down, not just up)", got)
	}
}

// An empty Status.Phase (a brand-new EphemeralRunner whose status hasn't been written
// yet) must count as Pending, matching the scale-down loop's own isIdle treatment of an
// empty phase (ephemeralrunnerset_controller.go).
func TestRecordFleetMetrics_EmptyPhaseCountsAsPending(t *testing.T) {
	r := &EphemeralRunnerSetReconciler{}
	ers := &giteaactionsv1alpha1.EphemeralRunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-phase-set", Namespace: "gitea-runners"},
	}
	grs := &giteaactionsv1alpha1.GiteaRunnerSet{}

	r.recordFleetMetrics(ers, grs, &giteaactionsv1alpha1.EphemeralRunnerList{
		Items: []giteaactionsv1alpha1.EphemeralRunner{{}},
	})

	if got := testutil.ToFloat64(metrics.EphemeralRunnerPhaseCount.WithLabelValues("empty-phase-set", "gitea-runners", "Pending")); got != 1 {
		t.Errorf("Pending count = %v, want 1 for an EphemeralRunner with empty Status.Phase", got)
	}
}
