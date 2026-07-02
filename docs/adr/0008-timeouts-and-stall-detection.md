# ADR 0008: Timeouts and stuck-vs-slow detection

## Status

Proposed (2026-07-02)

bd issue: garc-7ft.10. Anchors the implementation slice of the same name (P2 hardening,
epic garc-7ft). Depends on the walking skeleton (garc-7ft.7, closed).

Related ADRs: CRD hierarchy (0003, `EphemeralRunner.status`), scaling algorithm (0007,
busy-runner-never-killed precedent this ADR must stay consistent with).

## Context

An ephemeral runner pod can fail to make progress in two different ways, and the
operator must tell them apart:

1. **Stuck**: the pod is alive but the job inside it is wedged (deadlocked build,
   network partition to Gitea, a step that hangs forever). Nothing will ever finish
   this on its own; the pod must be killed and the job failed so the slot is freed.
2. **Slow**: the pod is alive and the job is legitimately still working (a long test
   suite, a big image pull, a slow build). Killing this would destroy real work for no
   reason.

Today (garc-7ft.7/.9) the operator has no timeout at all: a wedged pod runs forever,
holding a Gitea runner row `busy: true` and consuming cluster resources indefinitely
until someone notices. `EphemeralRunnerStatus.LastObservedTime` (ADR 0003) exists but
is currently only bumped on **phase transitions** (Pending -> Running -> Succeeded),
not on in-job progress -- so as written it cannot distinguish stuck from slow; a
6-hour hung build and a 6-hour legitimately-running build look identical.

The scaling ADR (0007) already established a hard rule this ADR must not violate:
**busy runners are never killed** except for a reason the operator has actively
decided ("this job is dead"), never as a side effect of scale-down. A stall-kill is
the one narrow, deliberate exception -- it must be visibly different from a scale-down
delete (different code path, different Gitea-side effect: the task should be reported
failed, not silently orphaned).

## Decision

### 1. Two independent signals, not one

- **Hard cap (`activeDeadlineSeconds`)**: an absolute ceiling on total pod lifetime,
  set on the Pod spec itself (native Kubernetes field -- kubelet enforces it, killing
  the pod and marking it `Failed` with reason `DeadlineExceeded` with zero operator
  code required for the kill itself). This is the backstop: even if the stall-window
  logic below has a bug, nothing runs forever.
- **Stall window (liveness)**: a shorter, operator-enforced check that a pod is making
  *progress*, independent of the hard cap. A job can be well under the hard cap and
  still be stuck (a 10-minute deadlock inside a 6-hour cap).

These are deliberately two separate mechanisms: the hard cap is a dumb ceiling (correct
by construction, kubelet-enforced); the stall window is where the "is this actually
progressing" judgment call lives, and is allowed to be wrong in the safe direction
(see Decision 3).

### 2. Progress signal: pod-phase transitions only in v1, not log tailing

The task description mentions "task log/heartbeat progress." For v1 the operator does
**not** tail act_runner's job logs or parse Gitea's task log stream -- that duplicates
Gitea's own log ownership (the boundary ADR 0003/SPEC already draws: "Gitea owns
job/log status; operator owns infra status") and adds a log-reading credential and
parsing surface for marginal benefit.

Instead, v1's progress signal is **pod-phase and container-restart transitions**,
already visible to the operator with zero new credentials:
- Any Pod condition transition (`Ready`, container restarts, `PodScheduled`) updates
  `LastObservedTime`.
- A pod stuck `Pending` (unschedulable) is a distinct, faster-firing case (Decision 4)
  from a pod `Running` with no observed change.

