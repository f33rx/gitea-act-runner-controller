package main

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
)

func grs(name string, labels ...string) giteaactionsv1alpha1.GiteaRunnerSet {
	return giteaactionsv1alpha1.GiteaRunnerSet{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "ns"},
		Spec:       giteaactionsv1alpha1.GiteaRunnerSetSpec{Labels: labels, MaxRunners: 10},
	}
}

func job(labels ...string) gitea.Job { return gitea.Job{Labels: labels} }

// Verifies ADR 0007 label semantics: all-match (subset) and one count per job.
func TestAssignQueuedJobs_SingleSet(t *testing.T) {
	sets := []giteaactionsv1alpha1.GiteaRunnerSet{grs("a", "ubuntu-latest", "self-hosted")}
	cases := []struct {
		name string
		jobs []gitea.Job
		want int
	}{
		{"single label matches", []gitea.Job{job("ubuntu-latest")}, 1},
		{"multi-label subset matches once (no double count)", []gitea.Job{job("ubuntu-latest", "self-hosted")}, 1},
		{"job needs a label the set lacks -> no match", []gitea.Job{job("ubuntu-latest", "gpu")}, 0},
		{"empty job labels -> no match", []gitea.Job{job()}, 0},
		{"mixed batch counts only matching jobs", []gitea.Job{
			job("ubuntu-latest"),        // match
			job("windows"),              // no
			job("ubuntu-latest", "gpu"), // no (gpu not in set)
			job("self-hosted"),          // match
		}, 2},
	}
	for _, c := range cases {
		if got := assignQueuedJobs(c.jobs, sets, nil)[setKey{"ns", "a"}]; got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}

// A job that several sets can run counts toward one of them, the one with the fewest
// labels, so a generic job neither starts a runner in every set nor takes a GPU runner.
func TestAssignQueuedJobs_EachJobCountsOnce(t *testing.T) {
	sets := []giteaactionsv1alpha1.GiteaRunnerSet{
		grs("gpu", "ubuntu-latest", "gpu"),
		grs("plain", "ubuntu-latest"),
		grs("plain-b", "ubuntu-latest"),
	}
	got := assignQueuedJobs([]gitea.Job{
		job("ubuntu-latest"),
		job("ubuntu-latest"),
		job("ubuntu-latest", "gpu"),
	}, sets, nil)

	want := map[setKey]int{{"ns", "plain"}: 2, {"ns", "gpu"}: 1}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: got %d, want %d (all %v)", k.name, got[k], n, got)
		}
	}
}

// A job the narrowest matching set has no room for falls through to the next matching
// set, so a full or paused set does not strand jobs another set could run.
func TestAssignQueuedJobs_FullSetFallsThrough(t *testing.T) {
	small := grs("small", "linux")
	small.Spec.MaxRunners = 2
	paused := grs("paused", "linux")
	paused.Spec.MaxRunners = 0
	big := grs("big", "linux", "large")
	sets := []giteaactionsv1alpha1.GiteaRunnerSet{small, paused, big}
	counts := map[setKey]setRunners{{"ns", "small"}: {total: 2, occupied: 1}}

	got := assignQueuedJobs([]gitea.Job{job("linux"), job("linux"), job("linux")}, sets, counts)
	want := map[setKey]int{{"ns", "small"}: 1, {"ns", "big"}: 2}
	if len(got) != len(want) || got[setKey{"ns", "small"}] != 1 || got[setKey{"ns", "big"}] != 2 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// A runner is occupied while Gitea shows it running a job, whatever its phase (the phase
// trails the claim), and while it is finishing or being deleted, since the
// EphemeralRunnerSet still counts it.
func TestCountRunners(t *testing.T) {
	now := metav1.Now()
	runner := func(set, name string, phase giteaactionsv1alpha1.EphemeralRunnerPhase, deleting bool) giteaactionsv1alpha1.EphemeralRunner {
		r := giteaactionsv1alpha1.EphemeralRunner{
			ObjectMeta: metav1.ObjectMeta{Namespace: "ns", Name: name},
			Spec:       giteaactionsv1alpha1.EphemeralRunnerSpec{GiteaRunnerSetName: set},
			Status:     giteaactionsv1alpha1.EphemeralRunnerStatus{Phase: phase},
		}
		if deleting {
			r.DeletionTimestamp = &now
		}
		return r
	}
	got := countRunners([]giteaactionsv1alpha1.EphemeralRunner{
		runner("a", "a-1", giteaactionsv1alpha1.EphemeralRunnerPending, false),   // holds a job, phase lags
		runner("a", "a-2", giteaactionsv1alpha1.EphemeralRunnerRunning, false),   // idle
		runner("a", "a-3", giteaactionsv1alpha1.EphemeralRunnerSucceeded, false), // finishing
		runner("a", "a-4", giteaactionsv1alpha1.EphemeralRunnerRunning, true),    // being deleted
		runner("b", "b-1", giteaactionsv1alpha1.EphemeralRunnerRunning, false),   // set not in this source
	}, []giteaactionsv1alpha1.GiteaRunnerSet{grs("a", "x")}, map[string]bool{"a-1": true, "b-1": true})

	want := map[setKey]setRunners{{"ns", "a"}: {total: 4, occupied: 3}}
	if len(got) != len(want) || got[setKey{"ns", "a"}] != want[setKey{"ns", "a"}] {
		t.Fatalf("got %v, want %v", got, want)
	}
}

// minRunners is a floor on idle runners: a warm runner that takes a job is replaced.
func TestDesiredRunners(t *testing.T) {
	spec := func(lo, hi int32) giteaactionsv1alpha1.GiteaRunnerSetSpec {
		return giteaactionsv1alpha1.GiteaRunnerSetSpec{MinRunners: lo, MaxRunners: hi}
	}
	cases := []struct {
		name     string
		occupied int
		queued   int
		spec     giteaactionsv1alpha1.GiteaRunnerSetSpec
		want     int32
	}{
		{"one per busy and queued job", 1, 1, spec(0, 5), 2},
		{"warm runner busy, keep one idle", 1, 0, spec(1, 5), 2},
		{"queued jobs cover the idle floor", 0, 3, spec(1, 5), 3},
		{"nothing to do", 0, 0, spec(0, 5), 0},
		{"capped at max", 4, 3, spec(1, 5), 5},
	}
	for _, c := range cases {
		if got := desiredRunners(setRunners{occupied: c.occupied}, c.queued, c.spec); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
