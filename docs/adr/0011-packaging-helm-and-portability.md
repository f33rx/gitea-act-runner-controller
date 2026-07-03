# ADR 0011: Packaging - Helm chart, RBAC scope, and GA-core portability

## Status

Proposed (2026-07-02)

bd issue: garc-7ft.13. Anchors the implementation slice of the same name (P2
hardening, epic garc-7ft). Depends on demand-driven scaling (garc-7ft.8, closed),
guaranteed teardown (garc-7ft.9, closed), and the resource-policy-resolution ADR
(garc-7ft.4 / ADR 0004, closed as design-only -- see Decision 3 below for what that
means for this ADR).

Related ADRs: credential model (0006, the read/write credential split this chart
must template), resource policy resolution (0004, design-only, no CRD field exists
yet), image build strategy (0005, sent back on an Autopilot admission error, still
blocked on the garc-6uf spike).

## Context

Everything the operator needs to run (CRDs, the manager Deployment + RBAC, the
listener Deployment + RBAC, sample `GiteaRunnerSet` manifests) exists today as flat
YAML under `config/`, applied by hand (`kubectl apply -f config/manager/manager.yaml`,
as this session's live-testing did). There is no packaging: no Helm chart, no
templated values, no versioned release artifact. This is the last of the four P2
hardening slices; unlike 0008/0009/0010, it does not change runtime behavior --
it is entirely about how an operator (the persona, not the software) *installs* this
software into a cluster they don't already have `config/` checked out for.

Two things surfaced during this session's work on the other three P2 slices directly
bear on this ADR and must be addressed here, not silently carried forward into the
chart unexamined:

1. **garc-kvm / garc-csb (both open, P2, filed 2026-07-02):** `config/manager/
   manager.yaml`'s RBAC is a single cluster-wide `ClusterRole`/`ClusterRoleBinding`
   granting `pods`/`secrets` `get;list;watch;create;update;patch;delete` across
   **every namespace in the cluster**, not just the namespace(s) where
   `GiteaRunnerSet`s actually live. garc-csb is an open research task asking whether
   this needs to be scoped down before v1 or is an acceptable, documented risk. A
   Helm chart is exactly where "one `Role` per managed namespace vs. one
   `ClusterRole`" becomes a real, user-facing install-time decision (a values-file
   choice), so this ADR cannot package the existing `ClusterRole` verbatim without
   either resolving garc-csb first or explicitly deferring it with a documented
   reason -- silently templating the current shape would look like this ADR settled
   the RBAC-scope question when it did not.
2. **ADR 0004 (resource policy resolution) is design-only.** It defines a
   repo -> org -> global resolution order and a "carrier" concept but does not
   implement a `ResourcePolicy` CRD or any field on `GiteaRunnerSet`/`EphemeralRunner`
   that currently accepts resource requests/limits from the chart's values. The
   original bead description says the chart should carry "resourcePolicy" as a
   `GiteaRunnerSet` value -- that field does not exist in the API today.

Also relevant: garc-7ft.13's own notes record a pivot (2026-06-30) from GKE Standard
to **GKE Autopilot** as the v1 portability target, which requires validating that a
restricted `securityContext` (`runAsNonRoot`, drop `ALL` capabilities, no
`privileged`, `seccompProfile: RuntimeDefault`) admits with no `WorkloadAllowlist`
exception. The listener's pod spec (`config/manager/listener-deployment.yaml`)
already ships exactly this restricted profile and was written with Autopilot in
mind; the manager's pod spec (`config/manager/manager.yaml`) does not yet set an
explicit `securityContext` at all. Separately, ADR 0005 (image build strategy) was
sent back by arch review on a factual Autopilot admission error (rootless BuildKit
needs `seccomp: Unconfined`, not `RuntimeDefault`) and is blocked on garc-6uf, a
spike requiring a live Autopilot cluster this session does not have access to. That
spike is scoped to the **runner pod's build container**, a different pod than the
manager/listener this ADR packages -- it does not block chart authoring, but it does
mean this ADR cannot yet claim "full GA-core-only portability validated end to end,"
only "manager + listener pods validated" (once a real cluster is available -- see
Decision 5).

## Decision

### 1. Chart layout: unmanaged CRDs directory, templated RBAC/Deployments, values-driven GiteaRunnerSet

Standard Helm convention for CRD-owning operators (matching common practice: CRDs in
`crds/`, installed once and not upgraded/rolled-back by Helm's normal lifecycle,
since Helm intentionally does not diff or delete CRDs on upgrade/uninstall to avoid
accidental data loss of custom resources):

