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
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
)

// ADR 0008 acceptance criteria: a hung (Running, no progress) job is failed once the
// stall window elapses; a slow-but-progressing job is left running under the window;
// a Pending (pre-claim) runner is retried via deletion once its own timeout elapses.

func TestCheckTimeout_RunningPastStallWindowIsStuck(t *testing.T) {
	r := &EphemeralRunnerReconciler{}
	start := metav1.NewTime(time.Now().Add(-20 * time.Minute))
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:          giteaactionsv1alpha1.EphemeralRunnerRunning,
			PhaseStartTime: &start,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			StallWindow: &metav1.Duration{Duration: 15 * time.Minute},
		},
	}

	timedOut, reason := r.checkTimeout(runner)
	if !timedOut {
		t.Fatalf("expected a Running runner past its stall window to be timed out")
	}
	if reason == "" {
		t.Errorf("expected a non-empty reason")
	}
}

// This is the case the log-heartbeat exists for: a job whose pod phase has been
// Running for far longer than the stall window, but whose container log kept producing
// output recently (LastProgressTime) must NOT be killed -- it is slow, not stuck.
func TestCheckTimeout_RunningWithRecentLogProgressIsLeftAlone(t *testing.T) {
	r := &EphemeralRunnerReconciler{}
	longAgo := metav1.NewTime(time.Now().Add(-20 * time.Minute))
	recentProgress := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:            giteaactionsv1alpha1.EphemeralRunnerRunning,
			PhaseStartTime:   &longAgo,
			LastProgressTime: &recentProgress,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			StallWindow: &metav1.Duration{Duration: 15 * time.Minute},
		},
	}

	timedOut, _ := r.checkTimeout(runner)
	if timedOut {
		t.Fatalf("a runner with recent log progress must not be killed even though PhaseStartTime alone is well past the stall window")
	}
}

// The inverse: log progress that itself predates the stall window (no output in a
// while, even though something was logged much earlier) must still fire.
func TestCheckTimeout_RunningWithStaleLogProgressIsStuck(t *testing.T) {
	r := &EphemeralRunnerReconciler{}
	longAgo := metav1.NewTime(time.Now().Add(-30 * time.Minute))
	staleProgress := metav1.NewTime(time.Now().Add(-20 * time.Minute))
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:            giteaactionsv1alpha1.EphemeralRunnerRunning,
			PhaseStartTime:   &longAgo,
			LastProgressTime: &staleProgress,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			StallWindow: &metav1.Duration{Duration: 15 * time.Minute},
		},
	}

	timedOut, reason := r.checkTimeout(runner)
	if !timedOut {
		t.Fatalf("a runner whose log progress itself is older than the stall window must be treated as stuck")
	}
	if reason == "" {
		t.Errorf("expected a non-empty reason")
	}
}

func TestCheckTimeout_RunningUnderStallWindowIsLeftAlone(t *testing.T) {
	r := &EphemeralRunnerReconciler{}
	start := metav1.NewTime(time.Now().Add(-5 * time.Minute))
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:          giteaactionsv1alpha1.EphemeralRunnerRunning,
			PhaseStartTime: &start,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			StallWindow: &metav1.Duration{Duration: 15 * time.Minute},
		},
	}

	timedOut, _ := r.checkTimeout(runner)
	if timedOut {
		t.Fatalf("a slow-but-progressing runner under the stall window must not be killed")
	}
}

func TestCheckTimeout_PendingPastTimeoutIsRetried(t *testing.T) {
	r := &EphemeralRunnerReconciler{}
	start := metav1.NewTime(time.Now().Add(-10 * time.Minute))
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:          giteaactionsv1alpha1.EphemeralRunnerPending,
			PhaseStartTime: &start,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			PendingTimeout: &metav1.Duration{Duration: 5 * time.Minute},
		},
	}

	timedOut, reason := r.checkTimeout(runner)
	if !timedOut {
		t.Fatalf("expected a Pending runner past its pending timeout to be timed out (pre-claim retry path)")
	}
	if reason == "" {
		t.Errorf("expected a non-empty reason")
	}
}

