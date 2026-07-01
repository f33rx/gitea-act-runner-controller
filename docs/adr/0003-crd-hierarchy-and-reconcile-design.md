# 0003 - CRD hierarchy and reconcile design

## Status

Accepted (2026-06-30, batch arch review; ratify-with-nits, nits applied)

## Context

The operator provisions ephemeral act_runner pods for Gitea Actions: every job runs
in a fresh pod that executes exactly one task and is then torn down (SPEC sec. 3,
DECISIONS D5). We must decide how the operator models a runner in the Kubernetes API:
whether the controller manages bare `Pod` objects directly, or introduces a hierarchy
of custom resources. DECISIONS D1 left this open and flagged it for arch review; this
ADR closes it.

Three facts make this decision load-bearing rather than cosmetic.

1. **Gitea is pull-based, and runner identity lives in Gitea's database, not in
   Kubernetes** (SPEC sec. 4). A runner is registered with `act_runner register
   --ephemeral`; that creates a row in Gitea (uuid, token, labels, online/ephemeral
   state) that is *independent of the pod*. Deleting the pod does not deregister the
   runner. So the operator needs a place to record per-runner Gitea-side state and to
   hang teardown logic (a finalizer) that runs *before* the pod disappears. A bare Pod
   gives us neither a natural home for that state nor a clean finalizer story.

2. **Scaling is demand-derived, not push-driven** (SPEC sec. 4, 5.1; DECISIONS D4).
   Gitea has no "job available" push; the listener polls `GET
   /api/v1/admin/actions/jobs?status=queued`, buckets queued jobs by their `labels[]`
   client-side, and computes a desired replica count per runner pool. That decision
   has to be written somewhere observable and reconciled against actual pods. This is
   the same control shape ARC and Gitea Enterprise ARC use, and both express it as a
   replica-bearing CRD with a `targetSize` status (SPEC sec. 12).

3. **Crashes must be controller-driven to preserve one-job-per-pod** (SPEC sec. 6.4).
   The live probe (Gitea 1.26.1 + act_runner 0.2.13, 2026-06-30) showed that a
   SIGKILL'd ephemeral runner leaves an orphaned `online`/`ephemeral:true` row in
   Gitea *and* a task stuck `in_progress` until Gitea's server-side zombie-task reaper
   clears it (`ZOMBIE_TASK_TIMEOUT`, default ~10 min). Recovering
   from that requires a first-class object the controller can observe, retry with
   backoff, and finalize. A `restartPolicy` that silently restarts the container would
   violate the one-job-per-pod isolation guarantee.

This ADR also incorporates findings from the live probe (garc-7ft.2) that directly
shape the reconcile design and the finalizer ordering, recorded inline below so the
constraints travel with the decision.

GitHub's actions-runner-controller (ARC) is the proven architectural blueprint for
this exact problem on GitHub Actions, and Gitea Enterprise ARC is the closest prior
art on Gitea (SPEC sec. 12). We adopt their CRD hierarchy and safety patterns
faithfully, and diverge only where Gitea's API forces it (the listener and the
finalizer-as-safety-net, below).

## Decision

Adopt an ARC-faithful **CRD-per-runner hierarchy**, not bare Pods. The chain is:

```
GiteaRunnerSet            (user-facing scale set)
  -> EphemeralRunnerSet   (desired replica count + patchID)
       -> EphemeralRunner (one per pod; carries Gitea-side state; dual finalizer)
            -> Pod        (restartPolicy: Never)
            -> Secret     (per-pod registration token, owner-ref'd)
```

Each level owns the level below via an `ownerReference`, so deletion cascades through
Kubernetes garbage collection. A separate **Listener** pod (the demand bridge) is owned
by the `GiteaRunnerSet` but sits outside the runner chain; it writes scaling decisions
into the `EphemeralRunnerSet`, it does not own runners.

### Why CRD-per-runner over bare Pods

A bare Pod cannot carry the things this design needs:

- **Gitea-side state** - the runner id/uuid issued by Gitea, the current task/job
  reference, and the runner's phase as the operator sees it. This is not Kubernetes
  state; it has nowhere to live on a Pod.
- **Finalizers with semantics** - we need teardown to deregister from Gitea *before*
  the pod is allowed to vanish (see dual finalizer). Pods support finalizers, but the
  logic and the Gitea identity it needs belong on a dedicated object.
- **Owner-ref GC** - a first-class set object lets the listener express "I want N
  runners" by patching one field, and lets Kubernetes cascade-delete the whole tree.
