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

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
)

// SweepReconciler periodically sweeps for orphaned Gitea runners whose pods are gone.
// This catches force-deletes (kubectl delete --force) and crashes that bypass the finalizer.
type SweepReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	lastSweep time.Time
}

//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=gitearunnersets,verbs=get;list;watch
//+kubebuilder:rbac:groups=giteaactions.blackrabbit.dev,resources=ephemeralrunners,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile runs a periodic sweep for orphaned runners.
// It requeues every 60 seconds, and performs a sweep if 60+ seconds have passed.
func (r *SweepReconciler) Reconcile(ctx context.Context, _ ctrl.Request) (ctrl.Result, error) {
	log := log.FromContext(ctx)

	now := time.Now()
	// Only run the sweep every 60 seconds to avoid hammering the API.
	if r.lastSweep.Add(60 * time.Second).After(now) {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	r.lastSweep = now

	// Get the teardown credential Secret.
	teardownSecretName := types.NamespacedName{
		Namespace: "gitea-actions-controller",
		Name:      "gitea-teardown-credential",
	}
	teardownSecret := &corev1.Secret{}
	if err := r.Get(ctx, teardownSecretName, teardownSecret); err != nil {
		log.Error(err, "failed to read teardown credential Secret", "secret", teardownSecretName)
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	token := string(teardownSecret.Data["token"])
	if token == "" {
		log.Error(fmt.Errorf("empty token"), "failed to read token from teardown Secret")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Get all GiteaRunnerSets to discover which orgs/URLs to sweep.
	// GiteaRunnerSet is the stable, persistent object; it outlives individual EphemeralRunners.
	// This ensures we can sweep orphaned runners even when no EphemeralRunner CRs exist.
	runnerSets := &giteaactionsv1alpha1.GiteaRunnerSetList{}
	if err := r.List(ctx, runnerSets); err != nil {
		log.Error(err, "failed to list GiteaRunnerSets")
		return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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

	return ctrl.Result{RequeueAfter: 30 * time.Second}, nil
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
	log := log.FromContext(ctx)

	client := gitea.NewClient(giteaURL, token)
	runners, err := client.ListOrgRunners(org)
	if err != nil {
		log.Error(err, "failed to list org runners", "org", org)
		return
	}

	// For each ephemeral runner in Gitea, check if there's an owning EphemeralRunner CR
	// whose pod is still running. Use the runner NAME as the stable identity.
	runnerList := &giteaactionsv1alpha1.EphemeralRunnerList{}
	if err := r.List(ctx, runnerList); err != nil {
		log.Error(err, "failed to list EphemeralRunners for sweep")
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
				log.V(2).Info("runner has live pod", "name", er.Name, "pod", pod.Name)
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
			log.V(2).Info("runner is claimed by a live CR", "name", runnerRow.Name, "id", runnerRow.ID)
			continue
		}

		// Orphaned: no CR claims it.
		log.Info("found orphaned ephemeral runner, deregistering", "org", org, "runnerId", runnerRow.ID, "name", runnerRow.Name)
		statusCode, err := client.DeregisterOrgRunner(org, runnerRow.ID)
		if err != nil {
			log.Error(err, "failed to deregister orphaned runner", "runnerId", runnerRow.ID)
			continue
		}

		if statusCode != 204 && statusCode != 404 {
			log.Error(fmt.Errorf("unexpected status code"), "deregister returned non-204", "statusCode", statusCode)
		} else {
			log.Info("deregistered orphaned runner", "runnerId", runnerRow.ID, "statusCode", statusCode)
		}
	}
}

// SetupWithManager starts a goroutine that periodically sweeps for orphaned runners.
func (r *SweepReconciler) SetupWithManager(_ ctrl.Manager) error {
	r.lastSweep = time.Now()

	// Start a periodic sweep in the background.
	go r.periodicSweep()

	return nil
}

// periodicSweep runs the sweep every 60 seconds.
func (r *SweepReconciler) periodicSweep() {
	ticker := time.NewTicker(60 * time.Second)
	defer ticker.Stop()

	for range ticker.C {
		ctx := context.Background()
		_, _ = r.Reconcile(ctx, ctrl.Request{
			NamespacedName: types.NamespacedName{
				Namespace: "gitea-actions-controller",
				Name:      "sweep",
			},
		})
	}
}
