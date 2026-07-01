# 0005 - Image build strategy: daemonless builder, docker-build UX, and build cache

Status: Proposed

Owner: lead-software-engineer
Tracking: bd garc-7ft.5
Date: 2026-06-30

## Context

The v1 execution model (SPEC sec. 7; DECISIONS D2) dropped privileged Docker-in-Docker
(DinD) so that every pod the operator creates - controller, listener, runner, and any
build pod - admits on **GKE Autopilot under a restricted securityContext** with no
WorkloadAllowlist, no org-policy change, and no privileged container (SPEC sec. 9;
DECISIONS D3). Concretely, every pod must satisfy:

- `runAsNonRoot: true`, an explicit non-zero `runAsUser`/`fsGroup`
- `allowPrivilegeEscalation: false`
- `capabilities.drop: [ALL]`
- `seccompProfile.type: RuntimeDefault`
- no `privileged`, no host namespaces, no host mounts, no Docker socket

That constraint is non-negotiable: it is the property that makes Autopilot the v1
target instead of a later gated port. Any image-build mechanism we pick must hold it.

The confirmed job need is narrow and known (SPEC sec. 7; personas, Sam): workflows
**`docker build` an image and push it**. That is the entire in-scope surface.
Explicitly **out of v1 scope** and therefore not a requirement on this decision:

- `services:` containers - need a daemon to run sidecar service images.
- raw `docker run` / `docker exec` / `docker ps` in steps - need a daemon.

