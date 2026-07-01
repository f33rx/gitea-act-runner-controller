# 0004 - Resource policy resolution

## Status

Accepted (2026-06-30, batch arch review; ratify-with-nits, nits applied)

## Context

Every runner pod the operator creates needs CPU and memory `requests` and `limits`.
Picking those numbers is not a one-size-fits-all problem:

- Different repos and orgs have materially different workloads. A docs repo that runs
  lint and unit tests wants a small pod; a repo that builds container images wants a
  large one. A single global number is either too small (image builds OOM or throttle)
  or too large (every trivial job over-provisions and costs more).
- The platform engineer (persona Priya, SPEC sec. 2) wants to set a generous default
  once and then tighten or loosen it for specific teams without editing pod templates
  by hand.
- SPEC sec. 6.1 already states the intent: per-pod requests and limits come from a
  resolved resource policy, resolution order most-specific-wins (repo, then org, then
  global default), expressed as a policy "keyed by scope" with the carrier (CRD vs
  ConfigMap) deferred to this ADR.

Two facts about the v1 target shape the sizing and the limits semantics, and both
correct now-obsolete language elsewhere in the spec:

1. **There is no DinD sidecar to size.** Post-pivot (DECISIONS.md D2, SPEC sec. 7, 9),
   v1 dropped privileged Docker-in-Docker entirely. The runner pod is a single
   unprivileged `act_runner` container; when a job builds an image it invokes a
   **daemonless builder** (BuildKit-rootless / Buildah / Kimia / Kaniko-fork, selected
   in ADR `0005-image-build-strategy.md`) in-process, not a separate privileged sidecar.
   The old "runner container + DinD sidecar, sized separately" model no longer exists.
   Any spec language describing a runner/DinD sizing split is **obsolete** and must be
   read as "size the single runner container for the heaviest thing it runs, which is a
   daemonless image build."

