package controller

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

// recordLogProgress matches in-progress jobs by the name act_runner registered under.
// Left to its default that name is the pod hostname, which the kubelet truncates at 63
// characters, so a long runner set name would silently break job matching. Pin it.
func TestConstructPod_PinsRunnerName(t *testing.T) {
	scheme := newTestScheme(t)
	r := &EphemeralRunnerReconciler{Scheme: scheme}
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "a-very-long-gitearunnerset-name-that-exceeds-the-hostname-limit-runner-0",
			Namespace: "gitea-runners",
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{Labels: []string{"ubuntu-latest"}},
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tok", Namespace: "gitea-runners"}}

	pod := r.constructPod(context.Background(), runner, secret)

	var got string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == envGiteaRunnerName {
			got = e.Value
		}
	}
	if got != runner.Name {
		t.Fatalf("%s = %q, want %q", envGiteaRunnerName, got, runner.Name)
	}
	if len(runner.Name) <= 63 {
		t.Fatal("fixture name must exceed the 63-char hostname limit to be meaningful")
	}
}
