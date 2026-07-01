# ADR 0006: Credential model (operator token, per-pod registration token, teardown)

## Status

Accepted (2026-06-30, batch arch review; ratify-with-nits, nits applied)

UPDATE 2026-06-30 (spike garc-3bk RESOLVED): the org-scoped path is now proven
end-to-end against live Gitea 1.26.1. A fully org-scoped operator needs **zero admin
scope**: the listener reads `GET /api/v1/orgs/{org}/actions/jobs?status=queued`
(`read:organization`), and the teardown controller deletes via
`DELETE /api/v1/orgs/{org}/actions/runners/{id}` (`write:organization`, returns 204 --
no `write:admin`). Org-scoped ephemeral runners pick up jobs and auto-delete on
graceful completion identically to instance-scoped ones. The `read:admin`/`write:admin`
model below is now the **fallback for whole-instance / multi-org deployments**; the
**org-scoped model is the recommended default**. Decision section 1 updated to reflect
this.

bd issue: garc-7ft.6. Blocks the walking skeleton (garc-7ft.7).

Related ADRs: CRD hierarchy (0003), resource policy (0004), build strategy (0005).
Source spec: `docs/product/gitea-actions-operator/SPEC.md` sections 6.3 (credential
management) and 6.4 (teardown). Decision log: `docs/product/gitea-actions-operator/DECISIONS.md`.

## Context

The operator runs ephemeral act_runner pods on Kubernetes against a single Gitea
instance. Three distinct trust boundaries need credentials, and they have very
different blast radii:

1. The **demand listener** -- the most exposed long-running component -- must read
   Gitea's admin jobs queue (`GET /api/v1/admin/actions/jobs?status=queued`) to derive
   how many runners each pool wants.
2. The **reconcile/teardown controller** must deregister orphaned runner rows left
   behind when a runner pod is SIGKILL'd mid-job (the crash path), via
   `DELETE /api/v1/admin/actions/runners/{id}`.
3. Each **runner pod** must register itself with Gitea so its act_runner can poll for
   and execute exactly one task, then exit.

Because runner pods execute untrusted job code (CI workflows authored by developers),
the cardinal rule is that **no high-value Gitea credential may ever enter a runner
pod**. A leaked operator admin token would compromise the whole Gitea instance; a
leaked per-pod registration token must be worth as little as possible.

This ADR was written after a live validation session against **Gitea 1.26.1 +
act_runner 0.2.13**. The findings below are confirmed against that live target, not
assumed; they correct several earlier assumptions and are the basis for the decision.

### Live-confirmed findings (Gitea 1.26.1, act_runner 0.2.13)

- **Scope split on the admin Actions API is real and asymmetric.**
  - Reading the demand queue (`GET /api/v1/admin/actions/jobs?status=queued`) needs
    only **`read:admin`**.
  - Deregistering a runner (`DELETE /api/v1/admin/actions/runners/{id}`) needs
    **`write:admin`** -- the call returns **HTTP 403** without it.
  - Runners are listed at `/api/v1/admin/actions/runners` (not `/admin/runners`).
- **`write:admin` is the single most powerful Gitea scope** (it grants the full admin
  API surface). In this design it is needed for exactly **one** operation: deleting an
  orphaned runner row after a runner pod crash. Granting full admin to do one DELETE is
  a security concern we call out explicitly below.
- **An org-scoped runners route exists.** `GET /api/v1/orgs/{org}/actions/runners`
  returned **HTTP 200**. This suggests that if the operator registered runners as
  **org-scoped** rather than instance-scoped, deregistration might be possible under
  **`write:organization`** -- a far smaller blast radius than `write:admin`. We did
  **not** fully prove the org-scoped DELETE path in this session: our stray rows were
  instance-scoped, so the org-scoped DELETE returned **HTTP 404 "No permission to
  access this runner."** This is recorded as the key open spike below.
- **Gitea registration tokens are reusable, not single-use, and are NOT API bearer
  tokens.** A registration token returns **HTTP 401 against every REST route** -- it
  authenticates only `act_runner register`. Isolation therefore cannot rest on token
  single-use.