func TestCheckTimeout_PendingUnderTimeoutIsLeftAlone(t *testing.T) {
	r := &EphemeralRunnerReconciler{}
	start := metav1.NewTime(time.Now().Add(-1 * time.Minute))
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:          giteaactionsv1alpha1.EphemeralRunnerPending,
			PhaseStartTime: &start,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			PendingTimeout: &metav1.Duration{Duration: 5 * time.Minute},
		},
	}

	timedOut, _ := r.checkTimeout(runner)
	if timedOut {
		t.Fatalf("a Pending runner under its pending timeout must not be deleted yet")
	}
}

// No configured window/timeout (nil, e.g. neither a GiteaRunnerSet override nor a
// manager default) must disable the corresponding check entirely -- never fire on a
// zero-value duration.

func TestCheckTimeout_NilStallWindowNeverFires(t *testing.T) {
	r := &EphemeralRunnerReconciler{}
	start := metav1.NewTime(time.Now().Add(-999 * time.Hour))
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:          giteaactionsv1alpha1.EphemeralRunnerRunning,
			PhaseStartTime: &start,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			StallWindow: nil,
		},
	}

	timedOut, _ := r.checkTimeout(runner)
	if timedOut {
		t.Fatalf("a nil StallWindow must disable stall detection, even for a very old phase start")
	}
}

func TestCheckTimeout_NilPendingTimeoutNeverFires(t *testing.T) {
	r := &EphemeralRunnerReconciler{}
	start := metav1.NewTime(time.Now().Add(-999 * time.Hour))
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:          giteaactionsv1alpha1.EphemeralRunnerPending,
			PhaseStartTime: &start,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			PendingTimeout: nil,
		},
	}

	timedOut, _ := r.checkTimeout(runner)
	if timedOut {
		t.Fatalf("a nil PendingTimeout must disable pending-timeout detection")
	}
}

func TestCheckTimeout_NoPhaseStartTimeNeverFires(t *testing.T) {
	r := &EphemeralRunnerReconciler{}
	runner := &giteaactionsv1alpha1.EphemeralRunner{
		Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
			Phase:          giteaactionsv1alpha1.EphemeralRunnerRunning,
			PhaseStartTime: nil,
		},
		Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
			StallWindow: &metav1.Duration{Duration: time.Second},
		},
	}

	timedOut, _ := r.checkTimeout(runner)
	if timedOut {
		t.Fatalf("a runner with no observed PhaseStartTime must not be treated as timed out")
	}
}

// Terminal phases (Succeeded/Failed) are handled by the existing auto-teardown path,
// not the timeout check -- checkTimeout must not also flag them.

func TestCheckTimeout_TerminalPhasesAreIgnored(t *testing.T) {
	r := &EphemeralRunnerReconciler{}
	start := metav1.NewTime(time.Now().Add(-999 * time.Hour))

	for _, phase := range []giteaactionsv1alpha1.EphemeralRunnerPhase{
		giteaactionsv1alpha1.EphemeralRunnerSucceeded,
		giteaactionsv1alpha1.EphemeralRunnerFailed,
	} {
		runner := &giteaactionsv1alpha1.EphemeralRunner{
			Status: giteaactionsv1alpha1.EphemeralRunnerStatus{
				Phase:          phase,
				PhaseStartTime: &start,
			},
			Spec: giteaactionsv1alpha1.EphemeralRunnerSpec{
				StallWindow:    &metav1.Duration{Duration: time.Second},
				PendingTimeout: &metav1.Duration{Duration: time.Second},
			},
		}

		if timedOut, _ := r.checkTimeout(runner); timedOut {
			t.Errorf("phase %s must not be flagged by checkTimeout (handled by auto-teardown instead)", phase)
		}
	}
}
