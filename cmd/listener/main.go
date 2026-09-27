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
	"github.com/f33rx/gitea-act-runner-controller/internal/watchns"
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
	var watchNamespaces string
	flag.DurationVar(&pollInterval, "poll-interval", 10*time.Second, "Interval to poll Gitea for queued jobs")
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated namespaces to cache GiteaRunnerSets/EphemeralRunnerSets/EphemeralRunners/Secrets in. Required "+
			"under namespace-scoped RBAC; the default cluster-wide cache cannot sync with only a Role. "+
			"Empty = all namespaces.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	namespaces := watchns.Parse(watchNamespaces)
	if len(namespaces) > 0 {
		setupLog.Info("restricting cache to namespaces", "namespaces", namespaces)
	}

	// Create a minimal manager to get a working client.
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache:  watchns.CacheOptions(namespaces),
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

	// Jobs are fetched once per org, and each queued job is counted toward one set.
	sources := map[demandSource][]giteaactionsv1alpha1.GiteaRunnerSet{}
	var order []demandSource
	for _, rs := range runnerSets.Items {
		// Only handle org-scoped runner sets for now.
		if rs.Spec.RunnerScope != "org" {
			continue
		}
		src := demandSource{rs.Spec.GiteaConfigURL, rs.Spec.OrgName}
		if _, seen := sources[src]; !seen {
			order = append(order, src)
		}
		sources[src] = append(sources[src], rs)
	}
	type orgQueue struct {
		waiting []gitea.Job
		active  map[string]bool
	}
	polled := map[demandSource]orgQueue{}
	for _, src := range order {
		jobs, ok := l.orgJobs(ctx, src, sources[src])
		if !ok {
			continue
		}
		q := orgQueue{active: map[string]bool{}}
		for _, job := range jobs {
			switch job.Status {
			case "queued":
				q.waiting = append(q.waiting, job)
			case "in_progress":
				q.active[job.RunnerName] = true
			}
		}
		polled[src] = q
	}

	// Listed after the jobs, so a runner created meanwhile counts as present and idle.
	runners := &giteaactionsv1alpha1.EphemeralRunnerList{}
	if err := l.client.List(ctx, runners); err != nil {
		return err
	}
	queued := map[setKey]int{}
	counts := map[setKey]setRunners{}
	for src, q := range polled {
		srcCounts := countRunners(runners.Items, sources[src], q.active)
		for k, c := range srcCounts {
			counts[k] = c
		}
		for k, n := range assignQueuedJobs(q.waiting, sources[src], srcCounts) {
			queued[k] = n
		}
	}

	for _, rs := range runnerSets.Items {
		key := setKey{rs.Namespace, rs.Name}
		c, polled := counts[key]
		if !polled {
			continue
		}

		desiredCount := desiredRunners(c, queued[key], rs.Spec)
		log.V(1).Info("computed desired replica count", "name", rs.Name,
			"desired", desiredCount, "runners", c.total, "occupied", c.occupied, "queued", queued[key],
			"min", rs.Spec.MinRunners, "max", rs.Spec.MaxRunners)

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
				ensureOwnedBy(ers, &rs)
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
			// Update the EphemeralRunnerSet replicas and patchID if needed. Sets created
			// before the owner reference existed pick it up here.
			// The set scales up only for a PatchID it has not acted on, so a runner that
			// finished after the last poll is not replaced until demand is read again.
			ownerAdded := ensureOwnedBy(ers, &rs)
			scaleUp := desiredCount > int32(c.total) // #nosec G115 - runner count is small
			if ers.Spec.Replicas != desiredCount || ers.Spec.PatchID == 0 || ownerAdded || scaleUp {
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

// orgJobs lists an org's queued and in-progress jobs using the first set whose
// credential Secret is usable; the sets share the org, so any one of them can read it.
func (l *Listener) orgJobs(ctx context.Context, src demandSource, sets []giteaactionsv1alpha1.GiteaRunnerSet) ([]gitea.Job, bool) {
	log := ctrl.Log.WithName("listener")
	for _, rs := range sets {
		credSecret := &corev1.Secret{}
		credKey := client.ObjectKey{Namespace: rs.Namespace, Name: rs.Spec.GiteaConfigSecretRef.Name}
		if err := l.client.Get(ctx, credKey, credSecret); err != nil {
			log.Error(err, "failed to get Gitea credential secret", "secret", credKey)
			continue
		}
		token := string(credSecret.Data[rs.Spec.GiteaConfigSecretRef.Key])
		if token == "" {
			log.Error(nil, "empty token in Gitea credential secret", "secret", credKey)
			continue
		}
		jobs, total, err := gitea.NewClient(src.url, token).ListOrgJobs(ctx, src.org, "queued", "in_progress")
		if err != nil {
			log.Error(err, "failed to list jobs", "org", src.org)
			continue
		}
		if len(jobs) < total {
			log.Info("job listing is partial; runner sets may be undersized", "org", src.org, "listed", len(jobs), "total", total)
		}
		log.V(1).Info("polled Gitea", "org", src.org, "jobs", len(jobs), "sets", len(sets))
		return jobs, true
	}
	return nil, false
}

// ensureOwnedBy makes the GiteaRunnerSet the EphemeralRunnerSet's controller owner so
// that deleting the set cascades through GC instead of leaving the EphemeralRunnerSet
// (and its runners) reconciling forever (garc-6dx). BlockOwnerDeletion is deliberately
// unset: it is only needed for foreground deletion, and setting it requires update on
// gitearunnersets/finalizers under the OwnerReferencesPermissionEnforcement admission
// plugin, a grant the listener does not have. Returns true when a reference was added.
func ensureOwnedBy(ers *giteaactionsv1alpha1.EphemeralRunnerSet, rs *giteaactionsv1alpha1.GiteaRunnerSet) bool {
	for _, ref := range ers.OwnerReferences {
		if ref.UID == rs.UID {
			return false
		}
	}
	isController := true
	ers.OwnerReferences = append(ers.OwnerReferences, metav1.OwnerReference{
		APIVersion: giteaactionsv1alpha1.GroupVersion.String(),
		Kind:       "GiteaRunnerSet",
		Name:       rs.Name,
		UID:        rs.UID,
		Controller: &isController,
	})
	return true
}

// generatePatchIDInt generates a monotonic patch ID for listener/controller coordination.
func generatePatchIDInt() int64 {
	return time.Now().UnixNano()
}
