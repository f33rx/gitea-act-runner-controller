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

package main

import (
	"context"
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/gitea"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(giteaactionsv1alpha1.AddToScheme(scheme)) // Registers all giteaactions CRDs
}

func main() {
	var pollInterval time.Duration
	flag.DurationVar(&pollInterval, "poll-interval", 10*time.Second, "Interval to poll Gitea for queued jobs")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// Create a minimal manager to get a working client.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	// Run the listener loop.
	setupLog.Info("starting listener", "pollInterval", pollInterval)
	listener := &Listener{
		client:       mgr.GetClient(),
		pollInterval: pollInterval,
	}

	ctx := context.Background()
	go func() {
		if err := mgr.Start(ctx); err != nil {
			setupLog.Error(err, "manager failed")
		}
	}()

	// Wait for cache to sync
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		setupLog.Error(nil, "failed to wait for cache sync")
		os.Exit(1)
	}

	if err := listener.Run(ctx); err != nil {
		setupLog.Error(err, "listener failed")
		os.Exit(1)
	}
}

// Listener polls Gitea for queued jobs and patches EphemeralRunnerSet replicas.
type Listener struct {
	client       client.Client
	pollInterval time.Duration
}

// Run starts the listener loop.
func (l *Listener) Run(ctx context.Context) error {
	log := ctrl.Log.WithName("listener")
	ticker := time.NewTicker(l.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			log.Info("listener shutting down")
			return nil
		case <-ticker.C:
			if err := l.syncDemand(ctx); err != nil {
				log.Error(err, "failed to sync demand")
			}
		}
	}
}

// syncDemand polls Gitea for queued jobs and updates EphemeralRunnerSet replicas.
func (l *Listener) syncDemand(ctx context.Context) error {
	log := ctrl.Log.WithName("listener")

	// List all GiteaRunnerSets in the cluster.
	runnerSets := &giteaactionsv1alpha1.GiteaRunnerSetList{}
	if err := l.client.List(ctx, runnerSets); err != nil {
		return err
	}

	for _, rs := range runnerSets.Items {
		// Only handle org-scoped runner sets for now.
		if rs.Spec.RunnerScope != "org" {
			continue
		}

		// Get the Gitea credential Secret.
		credSecret := &corev1.Secret{}
		credKey := client.ObjectKey{
			Namespace: rs.Namespace,
			Name:      rs.Spec.GiteaConfigSecretRef.Name,
		}
		if err := l.client.Get(ctx, credKey, credSecret); err != nil {
			log.Error(err, "failed to get Gitea credential secret", "secret", credKey)
			continue
		}

		token := string(credSecret.Data[rs.Spec.GiteaConfigSecretRef.Key])
		if token == "" {
			log.Error(nil, "empty token in Gitea credential secret", "secret", credKey)
			continue
		}

		// Poll Gitea for queued jobs.
		giteaClient := gitea.NewClient(rs.Spec.GiteaConfigURL, token)
		jobs, totalCount, err := giteaClient.ListOrgQueuedJobs(rs.Spec.OrgName)
		if err != nil {
			log.Error(err, "failed to list queued jobs", "org", rs.Spec.OrgName)
			continue
		}

		// Count jobs that match this runner set's labels.
		matchingJobs := l.countMatchingJobs(jobs, rs.Spec.Labels)
		log.V(1).Info("polled Gitea", "org", rs.Spec.OrgName, "totalQueued", totalCount,
			"matchingJobs", matchingJobs, "labels", rs.Spec.Labels)

		// Compute desired replica count: clamp matching jobs between min and max.
		desiredCount := l.clamp(int32(matchingJobs), rs.Spec.MinRunners, rs.Spec.MaxRunners) // #nosec G115 - matchingJobs is bounded
		log.V(1).Info("computed desired replica count", "name", rs.Name,
			"desired", desiredCount, "min", rs.Spec.MinRunners, "max", rs.Spec.MaxRunners)

		// Get or create the EphemeralRunnerSet.
		ers := &giteaactionsv1alpha1.EphemeralRunnerSet{}
		ersKey := client.ObjectKey{
			Namespace: rs.Namespace,
			Name:      rs.Name,
		}
		patchIDInt := generatePatchIDInt()
		if err := l.client.Get(ctx, ersKey, ers); err != nil {
			if client.IgnoreNotFound(err) == nil {
				// Create the EphemeralRunnerSet.
				ers = &giteaactionsv1alpha1.EphemeralRunnerSet{
					ObjectMeta: metav1.ObjectMeta{
						Name:      rs.Name,
						Namespace: rs.Namespace,
					},
					Spec: giteaactionsv1alpha1.EphemeralRunnerSetSpec{
						Replicas: desiredCount,
						PatchID:  patchIDInt,
					},
				}
				if err := l.client.Create(ctx, ers); err != nil {
					log.Error(err, "failed to create EphemeralRunnerSet", "name", rs.Name)
					continue
				}
				log.Info("created EphemeralRunnerSet", "name", rs.Name, "replicas", desiredCount)
			} else {
				log.Error(err, "failed to get EphemeralRunnerSet", "name", rs.Name)
				continue
			}
		} else {
			// Update the EphemeralRunnerSet replicas and patchID if needed.
			if ers.Spec.Replicas != desiredCount || ers.Spec.PatchID == 0 {
				ers.Spec.Replicas = desiredCount
				ers.Spec.PatchID = patchIDInt
				if err := l.client.Update(ctx, ers); err != nil {
					log.Error(err, "failed to update EphemeralRunnerSet", "name", rs.Name)
					continue
				}
				log.Info("updated EphemeralRunnerSet", "name", rs.Name, "replicas", desiredCount, "patchID", ers.Spec.PatchID)
			}
		}

		// Update status fields for observability.
		ers.Status.TargetSize = desiredCount
		ers.Status.TargetSizeUpdatedAt = &metav1.Time{Time: time.Now()}
		if err := l.client.Status().Update(ctx, ers); err != nil {
			log.Error(err, "failed to update EphemeralRunnerSet status", "name", rs.Name)
		}
	}

	return nil
}

// countMatchingJobs counts how many jobs this runner set can serve.
// Per ADR 0007: a job matches only if its runs-on labels are a SUBSET of the set's
// labels (all-match), and each job is counted at most ONCE. A single runner claims
// exactly one job, so the count is the number of distinct matching jobs.
func (l *Listener) countMatchingJobs(jobs []gitea.Job, setLabels []string) int {
	setLabelSet := make(map[string]struct{}, len(setLabels))
	for _, sl := range setLabels {
		setLabelSet[sl] = struct{}{}
	}

	count := 0
	for _, job := range jobs {
		if jobMatchesSet(job.Labels, setLabelSet) {
			count++ // one runner per matching job
		}
	}
	return count
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

// clamp returns value clamped between lo and hi.
func (l *Listener) clamp(value, lo, hi int32) int32 {
	if value < lo {
		return lo
	}
	if value > hi {
		return hi
	}
	return value
}

// generatePatchIDInt generates a monotonic patch ID for listener/controller coordination.
func generatePatchIDInt() int64 {
	return time.Now().UnixNano()
}
