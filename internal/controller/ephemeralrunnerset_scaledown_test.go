package controller

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

// Scale-down may only delete a runner that has certainly not claimed a job. The cached
// EphemeralRunner phase lags the pod, so the pod is consulted too (garc-x32, garc-nme).
func TestScaleDownSafe(t *testing.T) {
	old := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	fresh := metav1.NewTime(time.Now().Add(-2 * time.Second))
	mkRunner := func(created metav1.Time, phase giteaactionsv1alpha1.EphemeralRunnerPhase) *giteaactionsv1alpha1.EphemeralRunner {
		return &giteaactionsv1alpha1.EphemeralRunner{
			ObjectMeta: metav1.ObjectMeta{Name: "r0", Namespace: "ns", CreationTimestamp: created},
			Status:     giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: phase},
		}
	}
	mkPod := func(phase corev1.PodPhase, node string, scheduledAt metav1.Time) *corev1.Pod {
		p := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "r0", Namespace: "ns"},
			Spec:       corev1.PodSpec{NodeName: node},
			Status:     corev1.PodStatus{Phase: phase},
		}
		if node != "" {
			p.Status.Conditions = []corev1.PodCondition{{
				Type: corev1.PodScheduled, Status: corev1.ConditionTrue, LastTransitionTime: scheduledAt,
			}}
		}
		return p
	}
	// A pod bound to a node but carrying no PodScheduled condition: only reachable from a
	// hand-built object or a stripped status, and it must not read as safe.
	mkPodNoCondition := func(node string) *corev1.Pod {
		return &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{Name: "r0", Namespace: "ns"},
			Spec:       corev1.PodSpec{NodeName: node},
			Status:     corev1.PodStatus{Phase: corev1.PodPending},
		}
	}
	// Pod phase stays Pending until every container is created, so a running act_runner
	// container can sit inside a Pending pod while a sidecar still pulls.
	withRunningContainer := func(p *corev1.Pod) *corev1.Pod {
		started := true
		p.Status.ContainerStatuses = []corev1.ContainerStatus{{
			Name:    "act-runner",
			Started: &started,
			State:   corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}
		return p
	}

	for _, tc := range []struct {
		name   string
		runner *giteaactionsv1alpha1.EphemeralRunner
		pod    *corev1.Pod
		safe   bool
		rank   scaleDownRank
	}{
		{"young runner is protected", mkRunner(fresh, giteaactionsv1alpha1.EphemeralRunnerPending), nil, false, 0},
		{"running runner is protected", mkRunner(old, giteaactionsv1alpha1.EphemeralRunnerRunning), mkPod(corev1.PodRunning, "n1", old), false, 0},
		{"old pending runner with no pod is safe", mkRunner(old, giteaactionsv1alpha1.EphemeralRunnerPending), nil, true, rankNoPod},
		{"old pending runner, pod unscheduled, is safe", mkRunner(old, giteaactionsv1alpha1.EphemeralRunnerPending), mkPod(corev1.PodPending, "", old), true, rankUnscheduled},
		{"old pending runner, pod just scheduled, is protected (garc-nme)", mkRunner(old, giteaactionsv1alpha1.EphemeralRunnerPending), mkPod(corev1.PodPending, "n1", fresh), false, 0},
		{"old runner with empty phase, pod already Running, is protected (garc-nme)", mkRunner(old, ""), mkPod(corev1.PodRunning, "n1", fresh), false, 0},
		{"old pending runner, pod scheduled long ago but still Pending, is safe", mkRunner(old, giteaactionsv1alpha1.EphemeralRunnerPending), mkPod(corev1.PodPending, "n1", old), true, rankScheduledPulling},
		{"Pending pod whose runner container already started is protected", mkRunner(old, giteaactionsv1alpha1.EphemeralRunnerPending), withRunningContainer(mkPod(corev1.PodPending, "n1", old)), false, 0},
		{"pod bound but carrying no PodScheduled condition is not treated as past grace", mkRunner(old, giteaactionsv1alpha1.EphemeralRunnerPending), mkPodNoCondition("n1"), true, rankUnscheduled},
	} {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newTestScheme(t)
			objs := []client.Object{tc.runner}
			if tc.pod != nil {
				objs = append(objs, tc.pod)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
			r := &EphemeralRunnerSetReconciler{Client: c, Scheme: scheme}
			safe, rank, why := r.scaleDownSafe(context.Background(), tc.runner)
			if safe != tc.safe {
				t.Fatalf("safe = %v (%s), want %v", safe, why, tc.safe)
			}
			if safe && rank != tc.rank {
				t.Fatalf("rank = %v (%s), want %v", rank, why, tc.rank)
			}
		})
	}
}
