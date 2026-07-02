# ADR 0009: HA (leader election) and graceful shutdown

## Status

Proposed (2026-07-02)

bd issue: garc-7ft.11. Anchors the implementation slice of the same name (P2
hardening, epic garc-7ft). Depends on the walking skeleton (garc-7ft.7, closed).

Related: garc-nox (the sweep was made a leader-elected manager Runnable, fixed
2026-07-01 -- this ADR generalizes that fix's premise to the whole manager).

## Context

The manager binary (`cmd/manager/main.go`) already has most of the leader-election
scaffolding controller-runtime provides: a `--leader-elect` flag, `LeaderElectionID`
set, and (as of garc-nox) the sweep runs as a leader-elected `Runnable` rather than a
raw goroutine ticker, so it does not double-fire across replicas. What is missing is
everything that turns that scaffolding into an actual HA deployment:

- `--leader-elect` defaults `false` and the shipped `config/manager/manager.yaml` runs
  `replicas: 1` -- there is no second replica for a standby to be.
- No RBAC grants `coordination.k8s.io` `leases` verbs, so leader election would fail
  to acquire even if enabled.
- `LeaderElectionReleaseOnCancel` and `GracefulShutdownTimeout` are unset (library
  defaults apply, unverified against this manager's actual reconcile durations).
- Nothing has verified, live, that running EphemeralRunner pods survive an operator
  restart -- this is a design property of the existing architecture (Decision 1 below
  explains why), not yet a proven one.

This ADR is deliberately narrow: controller-runtime's leader-election and manager
lifecycle machinery already does the hard distributed-systems part (lease-based
election, safe concurrent-reconcile prevention). The work here is **wiring it on
correctly and proving the failure mode the acceptance criteria cares about**, not
building leader election from scratch.

## Decision

### 1. Running runner pods are independent of the operator by construction -- verify, don't rebuild

`EphemeralRunner` pods poll Gitea directly for their one job and self-exit on
completion (ADR 0007); they do not maintain a live connection to the operator, and
`restartPolicy: Never` means the kubelet does not need the operator to keep them
alive either. So "running runner pods survive operator restart/upgrade" is **already
true by the existing architecture** -- an operator outage does not touch already-
running pods at all. What is *not* yet true without this ADR's other decisions is that
teardown, scale-down, and the sweep keep working *while the operator is briefly down or
mid-failover* -- a runner finishing its job during a leader gap will sit torn-down-
pending until a leader resumes reconciling. This ADR's acceptance criterion is
therefore satisfied by (a) proving pods are unaffected (a live test, no code change)
and (b) minimizing the failover gap (Decisions 2-4) so the pending-teardown window
stays short.

### 2. Enable leader election by default in the shipped manifest; 2 replicas

- `config/manager/manager.yaml` ships `replicas: 2` (not more -- controller-runtime
  leader election is active-standby, not active-active; a third replica adds cost with
  no additional safety) and `--leader-elect=true`.
- RBAC gains a `Role`/`ClusterRole` rule for `coordination.k8s.io` `leases`
  (`get;list;watch;create;update;patch`), scoped to the controller's own namespace
  (leases are namespaced; `LeaderElectionNamespace` is left to controller-runtime's
  default, which uses the manager's own namespace via the in-cluster config).
- The single-replica local-dev flow (`dev/e2e/run.sh`, `dev/gitea/README.md`) is
  unaffected: leader election with `replicas: 1` still works (the one replica is
  simply always the leader) and is not disabled for dev -- exercising the same code
  path in dev as production avoids an HA-only code path that is untested until
  production.

### 3. Fast failover: tune lease timing and release-on-cancel, do not rely on library defaults untuned

- Set `LeaderElectionReleaseOnCancel: true`. On a graceful SIGTERM, the leader
  releases its lease immediately as part of shutdown rather than waiting for the full
  `LeaseDuration` to expire -- this is what makes "a standby takes over within the
  lease window" fast for planned restarts/upgrades (the common case) instead of only
  bounding the crash case (unplanned death, where release-on-cancel cannot help and
  the standby must wait out `LeaseDuration`).
- Lease timing: keep controller-runtime's defaults (`LeaseDuration` 15s, `RenewDeadline`
  10s, `RetryPeriod` 2s) rather than hand-tuning. These are the same defaults used
  across the Kubernetes controller ecosystem and are already reasonable for this
  manager's reconcile workload (10s poll-driven reconciles, per ADR 0007); revisit only
  if live testing (Decision 5) shows an unacceptable failover gap.

### 4. Graceful drain: bound it, and make it correct for in-flight Gitea calls

- Set `GracefulShutdownTimeout` explicitly (proposed: 30s, matching controller-runtime's
  own default -- stated explicitly rather than left implicit, so a future controller-
  runtime upgrade cannot silently change this manager's shutdown behavior).
- controller-runtime already threads the manager's root context through to each
  reconciler's `ctx` and cancels it on SIGTERM; the existing reconcilers already
  respect `ctx` for their Kubernetes API calls (via the controller-runtime client).
  The one gap: the Gitea HTTP client calls in `internal/gitea` (deregister, list
  runners, registration tokens) are **not currently context-aware** -- they use
  `http.Client` calls built without threading the reconcile `ctx` in per-request
  timeouts. This ADR's implementation scope includes wiring `ctx` through those calls
  (`req.WithContext(ctx)` or an equivalent), so an in-flight deregister call during
  shutdown is cancelled cleanly within the grace period rather than either completing
  unbounded or being abruptly killed by process exit. Without this, a slow Gitea call
  during shutdown could hold up the drain until `GracefulShutdownTimeout` forcibly cuts
  it off anyway -- correct but noisier (a cancelled-by-timeout log line instead of a
  clean cancel).

### 5. Verification is a live 2-replica test, not just a unit test

The acceptance criteria describe a live failover scenario (2 replicas, kill the
leader, standby takes over within the lease window, in-flight reconciles drain, running
pods unaffected). This is proven the same way garc-c7i's teardown re-proof was: on the
garc-dev kind cluster, scale the deployment to 2, identify the leader (via the lease
object or logs), delete its pod mid-reconcile (ideally while a runner is mid-teardown,
to exercise the drain), and assert:
1. The standby acquires leadership within `LeaseDuration` + a small margin.
2. Any runner pod that was `Running` at kill time is untouched (still running,
   unaffected by the operator gap).
3. Reconciliation resumes correctly post-failover (no double-processed teardown, no
   stuck finalizer) -- the same conflict-retry and idempotent-reconcile properties
   already hardened in garc-c7i apply here too.

This is not a new test harness; it extends `dev/e2e/run.sh` (or a sibling script) with
a failover phase, consistent with garc-qmx's e2e-proof-not-just-unit-tests direction.

## Consequences

### Positive

- Uses controller-runtime's leader election as designed; no custom distributed-lock
  code to get wrong.
- Release-on-cancel makes the common case (planned restart/upgrade) fast, which is the
  scenario that actually happens routinely (deploys), vs. the crash case which is rarer
  and still bounded (by `LeaseDuration`).
- Running pods are provably unaffected because of the existing architecture (ADR 0007
  ephemerality), not because of new code -- lower risk.
- Dev and prod exercise the same leader-election code path (Decision 2), so HA is not
  a "only tested in prod" surface.

### Negative

- 2 replicas doubles the manager's baseline resource cost (still small -- the manager
  is a lightweight reconciler, not the runner pods).
- The Gitea-client context-threading change (Decision 4) touches `internal/gitea`,
  which is shared by the teardown finalizer, the sweep, and the listener's registration-
  token calls -- needs care not to regress the just-hardened garc-c7i conflict-retry
  and teardown paths. Mitigation: this ADR's implementation re-runs the garc-c7i live
  re-proof (all 4 criteria) after the change, not just new HA-specific tests.

### Risks

- **R-HA-1: failover gap longer than the lease window under load.** If the manager
  pod is slow to become `Ready` (image pull, slow start), the standby's readiness gate
  could lag lease acquisition. Mitigation: keep the manager image small/already-pulled
  (same image as pre-HA); readiness probe already exists (`/readyz`).
- **R-HA-2: split-brain during a network partition.** Standard leader-election risk;
  controller-runtime's lease-based election (server-side, via the API server) inherits
  Kubernetes' own consistency guarantees here -- not a risk this ADR introduces or can
  mitigate further; documented as an accepted, standard Kubernetes HA limitation.
- **R-HA-3: the Gitea-client context change has a wider blast radius than HA itself**
  (Decision 4's negative consequence). Mitigation: full garc-c7i re-proof as an
  explicit implementation gate, not just new tests.

## Open questions

1. **Should the listener Deployment (per-`GiteaRunnerSet`, ADR 0007) also run >1
   replica for its own HA?** Out of scope here -- the listener is stateless and
   crash-tolerant by design (0007 Decision 7: "a listener restart loses nothing"), so
   it does not need leader election at all; a single replica restarting is already
   acceptable. Noted so a future reader does not assume this ADR covers it.
2. **PodDisruptionBudget for the manager?** Not addressed here; would prevent a
   voluntary eviction (e.g. node drain) from taking down both replicas at once during
   a cluster upgrade. Candidate follow-up, not blocking this ADR's acceptance criteria.

## References

- ADR 0003 - CRD hierarchy: `restartPolicy: Never`, the reconcilers this ADR's drain
  behavior applies to.
- ADR 0007 - scaling algorithm: the ephemerality property that makes Decision 1's
  "pods survive by construction" claim true; listener statelessness (Open question 1).
- ADR 0006 - credential model: the teardown/registration credentials used by the
  `internal/gitea` calls this ADR threads context through (Decision 4).
- garc-nox: prior fix that made the sweep a leader-elected manager Runnable instead of
  a raw goroutine ticker -- the precedent this ADR generalizes to the whole manager.
- garc-c7i: the reconcile-hot-loop and conflict-retry fix whose properties this ADR's
  implementation must not regress (Decision 5, Risk R-HA-3).