This is coarser than true job-log liveness (a job that hangs mid-step with no pod-level
signal for the whole stall window won't be caught until the window elapses), but it is
consistent with the existing status/log ownership boundary and ships in v1 without a
new Gitea log-reading credential. **Explicitly flagged as a v2 candidate** if the stall
window proves too coarse in practice (Open question 2).

### 3. Stall window action: fail and tear down, default to false-negative over false-positive

When the stall window elapses with no progress signal:
1. Delete the Pod (the runner's only job is dead; there is nothing to preserve).
2. Set `EphemeralRunner.status.phase = Failed`, `reason = "stalled: no progress for
   <window>"`.
3. The existing auto-teardown path (garc-7ft.9, `pod finished -> initiating teardown`)
   fires normally: the finalizer deregisters the Gitea runner row. Gitea's own
   zombie-task reaper (~10min, per garc-7ft.9 evidence) independently reconciles the
   task's terminal state; the operator does not attempt to directly mark the Gitea task
   failed (no API for that as a runner-scoped credential; deregistering the runner is
   the operator's full authority here, consistent with ADR 0006's credential scoping).

**Default the stall window generously** (proposed default: 15 minutes of no pod-level
signal). A false negative (a genuinely stuck job runs a bit longer before being
caught) costs cluster resources; a false positive (a slow-but-fine job gets killed)
costs a developer's real work and violates the "busy runners are never killed" trust
the scaling ADR establishes. When forced to choose, this ADR chooses to wait longer.

### 4. Pending-pod case is separate and faster

A pod stuck `Pending` (unschedulable: no capacity, image pull failure, resource
policy rejected by admission) is not "slow," it never started -- there is no job
in flight to protect. This case uses a **separate, shorter timeout** (proposed
default: 5 minutes) and is treated as a **pre-claim failure**: the runner never
registered/claimed a Gitea task, so on timeout the operator deletes the
`EphemeralRunner` and lets the owning `EphemeralRunnerSet` recreate it (retry with
capped backoff -- see Decision 5), rather than going through the stall-fail path.

### 5. Retry scope: pre-claim only, never a claimed task

Per the acceptance criteria: retry is for a pod that dies **before** claiming a task
(crash-loop, scheduling failure, registration failure). Once a runner has claimed a
Gitea task (`busy: true`, a `JobRef` set), it is never silently re-run -- Gitea already
owns re-queue/retry semantics for a claimed-then-lost task (the crash-recovery path
from garc-i5b: an unstuck `in_progress` task is Gitea's zombie reaper's job, not the
operator's). The operator's retry logic therefore only applies to the Pending/pre-claim
case (Decision 4); a stalled *claimed* task (Decision 3) is failed, not retried --
retrying a claimed task would mean two runners could believe they own the same task,
which the scaling ADR's ephemeral-one-job-per-runner model forbids.

Capped backoff for pre-claim retries: exponential, small cap (proposed: 15s, 30s, 60s,
then hold at 60s), bounded attempt count is deliberately **not** set -- an
unschedulable pod should keep retrying until cluster capacity returns, not give up
and silently drop demand (consistent with 0007's "never silently drop demand" scale-up
principle). A persistently-failing pod is visible via the metrics this hardening slice
is a sibling to (garc-7ft.12), which is the intended detection surface for "this keeps
failing," not a retry-count cutoff.

### 6. Configuration: cluster default + per-`GiteaRunnerSet` override, no new CRD

ADR 0004 (resource policy resolution, repo -> org -> global) is **design-only** --
no `ResourcePolicy` CRD exists yet. Building the full scope-resolution chain as part
of this hardening slice would be scope creep. v1 timeout configuration is:
- A manager-wide default (flag/env on the controller, e.g.
  `--default-active-deadline-seconds`, `--default-stall-window`,
  `--default-pending-timeout`).
- An optional override on `GiteaRunnerSet.spec` (e.g.
  `activeDeadlineSeconds *int64`, `stallWindow *metav1.Duration`) since that is
  the CRD that already exists and is the natural per-fleet override point.

Repo/org-level resolution (the fuller ADR 0004 chain) is deferred until the
`ResourcePolicy` CRD itself lands; this ADR's config shape is designed to slot under
it later without a breaking change (the `GiteaRunnerSet`-level fields become the
"set" tier of that resolution chain).

## Consequences

### Positive

- A wedged job can no longer run forever; the hard cap is a kubelet-enforced backstop
  independent of any operator bug.
- The stuck/slow distinction is drawn conservatively (generous defaults, false-negative
  bias), so it cannot become a second "scale-down kills busy runners" incident
  (the exact garc-c7i Bug 2 class of bug) by design.
- No new Gitea credential or log-reading surface; stays inside the existing
  infra-status/no-log-proxying boundary.
- Pre-claim retry reuses the existing EphemeralRunnerSet recreate-on-delete behavior;
  no new retry machinery.

### Negative

- Coarse (pod-phase-only) progress signal means a job that hangs deep inside a single
  long-running step with no pod-level change is only caught at the stall window, not
  the moment it actually wedged. Acceptable for v1; flagged as a v2 candidate.
- Two new timeout knobs (+pending timeout) with sane-but-arguable defaults; needs
  tuning against real workload duration distributions once the fleet has traffic.

### Risks

- **R-TIMEOUT-1: stall window too short.** A legitimately slow job (big test suite)
  gets killed. Mitigation: generous default (15min), per-set override, false-negative
  bias by design (Decision 3).
- **R-TIMEOUT-2: hard cap too short for legitimate long builds.** Mitigation:
  per-set override; the hard cap should be set well above the stall window (it is
  the backstop, not the primary control).
- **R-TIMEOUT-3: unbounded pre-claim retry masks a persistent misconfiguration**
  (e.g. a resource policy that can never be admitted). Mitigation: metrics (garc-7ft.12)
  expose repeated Pending-timeout events per set; this is a dashboard/alerting problem,
  not a retry-count cutoff, per Decision 5's reasoning.

## Open questions

1. **Exact default values** (15min stall / 5min pending / no default hard cap vs. a
   conservative one like 6h) are starting points, not tuned. Revisit once the e2e
   harness (garc-qmx) or real fleet traffic gives duration data.
2. **Whether v2 needs true job-log liveness** (tailing act_runner output for a
   heartbeat) instead of pod-phase-only. Deferred; would need a credential/scope
   decision of its own if pursued (likely a narrow read-only log-tail credential,
   distinct from the teardown/registration credentials in ADR 0006).
3. **Whether the Pending-timeout retry should eventually feed a circuit breaker**
   (stop retrying a set that has failed pending N times in a row) rather than relying
   purely on external metrics/alerting. Left to operational experience.

## References

- ADR 0003 - CRD hierarchy: `EphemeralRunnerStatus.LastObservedTime`, `restartPolicy:
  Never`.
- ADR 0004 - resource policy resolution (design-only; this ADR's config is designed to
  slot under it later).
- ADR 0007 - scaling algorithm: busy-runners-never-killed precedent; the stall-kill is
  the one narrow, deliberate, visibly-different exception.
- garc-c7i / garc-i5b: prior live incidents this ADR is written to avoid repeating
  (busy-runner-killed class of bug; crash-recovery of an unstuck in_progress task is
  Gitea's job, not the operator retrying it).
