package controller

import (
	"context"
	"strings"
	"testing"
	"time"

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

// garc-xfn: a runner whose Pod is rejected at creation used to keep an empty phase, so
// the pending timeout never armed and it retried forever with the reason only in the
// manager log. It must go Pending with the rejection as its reason, raise Events on the
// runner and its set, keep its clock across retries, and be recycled after the timeout.
func TestReconcile_PodCreateRejected(t *testing.T) {
	denied := apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "set-runner-x",
		errorString(`admission webhook "warden-validating.common-webhooks.networking.gke.io" denied the request: privileged containers are not allowed`))
	cases := []struct {
		name        string
		createErr   error
		pendingFor  time.Duration // 0: phase not yet set
		wantPending bool
		wantDeleted bool
		wantEvents  int
	}{
		{name: "first rejection goes Pending and warns", createErr: denied, wantPending: true, wantEvents: 2},
		{name: "retry keeps the Pending clock", createErr: denied, pendingFor: time.Minute, wantPending: true, wantEvents: 2},
		{name: "past the pending timeout it is recycled", createErr: denied, pendingFor: 10 * time.Minute, wantDeleted: true, wantEvents: 2},
		{name: "pod already exists is not a rejection", createErr: apierrors.NewAlreadyExists(schema.GroupResource{Resource: "pods"}, "set-runner-x")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scheme := newTestScheme(t)
			runner := &giteaactionsv1alpha1.EphemeralRunner{
				ObjectMeta: metav1.ObjectMeta{Name: "set-runner-x", Namespace: "ns", Finalizers: []string{finalizerEphemeralRunner}},
				Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
					GiteaRunnerSetName: "set",
					RegistrationToken:  "tok",
					PendingTimeout:     &metav1.Duration{Duration: 5 * time.Minute},
				},
			}
			var since *metav1.Time
			if tc.pendingFor > 0 {
				t0 := metav1.NewTime(time.Now().Add(-tc.pendingFor).Truncate(time.Second))
				since = &t0
				runner.Status = giteaactionsv1alpha1.EphemeralRunnerStatus{
					Phase:          giteaactionsv1alpha1.EphemeralRunnerPending,
					PhaseStartTime: since,
				}
			}
			grs := &giteaactionsv1alpha1.GiteaRunnerSet{ObjectMeta: metav1.ObjectMeta{Name: "set", Namespace: "ns"}}
			funcs := interceptor.Funcs{Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1.Pod); ok {
					return tc.createErr
				}
				return c.Create(ctx, obj, opts...)
			}}
			c := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(runner).
				WithObjects(runner, grs).WithInterceptorFuncs(funcs).Build()
			rec := record.NewFakeRecorder(10)
			r := &EphemeralRunnerReconciler{Client: c, Scheme: scheme, Recorder: rec}

			key := types.NamespacedName{Namespace: runner.Namespace, Name: runner.Name}
			if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("reconcile: %v", err)
			}

			got := &giteaactionsv1alpha1.EphemeralRunner{}
			err := c.Get(context.Background(), key, got)
			deleted := apierrors.IsNotFound(err) || (err == nil && !got.DeletionTimestamp.IsZero())
			if deleted != tc.wantDeleted {
				t.Fatalf("deleted = %v, want %v (err %v)", deleted, tc.wantDeleted, err)
			}
			if tc.wantPending {
				if got.Status.Phase != giteaactionsv1alpha1.EphemeralRunnerPending || got.Status.PhaseStartTime == nil {
					t.Fatalf("status = %+v, want Pending with a start time", got.Status)
				}
				if !strings.Contains(got.Status.Reason, "privileged containers are not allowed") {
					t.Fatalf("reason = %q, want the admission message", got.Status.Reason)
				}
				if since != nil && !got.Status.PhaseStartTime.Equal(since) {
					t.Fatalf("PhaseStartTime moved from %v to %v; the timeout would never fire", since, got.Status.PhaseStartTime)
				}
			}
			if !tc.wantPending && !tc.wantDeleted && (got.Status.Phase != "" || got.Status.Reason != "") {
				t.Fatalf("status = %+v, want untouched", got.Status)
			}
			if n := len(rec.Events); n != tc.wantEvents {
				t.Fatalf("events = %d, want %d (runner and set)", n, tc.wantEvents)
			}
		})
	}
}

type errorString string

func (e errorString) Error() string { return string(e) }
