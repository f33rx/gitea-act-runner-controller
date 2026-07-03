# ADR 0010: Observability - kubectl visibility and Prometheus metrics

## Status

Proposed (2026-07-02)

bd issue: garc-7ft.12. Anchors the implementation slice of the same name (P2
hardening, epic garc-7ft). Depends on the walking skeleton (garc-7ft.7, closed).

Related ADRs: CRD hierarchy (0003, the status fields this ADR surfaces), scaling
algorithm (0007, `minRunners`/`maxRunners`/`Replicas`), timeouts and stall detection
(0008, the phases and stall events this ADR counts).

## Context

The operator's `EphemeralRunner`/`EphemeralRunnerSet`/`GiteaRunnerSet` CRDs already
carry a `status.phase` and related fields (ADR 0003), and controller-runtime already
serves a default `/metrics` endpoint (Go runtime, controller-runtime's own reconcile
counters/histograms per controller) -- but none of that is visible without already
knowing to look. Today:

- `kubectl get ephemeralrunners` shows only `NAME`/`AGE` (no `+kubebuilder:printcolumn`
  markers exist), so an operator has to `kubectl get -o yaml` every single runner to
  see its phase, which job it claimed, or which pod backs it.
- There are no operator-defined Prometheus metrics at all: no way to see fleet size
  (pending/running/failed runner counts), no way to see scale-up/scale-down activity
  against `minRunners`/`maxRunners`, and no way to see how often ADR 0008's stall/
  pending-timeout paths fire without grepping controller logs.

This is the last of the four P2 hardening slices (with 0008 timeouts, 0009 HA) that is
purely about making the operator's *own* behavior legible -- it adds no new
functionality to runner provisioning, scaling, or teardown.

The boundary this ADR must respect (established in 0003/SPEC and reaffirmed in 0008
Decision 2): **Gitea owns job/log content and status; the operator owns infra
status.** Metrics here describe Kubernetes-side objects (runners, pods, phases,
reconcile outcomes) the operator directly manages -- never job step counts, log
content, or anything that would require calling Gitea's job/log API. (ADR 0008's
`recordLogProgress` does call the Gitea job-log API, but only to compare a
`Content-Length` header for stall-liveness -- it does not expose per-job Gitea metrics,
and this ADR does not add any either, for the same ownership-boundary reason.)

## Decision

### 1. kubectl visibility: printcolumns on existing status fields, no new fields

All the data the acceptance criteria asks for ("kubectl shows per-runner phase") is
already on the CRD status structs (ADR 0003/0008); it is a visibility gap, not a data
gap. Add `+kubebuilder:printcolumn` markers:

- `EphemeralRunner`: `Phase` (`.status.phase`), `Reason` (`.status.reason`, wide-only),
  `Pod` (`.status.podName`), `Job` (`.status.jobRef`), `Age`.
- `EphemeralRunnerSet`: `Desired` (`.spec.replicas`), `Ready`
  (`.status.readyReplicas`), `Available` (`.status.availableReplicas`), `Age`.
- `GiteaRunnerSet`: `Min` (`.spec.minRunners`), `Max` (`.spec.maxRunners`), `Ready`
  (`.status.readyReplicas`), `Available` (`.status.availableReplicas`), `Age`.

No new status fields, no new reconcile logic -- this is purely `zz_generated`/CRD
manifest surface (`make manifests`) plus the marker comments.

### 2. Metrics library and registration: controller-runtime's existing registry

