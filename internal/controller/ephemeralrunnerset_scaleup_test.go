package controller

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

// Scale-up acts once per listener PatchID. Replicas count busy runners, so a runner that
// finishes between polls must not be replaced before the listener reads demand again,
// and a reconcile holding a stale copy of the set must not create runners twice.
func TestScaleUpOncePerPatchID(t *testing.T) {
	cases := []struct {
		name          string
		patchID       int64
		lastReconcile int64
		claimConflict bool
		survivor      bool
		wantRunners   int
	}{
		{name: "new PatchID creates runners with unique names", patchID: 2, lastReconcile: 1, wantRunners: 2},
		{name: "claimed PatchID does not backfill", patchID: 2, lastReconcile: 2, wantRunners: 0},
		// The old index names collided here: with one runner left, the next index was 1.
		{name: "surviving set-runner-1 does not block a new runner", patchID: 2, lastReconcile: 1, survivor: true, wantRunners: 2},
		{name: "stale set loses the claim and creates nothing", patchID: 2, lastReconcile: 1, claimConflict: true, wantRunners: 0},
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
				},
			}
			ers := &giteaactionsv1alpha1.EphemeralRunnerSet{
				ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace, UID: "ers-uid"},
				Spec:       giteaactionsv1alpha1.EphemeralRunnerSetSpec{Replicas: 2, PatchID: tc.patchID},
				Status:     giteaactionsv1alpha1.EphemeralRunnerSetStatus{LastReconcilePatchID: tc.lastReconcile},
			}
			creds := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: key.Namespace},
				Data:       map[string][]byte{"token": []byte("t")},
			}
			objs := []client.Object{grs, ers, creds}
			if tc.survivor {
				objs = append(objs, &giteaactionsv1alpha1.EphemeralRunner{
					ObjectMeta: metav1.ObjectMeta{Name: "set-runner-1", Namespace: key.Namespace, OwnerReferences: []metav1.OwnerReference{{
						APIVersion: giteaactionsv1alpha1.GroupVersion.String(), Kind: "EphemeralRunnerSet", Name: key.Name, UID: "ers-uid",
					}}},
					Status: giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerRunning},
				})
			}
			conflicted := false
			funcs := interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if tc.claimConflict && !conflicted {
					conflicted = true
					return apierrors.NewConflict(schema.GroupResource{Resource: "ephemeralrunnersets"}, obj.GetName(), nil)
				}
				return c.SubResource(sub).Update(ctx, obj, opts...)
			}}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithIndex(&giteaactionsv1alpha1.EphemeralRunner{}, "metadata.ownerReferences.uid", ownerUIDIndex).
				WithStatusSubresource(&giteaactionsv1alpha1.EphemeralRunnerSet{}).
				WithObjects(objs...).WithInterceptorFuncs(funcs).Build()
			r := &EphemeralRunnerSetReconciler{Client: c, Scheme: scheme, Recorder: record.NewFakeRecorder(10)}

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
			names := map[string]bool{}
			for _, rr := range runners.Items {
				if !strings.HasPrefix(rr.Name, "set-runner-") || names[rr.Name] {
					t.Fatalf("runner name %q: want a unique set-runner-<suffix>", rr.Name)
				}
				names[rr.Name] = true
			}
		})
	}
}