- **Per-runner failure/backoff** - retry of pods that fail to start or die before
  claiming a task needs a `failures` map with timestamps, persisted across reconciles.
- **An observable scaling decision** - `status.targetSize` on the set is the contract
  between the listener and the controller, and the thing Priya inspects with `kubectl`.

So the CRD-per-runner object is the seam that carries Gitea identity, finalizers,
status, owner-ref GC, and per-runner failure/backoff. That is the whole reason it
exists.

### GiteaRunnerSet (user-facing)

What the platform engineer deploys, one per label set / scale set. Spec fields:

- `giteaConfigUrl` - the Gitea instance URL.
- `giteaConfigSecretRef` - reference to the operator's Gitea credential Secret (the
  operator's, never the pod's; see ADR 0006).
- `runnerScope` - `instance` | `org` | `repo` (the registration scope).
- `labels` - the labels the runner pods advertise (e.g. `ubuntu-latest`, `dind`).
- `minRunners` / `maxRunners` - the warm-pool floor and the hard ceiling; the desired
  count is clamped to `[minRunners, maxRunners]`. `minRunners` may be 0 (scale to
  zero).
- `resourcePolicyRef` - reference to the resolved resource policy (see ADR 0004).
- `template: corev1.PodTemplateSpec` - full pod customization.