```
charts/gitea-actions-controller/
  Chart.yaml
  values.yaml
  crds/                          # copied verbatim from config/crd/, not templated
    giteaactions.blackrabbit.dev_ephemeralrunners.yaml
    giteaactions.blackrabbit.dev_ephemeralrunnersets.yaml
    giteaactions.blackrabbit.dev_gitearunnersets.yaml
  templates/
    manager-serviceaccount.yaml
    manager-rbac.yaml            # Role+RoleBinding OR ClusterRole+ClusterRoleBinding, see Decision 2
    manager-deployment.yaml
    listener-serviceaccount.yaml
    listener-rbac.yaml
    listener-deployment.yaml
    teardown-credential-secret.yaml   # optional, only if values.teardownCredential.create=true
    gitearunnerset.yaml           # optional, only rendered if values.giteaRunnerSet.enabled=true
    NOTES.txt
```

`GiteaRunnerSet` is templated as an optional resource (`values.giteaRunnerSet.enabled`,
default `false`) rather than required, so the chart's primary job (install the
operator) is decoupled from "also create my first runner set" -- an operator persona
installing this once for their org creates their org's `GiteaRunnerSet`s as separate,
ordinary `kubectl apply`/GitOps-managed manifests in the common case, not through
chart values re-applied on every `helm upgrade`.

### 2. RBAC scope: default to namespace-scoped Role, ClusterRole as an explicit opt-in -- this is garc-kvm's fix, landed here

Resolving garc-csb's research question as part of this ADR (it was filed to unblock
exactly this decision): **default the chart to namespace-scoped `Role`/
`RoleBinding`s**, one per namespace the operator is told to manage (a
`values.watchNamespaces: []` list, defaulting to the release namespace only). A
`values.clusterScope: false` toggle exists for the one legitimate reason to want
`ClusterRole`: a single shared operator instance managing `GiteaRunnerSet`s across
many teams' namespaces without redeploying the chart per namespace. This is opt-in,
not the default, because:

- The controller's actual blast radius (per garc-kvm's own analysis) is pods/secrets
  it owns via its own CRs -- it never needs to touch a namespace with no
  `GiteaRunnerSet` in it, so cluster-wide access is strictly broader than the
  real requirement for the common single-team/single-namespace install.
- Matching ADR 0006's already-established precedent: that ADR treats Gitea-side
  scope minimization (org-scoped tokens, split read/write credentials) as a
  first-class design concern; this brings the Kubernetes-side RBAC to the same
  standard rather than leaving it the one unscoped credential surface.
- A namespace-scoped default is also the safer default for the GKE Autopilot
  portability target (Decision 4): Autopilot deployments frequently share a project
  with other tenants' workloads more readily than a hand-rolled GKE Standard
  cluster would.

This closes garc-kvm as an implementation outcome of this ADR (not a separate PR):
the chart's default manifest shape is namespace-scoped from the start, so there is
no "narrow it later" migration to design.

### 3. Resource policy: values.yaml reserves the shape, does not template real logic yet

Since ADR 0004's `ResourcePolicy` CRD does not exist, this chart cannot template a
field that has no API to receive it. `values.yaml` reserves the documented shape
(commented-out, non-functional) under a `# resourcePolicy (reserved, ADR 0004 not
yet implemented as a CRD field):` heading, matching the field names ADR 0004 already
specified (`repo`/`org`/`global` resolution keys) so a future PR that lands the real
CRD field has an obvious, pre-agreed values-schema slot to wire into, rather than
inventing chart-values naming at that point under separate review. Until then, pod
resource requests/limits are a plain per-`GiteaRunnerSet` values override
(`values.giteaRunnerSet.resources`, mirroring a single flat
`corev1.ResourceRequirements`), consistent with ADR 0004's own Consequences section
("size the single runner container for the heaviest thing it runs" -- a single flat
policy, not yet the full repo->org->global chain).

### 4. Autopilot-safe pod spec defaults, explicit securityContext on the manager

