package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

// Characterizes the sweep's orphan rule: an ephemeral registration is deregistered
// unless an EphemeralRunner of that name has a live pod; non-ephemeral rows are never
// touched; and a failed DELETE is logged and the pass moves on to the next row.
func TestSweepOrgRunnersDeregistersOnlyOrphans(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/orgs/acme/actions/runners"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 4, "runners": []map[string]any{
				{"id": 1, "name": "claimed", "ephemeral": true},
				{"id": 2, "name": "orphan-a", "ephemeral": true},
				{"id": 3, "name": "orphan-b", "ephemeral": true},
				{"id": 4, "name": "persistent", "ephemeral": false},
			}})
		case req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/orgs/acme/actions/runners/"):
			id := req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:]
			mu.Lock()
			deleted = append(deleted, id)
			mu.Unlock()
			if id == "2" {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()

	scheme := newTestScheme(t)
	claimed := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{Name: "claimed", Namespace: "ns"},
		Status:     giteaactionsv1alpha1.EphemeralRunnerStatus{PodName: "claimed"},
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "claimed", Namespace: "ns"}}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(claimed, pod).Build()
	r := &SweepReconciler{Client: c, Scheme: scheme}

	r.sweepOrgRunners(context.Background(), srv.URL, "acme", "t")

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(deleted, ","); got != "2,3" {
		t.Errorf("want DELETEs for the two ephemeral orphans only, in order, continuing past the failure; got %q", got)
	}
}