We **consider grouping the spec as `{gitea, runtime, scalability, storage}`**, borrowed
from Gitea Enterprise ARC's `Runner` CRD shape (`spec.{gitea, runtime, scalability,
storage, cacheServer}`, SPEC sec. 12.1). The grouping keeps a wide spec legible:
`gitea` holds URL/secret/scope, `runtime` holds the pod template and advertised labels,
`scalability` holds min/max and the resource-policy reference, `storage` holds the
build-cache wiring (ADR 0005). The flat-vs-grouped choice is left open below; the field
*set* is decided here.

### EphemeralRunnerSet (desired count + patchID)

Owns the desired runner count for one pool. Key fields:

- `spec.replicas` - the desired number of ephemeral runners. The **listener** patches
  this.
- `spec.patchID` - a monotonic token the listener stamps on each scaling patch. The
  **controller** echoes the `patchID` it last acted on. This coordinates listener vs
  controller to **avoid scale-down races**: the controller only honors a scale-down for
  the `patchID` it is currently reconciling, so a stale decision cannot delete a runner
  that a newer decision wants to keep. This is a direct ARC lesson (SPEC sec. 5.1,
  5.2).
- `status.targetSize` + `status.targetSizeUpdatedAt` - the controller's
  scaling-decision *output*, surfaced on status so the decision is observable, then
  reconciled by creating/deleting `EphemeralRunner` objects to match. Borrowed from
  Gitea Enterprise ARC (SPEC sec. 12.1). The flow is: listener computes demand ->
  writes `targetSize` -> patches `replicas` + `patchID` -> controller reconciles
  `EphemeralRunner` count.

### EphemeralRunner (one per pod)

One object per runner pod; this is the heart of the design. Fields:

- `status.runnerId` / `status.runnerUuid` - the identity Gitea assigned at
  registration (used to deregister; needs `write:organization` on the org route --
  recommended default -- or `write:admin` on the admin route as fallback; ADR 0006).
- `status.phase` + `status.reason` - infra-level phase
  (`Pending`/`Running`/`Succeeded`/`Failed`) with a reason string. This is *infra*
  status only; Gitea owns job/log status and reports it directly (SPEC sec. 6.5), so we
  do not duplicate it.
- `status.jobRef` - the current task/job the runner claimed, for surfacing in `kubectl`
  and for the stuck-task sweep.
- `status.failures` - a map of failure timestamps for **capped exponential backoff**
  (ARC-style, max ~5 attempts; SPEC sec. 6.2). Drives retry of pods that fail to start
  or die *before* claiming a task. A pod that fails *during* a claimed task is **not**
  silently retried (re-running partially-executed CI is unsafe; rerun is Gitea's/the
  user's decision).
- **Dual finalizer** - see below.

The `EphemeralRunner` owns its `Pod` and its per-pod `Secret` via owner refs.

### Pod

`restartPolicy: Never`, so a crashed container does **not** silently restart;
the crash surfaces to the `EphemeralRunner` controller, which decides
recovery. This is what preserves one-job-per-pod (DECISIONS D5): the pod runs one task
and is then torn down or, on crash, controller-recreated as a *fresh* `EphemeralRunner`
rather than reused. The pod runs the act_runner container unprivileged with a restricted
securityContext (SPEC sec. 7, 9); no DinD, no Docker socket, no privileged container
(see ADR 0005 for the build strategy).

### Dual finalizer on EphemeralRunner

Teardown runs two ordered steps before the `EphemeralRunner` (and its owned Pod +
Secret) may be deleted:

1. **Deregister the runner from Gitea** - org-scoped `DELETE
   /api/v1/orgs/{org}/actions/runners/{id}` under `write:organization` (returns 204;
   the recommended default, spike garc-3bk), or the admin route `DELETE
   /api/v1/admin/actions/runners/{id}` under `write:admin` (returns 403 without it) as
   the whole-instance/multi-org fallback - live-confirmed, ADR 0006. The runner scope
   (`spec.runnerScope`) and the credential tier agree.
2. **Delete the pod + the per-pod Secret** - the owner-ref'd pod and its registration
   token Secret.

The invariant: **a pod cannot fully disappear until Gitea has been told.** The finalizer
guarantees the deregister step is attempted before Kubernetes GC removes the object.

The deregister step is a **safety net, not the primary teardown path**, because of the
live-probe behavior below.

### Live-probe findings that constrain this design

Probed against **Gitea 1.26.1 + act_runner 0.2.13** (garc-7ft.2, 2026-06-30):

- **Ephemeral is set at REGISTRATION**, via the flag `act_runner register --ephemeral`.
  In the standard `gitea/act_runner` Docker image, the env var **`GITEA_RUNNER_EPHEMERAL=1`
  is the documented way to enable this** -- the image entrypoint (`run.sh`) translates it
  into `register --ephemeral` (verified against upstream source). It is distinct from the
  daemon `--once` flag, which the entrypoint maps from `GITEA_RUNNER_ONCE`; `--once` is a
  weaker "run one job then exit" that does NOT make the runner ephemeral at the Gitea side
  (no `ephemeral: true` row, no auto-delete). Only `--ephemeral` produces a row with
  `ephemeral: true` and the self-exit-plus-auto-delete behavior. (Our 2026-06-30 probe
  initially missed the env var because it invoked `register`/`daemon` directly, bypassing
  the entrypoint that consumes `GITEA_RUNNER_EPHEMERAL`; the operator can use either the
  flag directly or the env var via the entrypoint.)
- **Happy path needs no operator deregister.** On graceful one-task completion the
  act_runner daemon self-exits and Gitea **auto-deletes** the runner row server-side
  (`CleanupEphemeralRunners`, PR #33570 - confirmed firing). So the common teardown
  requires no deregister call from the operator. The finalizer's deregister step is the
  **safety net** for the paths Gitea does *not* clean up.
- **Crash path is the dangerous one.** A SIGKILL mid-job leaves the runner row
  **orphaned** (`status: online`, `ephemeral: true`) **and** the task stuck
  `in_progress` until Gitea's **server-side zombie-task reaper** clears it. Because the
  runner is dead, the workflow `jobs.<id>.timeout-minutes` and act_runner's own
  `timeout` (default 3h, act_runner `config.yaml` `runner.timeout`) do NOT apply --
  those are enforced by the live runner, which is gone.
  The relevant timer is Gitea's `[actions] ZOMBIE_TASK_TIMEOUT` (a task that stopped
  heartbeating), **default 10 minutes** (source: `modules/setting/actions.go`). Gitea
  does not detect the dead runner faster than that. Therefore the `EphemeralRunner`
  controller's crash handling **plus a periodic reconcile sweep** (ADR for garc-7ft.9)
  must: (a) detect dead pods; (b) deregister orphaned `online`/`ephemeral:true` rows
  whose owning pod is gone; and (c) **surface** the stuck task in CR status and metrics.
- **The operator CANNOT actively cancel the stuck task in v1 (live-confirmed, spike
  garc-i5b, Gitea 1.26.1).** There is no Actions cancel route in the REST API. Tested
  against a real crashed runner: deleting the orphaned runner row returns 204 but the
  task stays `in_progress` (it is pinned to the dead `runner_id`); `DELETE` on the run
  returns 400 "this workflow run is not done" (only completed runs are deletable);
  `rerun` likewise returns 400; and a freshly registered runner with matching labels
  does NOT pick up the orphaned task (no auto-requeue). So a crashed `in_progress` task
  is reaped **only** by Gitea's server-side zombie-task reaper (~10 min default). The
  operator's recovery is therefore limited to: deregister the orphaned runner (so it
  stops counting as capacity) and surface the stuck task. Mitigation guidance lives with
  the teardown slice (garc-7ft.9): recommend a low Gitea instance `ZOMBIE_TASK_TIMEOUT`
  for faster crash reaping (this is the correct lever -- NOT the workflow
  `timeout-minutes`, which a dead runner cannot enforce). **Upstream watch:** go-gitea
  PR #35382 adds `POST /actions/runs/{run}/cancel` (and `/approve`) to the REST API,
  milestone **1.28.0** (open as of 2026-06, not in 1.26.1). When it ships, the sweep
  could actively cancel a stuck run instead of waiting on the zombie reaper -- a v1.1
  enhancement, gated on the operator's target Gitea version.
- **Label shapes differ and must be normalized.** Runner labels from the Gitea admin
  API are objects `{id, name, type}`; job `labels[]` are bare strings. The operator must
  normalize both to a common form when matching jobs to pools (SPEC sec. 4, 7).

### Cross-cutting GC and ownership

The owner-ref chain (`GiteaRunnerSet -> EphemeralRunnerSet -> EphemeralRunner -> Pod` /
`Secret`) gives cascading garbage collection for free: deleting a `GiteaRunnerSet`
tears down its sets, runners, pods, and per-pod Secrets. The per-pod registration token
Secret is owner-ref'd to the `EphemeralRunner`/Pod so it never outlives the workload
(ADR 0006). All kinds use `apiextensions.k8s.io/v1` CRDs and GA core APIs only, for
portability and Autopilot admission (SPEC sec. 6.6, 9).

## Consequences

### Positive

- **First-class home for Gitea-side state.** Runner id/uuid, current job, phase, and
  failure history live on a dedicated object instead of being inferred from a Pod.
- **Cascading teardown is automatic.** Owner refs + GC mean no bespoke deletion
  bookkeeping; deleting a set cleans up its whole tree, including secrets.
- **Crashes are controller-driven.** `restartPolicy: Never` plus the `failures` backoff
  map preserves one-job-per-pod and gives bounded, observable retry of pre-claim
  failures.
- **No orphaned registrations under normal operation.** The dual finalizer guarantees a
  deregister attempt before the object vanishes; combined with Gitea's auto-delete on
  graceful exit, the happy path is clean and the crash path is recoverable.
- **The scaling decision is observable.** `status.targetSize` /
  `targetSizeUpdatedAt` make "what does the operator think it should run?" a `kubectl
  get` away, easing debugging and metrics.
- **Scale-down races are designed out.** The `patchID` handshake between listener and
  controller prevents a stale decision from deleting a wanted runner - an ARC lesson
  adopted rather than re-learned.
- **Faithful to proven prior art.** Mirroring ARC's hierarchy and Gitea Enterprise ARC's
  `targetSize`/spec-grouping lowers design risk and keeps future convergence open.

### Negative

- **More moving parts than bare Pods.** Three CRD kinds plus a listener mean more
  controllers, more reconcile loops, more CRD schema to version and maintain.
- **Status duplication risk.** Infra status on `EphemeralRunner` lives alongside the
  authoritative job/log status Gitea already reports; we must keep the boundary crisp
  (operator owns infra, Gitea owns job/log) to avoid drift and confusion.
- **Finalizer-blocked deletion.** A finalizer that cannot reach Gitea will stall object
  deletion; the controller must bound its deregister attempts and not wedge GC
  indefinitely.
- **Two-credential split adds operational surface.** The least-privilege listener
  (`read:organization` by default, `read:admin` fallback) and the higher-privilege
  teardown path (`write:organization` by default, `write:admin` fallback) hold different
  credentials (ADR 0006), which is more to provision and rotate.

### Risks

- **Force-delete bypasses the finalizer.** `kubectl delete --force
  --grace-period=0` removes the object without running the dual finalizer, orphaning the
  Gitea runner. Mitigation: the periodic reconcile sweep (garc-7ft.9) detects and
  deregisters orphaned `online`/`ephemeral:true` rows whose pod is gone; documented
  hazard. Note the coupling to scaling: until the next sweep, an orphaned `online` row
  with matching labels counts as live capacity in the listener's demand math (ADR 0007
  Decision 1 buckets by runner labels), inflating apparent capacity and suppressing
  scale-up. The sweep period therefore bounds the capacity-accounting error, and the
  scaling algorithm (ADR 0007) must tolerate transiently-stale runner counts.
- **Stuck task lingers ~10 min on crash, and the operator cannot cancel it.** A
  SIGKILL'd runner leaves its task `in_progress` until Gitea's zombie reaper
  (`ZOMBIE_TASK_TIMEOUT`, default ~10 min) clears it. There is no cancel API
  (garc-i5b), so the sweep can only deregister the runner row and surface the stuck
  task -- it cannot fail it. Mitigation: recommend a low `ZOMBIE_TASK_TIMEOUT` on the
  Gitea instance for faster reaping.
- **Gitea API churn.** The Actions API is changing across 1.24/1.25/1.26; the probe
  already corrected `waiting` -> `queued` and found the `labels` filter a server-side
  no-op. Mitigation: isolate Gitea access behind one client package, pin tested
  versions, re-probe on upgrade (SPEC R3).
- **`patchID` protocol drift.** If listener and controller disagree on `patchID`
  semantics, scale-down protection silently degrades. Mitigation: specify the handshake
  precisely and cover it with controller tests.

## Open questions

- **`ResourcePolicy` as CRD vs ConfigMap** - deferred to ADR 0004; `resourcePolicyRef`
  is a reference either way.
- **Flat vs grouped GiteaRunnerSet spec** - whether to adopt the
  `{gitea, runtime, scalability, storage}` grouping from Gitea Enterprise ARC or keep a
  flat spec. The field set is decided here; the layout is not.
- **Where the periodic reconcile sweep lives** - a dedicated controller vs a requeue
  on the `EphemeralRunnerSet` controller; the sweep's design is owned by garc-7ft.9
  (teardown slice).
- **Listener ownership and lifecycle - RESOLVED in ADR 0007.** One listener Deployment
  per `GiteaRunnerSet`, owner-referenced by the set (GC'd with it, recreated if
  missing), stateless and crash-tolerant, holding only the read credential. See ADR
  0007 Decision 7.
- **Stuck-task cancellation API - RESOLVED (garc-i5b): there is none.** Gitea 1.26.1 has
  no Actions cancel route; a crashed `in_progress` task cannot be operator-cancelled and
  is reaped only by Gitea's zombie reaper (~10 min). The sweep deregisters the orphaned
  runner and surfaces the task; it cannot fail it. See the crash-path subsection above.
- **`failures` backoff tuning** - the cap (~5) and the backoff curve are placeholders to
  validate against real failure patterns.

## References

- SPEC `docs/product/gitea-actions-operator/SPEC.md` sec. 4 (pull-based constraint),
  5.1 (control loop), 5.2 (CRD hierarchy), 6.4 (error recovery / dual finalizer), 12
  (related work: ARC, Gitea Enterprise ARC).
- DECISIONS `docs/product/gitea-actions-operator/DECISIONS.md` D1 (CRD vs bare Pods),
  D5 (one-pod-per-job intent), D4 (control loop), D2/D3 (daemonless / Autopilot
  context).
- Live probe garc-7ft.2 (Gitea 1.26.1 + act_runner 0.2.13): `register --ephemeral`
  flag, graceful self-exit + Gitea auto-delete (`CleanupEphemeralRunners`, PR #33570),
  crash-path orphan + zombie-reaped stuck task (~10 min), label-shape normalization.
- ADR 0004 - resource policy (`resourcePolicyRef`, `ResourcePolicy` CRD-vs-ConfigMap).
- ADR 0005 - build strategy (daemonless builder, build cache, `storage` spec wiring).
- ADR 0006 - credential model (operator credential, per-pod registration token,
  read/write privilege split -- org-scoped `read:organization`/`write:organization`
  default, `read:admin`/`write:admin` fallback -- per-pod Secret owner-ref).
- ADR 0007 - scaling algorithm and listener lifecycle (`targetSize`/`patchID` semantics,
  per-set listener ownership, safe scale-down via the `busy` signal).
- Prior art: GitHub actions-runner-controller (AutoscalingRunnerSet ->
  EphemeralRunnerSet -> EphemeralRunner -> Pod, `restartPolicy: Never`, dual finalizer,
  isolated listener, `patchID`); Gitea Enterprise ARC `Runner` CRD
  (`spec.{gitea, runtime, scalability, storage, cacheServer}`, `status.targetSize` +
  `targetSizeUpdatedAt`).

This ADR blocks the walking skeleton (garc-7ft.7) and the teardown slice (garc-7ft.9).