Use `sigs.k8s.io/controller-runtime/pkg/metrics`' global `Registry` (a
`prometheus.Registerer`) via `metrics.Registry.MustRegister(...)` at `init()` or
manager-setup time, the same registry controller-runtime's own reconcile metrics
already register to and the same `metricsserver.Options{BindAddress: metricsAddr}`
endpoint already serves (`cmd/manager/main.go`, currently `:8080`, already exposed --
see ADR 0009's manifest, which did not change this port). No new server, no new
dependency: `client-go`'s and controller-runtime's existing transitive
`prometheus/client_golang` is already vendored.

### 3. Metric set: fleet gauges + lifecycle counters, org/set-scoped labels only

Naming follows Prometheus/kube-ecosystem convention: `<namespace>_<subsystem>_<name>`,
namespace `giteaactions`.

**Gauges** (current-state snapshots, updated on every reconcile that touches the
relevant object):

- `giteaactions_ephemeralrunner_phase_count{gitearunnerset, namespace, phase}` --
  count of `EphemeralRunner` objects currently in each phase
  (`Pending`/`Running`/`Succeeded`/`Failed`). Recomputed from the informer cache on
  each `EphemeralRunnerSet` reconcile (cheap: it already lists owned runners for
  scaling decisions per ADR 0007) rather than incremented/decremented per-event, so a
  missed or reordered event can never leave a stale count -- the gauge is always
  reset to the current cache state.
- `giteaactions_ephemeralrunnerset_desired{gitearunnerset, namespace}`,
  `_min{...}`, `_max{...}`, `_ready{...}`, `_available{...}` -- direct mirrors of
  `EphemeralRunnerSet.spec.replicas`/`.spec.minRunners`/`.spec.maxRunners`/
  `.status.readyReplicas`/`.status.availableReplicas`, letting the acceptance
  criteria's "desired vs min/max" be graphed directly.

**Counters** (monotonic lifecycle events, incremented at the point the reconciler
already decides the transition happened -- no new detection logic):

- `giteaactions_job_started_total{gitearunnerset, namespace}` -- incremented once
  when an `EphemeralRunner` transitions into `Running`
  (`updateRunnerStatusFromPod`'s existing phase-change branch, ADR 0003).
- `giteaactions_job_completed_total{gitearunnerset, namespace, result}` --
  incremented once when an `EphemeralRunner` transitions into `Succeeded` or `Failed`
  (`result` label value `succeeded`/`failed`), at the same auto-teardown decision
  point that already exists in `ephemeralrunner_controller.go`'s `Reconcile`.
- `giteaactions_runner_stalled_total{gitearunnerset, namespace}` and
  `giteaactions_runner_pending_timeout_total{gitearunnerset, namespace}` --
  incremented at `checkTimeout`'s two existing fire points (ADR 0008 Decisions 3-4),
  directly satisfying 0008's own Risk R-TIMEOUT-3 mitigation ("metrics expose repeated
  Pending-timeout events per set").

**Labels are deliberately coarse**: `gitearunnerset` + `namespace` only, never
per-runner or per-pod-name labels. A label cardinality of "one series per fleet" is
bounded by the number of `GiteaRunnerSet` objects (small, operator-managed); a label
of runner name would be bounded by churn rate (unbounded over time, a classic
Prometheus cardinality trap for ephemeral-resource controllers) and buys no
aggregate-visibility value the acceptance criteria asks for. `namespace` (the
`GiteaRunnerSet`'s namespace) is included because ADR-0008-era research already
flagged garc-kvm (RBAC not namespace-scoped) as an open gap -- these labels keep
metrics usable if/when that lands.

### 4. Implementation surface: a small internal/metrics package, wired at existing decision points

A new `internal/metrics` package defines and registers the gauges/counters (Decision
2/3). Each already-identified decision point (phase-change branch in
`updateRunnerStatusFromPod`, the `Succeeded`/`Failed` auto-teardown branch, the two
`checkTimeout` fire sites, `EphemeralRunnerSet`'s existing per-reconcile scale
calculation) gets a single metrics call added inline -- no new watches, no new
reconcile loops, no additional Gitea or Kubernetes API calls. This keeps the
metrics slice additive to 0008/0009's already-hardened control flow rather than a
parallel observation path that could drift from it.

## Consequences

### Positive

- Closes the acceptance criteria's kubectl-visibility gap with zero new status
  fields or reconcile logic -- printcolumns are a manifest-only change.
- Metrics are wired at decision points the reconcilers already have (phase
  transitions, `checkTimeout` fire sites, scale calculation), so they cannot
  drift out of sync with the actual state machine the way a separately-computed
  metrics pass could.
- Directly satisfies ADR 0008's own deferred Risk R-TIMEOUT-3 mitigation
  (dashboards/alerting on repeated pending-timeout events), closing an open
  cross-ADR dependency.
- Bounded, small label cardinality (per-set, not per-runner) avoids the standard
  ephemeral-resource-controller Prometheus cardinality trap.

### Negative

- Gauges recomputed from a full list-owned-runners pass on every
  `EphemeralRunnerSet` reconcile add a small amount of CPU per reconcile; judged
  negligible since the list call itself already happens for scaling (ADR 0007) and
  reconciles are 10s-interval, not per-event-storm.
- Counters reset to zero on manager restart (standard Prometheus counter behavior,
  not operator-specific) -- rate()/increase() queries across a restart boundary need
  the usual PromQL counter-reset handling; not a new operational burden but worth
  documenting in deploy docs (garc-7ft.13).

### Risks

- **R-OBS-1: label cardinality creep.** A future change adding a per-runner or
  per-pod label would reintroduce the cardinality trap this ADR explicitly avoids.
  Mitigation: this ADR's Decision 3 is the enforced ceiling; any new label needs its
  own review, not a drive-by addition.
- **R-OBS-2: gauge/counter drift from actual reconcile behavior if a future change
  adds a new phase-transition path without also updating the metrics call.**
  Mitigation: metrics calls live inline at the same decision points as the state
  transitions themselves (Decision 4), not in a separate pass, so a new transition
  path is naturally adjacent to its metrics call in the diff.

## Open questions

1. **Whether a `giteaactions_gitea_api_errors_total{operation}` counter (sweep list
   failures, teardown deregister failures, ADR 0008's job-log/discovery call
   failures) belongs in this slice or a later one.** Leaning yes (it is Kubernetes/
   operator-side observability, not Gitea job/log content) but deferred to
   implementation review since it wasn't named in the original acceptance criteria.
2. **Dashboard/alerting rules themselves** (Grafana JSON, PrometheusRule
   `alert:` definitions) are out of scope for this ADR -- it defines the metrics
   surface; consuming it into dashboards is a deploy/packaging concern
   (garc-7ft.13) or an operational follow-up.

## References

- ADR 0003 - CRD hierarchy: the existing `status.phase`/`jobRef`/`podName` fields
  this ADR surfaces via printcolumns, adds no new fields to.
- ADR 0007 - scaling algorithm: `minRunners`/`maxRunners`/`Replicas`/
  `readyReplicas`/`availableReplicas`, the existing per-reconcile owned-runner list
  this ADR's gauges piggyback on.
- ADR 0008 - timeouts and stall detection: `checkTimeout`'s two fire points (this
  ADR's counter sources); Risk R-TIMEOUT-3's metrics mitigation, which this ADR
  closes; the Gitea job/log ownership boundary this ADR's Context section reaffirms.
- ADR 0009 - HA and graceful shutdown: the existing `metricsserver.Options` bind
  address this ADR's metrics are served from, unchanged.
- garc-kvm: the namespace-scoped-RBAC gap whose eventual fix this ADR's
  `namespace` label is chosen to remain compatible with.
