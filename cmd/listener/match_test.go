package main

import (
	"testing"

	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
)

// Verifies ADR 0007 label semantics: all-match (subset) and one count per job.
func TestCountMatchingJobs(t *testing.T) {
	l := &Listener{}
	set := []string{"ubuntu-latest", "self-hosted"}

	cases := []struct {
		name string
		jobs []gitea.Job
		want int
	}{
		{"single label matches", []gitea.Job{{Labels: []string{"ubuntu-latest"}}}, 1},
		{"multi-label subset matches once (no double count)",
			[]gitea.Job{{Labels: []string{"ubuntu-latest", "self-hosted"}}}, 1},
		{"job needs a label the set lacks -> no match",
			[]gitea.Job{{Labels: []string{"ubuntu-latest", "gpu"}}}, 0},
		{"empty job labels -> no match",
			[]gitea.Job{{Labels: []string{}}}, 0},
		{"mixed batch counts only matching jobs",
			[]gitea.Job{
				{Labels: []string{"ubuntu-latest"}},        // match
				{Labels: []string{"windows"}},              // no
				{Labels: []string{"ubuntu-latest", "gpu"}}, // no (gpu not in set)
				{Labels: []string{"self-hosted"}},          // match
			}, 2},
	}
	for _, c := range cases {
		if got := l.countMatchingJobs(c.jobs, set); got != c.want {
			t.Errorf("%s: got %d, want %d", c.name, got, c.want)
		}
	}
}
