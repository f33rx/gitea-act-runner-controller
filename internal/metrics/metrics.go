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

// Package metrics defines the operator's Prometheus metrics (ADR 0010). Labels are
// deliberately limited to gitearunnerset+namespace (never per-runner or per-pod names)
// to keep cardinality bounded by the number of managed fleets, not runner churn.
package metrics

import "github.com/prometheus/client_golang/prometheus"

const namespace = "giteaactions"

var (
	// EphemeralRunnerPhaseCount is a gauge reset to the current informer-cache state on
	// every EphemeralRunnerSet reconcile (never incremented/decremented per-event), so a
	// missed or reordered event can never leave a stale count.
	EphemeralRunnerPhaseCount = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "ephemeralrunner",
		Name:      "phase_count",
		Help:      "Current number of EphemeralRunners in each phase, per GiteaRunnerSet.",
	}, []string{"gitearunnerset", "namespace", "phase"})

	// EphemeralRunnerSetDesired mirrors EphemeralRunnerSet.spec.replicas.
	EphemeralRunnerSetDesired = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "ephemeralrunnerset",
		Name:      "desired",
		Help:      "EphemeralRunnerSet.spec.replicas (the scaling decision's target size).",
	}, []string{"gitearunnerset", "namespace"})

	// EphemeralRunnerSetMin mirrors GiteaRunnerSet.spec.minRunners.
	EphemeralRunnerSetMin = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "ephemeralrunnerset",
		Name:      "min",
		Help:      "GiteaRunnerSet.spec.minRunners (the warm floor).",
	}, []string{"gitearunnerset", "namespace"})

	// EphemeralRunnerSetMax mirrors GiteaRunnerSet.spec.maxRunners.
	EphemeralRunnerSetMax = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "ephemeralrunnerset",
		Name:      "max",
		Help:      "GiteaRunnerSet.spec.maxRunners (the hard ceiling).",
	}, []string{"gitearunnerset", "namespace"})

	// EphemeralRunnerSetReady mirrors EphemeralRunnerSet.status.readyReplicas.
	EphemeralRunnerSetReady = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "ephemeralrunnerset",
		Name:      "ready",
		Help:      "EphemeralRunnerSet.status.readyReplicas.",
	}, []string{"gitearunnerset", "namespace"})

	// EphemeralRunnerSetAvailable mirrors EphemeralRunnerSet.status.availableReplicas.
	EphemeralRunnerSetAvailable = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Subsystem: "ephemeralrunnerset",
		Name:      "available",
		Help:      "EphemeralRunnerSet.status.availableReplicas.",
	}, []string{"gitearunnerset", "namespace"})

	// JobStartedTotal is incremented once per EphemeralRunner transition into Running,
	// at the same phase-change decision point updateRunnerStatusFromPod already has.
	JobStartedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "job",
		Name:      "started_total",
		Help:      "Total EphemeralRunner transitions into the Running phase.",
	}, []string{"gitearunnerset", "namespace"})

	// JobCompletedTotal is incremented once per EphemeralRunner transition into a
	// terminal phase, labeled by result (succeeded/failed).
	JobCompletedTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "job",
		Name:      "completed_total",
		Help:      "Total EphemeralRunner transitions into a terminal phase, by result.",
	}, []string{"gitearunnerset", "namespace", "result"})

	// RunnerStalledTotal is incremented at checkTimeout's Running-phase fire point
	// (ADR 0008 Decision 3).
	RunnerStalledTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "runner",
		Name:      "stalled_total",
		Help:      "Total EphemeralRunners torn down for exceeding the stall window (ADR 0008).",
	}, []string{"gitearunnerset", "namespace"})

	// RunnerPendingTimeoutTotal is incremented at checkTimeout's Pending-phase fire
	// point (ADR 0008 Decision 4).
	RunnerPendingTimeoutTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Subsystem: "runner",
		Name:      "pending_timeout_total",
		Help:      "Total EphemeralRunners deleted for exceeding the pending timeout (ADR 0008).",
	}, []string{"gitearunnerset", "namespace"})
)

// Register adds all operator metrics to the given registry. Called once from
// cmd/manager/main.go with controller-runtime's metrics.Registry, the same registry
// controller-runtime's own reconcile metrics use and the metrics server already serves.
func Register(reg prometheus.Registerer) {
	reg.MustRegister(
		EphemeralRunnerPhaseCount,
		EphemeralRunnerSetDesired,
		EphemeralRunnerSetMin,
		EphemeralRunnerSetMax,
		EphemeralRunnerSetReady,
		EphemeralRunnerSetAvailable,
		JobStartedTotal,
		JobCompletedTotal,
		RunnerStalledTotal,
		RunnerPendingTimeoutTotal,
	)
}
