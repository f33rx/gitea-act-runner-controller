package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

// Status.RunnerID is never populated, so the finalizer must find the registration by
// name and deregister it itself; once the GiteaRunnerSet is gone the sweep can no
// longer discover the org (garc-6dx).
func TestFinalizerDeregistersByName(t *testing.T) {
	var mu sync.Mutex
	var deleted []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch {
		case req.Method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/orgs/acme/actions/runners"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"total_count": 3, "runners": []map[string]any{
				{"id": 7, "name": "set-runner-0", "ephemeral": true},
				{"id": 8, "name": "set-runner-1", "ephemeral": true},
				{"id": 9, "name": "set-runner-0", "ephemeral": false},
			}})
		case req.Method == http.MethodDelete && strings.Contains(req.URL.Path, "/orgs/acme/actions/runners/"):
			mu.Lock()
			deleted = append(deleted, req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:])
			mu.Unlock()
			w.WriteHeader(http.StatusNoContent)
		default:
			http.NotFound(w, req)
		}
	}))
	defer srv.Close()

	scheme := newTestScheme(t)
	now := metav1.NewTime(time.Now())
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		ObjectMeta: metav1.ObjectMeta{
			Name: "set-runner-0", Namespace: "ns",
			Finalizers:        []string{finalizerEphemeralRunner},
			DeletionTimestamp: &now,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{GiteaConfigURL: srv.URL, OrgName: "acme"},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "gitea-teardown-credential", Namespace: "gitea-actions-controller"},
		Data:       map[string][]byte{"token": []byte("t")},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(runner, secret).Build()
	r := &EphemeralRunnerReconciler{Client: c, Scheme: scheme}

	res, err := r.handleDeletion(context.Background(), runner)
	if err != nil || res.Requeue {
		t.Fatalf("handleDeletion: res=%+v err=%v", res, err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(deleted) != 1 || deleted[0] != "7" {
		t.Errorf("want exactly the ephemeral registration named set-runner-0 (id 7) deleted, got %v", deleted)
	}
	err = c.Get(context.Background(), types.NamespacedName{Name: "set-runner-0", Namespace: "ns"}, &giteaactionsv1alpha1.EphemeralRunner{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("finalizer should be removed and the CR gone, got err=%v", err)
	}
}
