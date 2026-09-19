package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

// Deleting a GiteaRunnerSet must cascade to its EphemeralRunnerSet (garc-6dx), which
// needs a controller owner reference the listener is responsible for stamping.
func TestEnsureOwnedBy(t *testing.T) {
	rs := &giteaactionsv1alpha1.GiteaRunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns", UID: types.UID("grs-uid")},
	}
	ers := &giteaactionsv1alpha1.EphemeralRunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"},
	}

	if !ensureOwnedBy(ers, rs) {
		t.Fatal("first call must add the reference")
	}
	if len(ers.OwnerReferences) != 1 {
		t.Fatalf("want 1 owner reference, got %d", len(ers.OwnerReferences))
	}
	ref := ers.OwnerReferences[0]
	if ref.UID != rs.UID || ref.Kind != "GiteaRunnerSet" || ref.Name != "set" ||
		ref.APIVersion != giteaactionsv1alpha1.GroupVersion.String() {
		t.Errorf("unexpected reference %+v", ref)
	}
	if ref.Controller == nil || !*ref.Controller {
		t.Error("reference must mark the GiteaRunnerSet as controller")
	}
	if ref.BlockOwnerDeletion != nil {
		t.Error("BlockOwnerDeletion must stay unset (no finalizers RBAC on the listener)")
	}

	if ensureOwnedBy(ers, rs) {
		t.Error("second call must be a no-op")
	}
	if len(ers.OwnerReferences) != 1 {
		t.Errorf("reference duplicated: %+v", ers.OwnerReferences)
	}
}
