package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

// ADR 0008: the Pending clock must start when the Pod is created. Live testing showed
// that if the creation status write lands before the first Pod event is observed, the
// later event is a no-op (same phase, same reason) and PhaseStartTime stays nil, so the
// pending timeout never arms and an unschedulable runner is never retried.
func TestReconcile_PodCreationStartsPendingClock(t *testing.T) {
	scheme := newTestScheme(t)
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{Name: "runner-clock", Namespace: "gitea-runners"},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			GiteaRunnerSetName: "clock-set",
			RegistrationToken:  "tok",
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(runner).WithObjects(runner).Build()
	r := &EphemeralRunnerReconciler{Client: c, Scheme: scheme}

	key := types.NamespacedName{Namespace: runner.Namespace, Name: runner.Name}
	// First pass adds the finalizer and creates the token Secret and Pod, writing the
	// initial Pending status; it may take more than one requeue to get there.
	for i := 0; i < 3; i++ {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
		pod := &corev1.Pod{}
		if err := c.Get(context.Background(), key, pod); err == nil {
			break
		}
	}

	got := &giteaactionsv1alpha1.EphemeralRunner{}
	if err := c.Get(context.Background(), key, got); err != nil {
		t.Fatalf("get runner: %v", err)
	}
	if got.Status.Phase != giteaactionsv1alpha1.EphemeralRunnerPending {
		t.Fatalf("phase = %q, want Pending", got.Status.Phase)
	}
	if got.Status.PhaseStartTime == nil {
		t.Fatalf("PhaseStartTime is nil after pod creation; pending timeout would never arm")
	}
}