- **Ephemeral mode is a registration-time flag.** The runner is launched as
  `act_runner register --ephemeral`. In the standard `gitea/act_runner` image the env var
  **`GITEA_RUNNER_EPHEMERAL=1` is the documented enabler** -- the image entrypoint maps it
  to `register --ephemeral` (verified in upstream `run.sh`). It is distinct from
  `daemon --once` (mapped from `GITEA_RUNNER_ONCE`), which only runs one job then exits but
  does NOT mark the runner ephemeral server-side. Only `--ephemeral` produces a row with
  `ephemeral: true` and the self-exit-plus-auto-delete behavior. (Either the flag or the
  env var works; our initial probe bypassed the entrypoint and so missed the env var.)
- **Graceful ephemeral completion auto-deletes the runner row server-side** (Gitea's
  `CleanupEphemeralRunners`, observed firing). No operator API call is needed on the
  happy path.
- **The crash path is the only residual.** A SIGKILL'd ephemeral runner leaves its row
  orphaned as `online`/`ephemeral:true`, and its task stuck `in_progress` until Gitea's
  server-side zombie reaper clears it (`[actions] ZOMBIE_TASK_TIMEOUT`, default ~10 min --
  the workflow/runner timeouts do not apply once the runner is dead). Gitea does not
  detect the dead runner faster than that. The teardown DELETE (org-scoped, or
  `write:admin` in the fallback) exists to deregister the orphaned row; there is no
  task-cancel call (see Open questions 2).
- **Gitea has no JIT-config API.** GitHub ARC injects a just-in-time runner config that
  is single-use by construction; Gitea offers no equivalent, so we must inject a
  reusable registration token instead and rely on a different isolation mechanism.

## Decision

### 1. Privilege split: separate credentials for read and for teardown (headline)

The operator holds **two distinct Gitea credentials**, never one combined token, and
each is scoped as narrowly as the deployment allows. There are two supported scoping
tiers; **org-scoped is the recommended default** (spike garc-3bk, live-confirmed):

