# ADR 0007: Demand-driven scaling algorithm and listener lifecycle

## Status

Accepted (2026-06-30, batch arch review; ratify-with-nits, nits applied)

bd issue: garc-jhs (design). Anchors the implementation slice garc-7ft.8
(demand-driven scaling) and tightens the listener parts of ADR 0003 (CRD hierarchy)
and ADR 0006 (credential model).

Related ADRs: CRD hierarchy (0003), resource policy (0004), build strategy (0005),
credential model (0006).
Source spec: `docs/product/gitea-actions-operator/SPEC.md` sections 4 (pull-based
constraint), 5.1 (control loop), 5.2 (CRD hierarchy).

## Context

The operator's core job is to turn Gitea's job demand into the right number of
ephemeral runner pods. Gitea is pull-based: it never pushes "a job is available," and
there is no long-poll an operator can subscribe to (SPEC sec. 4). The only demand
signal is the admin/org jobs queue, which the operator must poll.

This ADR settles the control loop that everything else feeds: how the listener
computes a desired replica count, how scale-up and scale-down behave, how runners are
not killed mid-job, and what the listener itself is as a runtime object.

### Live findings that constrain the design (Gitea 1.26.1, this session)

- Demand is read from `GET /api/v1/orgs/{org}/actions/jobs?status=queued` (org-scoped,
  `read:organization` -- the **recommended default**, proven end-to-end in spike
  garc-3bk) or `GET /api/v1/admin/actions/jobs?status=queued` (instance, `read:admin` --
  the whole-instance/multi-org fallback). A fully org-scoped operator needs zero admin
  scope (ADR 0006). A job awaiting a runner is `queued`, NOT `waiting` (garc-7ft.1).
- The server `labels` filter is a **no-op**: every label value returns all jobs. The
  listener MUST bucket queued jobs by their `labels[]` (bare strings) **client-side**
  (garc-7ft.1).
- `X-Total-Count` gives total queued depth in one header (fast "is anything queued?"
  gate); per-label depth requires reading the job objects.
- A runner row exposes a **`busy`** boolean and a `status` (`online`/`offline`) -- so
  the operator can distinguish idle from busy runners directly (this session).
- Runners are **ephemeral**: each picks exactly one job, then the daemon self-exits and
  Gitea auto-deletes the row (garc-7ft.2). This materially simplifies scale-down (see
  Decision 3).
- Unclaimed-job sentinels: `runner_id: 0`, `started_at: 1970-01-01`.

## Decision

### 1. Demand model: count queued + in-flight, bucket per label set

For each `GiteaRunnerSet` (which advertises a fixed label set `L`), the listener
computes a **desired** runner count every poll:

```
poll the queue (status=queued) for the set's scope (org or instance)
queuedForL = count of queued jobs whose labels[] are satisfied by L   # client-side bucket
demand = queuedForL                       # one ephemeral runner per queued job
desired = clamp(max(demand, minRunners) , minRunners, maxRunners)
```

`desired` is a **total**, not a delta. The controller (ADR 0003) realizes it by counting
existing runners toward the total, so already-warm idle runners are not double-counted --
there is no explicit `inFlight` subtraction term. See Decision 4 (total-desired
idempotence) and Open question 1 for why this converges without one.

Key points:

- **One ephemeral runner per queued job.** Because a runner takes exactly one job,
  `demand == queuedForL` is the natural target -- N queued jobs want N runners. There
  is no "jobs per runner" division as in reusable-runner systems.
- **Label satisfaction is a subset test**, normalized for the two label shapes: job
  labels are bare strings, runner/Set labels may be objects `{id,name,type}` (ADR
  0003). A queued job matches set `L` if its `runs-on` labels are a subset of `L`.
- **Idle warm runners already count.** A started-but-unclaimed ephemeral runner
  (registered, `busy: false`) will claim the next matching queued job on its own poll.
  The listener must not double-provision. This is handled by targeting a desired *total*,
  not by adding deltas (see Decision 4, `targetSize`): because the controller realizes a
  total and counts existing runners toward it, warm idle runners are inherently included
  and no explicit idle-subtraction is needed.

### 2. Scale-up: responsive, clamped, debounced

- On each poll, if `desired > current targetSize`, the listener raises `targetSize`
  immediately (scale-up is latency-sensitive: a waiting job is a developer waiting).
- Clamp to `maxRunners`. If `desired > maxRunners`, excess jobs stay queued; the
  shortfall is exported as the metric `jobs_queued_over_max` (the count of queued jobs
  the cap is preventing us from serving) so Priya can raise the cap. We never silently
  drop demand -- queued jobs simply wait, which is Gitea's normal behavior.
- **Poll interval** is a config knob (default 10s, in the range the open-source
  autoscalers use: rustunit polls 5s, KEDA 30s). Lower = snappier start, more API
  load. This is the dominant contributor to time-to-job-start beyond the warm floor.

### 3. Scale-down: ephemerality makes this almost free

This is the part that is hard in reusable-runner systems and easy here.

