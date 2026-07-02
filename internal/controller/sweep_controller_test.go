/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatalf("add client-go scheme: %v", err)
	}
	if err := giteaactionsv1alpha1.AddToScheme(s); err != nil {
		t.Fatalf("add v1alpha1 scheme: %v", err)
	}
	return s
}

// In an HA (multi-replica) deployment only the leader may sweep; otherwise
// every replica races to deregister the same runners.
func TestSweepReconcilerNeedsLeaderElection(t *testing.T) {
	r := &SweepReconciler{}
	if !r.NeedLeaderElection() {
		t.Fatal("sweep must require leader election so only the leader sweeps")
	}
}

// Start must block until its context is cancelled and then return nil, so the
// manager can drain it cleanly on SIGTERM. A missing teardown Secret makes each
// sweep pass a no-op, which is fine: we are testing the lifecycle, not teardown.
func TestSweepReconcilerStartDrainsOnContextCancel(t *testing.T) {
	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &SweepReconciler{
		Client:        c,
		Scheme:        scheme,
		SweepInterval: 10 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- r.Start(ctx)
	}()

	// Let a few sweep passes run, then signal shutdown.
	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Start returned error on shutdown: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return within 2s of context cancellation (loop is not draining)")
	}
}

// A zero SweepInterval must fall back to the default rather than spinning a
// zero-duration ticker (which panics).
func TestSweepReconcilerDefaultIntervalDoesNotPanic(t *testing.T) {
	scheme := newTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	r := &SweepReconciler{
		Client: c,
		Scheme: scheme,
		// SweepInterval intentionally left zero.
	}

	// Cancel almost immediately: we only need Start to reach the ticker setup
	// (which would panic on a zero duration) and run its initial sweep once.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	if err := r.Start(ctx); err != nil {
		t.Fatalf("Start with default interval returned error: %v", err)
	}
}
