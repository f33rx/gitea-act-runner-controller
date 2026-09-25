package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

// Scale-up copies the GiteaRunnerSet template onto each new runner, and an invalid
// template blocks scale-up instead of producing runners that can never get a pod.
func TestScaleUpRunnerTemplate(t *testing.T) {
	cases := []struct {
		name        string
		template    string
		wantRunners int
	}{
		{"valid template is copied to the runner", `{"spec":{"containers":[{"name":"act-runner","image":"example/r:1"}]}}`, 1},
		{"invalid template creates no runners", `{"spec":{"containerz":[]}}`, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newTestScheme(t)
			key := types.NamespacedName{Name: "set", Namespace: "ns"}
			grs := &giteaactionsv1alpha1.GiteaRunnerSet{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
				Spec: giteaactionsv1alpha1.GiteaRunnerSetSpec{
					GiteaConfigURL:       "http://gitea:3000",
					GiteaConfigSecretRef: giteaactionsv1alpha1.SecretKeySelector{Name: "creds", Key: "token"},
					Labels:               []string{"ubuntu-latest"},
					Template:             &runtime.RawExtension{Raw: []byte(tc.template)},
				},
			}
			ers := &giteaactionsv1alpha1.EphemeralRunnerSet{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "ers-uid"},
				Spec:       giteaactionsv1alpha1.EphemeralRunnerSetSpec{Replicas: 1},
			}
			creds := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: key.Namespace},
				Data:       map[string][]byte{"token": []byte("t")},
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&giteaactionsv1alpha1.EphemeralRunner{}, "metadata.ownerReferences.uid", ownerUIDIndex).
				WithStatusSubresource(&giteaactionsv1alpha1.EphemeralRunnerSet{}).
				WithObjects(grs, ers, creds).Build()
			rec := record.NewFakeRecorder(10)
			r := &EphemeralRunnerSetReconciler{Client: c, Scheme: scheme, Recorder: rec}

			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			runners := &giteaactionsv1alpha1.EphemeralRunnerList{}
			if err := c.List(context.Background(), runners); err != nil {
				t.Fatal(err)
			}
			if len(runners.Items) != tc.wantRunners {
				t.Fatalf("runners = %d, want %d", len(runners.Items), tc.wantRunners)
			}
			if tc.wantRunners == 0 {
				select {
				case e := <-rec.Events:
					if !strings.Contains(e, "InvalidTemplate") {
						t.Fatalf("event = %q, want InvalidTemplate", e)
					}
				default:
					t.Fatal("an invalid template must be reported as an Event on the GiteaRunnerSet")
				}
				return
			}
			tmpl, err := decodeRunnerTemplate(runners.Items[0].Spec.Template)
			if err != nil {
				t.Fatal(err)
			}
			if len(tmpl.Spec.Containers) != 1 || tmpl.Spec.Containers[0].Image != "example/r:1" {
				t.Fatalf("runner template = %+v, want the set's template", tmpl.Spec.Containers)
			}
		})
	}
}