- **Busy runners are never killed.** Because each runner is ephemeral, a busy runner is
  mid-its-only-job; when it finishes it self-exits and Gitea auto-deletes it (garc-7ft.2).
  Scale-down therefore does **not** require terminating busy pods -- they drain
  themselves. This is the Gitea-ephemeral analogue of ARC's "decreasing desired
  replicas never terminates a running job."
- **Scale-down = stop creating + trim idle.** When `desired < current`, the operator:
  1. lowers `targetSize`;
  2. the `EphemeralRunnerSet` controller (ADR 0003) reconciles by deleting only
     runners that are **idle** -- a pod whose runner row shows `busy: false` (or that
     has not yet claimed a job), never a busy one. The `busy` field is the live signal;
     a pod with no started job and an idle row is safe to remove.
  3. busy runners are left alone; they remove themselves on completion.
- **No forced mid-job termination path in v1.** If `targetSize` drops below the number
  of busy runners, the busy ones simply outlive the scale-down and self-drain. The
  effective floor of "running pods" is `max(targetSize, busyCount)` until they finish.

### 4. targetSize + patchID coordination (no races)

- The listener writes the desired count to `EphemeralRunnerSet.status.targetSize` (+
  `targetSizeUpdatedAt`) and patches `spec.replicas` with a monotonically increasing
  `spec.patchID` (ADR 0003, borrowed from ARC and Gitea Enterprise ARC).
- The `EphemeralRunnerSet` controller only acts on a patch whose `patchID` is newer
  than the last one it reconciled. This prevents a stale listener patch from undoing a
  newer scale decision, and prevents listener/controller from fighting over replica
  count -- the listener decides desired, the controller realizes it, `patchID` orders
  the conversation.
- The listener computes a **total desired**, not a delta, so a missed or duplicated
  poll is self-correcting on the next cycle (idempotent reconciliation).

### 5. Warm pool floor and scale-to-zero

- `minRunners` (incl. **0**) is a warm floor: keep this many idle ephemeral runners
  always registered and polling, so the common case is "a runner is already waiting
  when a job appears" -- cutting cold-start latency. `desired = max(demand, minRunners)`.
- `minRunners: 0` enables true **scale-to-zero**: when the queue is empty the operator
  holds no pods and no cost. The first queued job triggers a scale-up from zero (paying
  one cold start). This is a differentiator over Gitea Enterprise ARC (no documented
  scale-to-zero).
- Warm runners are themselves ephemeral: a warm runner that claims a job is consumed
  and the floor is refilled by the next reconcile. The floor is a *steady-state count*,
  not a set of pinned pods.

### 6. Flap avoidance (hysteresis)

- **Scale-up is immediate; scale-down is delayed.** A `scaleDownDelay` (config, default
  ~60s) requires `desired` to stay below `current` for the whole window before the
  operator trims idle runners. This prevents thrashing when the queue oscillates (a
  burst that briefly empties then refills). Scale-up has no such delay -- we never make
  a waiting job wait longer to avoid a flap.
- Because runners are ephemeral and idle ones are cheap to recreate, the cost of a
  too-eager scale-down is a cold start, not a killed job -- so the delay can be short.

### 7. Listener lifecycle and ownership (resolves ADR 0003/0006 gap)

- **Topology: one listener Deployment per `GiteaRunnerSet`.** Each set has its own
  scope (org/instance), label set, and credential; a per-set listener keeps those
  isolated and lets sets scale independently. (ARC uses one listener per
  AutoscalingRunnerSet; we mirror it.) A single shared listener would entangle
  credentials and scopes across sets and is rejected.
- **Ownership:** the listener Deployment is **owned by the `GiteaRunnerSet`** (owner
  reference), so it is garbage-collected when the set is deleted and recreated by the
  `GiteaRunnerSet` controller if it goes missing. The listener is part of the set's
  reconciled child set, alongside the `EphemeralRunnerSet`.
- **Credential:** the listener mounts **only the read credential** --
  `read:organization` is the recommended default (spike garc-3bk, live-confirmed on
  Gitea 1.26.1), with `read:admin` the whole-instance fallback (ADR 0006) -- from its own
  Secret. It can read the queue and patch the `EphemeralRunnerSet` (via RBAC, a
  Role/RoleBinding on its ServiceAccount) and nothing else. It holds **no** write
  credential to Gitea: it cannot register, delete, or modify runners. Teardown/deletion
  is the controller's job with the write credential. This keeps the most
  network-exposed, long-running component least-privileged.
- **Restart behavior:** the listener is stateless -- it derives desired count from the
  live queue each poll and writes it to `status.targetSize`. A listener restart loses
  nothing: on startup it polls and re-asserts the desired count. Running runner pods are
  independent (they poll Gitea directly) and are unaffected by a listener restart. The
  `EphemeralRunnerSet` controller (in the manager, leader-elected) keeps realizing the
  last `targetSize` even while the listener is briefly down.
- **Separation of duties:** listener = decides desired (read-only to Gitea); manager
  controllers = realize it and own teardown (write to Gitea). The two never share a
  credential.

## Consequences

### Positive