`values.yaml` defaults the manager Deployment's pod spec to the same restricted
profile the listener already ships (`runAsNonRoot`, drop `ALL` capabilities, no
`privileged`, `allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`,
`readOnlyRootFilesystem: true` where the binary's own filesystem needs allow it).
This closes the gap noted in Context (the manager's current `config/manager/
manager.yaml` sets no explicit `securityContext` at all, unlike the listener) as
part of this chart, rather than leaving the manager as the one component without an
explicit, reviewed security posture. `GiteaRunnerSet.spec.template` (the runner pod
template, `RawExtension` today) is left as an operator-supplied value with the same
restricted profile as the chart's *documented default recommendation* in
`values.yaml` comments and `NOTES.txt`, not enforced by the chart -- the actual
runner pod's admission-safe shape (especially the build-container security context
question ADR 0005/garc-6uf is still resolving) is out of this ADR's scope; this ADR
packages the operator (manager + listener), not the ephemeral runner pods it creates.

### 5. Validation: `helm lint`/`helm template` now; live Autopilot admission is a separate, explicitly-tracked gate

Two different claims must not be conflated:

- **"The chart renders valid, schema-conformant Kubernetes manifests."** Verifiable
  now, without any live cluster, via `helm lint` and `helm template | kubectl apply
  --dry-run=server` (dry-run against any reachable API server for schema
  validation, without actually creating resources) or `kubeconform`. This ADR's
  implementation is gated on this passing before merge.
- **"The chart's default pod specs actually admit on GKE Autopilot with no
  WorkloadAllowlist exception."** Requires a live Autopilot cluster. This is the
  same class of validation garc-6uf already exists to do for the runner pod's build
  container; this ADR's manager/listener pods need an equivalent (smaller, since
  their security posture is already the ordinary "restricted" profile with no
  BuildKit-specific seccomp exception) live check. **Not blocked on garc-6uf**
  (different pods, no rootless-BuildKit-specific requirement), but **is** blocked on
  the same missing resource: a real GKE Autopilot cluster, not available in this
  session. Tracked as this ADR's own follow-up spike rather than silently assumed
  to pass because the manifests look reasonable.

## Consequences

### Positive

- Resolves garc-kvm/garc-csb as a direct outcome (namespace-scoped default RBAC)
  rather than leaving that P2 gap open indefinitely alongside a new chart that would
  otherwise have re-shipped the unscoped `ClusterRole` by default.
- CRDs-as-unmanaged-directory follows established Helm convention for
  operator-owning charts; avoids Helm's CRD-upgrade footguns (no accidental
  deletion of in-use CRDs on `helm uninstall`).
- `values.yaml`'s reserved-but-inert resourcePolicy shape gives ADR 0004's eventual
  CRD field an agreed landing spot instead of a second round of naming bikeshedding
  when it lands.
- Explicit manager `securityContext` closes a real asymmetry (listener had one,
  manager didn't) that was never a deliberate decision, just an oversight.

### Negative

- `clusterScope: false` as the default is a behavior change from today's
  `config/manager/manager.yaml` (which is already `ClusterRole`) for anyone
  currently applying the raw manifests directly rather than the new chart; the raw
  `config/` manifests are not modified by this ADR (only the new chart differs), so
  this is additive, not a breaking change to existing deployments, but should be
  called out in migration/deploy docs.
- The chart cannot fully satisfy garc-7ft.13's "validate GA-core-only portability"
  acceptance criterion end-to-end without a live Autopilot cluster; this ADR ships
  with that gate explicitly open (Decision 5), not silently assumed passing.
- Namespace-scoped-by-default requires per-namespace `Role`/`RoleBinding`
  templating (a small loop over `values.watchNamespaces` in the chart), a bit more
  template complexity than a single flat `ClusterRole`.

### Risks

- **R-PKG-1: multi-namespace operators choosing `clusterScope: false` must remember
  to add each new managed namespace to `values.watchNamespaces` and `helm upgrade`.**
  Mitigation: `NOTES.txt` (Helm's post-install output) explicitly documents this;
  `clusterScope: true` remains available for the "don't want to think about this"
  case, at the cost of broader RBAC.
- **R-PKG-2: the resourcePolicy reserved-shape values placeholder drifts from
  whatever ADR 0004's eventual CRD field actually looks like**, if that field's
  design changes before implementation. Mitigation: it is documented as
  non-functional/reserved, not a committed schema; the real field's own
  implementation PR is the source of truth, not this chart.
- **R-PKG-3: Autopilot admission validation (Decision 5) has no committed
  timeline** since it depends on cluster access outside this session's control,
  same constraint blocking garc-6uf. Mitigation: filed as its own follow-up bead
  (see below) rather than silently left as an implicit TODO inside this ADR.

## Open questions

1. **Should `values.watchNamespaces` support a `*`/all-namespaces sentinel distinct
   from `clusterScope: true`**, or are they the same thing in practice (a
   `ClusterRole` already implies all-namespaces)? Leaning toward treating them as
   the same toggle (no separate sentinel) unless a concrete use case needs the
   distinction.
2. **Chart versioning/release process** (semver policy relative to CRD schema
   changes, whether CRDs get their own chart per the "CRDs-chart" pattern some
   projects use to version them independently of the app chart) is deferred to
   implementation; not blocking this ADR's acceptance.

## References

- ADR 0004 - resource policy resolution: the design-only repo->org->global chain
  this ADR's `values.yaml` reserves a values-schema slot for for (Decision 3),
  without implementing.
- ADR 0005 - image build strategy: sent back on an Autopilot admission error,
  blocked on garc-6uf; a different pod (runner/build container) than the ones this
  ADR packages, so not a blocking dependency, but the reason Decision 4 draws a
  careful line between "manager/listener pods" and "runner pods" scope.
- ADR 0006 - credential model: the org-scoped/split read-write credential
  precedent this ADR's RBAC-scoping decision (Decision 2) is brought up to match.
- garc-kvm / garc-csb: the open RBAC-scope gap and its research task, resolved by
  Decision 2 (namespace-scoped default) as part of this ADR rather than a separate
  fix PR.
- garc-6uf: the blocked live-Autopilot-cluster spike for the runner/build pod;
  cited in Decision 5 as the same class of validation gap this ADR's manager/
  listener admission check has, though a different, smaller check on different
  pods.
