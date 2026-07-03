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
	"flag"
	"os"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	giteaactionsv1alpha1 "github.com/f33rx/gitea-act-runner-controller/api/v1alpha1"
	"github.com/f33rx/gitea-act-runner-controller/internal/controller"
)

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
		"Default duration a Running EphemeralRunner may show no phase-change progress before "+
			"it is presumed stuck and torn down. 0 disables stall detection by default.")
	flag.DurationVar(&defaultPendingTimeout, "default-pending-timeout", 5*time.Minute,
		"Default duration an EphemeralRunner may stay Pending (never claimed a job) before "+
			"it is deleted and retried with backoff by the owning EphemeralRunnerSet. "+
			"0 disables pending-timeout detection by default.")
	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme: scheme,
		Metrics: metricsserver.Options{
			BindAddress: metricsAddr,
		},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "gitea-actions-controller.blackrabbit.dev",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err = (&controller.EphemeralRunnerReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "EphemeralRunner")
		os.Exit(1)
	}

	if err = (&controller.SweepReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Sweep")
		os.Exit(1)
	}

	if err = (&controller.EphemeralRunnerSetReconciler{
		Client:                       mgr.GetClient(),
		Scheme:                       mgr.GetScheme(),
		DefaultActiveDeadlineSeconds: defaultActiveDeadlineSeconds,
		DefaultStallWindow:           defaultStallWindow,
		DefaultPendingTimeout:        defaultPendingTimeout,
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