2. **The operator emits `limits == requests` explicitly, so behavior is deterministic
   regardless of the cluster's normalization.** On GKE Autopilot, the v1 deployment
   target (DECISIONS.md D3, SPEC sec. 9), a container that sets a limit higher than its
   request has its request raised up to the limit ("Autopilot sets requests to the value
   of limits"). The inverse ("set only a request and Autopilot forces limit == request")
   holds on the default (non-bursting) configuration but NOT on bursting-enabled
   Autopilot clusters, so we do not lean on the cluster to normalize for us. Instead the
   operator always emits `limits == requests` on every pod (section 4). This makes the
   number you request the number you are billed for and the hard ceiling the kernel
   enforces, identically on Autopilot (any mode) and on a vanilla cluster -- a
   portability win. The policy therefore cannot rely on "burst headroom" between request
   and limit; there is none by construction.

We also have a hard portability constraint (SPEC sec. 6.6): the policy and the pods it
produces must use GA core APIs only, with no hardcoded StorageClass, provisioner,
zone, instance type, or other cloud-provider coupling. The policy describes compute
requests/limits and pod-level knobs that resolve per scope; it must not smuggle in
cluster-specific assumptions.

This ADR decides (a) how a policy is resolved for a given job, (b) what object carries
the policy, and (c) the shipped defaults and the `limits == requests` handling.

## Decision

### 1. Resolution order: repo, then org, then global default (most specific wins)

For each runner pod the operator is about to create, it resolves the effective resource
policy by selecting the most specific scope that has a policy defined, in this order:

```
repo-scoped policy   (most specific)
   falls back to ->  org-scoped policy
   falls back to ->  global default policy  (least specific, always present)
```

Resolution is **whole-policy selection, not per-field merge.** The most specific scope
that matches supplies the entire effective policy. We deliberately do not deep-merge
fields across scopes (e.g. "take CPU from the org policy but memory from the repo
policy"). Whole-policy selection is predictable, trivially explainable in `kubectl`
output and logs ("this pod used the repo-scoped policy `team-a/builder`"), and avoids
the surprising-action-at-a-distance that field-level merge invites. An org admin who
sets a repo policy gets exactly that policy, not a silent blend.

The **global default policy always exists** (we ship one, see section 3), so resolution
can never fail to produce a policy. A repo with no repo-scoped or org-scoped policy
deterministically lands on the global default.

The scope of a job is known at provisioning time from the GiteaRunnerSet's
`runnerScope` (instance / org / repo, SPEC sec. 5.2) and the queued job's
org/repo coordinates surfaced by the demand listener. The operator records which policy
and which scope won on the EphemeralRunner status so the decision is observable.

### 2. Policy carrier: a `ResourcePolicy` CRD (recommended), not a ConfigMap

We evaluated two carriers.

**Option A - a `ResourcePolicy` CRD, keyed by scope (RECOMMENDED).**

A namespaced CRD whose spec carries the scope selector (instance / org / repo plus the
org or repo identifier) and the resource shape (requests/limits, and the scope-resolved
pod knobs that belong with sizing such as `activeDeadlineSeconds`, per SPEC sec. 6.2).
GiteaRunnerSet references the default policy via `resourcePolicyRef` (already in the
spec's GiteaRunnerSet shape, SPEC sec. 5.2); org- and repo-scoped policies are
discovered by the operator via the scope key.

Rationale:

- **First-class schema validation.** The API server rejects a malformed policy at apply
  time via the CRD's OpenAPI schema (quantities are valid resource.Quantity strings,
  scope is a known enum, identifiers are present when the scope requires them). With a
  ConfigMap, the operator only discovers a typo'd `2gI` or a negative CPU at the moment
  it tries to build a pod, which is the worst time to find out.
- **First-class status.** A CRD can carry `status` (resolved, conditions, which
  GiteaRunnerSets consume it, last validation result), so Priya can `kubectl get
  resourcepolicy` and see the fleet's sizing at a glance, and the operator can surface a
  clear condition when a policy is invalid or shadowed.
- **Matches the operator's CRD-centric model.** The whole operator is already a CRD
  hierarchy - GiteaRunnerSet, EphemeralRunnerSet, EphemeralRunner (SPEC sec. 5.2, ADR
  `0003-crd-hierarchy-and-reconcile-design.md`). A `ResourcePolicy` CRD is consistent with that model, gets
  the same RBAC story, owner-ref/GC semantics, watch/reconcile plumbing, and admission
  webhook surface as everything else. A ConfigMap would be the one piece of policy that
  lives outside the typed API and has to be special-cased.
- **Watchable.** controller-runtime can watch ResourcePolicy objects and re-resolve /
  re-emit affected runner sizing on change, with typed informer caching. ConfigMaps are
  watchable too, but the typed object gives stronger guarantees and a cleaner mapping.

**Option B - a ConfigMap keyed by scope (the tradeoff, NOT recommended).**

The same data, encoded as YAML/JSON inside a single ConfigMap (or one ConfigMap per
scope), parsed by the operator.

Genuine advantages, stated honestly:

- **Simpler.** No new CRD to define, version, install, RBAC, or maintain. One fewer
  type in the bundle.
- **No CRD install step.** A ConfigMap needs no `CustomResourceDefinition` to land
  first; it works on any cluster with zero API extension.
- **Familiar.** Operators reach for ConfigMaps for config by reflex.

Why we still reject it for v1:

- **No schema validation.** The API server treats the body as opaque text. Every
  invariant (valid quantities, known scope, required identifiers, requests within a sane
  envelope) has to be re-implemented as ad hoc parse-and-validate code in the operator,
  and errors surface late (at pod-build time) instead of at apply time.
- **No typed status / conditions.** There is nowhere first-class to report "this policy
  is invalid" or "this policy is shadowed by a more specific one"; we would bolt status
  onto annotations or events, which is exactly the kind of special-casing the CRD avoids.
- **Inconsistent with the rest of the operator.** It would be the only policy surface
  that is not a typed CRD, fragmenting the mental model and the tooling.

**Decision: ship the `ResourcePolicy` CRD.** The validation and status wins are
load-bearing for an operator whose entire value proposition is "set policy once and
trust it," and the consistency with the existing CRD hierarchy outweighs the modest
extra surface of one more CRD. The ConfigMap's only real edge - skipping a CRD install -
is marginal in a project that already installs several CRDs and ships via Helm. We note
a ConfigMap fallback could be offered later as a convenience for the global default
only, but that is not a v1 commitment and would not replace the CRD.

### 3. Shipped defaults reflect single-container, image-build-hungry sizing

The shipped global default policy sizes the **single unprivileged runner container** for
the heaviest realistic v1 workload, which is a daemonless image build. There is no
separate sidecar line item.

Default global policy (illustrative, tunable in the Helm values and overridable per
scope):

```
requests:
  cpu:    "2"      # 2 vCPU - daemonless image builds are CPU-bound
  memory: "4Gi"    # 4Gi    - layer assembly and large build contexts are memory-hungry
activeDeadlineSeconds: <hard cap, see SPEC 6.2 and bd garc-7ft.10>
```

Notes on the defaults:

- These are deliberately on the **generous** side. Under-requesting a build pod on the
  v1 target does not throttle gracefully into a slower-but-correct build; with
  `limits == requests` it produces OOM kills (memory) and hard CPU throttling that can
  push a build past its deadline (see section 4). A right-sized default that the
  platform engineer tightens for cheap repos is safer than a stingy default that the
  developer hits as a mysterious OOM.
- The old "DinD sidecar sizing" framing is **explicitly obsolete.** v1 has no DinD
  sidecar; do not size one. All sizing applies to the one runner container, which is
  also where the daemonless builder runs.
- The defaults are knobs, not law. The whole point of the resolution hierarchy is that
  Priya sets a generous global default and tightens per org/repo for known-small
  workloads and loosens per repo for known-heavy ones.
- **Autopilot admits only within a CPU:memory ratio band.** The default general-purpose
  compute class admits a pod only if its CPU:memory ratio is within **1:1 to 1:6.5 GiB
  per vCPU**; outside that band Autopilot silently **raises the smaller resource** at
  admission. The shipped 2 vCPU / 4Gi default is 1:2, safely inside the band -- but a
  *tightened per-scope* policy can trip it: `1 vCPU / 8Gi` (1:8, over 6.5) gets its CPU
  bumped, and `4 vCPU / 2Gi` (1:0.5, under 1:1) gets its memory bumped. Priya would then
  be billed for a silently-mutated larger pod, and the EphemeralRunner status (section 4)
  records the *requested* value, not the admitted one, defeating observability for
  exactly the mutated case. The resource policy CRD MUST validate the ratio at apply time
  (turning a silent runtime bump into an apply-time rejection -- precisely the
  CRD-over-ConfigMap advantage of section 2), and the operator SHOULD additionally record
  the *admitted* request on status, not just the requested one.

### 4. `limits == requests`: request is the effective ceiling

Because the v1 target (GKE Autopilot) forces `limits == requests` (section 1, item 2),
the policy treats the **request as the effective ceiling.** Concretely:

- The policy's authoritative knob per scope is the **request**. The operator sets
  `limits` equal to `requests` on the pod it emits, so the manifest is explicit and the
  behavior is identical on a cluster that does not force the equality (a portability
  win: the pod behaves the same on Autopilot and on a vanilla cluster). We do not rely
  on the cluster to do the normalization for us.
- The docs must warn, prominently, in both directions:
  - **Over-requesting costs money.** A pod bills for `lifetime x requests` (SPEC sec.
    6.1). A policy that requests 8 vCPU / 16Gi for jobs that need 1 vCPU / 2Gi burns
    money on every job for headroom it never uses. Right-size per scope.
  - **Under-requesting causes OOM and throttle.** With no headroom above the request,
    a memory spike during layer assembly is an OOM kill (job fails), and a CPU-bound
    build that wants more than its request is hard-throttled (job slows, can trip
    `activeDeadlineSeconds` and fail as `DeadlineExceeded`, SPEC sec. 6.2). The failure
    mode of stinginess is a failed build, not a cheap build.
- The resolved effective request is recorded on the EphemeralRunner status so the
  cost/ceiling for a given job is observable after the fact.

### 5. `activeDeadlineSeconds` ownership: this policy owns the field

The per-pod hard cap (`activeDeadlineSeconds`, SPEC sec. 6.2) resolves per scope exactly
like CPU/memory, so **this ResourcePolicy owns the field** -- it is defined here, on the
policy object, and resolved through the same repo->org->global hierarchy. The
timeout/stuck-vs-slow design (SPEC sec. 6.2, bd garc-7ft.10) **references** the resolved
value (it reads the cap the policy set to drive its `DeadlineExceeded` handling); it does
not define or carry the field. This single-owner rule prevents the field being declared in
two places. The progress-liveness / stall-window knobs (which are behavioral, not sizing)
remain owned by the timeout design, not this policy.

### 6. Portability constraints on the policy

Per SPEC sec. 6.6, the ResourcePolicy and the pods it produces:

- Carry **compute requests/limits and pod-level scheduling/lifecycle knobs that resolve
  per scope** (CPU, memory, `activeDeadlineSeconds`), and nothing cloud-specific.
- **Do not** hardcode a StorageClass, provisioner, zone, region, instance type, or node
  selector tied to a particular cloud. Storage decisions (build cache) live in the build
  strategy (ADR `0005-image-build-strategy.md`), which already mandates registry-based caching
  over RWX/PD volumes for exactly this portability reason; the resource policy does not
  reach into storage.
- Use only `resource.Quantity` strings and GA core fields, so the same policy applies on
  Autopilot, GKE Standard, and any conformant distro.

## Consequences

### Positive

- **Predictable, observable sizing.** Whole-policy selection plus most-specific-wins
  means "which numbers did this pod get, and why" is a one-line answer, recorded on
  status and in logs.
- **Validation at apply time.** The CRD's schema rejects malformed policies when Priya
  applies them, not hours later when a pod fails to build.
- **Consistent with the operator's model.** One more typed CRD, same RBAC / status /
  watch / GC machinery as the rest of the hierarchy (ADR `0003-crd-hierarchy-and-reconcile-design.md`); no
  special-cased config surface.
- **Correct sizing for the real v1 workload.** Defaults reflect a single
  image-build-hungry container, not an obsolete runner/DinD split, so out-of-the-box
  builds are not starved.
- **Cost control where it belongs.** Per-scope tightening is the cost lever; the
  platform engineer turns one dial (the policy) rather than editing pod templates.
- **Portable by construction.** No StorageClass/zone/instance coupling; identical
  request/limit behavior on Autopilot and elsewhere.

### Negative

- **One more CRD to define, version, install, and document.** Net new API surface and a
  small install-order consideration (the CRD must land before policies referencing it).
- **Whole-policy selection is coarse.** An org that wants "org defaults but bump memory
  on one repo" must restate the full policy at the repo scope rather than overriding a
  single field. This is a deliberate trade of flexibility for predictability; we accept
  the slight verbosity.
- **Defaults are generous, so the naive cost is higher.** A platform engineer who never
  tightens any scope pays for 2 vCPU / 4Gi on trivial jobs. We accept this because the
  failure mode of the opposite default (stingy) is failed builds, which is worse; we
  mitigate with clear docs steering tightening per scope.

### Risks

- **R-policy-1: `limits == requests` surprises operators from non-Autopilot clusters.**
  An operator used to request/limit headroom may set a low request expecting burst.
  Mitigation: the operator always emits `limits == requests` explicitly (section 4), and
  the docs call out the no-headroom semantics in both cost and OOM/throttle directions.
- **R-policy-2: under-resourced builds present as flaky jobs.** An OOM-killed or
  throttled build can look like a flaky test to a developer. Mitigation: generous
  defaults; surface the resolved request and the termination reason (OOMKilled /
  DeadlineExceeded) on EphemeralRunner status so the cause is legible, not mysterious.
- **R-policy-3: scope resolution depends on correctly identifying a job's org/repo.**
  If the demand listener mis-buckets a job's org/repo, the wrong policy could be
  selected. Mitigation: resolution falls back safely to the global default (never
  fails), and the chosen scope is recorded on status for audit; the listener's
  client-side label/scope bucketing is covered by the scaling slice (garc-7ft.8).
- **R-policy-4: stale or shadowed policies go unnoticed.** A repo policy may be shadowed
  by, or drift from, an org policy. Mitigation: CRD status reports which policies are
  active vs shadowed; a ConfigMap carrier would have made this materially harder, which
  reinforces the CRD choice.
- **R-policy-5: Autopilot silently mutates policies outside its CPU:memory ratio band.**
  A tightened per-scope policy outside 1:1..1:6.5 GiB/vCPU is admitted with the smaller
  resource raised, so Priya is billed for and gets a pod she did not specify, and status
  shows the requested (not admitted) numbers. Mitigation: validate the ratio in the CRD
  schema/admission webhook so an out-of-band policy is rejected at apply time (section 3);
  record the admitted request on status. This is an Autopilot-specific admission behavior;
  on non-Autopilot clusters the exact numbers are honored.

## Open questions

- **Field-merge escape hatch.** Do we ever want an opt-in per-field override (e.g.
  inherit org policy, override only memory)? v1 says no (whole-policy selection). Revisit
  only if real demand appears; if added, it must stay explicitly opt-in so the default
  remains predictable.
- **Per-label / per-GiteaRunnerSet sizing.** Resolution here is repo/org/global. Do we
  also need sizing keyed by runner label (e.g. a `large` label that selects a bigger
  policy) for jobs that self-select size via `runs-on`? Tracked as a possible later axis,
  not v1.
- **ConfigMap convenience for the global default.** Should we offer a ConfigMap-only
  path for the single global default as a zero-CRD quickstart, while keeping the CRD for
  org/repo scopes? Deferred; not a v1 commitment.
- **Default values tuning.** The 2 vCPU / 4Gi global default is an informed starting
  point, not measured. It should be validated against real build workloads during the
  build-strategy work (ADR `0005-image-build-strategy.md`) and adjusted before GA.
- **Interaction with `activeDeadlineSeconds` - RESOLVED (section 5).** This policy owns
  the `activeDeadlineSeconds` field (it resolves per scope like sizing); the timeout
  design (SPEC sec. 6.2, garc-7ft.10) references the resolved value and owns the
  behavioral stall-window knobs. Single owner, defined in one place.

## References

- SPEC `docs/product/gitea-actions-operator/SPEC.md` sec. 6.1 (resource management),
  sec. 6.2 (timeouts, `activeDeadlineSeconds`), sec. 6.6 (portability), sec. 7 / 9
  (daemonless, no DinD; GKE Autopilot v1 target).
- DECISIONS.md D2 (daemonless builder, no DinD), D3 (GKE Autopilot v1 target).
- ADR `0003-crd-hierarchy-and-reconcile-design.md` - the CRD hierarchy this policy plugs into
  (GiteaRunnerSet `resourcePolicyRef`).
- ADR `0005-image-build-strategy.md` - daemonless builder selection and registry-based build
  cache (owns storage/cache, which this policy deliberately does not).
- ADR `0006-credential-model.md` - per-pod credential model (separate concern; resolved
  alongside per-pod sizing at provisioning time).
- bd issue garc-7ft.4 (this ADR). Blocks packaging: bd issue garc-7ft.13 (the bundle
  ships the ResourcePolicy CRD, the default policy, and the Helm values that expose it).