Both are excluded precisely because they would reintroduce a daemon, and a daemon on
Autopilot means privilege. The forward path for them is upstream native-Kubernetes
execution (gitea/runner PR #1000), tracked as a watch item (DECISIONS D8), not a v1
dependency.

Three sub-decisions follow from this, and this ADR owns all three:

1. **Which daemonless builder** the runner image ships and the operator targets.
2. **How a user's `docker build` invocation reaches that builder** - a baked-in shim
   versus requiring workflows to call the builder directly.
3. **How build cache survives** the fact that every job runs in a fresh ephemeral pod
   with an empty local cache (SPEC sec. 8; DECISIONS D6).

A hard external fact bounds sub-decision 1: **Google archived Kaniko in June 2025**
(DECISIONS D2). Kaniko was the obvious daemonless choice for years; it is now
unmaintained upstream. We therefore must not hard-pin upstream Kaniko, and we should
treat the builder choice as replaceable rather than load-bearing.

This ADR cross-references the CRD hierarchy (ADR 0003), the resource policy (ADR
0004), and the credential model (ADR 0006). It **blocks the walking skeleton**
(garc-7ft.7), which needs a concrete builder, invocation path, and cache target to
build and push one image end-to-end.

## Decision

### Decision 1 - Daemonless builder: BuildKit-rootless as the primary, behind a pluggable seam

We adopt **BuildKit in rootless mode** (`buildkitd --oci-worker-rootless` /
`buildctl`, or the equivalent embedded library path) as the **v1 default builder**,
and we make the builder a **pluggable choice** so that this selection is not
architecturally load-bearing.

Rationale, scored against the four criteria that matter here:

- **Autopilot / unprivileged constraint (gate).** Rootless BuildKit runs without a
  Docker daemon and without `privileged`. It uses user namespaces; on a node where
  unprivileged user-namespace cloning is available (default on modern GKE node
  images), it needs no added Linux capabilities and runs under
  `capabilities.drop: [ALL]` with `seccompProfile: RuntimeDefault`. It does **not**
  require `/dev/fuse` for the core build path when using the native snapshotter, which
  keeps the pod inside the restricted profile. This is the gate every candidate must
  pass, and BuildKit passes it.

- **Dockerfile compatibility (high weight).** BuildKit is the engine **behind**
  `docker build` (it is what modern Docker and `docker buildx` already call), so it has
  the broadest and most faithful Dockerfile feature coverage of any daemonless option:
  multi-stage builds, `--mount=type=cache`, `--mount=type=secret`, heredocs, build
  args, target selection, and `--platform`. Since our migration story is "users already
  write `docker build`," maximal Dockerfile fidelity directly reduces the leakiness
  risk in Decision 2.

- **Cache support (high weight).** BuildKit has **mature registry cache export/import**
  via `--export-cache type=registry` / `--import-cache type=registry` (and the
  higher-fidelity `mode=max` and `type=inline` variants). This is exactly the portable,
  RWX-free cache primitive Decision 3 depends on. No candidate has a more proven
  registry-cache story.

- **Maintenance health (high weight given the Kaniko lesson).** BuildKit is an actively
  maintained moby project with frequent releases and a large contributor base - the
  opposite of Kaniko's archived status.

Alternates considered and kept as drop-in replacements behind the seam:

- **Buildah** - Mature, actively maintained (containers org), strong rootless support,
  excellent Dockerfile compatibility, and it can `--layers` cache and push cache to a
  registry. It is a fully credible primary; we rank it just behind BuildKit on
  registry-cache ergonomics and on the directness of its mapping to the existing
  `docker build` semantics (Buildah's native UX is `bud` plus its own verbs). Buildah
  is our **first fallback** if BuildKit's rootless requirements prove awkward on a
  target cluster.

- **Kimia** - A daemonless, rootless-friendly builder positioned as a Kaniko successor
  for the post-archive world. Promising and worth tracking, but **less battle-tested**
  than BuildKit/Buildah today; we treat it as a candidate the pluggable seam lets us
  adopt later without a redesign, not as the v1 default.

- **A maintained Kaniko fork** - Several community forks exist after the June 2025
  archive. Kaniko's appeal was that it needs neither daemon nor user namespaces, which
  can simplify the securityContext. But forks vary in upkeep and provenance, and
  betting v1 on an unofficial fork is the kind of single-point maintenance risk we are
  explicitly trying to avoid. Acceptable as a pluggable option for environments where
  user namespaces are unavailable; **not** the default.

**The pluggable seam.** The runner image and the operator address the builder through
one narrow internal interface - "given a build context, Dockerfile, target ref, and
cache config, produce and push an image, streaming logs" - selected by a
`GiteaRunnerSet`-level builder field (e.g. `buildkit` | `buildah` | `kaniko`) with
`buildkit` as the default. Because the choice is configuration, not architecture,
switching builders (or adopting Kimia later) is an image-and-config change, not a
rewrite. This directly contains the Kaniko-archive class of risk.

### Decision 2 - docker-build UX: ship a thin `docker build` shim, scoped and documented

We **bake a thin `docker build` shim into the runner image** that translates the
common `docker build` invocation into the chosen builder's invocation, rather than
requiring every workflow to be rewritten to call the builder directly.

Rationale: the entire migration value proposition is that teams **already write
`docker build`/push** in their Gitea workflows (SPEC personas, Sam: "does not want to
know the operator exists"). A shim that intercepts `docker build` and routes it to
BuildKit lets the bulk of existing workflows run unchanged, which is the difference
between "drop-in" and "rewrite every pipeline." The shim covers the high-traffic flags
that map cleanly onto the builder (`-t/--tag`, `-f/--file`, `--build-arg`, `--target`,
`--platform`, `--push`, build context path) and wires in the registry cache flags from
Decision 3 by default.

We accept and document the **leakiness** explicitly. A shim is a translation layer, not
the real Docker CLI, so flags and behaviors that have no faithful builder equivalent
either no-op, error clearly, or behave subtly differently. The shim must **fail loudly
on unsupported flags** rather than silently ignore them, and the docs must state the
supported-flag set and that `docker run`/`docker ps`/`services:` are out of scope (they
need a daemon the pod does not run). Workflows that need a flag the shim cannot
faithfully reproduce can call the builder directly as an escape hatch; the direct-invoke
path remains available and supported.

Tradeoff stated plainly: option (a) shim buys migration UX at the cost of a maintained
translation surface and a class of "it worked under real Docker but not the shim"
support tickets; option (b) direct-invoke is simpler for us to maintain but pushes a
rewrite onto every consumer and undercuts the drop-in promise. We choose (a) because
the confirmed need (`docker build`/push only) is exactly the slice a thin shim covers
well, and the leak surface is bounded by the narrow in-scope command set.

### Decision 3 - Build cache: registry-based cache as the v1 default; no shared RWX volume

The v1 default cache is **registry-based layer cache** - the builder exports cache with
`--cache-to` and imports it with `--cache-from` against a **registry endpoint the job
already has credentials for**: a cluster-local pull-through / cache registry, or the
Gitea package registry / Artifact Registry the job already pushes its image to (see ADR
0006 for the credential path that makes this reachable from inside the pod).

Rationale: a fresh ephemeral pod has an **empty local cache every job** (SPEC sec. 8;
DECISIONS D6), so without a network-reachable cache, every `docker build` is cold -
unacceptable for a runner whose job is building images. Registry-based cache is:

- **Portable** - it needs only a registry and image-pull/push credentials, which the
  build already requires; it does not depend on any storage class or distro-specific
  volume feature.
- **RWX-free** - no shared filesystem, so no ReadWriteMany coupling (see below).
- **Concurrency-correct** - many ephemeral pods can read and write the same registry
  cache tags concurrently over the network; the registry, not a single mounted disk, is
  the shared point.

**We rule against a shared RWX cache volume as the default.** A ReadWriteMany PVC
couples v1 to storage-class and distro support for RWX, which is exactly the
portability tension we are avoiding (SPEC sec. 8; risk R4). It stays available as an
opt-in for clusters that have good RWX, but it is not the portable default.

**The Google Persistent Disk finding (the reason RWX-via-PD is a non-starter).** On
Autopilot a Persistent Disk / Hyperdisk is fully usable through a PVC, but a **standard
PD is ReadWriteOnce / single-node**. Therefore **one PD cannot be a shared cache across
concurrently-scheduled ephemeral pods** - the moment two build pods land on different
nodes, they cannot both mount the same RWO disk. A PD only fits as **per-pod ephemeral
scratch** (a private build workspace, discarded with the pod), or **behind a single
cache/registry pod** that owns the disk and serves all runners over the network - which
is just the registry-based design with local-disk backing. This finding reinforces the
registry-based default and is why "give every build pod the same disk" is not on the
table.

This matches the closest prior art: Gitea Enterprise ARC also uses a **cache-server pod
plus PVC served over the network**, not an RWX fan-out (SPEC sec. 12.1).

**Success metric.** v1 defines a **cache-hit ratio** on image builds - the fraction of
build layers (or build steps) satisfied from imported cache versus rebuilt from scratch,
exported as an operator metric and measured on a representative repeat-build workflow.
We target a cache-hit ratio **above a configured threshold** (initial goal: a clear
majority of unchanged layers served from cache on a second, source-unchanged build), and
we track the corresponding **build-wall-time reduction** between a cold first build and a
warm second build. This satisfies the garc-7ft.5 acceptance criterion that the ADR
"defines a cache-hit success metric."

## Consequences

### Positive

- All build pods admit on **GKE Autopilot under the restricted securityContext** with no
  allowlist and no privileged container - the central v1 payoff is preserved (SPEC
  sec. 9).
- The builder choice is **configuration, not architecture**: BuildKit today, Buildah or
  Kimia later, with no redesign - which neutralizes the Kaniko-archive risk for the next
  time an upstream tool changes status.
- BuildKit's position as the engine behind `docker build` gives the **highest Dockerfile
  fidelity** available daemonless, minimizing the shim's leak surface.
- The **shim keeps existing `docker build`/push workflows running unchanged**, delivering
  the drop-in migration story the personas need.
- **Registry-based cache is portable and concurrency-correct**, with no RWX/storage-class
  coupling, and reuses credentials the job already holds (ADR 0006).
- A concrete **cache-hit-ratio metric** makes build-cache effectiveness observable rather
  than asserted.

### Negative

- We **own a translation layer** (the shim) and its flag-coverage matrix; it is a
  maintained surface and a source of "real Docker did X, the shim did Y" support load.
- Rootless BuildKit has **node-level prerequisites** (unprivileged user-namespace
  cloning available on the node) that, while satisfied on default modern GKE node
  images, are a portability footnote we must document and test on other distros.
- Registry-based cache **costs registry storage and network round-trips** per build, and
  cache import/export latency can erode the win on very large layer sets; the cache-hit
  metric is how we catch that.
- `services:` containers and raw `docker` CLI remain **unsupported** in v1; teams relying
  on them must wait for the native-K8s path (PR #1000) or stay on a privileged runner
  elsewhere.

### Risks

- **R-build-1: shim leakiness.** A real workflow depends on a `docker build` flag or
  behavior the shim does not faithfully reproduce. Mitigation: fail loudly on unsupported
  flags; document the supported set and the direct-invoke escape hatch; keep the in-scope
  surface narrow (build/push only). (Tracks SPEC risk R2.)
- **R-build-2: builder maintenance churn.** Today's best builder may stall or change
  license/status (the Kaniko lesson). Mitigation: the pluggable seam makes replacement a
  config-and-image change; we keep Buildah validated as a ready fallback.
- **R-build-3: rootless node prerequisites.** A target cluster disables unprivileged user
  namespaces, breaking rootless BuildKit. Mitigation: Buildah fallback; a Kaniko-fork
  option for no-userns environments; document the prerequisite.
- **R-build-4: cache portability/cost.** Registry cache adds storage and transfer cost,
  and a misconfigured registry (auth, retention, GC) can silently disable caching.
  Mitigation: cache-hit metric and alerting on a collapsed hit ratio; sane default
  cache-tag and retention guidance. (Tracks SPEC risk R4.)

## Open questions

- **Cache mode and tag layout.** `mode=max` (cache all stages, larger registry
  footprint) versus `mode=min`/inline (smaller, less reuse), and the per-repo cache-tag
  naming and retention/GC policy. To resolve in the build-cache implementation slice.
- **Embedded daemon vs `buildctl` to a buildkitd.** Whether the runner image runs a
  short-lived in-pod `buildkitd` or links BuildKit as a library, and how that interacts
  with `restartPolicy: Never` and per-pod teardown (ADR 0003).
- **Shim flag-coverage matrix.** The exact supported-flag set, and which unsupported
  flags hard-error versus warn. Needs a survey of real in-house workflows.
- **Cache-hit threshold value.** The concrete numeric target and the canonical
  repeat-build benchmark used to measure it, to be fixed alongside the success-metrics
  work (SPEC sec. 10).
- **Registry credential reach into the build pod.** The precise injection of cache
  registry credentials into the unprivileged build pod is owned by ADR 0006; this ADR
  assumes that path exists.

## References

- SPEC `docs/product/gitea-actions-operator/SPEC.md` sec. 7 (job execution model -
  daemonless, no privilege), sec. 8 (build cache), sec. 9 (GKE Autopilot target),
  sec. 10 (success metrics), sec. 11 (risks R2/R4).
- DECISIONS `docs/product/gitea-actions-operator/DECISIONS.md` D2 (daemonless builder,
  no DinD; Kaniko archived June 2025), D3 (GKE Autopilot v1 target), D6 (build cache in
  v1 scope).
- ADR 0003 - CRD hierarchy (runner pod lifecycle, `restartPolicy: Never`, finalizers).
- ADR 0004 - resource policy (build pods are CPU/memory-hungry; per-scope sizing).
- ADR 0006 - credential model (registry credentials reaching the unprivileged build
  pod for cache push/pull).
- bd garc-7ft.5 (this ADR); **blocks** garc-7ft.7 (walking skeleton).
- Upstream watch item: gitea/runner PR #1000 (native-Kubernetes execution), the future
  path for `services:` and raw `docker` CLI.
