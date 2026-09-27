package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
)

func ptr[T any](v T) *T { return &v }

// garc-lby: a queued job gets its own runner while another runs. The busy runner is
// known from Gitea, not from its status: its phase still reads Pending and it has no
// JobRef, which is the gap between a claim and the controller recording it.
func TestSyncDemand_CountsRunnersHoldingJobs(t *testing.T) {
	cases := []struct {
		name         string
		min          int32
		existing     *giteaactionsv1alpha1.EphemeralRunnerSet
		wantReplicas int32
		wantNewPatch bool
	}{
		{name: "creates the set sized to busy plus queued", wantReplicas: 2},
		{name: "idle floor above the queue", min: 2, wantReplicas: 3},
		{
			name: "runners missing restamps the PatchID even when replicas match",
			existing: &giteaactionsv1alpha1.EphemeralRunnerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns", OwnerReferences: []metav1.OwnerReference{{
					APIVersion: giteaactionsv1alpha1.GroupVersion.String(), Kind: "GiteaRunnerSet",
					Name: "set", UID: "grs-uid", Controller: ptr(true),
				}}},
				Spec: giteaactionsv1alpha1.EphemeralRunnerSetSpec{Replicas: 2, PatchID: 1},
			},
			wantReplicas: 2, wantNewPatch: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Answers with only the statuses asked for, as Gitea does.
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				all := map[string]gitea.Job{
					"in_progress": {ID: 1, Status: "in_progress", RunnerName: "set-runner-abcde", Labels: []string{"ubuntu-latest"}, StartedAt: "2026-09-27T14:23:11Z"},
					"queued":      {ID: 2, Status: "queued", Labels: []string{"ubuntu-latest"}},
				}
				resp := gitea.ListOrgJobsResponse{}
				for _, st := range r.URL.Query()["status"] {
					if j, ok := all[st]; ok {
						resp.Jobs = append(resp.Jobs, j)
					}
				}
				resp.TotalCount = len(resp.Jobs)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(resp)
			}))
			defer srv.Close()

			scheme := runtime.NewScheme()
			utilruntime.Must(clientgoscheme.AddToScheme(scheme))
			utilruntime.Must(giteaactionsv1alpha1.AddToScheme(scheme))

			set := &giteaactionsv1alpha1.GiteaRunnerSet{
				ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns", UID: "grs-uid"},
				Spec: giteaactionsv1alpha1.GiteaRunnerSetSpec{
					GiteaConfigURL:       srv.URL,
					GiteaConfigSecretRef: giteaactionsv1alpha1.SecretKeySelector{Name: "creds", Key: "token"},
					RunnerScope:          "org",
					OrgName:              "org",
					Labels:               []string{"ubuntu-latest"},
					MinRunners:           tc.min,
					MaxRunners:           5,
				},
			}
			busy := &giteaactionsv1alpha1.EphemeralRunner{
				ObjectMeta: metav1.ObjectMeta{Name: "set-runner-abcde", Namespace: "ns"},
				Spec:       giteaactionsv1alpha1.EphemeralRunnerSpec{GiteaRunnerSetName: "set"},
				Status:     giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: giteaactionsv1alpha1.EphemeralRunnerPending},
			}
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Name: "creds", Namespace: "ns"},
				Data:       map[string][]byte{"token": []byte("tok")},
			}
			objs := []client.Object{set, busy, secret}
			if tc.existing != nil {
				objs = append(objs, tc.existing)
			}
			c := fake.NewClientBuilder().WithScheme(scheme).
				WithStatusSubresource(&giteaactionsv1alpha1.EphemeralRunnerSet{}).
				WithObjects(objs...).Build()
			l := &Listener{client: c}

			if err := l.syncDemand(context.Background()); err != nil {
				t.Fatalf("syncDemand: %v", err)
			}
			ers := &giteaactionsv1alpha1.EphemeralRunnerSet{}
			if err := c.Get(context.Background(), types.NamespacedName{Namespace: "ns", Name: "set"}, ers); err != nil {
				t.Fatal(err)
			}
			if ers.Spec.Replicas != tc.wantReplicas {
				t.Fatalf("replicas = %d, want %d", ers.Spec.Replicas, tc.wantReplicas)
			}
			if tc.wantNewPatch && ers.Spec.PatchID == 1 {
				t.Fatal("PatchID not restamped; the set would not create the missing runner")
			}
		})
	}
}