- Scale-down is safe by construction: ephemerality means busy runners drain themselves;
  the operator only ever deletes idle runners, using the live `busy` signal. No
  mid-job-kill logic to get wrong.
- The demand model is simple and idempotent: one runner per queued job, total-desired
  (not deltas), self-correcting each poll.
- Scale-to-zero and a warm floor are the same knob (`minRunners`), trivially expressed.
- The listener is least-privilege, stateless, and crash-tolerant; its blast radius on
  compromise is "can read the queue and patch a replica count," never Gitea writes.
- patchID makes listener/controller coordination race-free without distributed locks.

### Negative

- Per-set listener Deployments cost one small always-on pod per GiteaRunnerSet (even at
  `minRunners: 0`, the listener itself runs to detect the first job). This is the price
  of scale-to-zero on a pull-based backend -- something has to poll. Documented; the
  listener is tiny (read-only poller).
- Client-side label bucketing means the listener fetches queued job objects (not just a
  count) when multiple label sets exist, costing more than a single count call. Bounded
  by queue depth; `X-Total-Count` short-circuits the empty-queue case.

### Risks

- **R-SCALE-1: poll interval vs API load.** Too-frequent polling loads Gitea; too-slow
  hurts start latency. Mitigation: configurable interval, sane 10s default, warm floor
  to decouple latency from poll rate, `X-Total-Count` fast path.
- **R-SCALE-2: `busy` signal staleness.** Scale-down targets idle runners by the `busy`
  field; if it lags, a runner could be deleted just as it claims a job. Mitigation:
  prefer deleting pods that have **never** started a job (no claim yet) over ones whose
  `busy` recently flipped; the ephemeral revoke-on-claim means a runner that has claimed
  is already off the idle list. Re-probe `busy` semantics on Gitea upgrade. If this race
  does fire (a runner deleted microseconds after claiming), the outcome is **not a
  silently lost job**: it degrades into the already-specified crash/orphan path -- an
  orphaned `online`/`ephemeral:true` row plus a task stuck `in_progress`, reaped by
  Gitea's zombie reaper (~10 min) and cleaned by the reconcile sweep (ADR 0003
  crash-path + garc-7ft.9). Bounded and recoverable, not data loss.
- **R-SCALE-3: thundering scale-up from zero.** A burst of N jobs at `minRunners: 0`
  spawns N cold starts at once. Mitigation: `maxRunners` clamp; optional small warm
  floor for latency-sensitive sets; pod resource policy (ADR 0004) bounds cluster load.
- **R-SCALE-4: Gitea API churn.** Already corrected `waiting`->`queued` and the no-op
  label filter. Mitigation: isolate queue access behind one client package; pin tested
  versions; re-probe on upgrade.

## Open questions

1. **Confirm total-`targetSize` convergence needs no explicit idle-subtraction.** The
   design relies purely on total-desired convergence (not delta accounting) to avoid
   double-provisioning warm idle runners -- so no `inFlight` subtraction term is used.
   The reasoning is sound (desired is a total; the controller counts existing runners
   toward it; the only hazard is a bounded, transient poll-lag window where a job still
   reads `queued` while the runner claiming it has not yet flipped `busy`, which
   self-corrects on the next poll -- worst case one transient extra cold-start pod that
   finds an empty queue and is trimmed; under-provisioning never occurs because queued
   jobs are never undercounted). This is a **confirm-empirically** item, not an
   unresolved algorithm choice: the garc-7ft.8 burst test must assert no sustained
   over-provisioning under a spike; if it ever shows one, an idle-subtraction term is the
   fallback.
2. **Scale-down idle-selection policy.** When trimming, which idle runner to remove
   (youngest, oldest, never-claimed-first). Never-claimed-first is the safe default;
   confirm against `busy`/claim signals in implementation.
3. **Poll interval and scaleDownDelay defaults.** 10s / 60s are informed starting
   points; tune against the time-to-job-start success metric (SPEC sec. 10) once the
   E2E harness (garc-nom) can measure it.
4. **Multi-set queue-read amplification.** If many GiteaRunnerSets share one org, each
   listener polls the same org queue. Whether to share a single cached org poll across
   sets (one read, many bucketers) is a later optimization, not v1.

## References

- ADR 0003 - CRD hierarchy: `EphemeralRunnerSet.status.targetSize`, `spec.patchID`,
  `restartPolicy: Never`, the controllers that realize the desired count.
- ADR 0006 - credential model: the `read:organization`/`read:admin` listener credential
  and the separation from the write credential.
- SPEC sec. 4 (pull-based constraint), 5.1 (control loop diagram), 5.2 (CRD hierarchy).
- bd memories: probe-correction-gitea-1-26-1-live-2026 (queued not waiting; label
  filter no-op; X-Total-Count), org-scoped-listener-also-confirmed-garc-3bk-2026
  (read:organization queue route -- the recommended default listener credential),
  ephemeral-lifecycle-validated-live-gitea-1-26-1 (ephemeral self-exit + auto-delete).
- GitHub actions-runner-controller: listener-patches-EphemeralRunnerSet pattern, and
  the guarantee that lowering desired replicas never terminates a running job.
