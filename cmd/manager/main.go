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
	"encoding/json"
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/controller"
	"github.com/f33rx/gitea-act-runner-controller/internal/metrics"
	"github.com/f33rx/gitea-act-runner-controller/internal/watchns"
)

// ADR 0009: explicit rather than left to controller-runtime's library defaults, so a
// future controller-runtime upgrade cannot silently change this manager's shutdown
// behavior. gracefulShutdownTimeout matches controller-runtime's own current default
// (30s); lease timing (LeaseDuration/RenewDeadline/RetryPeriod) is left at the
// library default, which this manager's 10s-poll-driven reconcile load does not
// warrant overriding.
const gracefulShutdownTimeout = 30 * time.Second

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(giteaactionsv1alpha1.AddToScheme(scheme))
}

func main() {
	var metricsAddr string
	var probeAddr string
	var enableLeaderElection bool
	var defaultActiveDeadlineSeconds int64
	var defaultStallWindow time.Duration
	var defaultPendingTimeout time.Duration
	var watchNamespaces string
	var teardownCredentialNamespace, teardownCredentialName string
	var runnerServiceAccount string
	var runnerImage string
	var runnerResources string
	flag.StringVar(&watchNamespaces, "watch-namespaces", "",
		"Comma-separated namespaces to cache and reconcile in. Required when the manager runs "+
			"under namespace-scoped RBAC (a Role per namespace); the default cluster-wide cache "+
			"issues LIST/WATCH a Role cannot authorize. Empty = all namespaces.")
	flag.StringVar(&teardownCredentialNamespace, "teardown-credential-namespace", controller.DefaultTeardownSecretNamespace,
		"Namespace of the Secret holding the org-write Gitea token used to deregister runners (ADR 0006).")
	flag.StringVar(&teardownCredentialName, "teardown-credential-name", controller.DefaultTeardownSecretName,
		"Name of the Secret holding the org-write Gitea token used to deregister runners (ADR 0006).")
	flag.StringVar(&runnerServiceAccount, "runner-service-account", controller.DefaultRunnerServiceAccountName,
		"ServiceAccount name set on runner Pods whose template names none. Must exist in each namespace that holds a GiteaRunnerSet.")
	flag.StringVar(&runnerImage, "default-runner-image", controller.DefaultRunnerImage,
		"Runner container image used when a GiteaRunnerSet's pod template does not set one.")
	flag.StringVar(&runnerResources, "default-runner-resources", "",
		"JSON corev1.ResourceRequirements applied to the runner container for each resource "+
			"its template sets neither a request nor a limit for. Empty = no defaults.")
	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. Required for HA (multi-replica) deployments so "+
			"leader-election-gated components such as the sweep run on only one replica at a time.")
	// ADR 0008: manager-wide timeout defaults. A GiteaRunnerSet can override any of
	// these; 0 here means "no default for that knob" (e.g. no hard cap unless a set
	// opts in). Defaults are deliberately conservative (see ADR 0008 Open question 1
	// for tuning); false-negative (wait longer) is preferred over false-positive
	// (kill a legitimately slow job).
	flag.Int64Var(&defaultActiveDeadlineSeconds, "default-active-deadline-seconds", 0,
		"Default hard cap (seconds) on total EphemeralRunner pod lifetime, kubelet-enforced. "+
			"0 = no default cap unless a GiteaRunnerSet sets activeDeadlineSeconds itself.")
	flag.DurationVar(&defaultStallWindow, "default-stall-window", 15*time.Minute,
		"Default duration an EphemeralRunner with a claimed job may show no job-log growth "+
			"before it is presumed stuck and torn down. 0 disables stall detection by default.")
	flag.DurationVar(&defaultPendingTimeout, "default-pending-timeout", 5*time.Minute,
		"Default duration an EphemeralRunner may go without claiming a job (pod Pending, or "+
			"Running and idle) before it is deleted and retried with backoff by the owning "+
			"EphemeralRunnerSet. "+
			"0 disables pending-timeout detection by default.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	// ADR 0010: register the operator's own metrics against controller-runtime's
	// default registry, the same one the metrics server below already serves.
	metrics.Register(crmetrics.Registry)

	namespaces := watchns.Parse(watchNamespaces)
	if len(namespaces) > 0 {
		setupLog.Info("restricting cache to namespaces", "namespaces", namespaces)
	}
	teardownCredential := types.NamespacedName{Namespace: teardownCredentialNamespace, Name: teardownCredentialName}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Cache:  watchns.CacheOptions(namespaces),
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "gitea-actions-controller.blackrabbitpursuits.com",
		// ADR 0009 Decision 3: release the lease immediately on a graceful SIGTERM
		// instead of waiting out the full LeaseDuration, so a standby takes over
		// promptly on planned restarts/upgrades (the common case). A crash still
		// bounds failover by LeaseDuration, which release-on-cancel cannot help.
		LeaderElectionReleaseOnCancel: true,
		GracefulShutdownTimeout:       ptrDuration(gracefulShutdownTimeout),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	var defaultRunnerResources corev1.ResourceRequirements
	if runnerResources != "" {
		if err := json.Unmarshal([]byte(runnerResources), &defaultRunnerResources); err != nil {
			setupLog.Error(err, "invalid --default-runner-resources")
			os.Exit(1)
		}
	}

	if err = (&controller.EphemeralRunnerReconciler{
		Client:                   mgr.GetClient(),
		Scheme:                   mgr.GetScheme(),
		TeardownCredential:       teardownCredential,
		RunnerServiceAccountName: runnerServiceAccount,
		RunnerImage:              runnerImage,
		RunnerResources:          defaultRunnerResources,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EphemeralRunner")
		os.Exit(1)
	}

	if err = (&controller.SweepReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		TeardownCredential: teardownCredential,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Sweep")
		os.Exit(1)
	}

	if err = (&controller.EphemeralRunnerSetReconciler{
		Client:                       mgr.GetClient(),
		Scheme:                       mgr.GetScheme(),
		APIReader:                    mgr.GetAPIReader(),
		DefaultActiveDeadlineSeconds: defaultActiveDeadlineSeconds,
		DefaultStallWindow:           defaultStallWindow,
		DefaultPendingTimeout:        defaultPendingTimeout,
		Recorder:                     mgr.GetEventRecorderFor("gitea-actions-controller"),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EphemeralRunnerSet")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func ptrDuration(d time.Duration) *time.Duration {
	return &d
}