**Recommended -- org-scoped (no admin scope at all):**
- **Listener credential -- `read:organization`.** Reads the demand queue via
  `GET /api/v1/orgs/{org}/actions/jobs?status=queued` (live-confirmed: 200, status
  filter honored, `labels[]` populated, `X-Total-Count` present, results correctly
  scoped to the org's repos). Can read that org's queue and nothing more. The listener's
  topology and ownership (one per `GiteaRunnerSet`, owner-referenced, stateless) are
  specified in ADR 0007; this Secret is mounted only into that listener pod.
- **Teardown credential -- `write:organization`.** The reconcile/teardown controller
  deletes orphaned rows via `DELETE /api/v1/orgs/{org}/actions/runners/{id}`
  (live-confirmed: **204** with a `write:organization` token, no `write:admin`).
  Registration tokens come from `POST /api/v1/orgs/{org}/actions/runners/registration-token`
  under the same scope.
- One credential pair **per managed org**. This is the least-privilege default.

**Fallback -- instance/admin-scoped (whole-instance or multi-org deployments):**
- **Listener credential -- `read:admin`** for `GET /api/v1/admin/actions/jobs`.
- **Teardown credential -- `write:admin`** for `DELETE /api/v1/admin/actions/runners/{id}`
  (live-confirmed: 403 without `write:admin`).
- Use only when one operator must span the whole instance or many orgs and per-org
  credentials are impractical. `write:admin` is the single most powerful Gitea scope,
  so prefer the org-scoped tier wherever the deployment is org-bounded.

**Runner pods hold neither** admin nor org credentials -- only a per-pod registration
token (section 2).

Each credential lives in its **own Kubernetes Secret**, mounted only into the
component that needs it, so a compromise of the network-exposed listener does not yield
write access. This is the operator-side analogue of ARC's "the high-value credential
never touches the runner pod" pattern.

The runner scope (`spec.runnerScope` on GiteaRunnerSet, ADR 0003) and the credential
tier must agree: an org-scoped GiteaRunnerSet uses the org credential pair; an
instance-scoped one uses the admin pair.

### 2. Per-pod registration token, owner-ref'd, injected via env

Each runner pod gets its **own per-pod Kubernetes Secret** containing a registration
token at the configured scope:

- The Secret carries an **`ownerReference` to the pod / EphemeralRunner** so it is
  garbage-collected with the workload (see teardown below).
- The token is injected into the act_runner container via **`secretKeyRef` / `envFrom`**
  (an environment variable) rather than a mounted volume.

**Env vs mounted volume.** Both keep the token in a per-pod Secret with owner-ref GC;
the difference is delivery. A mounted volume offers in-place rotation, but per-pod
tokens are single-use-per-pod and never rotated in place (a fresh pod always gets a
fresh token), so the rotation benefit is moot here. Env injection via `secretKeyRef`
is the simplest path: it matches how `act_runner register` already reads its token,
needs no volume/mount plumbing, and keeps the pod template minimal. **We recommend env
injection.** (If a future hardening pass wants the token off the process environment --
e.g. to avoid it appearing in `/proc/<pid>/environ` -- a mounted file is the fallback;
the per-pod Secret shape does not change.)

The pod's act_runner is launched as **`act_runner register --ephemeral`** -- the
registration-time flag (equivalently `GITEA_RUNNER_EPHEMERAL=1` via the image entrypoint),
distinct from the weaker `daemon --once` / `GITEA_RUNNER_ONCE`.

### 3. Isolation rests on `--ephemeral` plus revoke-on-claim, not on token single-use

Because Gitea registration tokens are **reusable** and are **not** API bearer tokens,
the isolation guarantee does **not** come from the token being single-use. It comes
from:

1. The runner registering with **`--ephemeral`**, so Gitea issues a runner identity
   that accepts exactly **one** task.
2. Gitea **revoking that runner's polling credential when the single task is claimed**,
   so the runner cannot fetch further work before untrusted job code runs. (Evidentiary
   basis: this is *inferred* from the `--ephemeral` contract and the observed self-exit +
   auto-delete behavior, NOT directly probed -- our live session confirmed one-task-then-
   exit + auto-delete but did not separately attempt a second `FetchTask` on a claimed
   ephemeral runner. It is the one load-bearing isolation claim not independently
   verified; R-CRED-4 tracks the re-probe, and it should be confirmed against the upstream
   `CleanupEphemeralRunners` / task-claim code path or a direct second-fetch probe before
   GA.)
3. The runner **self-exiting** after the one task, and Gitea **auto-deleting** the
   ephemeral row on graceful completion.

The registration token itself, even if exfiltrated by job code, is not a bearer token
(401s against every REST route) and cannot read or mutate Gitea data -- it only lets the
holder register more runners **at the token's scope**. Its residual value is therefore
**scope-dependent**: at **org/repo** scope it is genuinely low (a rogue runner can only
poach jobs within that org/repo, which the recommended org-scoped default keeps small);
at **instance** scope it is non-trivial (a rogue instance-scoped runner can be labeled to
poach jobs from any org/repo on the instance and thereby execute in -- or exfiltrate
secrets from -- workflows instance-wide, see R-CRED-3). This is a weaker primitive than
GitHub ARC's single-use JIT config, but Gitea has **no JIT-config API**, so injecting the
narrowest-scoped registration token that works plus relying on `--ephemeral` is the best
available equivalent -- and is the direct reason to prefer the org/repo scope over
instance scope.

### 4. Teardown and garbage collection

- **Per-pod Secret GC:** the registration-token Secret is owner-ref'd to the pod /
  EphemeralRunner, so it is **deleted together with the pod**. The token does not
  outlive the workload.
- **Happy path needs no operator call:** graceful ephemeral completion makes Gitea
  **auto-delete the runner row** server-side. The teardown controller does nothing on
  this path.
- **Crash path drives the teardown write scope:** a SIGKILL'd runner leaves an orphaned
  `online`/`ephemeral:true` row plus a task stuck `in_progress`. The reconcile sweep (the
  dual finalizer's first stage; SPEC 6.4) must (a) **deregister** orphaned ephemeral rows
  whose pod is gone -- via the org-scoped DELETE (`write:organization`, recommended) or
  the admin DELETE (`write:admin`, fallback); and (b) **surface** the stuck task in
  status/metrics. It **cannot cancel** the task: there is no Actions cancel API
  (garc-i5b), so the task waits for Gitea's zombie reaper (`ZOMBIE_TASK_TIMEOUT`, ~10 min
  default). The teardown write scope exists for the deregister, not for any cancel call.
- **High-value credentials never enter a runner pod** -- only the scoped, short-lived,
  low-value registration token does.

### 5. Rotation

- **Per-pod tokens are never rotated in place.** Every pod gets a fresh registration
  token at creation, and there is nothing long-lived in a pod to rotate. Pod lifecycle
  is the rotation mechanism.
- **The operator's own Gitea credentials** -- the listener read token and the teardown
  write token, in whichever tier the deployment uses (`read:organization` /
  `write:organization` by default, `read:admin` / `write:admin` in the whole-instance
  fallback) -- are **rotated out-of-band** via an external secret store (e.g. External
  Secrets Operator / a vault) or manual rotation, and this procedure is documented for
  the security/compliance persona (Dana). The operator reads each from its Secret on each
  use, so an external rotation of the underlying Secret is picked up without a code
  change.

## Consequences

### Positive

- **Least privilege by component.** The continuously-exposed listener holds only the
  read credential (`read:organization` by default, `read:admin` in the fallback tier);
  write scope is never co-located with the network-facing poller. A listener compromise
  cannot delete runners or mutate Gitea.
- **No high-value credential in untrusted pods.** Runner pods, which execute untrusted
  CI code, hold only a low-value reusable registration token that cannot call the REST
  API.
- **Clean GC.** Owner-ref'd per-pod Secrets disappear with their pods; no leaked
  long-lived tokens accumulate in the cluster.
- **Happy path is operator-free for credentials.** Graceful ephemeral completion
  auto-deletes rows, so the privileged DELETE is exercised only on the rare crash path.
- **Rotation is mostly free.** Pod churn rotates per-pod tokens automatically; only two
  operator credentials need an out-of-band rotation story.
- **Simplest viable injection.** Env via `secretKeyRef` matches act_runner's existing
  token-reading behavior and keeps the pod template minimal.

### Negative

- **Two operator Secrets to provision and manage** instead of one combined admin token.
  Priya (platform engineer) must mint and install two scoped credentials, and document
  both in the rotation runbook.
- **The registration token sits in the container environment** (`/proc/<pid>/environ`),
  reachable by job code. Mitigated by the token being low value (not a bearer token,
  single-pod, owner-ref GC'd), but it is not zero-exposure; a mounted-file variant is
  the fallback if this is unacceptable to a given deployment.
- **No JIT-config strength.** We cannot match GitHub ARC's single-use config; isolation
  depends on Gitea's `--ephemeral` + revoke-on-claim behaving as observed on 1.26.1.

### Risks

- **R-CRED-1: `write:admin` is over-broad.** Granting the most powerful Gitea scope to
  delete one orphaned row is a large blast radius for a single operation. If the
  teardown controller's Secret leaks, the attacker has full Gitea admin. Mitigation:
  isolate the credential to the teardown controller only; pursue the org-scoped spike
  (Open questions) to replace `write:admin` with `write:organization`; document the
  scope prominently for Dana.
- **R-CRED-2: Gitea API churn across versions.** The Actions API is new and changed
  between 1.24/1.25/1.26 (this session already corrected `queued` vs `waiting` and the
  `--ephemeral` flag). The exact scope requirements (`read:admin` for the queue,
  `write:admin` for DELETE) are pinned to 1.26.1 and must be **re-probed on Gitea
  upgrade**. Mitigation: isolate Gitea API access behind one client package; pin tested
  versions.
- **R-CRED-3: registration-token exfiltration.** Job code can read the env var. Residual
  value is low (see above), but a token at instance scope could be used to register
  rogue runners. Mitigation: prefer the narrowest registration scope that works
  (org/repo over instance where possible); the org-scoped spike also tightens this.
- **R-CRED-4: dependence on revoke-on-claim.** Isolation rests on Gitea revoking the
  polling credential when the single task is claimed. If a future Gitea version changes
  that behavior, an ephemeral runner could in principle fetch a second task. Mitigation:
  pin and re-probe; `restartPolicy: Never` and controller-driven teardown bound the
  window regardless.

## Open questions

1. **RESOLVED (spike garc-3bk, 2026-06-30): deregistration under `write:organization`
   works.** Proven end-to-end on live Gitea 1.26.1: an org-scoped ephemeral runner
   (registered via the org registration-token endpoint) is deleted by
   `DELETE /api/v1/orgs/{org}/actions/runners/{id}` returning **204** under a
   `write:organization` token -- no `write:admin`. It picks up org/repo jobs and
   auto-deletes on graceful completion identically to instance-scoped runners. The
   listener also drops to `read:organization` via
   `GET /api/v1/orgs/{org}/actions/jobs?status=queued`. **A fully org-scoped operator
   needs zero admin scope** -- now the recommended default (Decision section 1);
   `write:admin` remains only as the whole-instance/multi-org fallback.
2. **RESOLVED (spike garc-i5b, 2026-06-30): there is no stuck-task cancellation API.**
   Gitea 1.26.1 has no Actions cancel route. Live-tested against a crashed runner:
   deleting the runner row (204) leaves the task `in_progress`; `DELETE` run -> 400 "not
   done"; `rerun` -> 400 "not done"; a fresh same-label runner does not re-pick the task.
   A crashed `in_progress` task is reaped **only** by Gitea's server-side zombie reaper
   (`[actions] ZOMBIE_TASK_TIMEOUT`, default ~10 min; source `modules/setting/actions.go`).
   The workflow `jobs.<id>.timeout-minutes` and act_runner's own 3h timeout do NOT apply
   to a crash -- they are enforced by the live runner, which is gone. So the operator
   **cannot** cancel the task -- it can only deregister the orphaned runner (org-scoped
   DELETE, 204) and surface the stuck task in status/metrics. No new credential scope is
   required for cancellation because there is no cancellation call. Mitigation: recommend
   a low instance `ZOMBIE_TASK_TIMEOUT` (the correct lever for crash reaping, not the
   workflow timeout). Upstream watch: go-gitea PR #35382 adds `POST /actions/runs/{run}/cancel`
   in milestone 1.28.0 (open, not in 1.26.1); if/when it ships, the cancel scope (likely
   the same org/repo write scope) should be re-probed. See ADR 0003 reconcile-sweep
   section.
3. **Registration scope default - RESOLVED: org.** Following OQ1 (org-scoped path proven
   end-to-end, garc-3bk) and Decision section 1, v1 defaults registration to **org
   scope**, to agree with the recommended org-scoped credential tier and to keep the
   token-exfiltration blast radius small (Decision section 3: org-scope residual is low,
   instance-scope is not). The instance/admin tier remains the fallback for
   whole-instance/multi-org deployments. The genuinely-open remainder is narrower:
   **repo-scope support** -- whether to offer an even-tighter repo registration scope as
   an option -- which is deferred, not required for v1.
4. **Env vs mounted-file for high-security deployments.** Env injection is the v1
   recommendation; should we ship a mounted-file option for deployments that forbid
   secrets in the process environment, or defer it until requested?
5. **Cache/image-registry credential injection into the build pod (unowned seam).** ADR
   0005 (build strategy) defers to this ADR the credential path for injecting
   registry auth into the unprivileged build pod so it can push images and read/write
   layer cache (`--cache-to`/`--cache-from`). This ADR does **not** currently cover that
   -- its scope is the operator's read/teardown creds and the per-pod *registration*
   token, none of which is a registry credential. Registry auth for `docker build`/push
   is arguably a workflow-secret concern (Gitea Actions secrets provided to the job), not
   an operator-credential-model concern -- which is likely why it was omitted. Decide the
   owner: either add a "registry/cache credential" subsection here, or redirect 0005's
   pointer to the workflow-secrets mechanism. This must be settled before the walking
   skeleton (garc-7ft.7) can push cache end-to-end. Flagged by the cross-ADR review.

## References

- `docs/product/gitea-actions-operator/SPEC.md` -- section 6.3 (credential management),
  section 6.4 (error recovery and teardown), section 4 (Gitea pull-based constraint).
- `docs/product/gitea-actions-operator/DECISIONS.md` -- D4 (control loop / admin-poll),
  D5 (ephemeral-per-job intent).
- ADR 0003 -- CRD hierarchy (EphemeralRunner / per-pod Secret ownership).
- ADR 0004 -- resource policy.
- ADR 0005 -- build strategy.
- ADR 0007 -- scaling algorithm and listener lifecycle (the listener that holds the read
  credential; its topology, ownership, and stateless restart behavior).
- bd issue garc-7ft.6 (this ADR); blocks garc-7ft.7 (walking skeleton).
- Live validation session, 2026-06-30: Gitea 1.26.1 + act_runner 0.2.13 (scope probes,
  `--ephemeral` behavior, registration-token 401 probe, org-route 200/DELETE 404).
- Gitea `CleanupEphemeralRunners` (PR #33570) -- server-side auto-delete of ephemeral
  rows on graceful completion.
- GitHub actions-runner-controller -- the per-pod JIT-token / blast-radius pattern this
  design adapts (Gitea has no JIT-config API; we inject a registration token instead).
