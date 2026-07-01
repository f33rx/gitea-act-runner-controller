# Design decisions log -- Gitea Actions Kubernetes Operator

Running record of the product/design decisions made while drafting the spec, with
the reasoning and who decided. This is a snapshot derived from the conversation;
bd issues are the source of truth for work state.

## D1 -- Job model (CRD vs bare Pods)
**Decision:** Spec recommends; flagged for arch review.
**Status:** open -- spec to recommend a CRD-per-runner hierarchy (ARC-faithful),
ratified by lead-engineer + human.

## D2 -- Execution model (daemonless builder, no DinD)
**Decision (REVISED 2026-06-29):** v1 = **no Docker daemon in the runner pod.**
Image builds run through a **daemonless builder** (Kaniko fork / BuildKit-rootless /
Kimia / Buildah -- tool chosen in the build ADR) in an **unprivileged** pod. No
`docker:dind` sidecar, no privileged anywhere.
**Reasoning (user decision):** The Autopilot target matters more than keeping a Docker
daemon. Confirmed job needs are **`docker build`/push only** -- NOT `services:`
containers and NOT raw `docker` CLI in steps. That is exactly the case daemonless
builders handle cleanly: they build + push images from a Dockerfile with no daemon and
no privilege, so they admit on Autopilot. Dropping DinD removes the single privileged
component and the entire Autopilot WorkloadAllowlist eligibility gate at once.
**What this costs:** workflows that call `docker build` are rewritten to the chosen
builder (or shimmed). `services:` and raw `docker` CLI are **out of v1 scope** (no
daemon to back them); revisit if/when upstream native-K8s (PR #1000) lands.
**Tool caveat:** Google **archived Kaniko in June 2025**, so v1 does not hard-pin
Kaniko -- the builder is pluggable and the build ADR selects a maintained option.
**Prior spike finding (still true):** act_runner has NO native Kubernetes
step-execution mode -- only Docker and host backends. Native-K8s is only WIP PR #1000
(unmerged, "Proposed", no timeline). Gitea's "rootless" DinD example STILL sets
`privileged: true`, so rootless DinD is NOT an unprivileged/Autopilot path -- which is
why daemonless builders, not rootless DinD, are the answer.
**Status:** locked (revised). No longer needs privileged arch-review; instead the
build ADR (garc-7ft.5) now also owns builder selection + the `services:`/CLI
scope-cut.

## D7 -- Build our own vs adopt Gitea Enterprise ARC
**Decision:** Build our own operator.
**Reasoning:** Gitea Enterprise ships an official Actions Runner Controller, but it
is Enterprise-only (Gitea >23.8.0) and DinD-per-pod (not true ephemeral-per-job),
and gives us no portability / build-cache / GKE story. We differentiate:
ephemeral-per-job isolation, portability across distros, v1 build-cache, GKE
target, Community-compatible. Spec carries a "Related work" section making the
differentiation explicit.

## D8 -- Upstream native-K8s PR (#1000) -- watch item
**Decision:** Not a v1 dependency; track it.
**Reasoning:** gitea/runner PR #1000 ("Native Kubernetes Job Support") would, if
merged, let runners schedule step-pods natively and avoid privileged DinD (the
eventual Autopilot lever). It is unreviewed/unmerged with no timeline, so v1 cannot
depend on it. Spec records it as a forward-looking watch item.

## D3 -- v1 platform target
**Decision (REVISED 2026-06-29):** **GKE Autopilot for v1.** GKE Standard no longer
required.
**Reasoning:** This is now unblocked precisely because D2 dropped DinD. With no
privileged container, there is no WorkloadAllowlist eligibility gate -- the operator's
pods (controller, listener, unprivileged runner, daemonless builder) all admit on
Autopilot with a standard restricted securityContext (`runAsNonRoot`, `drop: [ALL]`,
`allowPrivilegeEscalation: false`, `seccompProfile: RuntimeDefault`). Autopilot was the
user's preferred target; removing privileged makes it the v1 default rather than a
later gated port.
**Superseded reasoning:** the old Standard-first call existed ONLY to host privileged
DinD. With DinD gone, that rationale is void.

## D4 -- Control loop architecture
**Decision:** Poll Gitea's admin jobs queue to derive demand; warm-pool floor as
optimization. Spike CONFIRMS details (not discovers the mechanism).
**Reasoning:** Gitea's backend is runner-pull, not operator-push -- no "job
available" push (ARC's GitHub mechanism has no Gitea equivalent). BUT (corrected
after Gitea Enterprise ARC research) Gitea's `GET /admin/actions/jobs?status=waiting`
admin endpoint DOES expose waiting jobs with label filters (gitea #32862, built for
KEDA). The earlier "#35134 hides waiting tasks" concern applies only to
public/repo-scoped routes, not the admin route. The convergent design across Gitea
Enterprise ARC, rustunit/gitea-ci-autoscaler, and the KEDA path is: admin-token
operator polls the queue, computes a desired count, writes `status.targetSize`,
reconciles pods. Spike (garc-7ft.1) verifies API behavior on the target version,
poll interval, label-filter fidelity, and the floor heuristic. Caveat (maintainer
lunny, #32862): admin-token polling suits a single internal instance, not
multi-tenant public hosting -- documented as a scope limitation.

## D5 -- Interpretation of "one pod per job"
**Decision:** Ephemeral-per-job is the real intent.
**Reasoning:** The guarantee that matters is: each job runs in a fresh, isolated,
ephemeral pod (one job, then teardown). Whether that pod is provisioned reactively
per-job or pulled from a small warm pool of ephemeral runners is an implementation
detail the spec may choose for robustness, given Gitea's pull-based API. Isolation
guarantee kept; exact provisioning moment relaxed.

## D6 -- Build cache
**Decision:** Solve in v1 (accepted as larger v1 scope).
**Reasoning:** A fresh DinD sidecar per ephemeral job has an empty layer cache
every run -> cold `docker build` every job. For a CI runner that builds images,
that is a first-class usability problem, not a footnote. v1 ships a caching
mechanism (shared cache volume or registry pull-through mirror; mechanism TBD in
design).

## Gates / arch-review flags
- Privileged-workload security posture (DinD): `needs-arch-review`.
- Control-loop architecture choice: `needs-arch-review` + spike-gated.
- GKE Autopilot WorkloadAllowlist eligibility (only if/when Autopilot port is
  pursued): `gate=needs-human` (outward-facing org-policy + procurement).

## Arch review outcome (2026-06-30)
ADRs 0003-0007 taken through a batch architecture review (6 reviewers: one per ADR plus
a cross-ADR consistency pass). Outcome:
- **0003 (CRD hierarchy), 0004 (resource policy), 0006 (credential model), 0007 (scaling
  + listener): RATIFY-WITH-NITS.** All architectures approved. Merge-condition nits
  applied 2026-06-30 (SPEC drift sync for the `GITEA_RUNNER_EPHEMERAL` env var -- it DOES
  exist, verified vs upstream source -- and the ~10-min `ZOMBIE_TASK_TIMEOUT` crash timer
  + no-cancel-API; 0003 finalizer + 0006/0007 credential wording moved to the org-scoped
  default with admin as fallback; 0004 Autopilot CPU:mem ratio-clamp + `limits==requests`
  premise + `activeDeadlineSeconds` ownership; 0007 dead `inFlight` term removed, OQ1
  recast as confirm-empirically). patchID handshake confirmed consistent across 0003/0007.
  These four are ready to move Proposed -> Accepted pending human ratification.
- **0005 (image build strategy): SEND-BACK.** Design direction sound (BuildKit-rootless
  primary, registry cache, fail-loud shim) but Decision 1's Autopilot admission claim is
  factually wrong: rootless BuildKit needs `seccomp: Unconfined` +
  `--oci-worker-no-process-sandbox` (NOT `RuntimeDefault`), and `procMount: Unmasked` is
  admission-rejected on Autopilot >=1.33. This is a narrowed security-posture change (one
  unprivileged-but-not-fully-restricted build pod) now reflected in SPEC sec. 6.6/9.
  Requires a Decision 1 rewrite + a live Autopilot admit+build probe before ratification.
  Tracked on garc-7ft.5.
