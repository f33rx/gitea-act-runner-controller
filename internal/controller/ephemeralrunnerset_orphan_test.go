package controller

import (
	"context"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

func ownerUIDIndex(rawObj client.Object) []string {
	var owners []string
	for _, ref := range rawObj.(*giteaactionsv1alpha1.EphemeralRunner).OwnerReferences {
		if ref.Kind == "EphemeralRunnerSet" {
			owners = append(owners, string(ref.UID))
		}
	}
	return owners
}

// An EphemeralRunnerSet whose GiteaRunnerSet is confirmed gone must delete itself
// (garc-6dx); one whose parent is merely not in the cache yet must wait.
func TestMissingParentDeletesOrphanedSet(t *testing.T) {
	scheme := newTestScheme(t)
	key := types.NamespacedName{Name: "set", Namespace: "ns"}
	mkERS := func() *giteaactionsv1alpha1.EphemeralRunnerSet {
		return &giteaactionsv1alpha1.EphemeralRunnerSet{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Spec:       giteaactionsv1alpha1.EphemeralRunnerSetSpec{Replicas: 1},
		}
	}
	builder := func(objs ...client.Object) *fake.ClientBuilder {
		return fake.NewClientBuilder().WithScheme(scheme).
			WithIndex(&giteaactionsv1alpha1.EphemeralRunner{}, "metadata.ownerReferences.uid", ownerUIDIndex).
			WithObjects(objs...)
	}

	t.Run("parent absent from API server: set is deleted", func(t *testing.T) {
		c := builder(mkERS()).Build()
		r := &EphemeralRunnerSetReconciler{Client: c, Scheme: scheme, APIReader: builder().Build()}
		if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		err := c.Get(context.Background(), key, &giteaactionsv1alpha1.EphemeralRunnerSet{})
		if !apierrors.IsNotFound(err) {
			t.Fatalf("want EphemeralRunnerSet deleted, got err=%v", err)
		}
	})

	t.Run("parent only missing from cache: set is kept and requeued", func(t *testing.T) {
		grs := &giteaactionsv1alpha1.GiteaRunnerSet{ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace}}
		c := builder(mkERS()).Build()
		r := &EphemeralRunnerSetReconciler{Client: c, Scheme: scheme, APIReader: builder(grs).Build()}
		res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("reconcile: %v", err)
		}
		if res.RequeueAfter == 0 {
			t.Error("want a requeue while the cache catches up")
		}
		if err := c.Get(context.Background(), key, &giteaactionsv1alpha1.EphemeralRunnerSet{}); err != nil {
			t.Fatalf("EphemeralRunnerSet must survive a transient cache miss: %v", err)
		}
	})
}
