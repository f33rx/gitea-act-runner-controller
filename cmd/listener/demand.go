package main

import (
	"sort"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
)

// setKey identifies a GiteaRunnerSet; its EphemeralRunnerSet and runners share the name.
type setKey struct{ namespace, name string }

// demandSource groups runner sets that poll the same Gitea org queue.
type demandSource struct{ url, org string }

// assignQueuedJobs counts each queued job toward exactly one set: the matching set with
// the fewest labels that still has room under maxRunners, ties broken by namespace/name.
// Counting a job toward every matching set would start a runner in each, and all but
// the one Gitea hands the job to would sit idle until reaped. The sets must all poll
// the org the jobs came from.
func assignQueuedJobs(jobs []gitea.Job, sets []giteaactionsv1alpha1.GiteaRunnerSet, counts map[setKey]setRunners) map[setKey]int {
	ordered := make([]giteaactionsv1alpha1.GiteaRunnerSet, len(sets))
	copy(ordered, sets)
	sort.SliceStable(ordered, func(i, j int) bool {
		a, b := ordered[i], ordered[j]
		if len(a.Spec.Labels) != len(b.Spec.Labels) {
			return len(a.Spec.Labels) < len(b.Spec.Labels)
		}
		if a.Namespace != b.Namespace {
			return a.Namespace < b.Namespace
		}
		return a.Name < b.Name
	})
	labelSets := make([]map[string]struct{}, len(ordered))
	room := make([]int, len(ordered))
	for i, rs := range ordered {
		labelSets[i] = make(map[string]struct{}, len(rs.Spec.Labels))
		for _, l := range rs.Spec.Labels {
			labelSets[i][l] = struct{}{}
		}
		room[i] = int(rs.Spec.MaxRunners) - counts[setKey{rs.Namespace, rs.Name}].occupied
	}

	queued := map[setKey]int{}
	for _, job := range jobs {
		for i, rs := range ordered {
			if room[i] > 0 && jobMatchesSet(job.Labels, labelSets[i]) {
				queued[setKey{rs.Namespace, rs.Name}]++
				room[i]--
				break
			}
		}
	}
	return queued
}

// setRunners counts a set's runners: all of them, as the EphemeralRunnerSet counts
// them, and those occupied, which cannot take a queued job.
type setRunners struct{ total, occupied int }

// countRunners tallies the runners of the given sets. A runner is occupied while Gitea
// shows it running a job (active holds those runner names), and while it is finishing
// or being deleted, since the EphemeralRunnerSet still counts it until it is gone.
func countRunners(runners []giteaactionsv1alpha1.EphemeralRunner, sets []giteaactionsv1alpha1.GiteaRunnerSet, active map[string]bool) map[setKey]setRunners {
	counts := make(map[setKey]setRunners, len(sets))
	for _, rs := range sets {
		counts[setKey{rs.Namespace, rs.Name}] = setRunners{}
	}
	for _, r := range runners {
		key := setKey{r.Namespace, r.Spec.GiteaRunnerSetName}
		c, ok := counts[key]
		if !ok {
			continue
		}
		c.total++
		finished := r.Status.Phase == giteaactionsv1alpha1.EphemeralRunnerSucceeded ||
			r.Status.Phase == giteaactionsv1alpha1.EphemeralRunnerFailed
		if r.DeletionTimestamp != nil || finished || active[r.Name] {
			c.occupied++
		}
		counts[key] = c
	}
	return counts
}

// desiredRunners sizes a set: every occupied runner stays, each queued job gets its own
// runner, and minRunners is a floor on idle runners, all capped at maxRunners.
func desiredRunners(c setRunners, queued int, rs giteaactionsv1alpha1.GiteaRunnerSetSpec) int32 {
	idle := max(queued, int(rs.MinRunners))
	return int32(min(c.occupied+idle, int(rs.MaxRunners))) // #nosec G115 - capped at an int32
}

// jobMatchesSet reports whether every one of the job's labels is provided by the set.
// An empty job-label list does not match (a job with no runs-on cannot be scheduled here).
func jobMatchesSet(jobLabels []string, setLabelSet map[string]struct{}) bool {
	if len(jobLabels) == 0 {
		return false
	}
	for _, jl := range jobLabels {
		if _, ok := setLabelSet[jl]; !ok {
			return false // job needs a label this set does not advertise
		}
	}
	return true
}
