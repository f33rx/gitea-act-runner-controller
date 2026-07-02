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
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
)

// defaultSweepInterval is how often the sweep runs when SweepInterval is unset.
const defaultSweepInterval = 60 * time.Second

// SweepReconciler periodically sweeps for orphaned Gitea runners whose pods are gone.
// This catches force-deletes (kubectl delete --force) and crashes that bypass the finalizer.
//
// It is registered with the controller-runtime manager as a Runnable (not an
// object-watching controller), so it is leader-election-gated and its lifecycle
// is bound to the manager: in an HA (multi-replica) deployment only the leader
// sweeps, and the loop drains cleanly when the manager receives SIGTERM.
type SweepReconciler struct {
	client.Client
	Scheme *runtime.Scheme

	// SweepInterval controls how often the sweep runs. Defaults to
	// defaultSweepInterval when zero.
	SweepInterval time.Duration
}

// Ensure SweepReconciler satisfies the manager runnable interfaces at compile time.
var (
	_ manager.Runnable               = &SweepReconciler{}
	_ manager.LeaderElectionRunnable = &SweepReconciler{}
)

//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=gitearunnersets,verbs=get;list;watch
//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=ephemeralrunners,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// SetupWithManager registers the sweep as a manager Runnable so it is
// leader-election-gated and lifecycle-managed by controller-runtime.
func (r *SweepReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return mgr.Add(r)
}

// NeedLeaderElection reports that the sweep must run only on the elected leader.
// Without this, every replica in an HA deployment would sweep concurrently and
// race to deregister the same runners.
func (r *SweepReconciler) NeedLeaderElection() bool {
	return true
}

// Start runs the periodic sweep until ctx is cancelled. It blocks, satisfying
// the manager.Runnable contract; the manager cancels ctx on shutdown (SIGTERM),
// which drains the loop cleanly.
func (r *SweepReconciler) Start(ctx context.Context) error {
	logger := log.FromContext(ctx).WithName("sweep")

	interval := r.SweepInterval
	if interval <= 0 {
		interval = defaultSweepInterval
	}

	logger.Info("starting orphaned-runner sweep", "interval", interval)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	// Run once immediately so a fresh leader does not wait a full interval.
	r.sweep(ctx)

	for {
		select {
		case <-ctx.Done():
			logger.Info("stopping orphaned-runner sweep")
			return nil
		case <-ticker.C:
			r.sweep(ctx)
		}
	}
}

// sweep performs a single sweep pass across all org-scoped GiteaRunnerSets.
// Errors are logged and swallowed: a single failing pass must not stop the loop.
func (r *SweepReconciler) sweep(ctx context.Context) {
	logger := log.FromContext(ctx).WithName("sweep")

	// Get the teardown credential Secret.
	teardownSecretName := types.NamespacedName{
		Namespace: "gitea-actions-controller",
		Name:      "gitea-teardown-credential",
	}
	teardownSecret := &corev1.Secret{}
	if err := r.Get(ctx, teardownSecretName, teardownSecret); err != nil {
		logger.Error(err, "failed to read teardown credential Secret", "secret", teardownSecretName)
		return
	}

	token := string(teardownSecret.Data["token"])
	if token == "" {
		logger.Error(fmt.Errorf("empty token"), "failed to read token from teardown Secret")
		return
	}

	// Get all GiteaRunnerSets to discover which orgs/URLs to sweep.
	// GiteaRunnerSet is the stable, persistent object; it outlives individual EphemeralRunners.
	// This ensures we can sweep orphaned runners even when no EphemeralRunner CRs exist.
	runnerSets := &giteaactionsv1alpha1.GiteaRunnerSetList{}
	if err := r.List(ctx, runnerSets); err != nil {
		logger.Error(err, "failed to list GiteaRunnerSets")
		return
	}

	// Track scanned orgs/URLs to avoid duplicate Gitea API calls.
	scanned := make(map[string]bool)

	for _, runnerSet := range runnerSets.Items {
		// Only sweep org-scoped runner sets.
		if runnerSet.Spec.RunnerScope != "org" {
			continue
		}

		key := fmt.Sprintf("%s:%s", runnerSet.Spec.GiteaConfigURL, runnerSet.Spec.OrgName)
		if scanned[key] {
			continue
		}
		scanned[key] = true

		r.sweepOrgRunners(ctx, runnerSet.Spec.GiteaConfigURL, runnerSet.Spec.OrgName, token)
	}
}

// sweepOrgRunners finds orphaned runners in a Gitea org and deregisters them.
// A runner is considered orphaned if:
// 1. It's ephemeral
// 2. NO EphemeralRunner CR has that name (stable identity)
// 3. NO running pod has that name
// The name-based identity check provides the grace period protection: a newly-registered runner
// will be matched by a live CR (whose pod is running), so it won't be deregistered even if
// status updates haven't fully propagated.
func (r *SweepReconciler) sweepOrgRunners(ctx context.Context, giteaURL, org, token string) {
	logger := log.FromContext(ctx).WithName("sweep")

	client := gitea.NewClient(giteaURL, token)
	runners, err := client.ListOrgRunners(org)
	if err != nil {
		logger.Error(err, "failed to list org runners", "org", org)
		return
	}

	// For each ephemeral runner in Gitea, check if there's an owning EphemeralRunner CR
	// whose pod is still running. Use the runner NAME as the stable identity.
	runnerList := &giteaactionsv1alpha1.EphemeralRunnerList{}
	if err := r.List(ctx, runnerList); err != nil {
		logger.Error(err, "failed to list EphemeralRunners for sweep")
		return
	}

	// Build a map of claimed runner names (CR exists with a live pod).
	claimedRunnerNames := make(map[string]bool)

	for _, er := range runnerList.Items {
		// Check if this CR has a pod that's actually running.
		if er.Status.PodName != "" {
			podName := types.NamespacedName{
				Namespace: er.Namespace,
				Name:      er.Status.PodName,
			}
			pod := &corev1.Pod{}
			if err := r.Get(ctx, podName, pod); err == nil {
				// Pod exists. This CR is still active and claims the runner by name.
				// Use CR name as the stable identity (matches Gitea runner name).
				claimedRunnerNames[er.Name] = true
				logger.V(2).Info("runner has live pod", "name", er.Name, "pod", pod.Name)
			}
		}
	}

	// Deregister ephemeral runners that are NOT claimed by any live CR+Pod.
	for _, runnerRow := range runners {
		if !runnerRow.Ephemeral {
			continue
		}

		// Check if this runner name is claimed by a live CR.
		if claimedRunnerNames[runnerRow.Name] {
			// Claimed, not orphaned.
			logger.V(2).Info("runner is claimed by a live CR", "name", runnerRow.Name, "id", runnerRow.ID)
			continue
		}

		// Orphaned: no CR claims it.
		logger.Info("found orphaned ephemeral runner, deregistering", "org", org, "runnerId", runnerRow.ID, "name", runnerRow.Name)
		statusCode, err := client.DeregisterOrgRunner(org, runnerRow.ID)
		if err != nil {
			logger.Error(err, "failed to deregister orphaned runner", "runnerId", runnerRow.ID)
			continue
		}

		if statusCode != 204 && statusCode != 404 {
			logger.Error(fmt.Errorf("unexpected status code"), "deregister returned non-204", "statusCode", statusCode)
		} else {
			logger.Info("deregistered orphaned runner", "runnerId", runnerRow.ID, "statusCode", statusCode)
		}
	}
}
