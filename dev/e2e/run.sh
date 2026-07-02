#!/usr/bin/env bash
# End-to-end proof for the garc operator: stand up a kind cluster + Gitea, deploy the
# operator, trigger a workflow, and assert the full ephemeral-runner lifecycle -- runners
# scale up, the job succeeds, and everything tears down to zero (pods, CRs, and Gitea
# runner rows). This is the CI form of the manual dev flow in dev/gitea/README.md.
#
# Idempotent and non-interactive: safe to re-run locally, and self-contained enough to run
# on a fresh GitHub Actions ubuntu runner. Requires: kind, kubectl, helm, docker, curl,
# python3, and (for the image) the repo's Makefile docker-build/docker-load targets.
#
# Usage:   dev/e2e/run.sh
# Env:
#   CLUSTER   kind cluster name           (default: garc-dev)
#   KEEP      1 = do not delete the cluster on exit (default: unset -> cleaned up in CI)
#   TIMEOUT   per-phase wait budget, secs  (default: 300)
set -euo pipefail

CLUSTER="${CLUSTER:-garc-dev}"
CTX="kind-${CLUSTER}"
TIMEOUT="${TIMEOUT:-300}"
REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
GITEA_CHART_VERSION="12.6.0"          # appVersion 1.26.1 -- the garc pinned target
ORG="garc-dev"
GH_REPO="hello-actions"
LOCAL_PORT="${LOCAL_PORT:-3000}"
API="http://localhost:${LOCAL_PORT}/api/v1"
ADMIN_USER="gitea_admin"
ADMIN_PASS="gitea_admin_pw_dev"
CONTROLLER_NS="gitea-actions-controller"

log()  { printf '\n[e2e] %s\n' "$*" >&2; }
fail() { printf '\n[e2e][FAIL] %s\n' "$*" >&2; dump_diagnostics; exit 1; }

PF_PID=""
cleanup() {
  local ec=$?
  [ -n "$PF_PID" ] && kill "$PF_PID" 2>/dev/null || true
  if [ "${KEEP:-0}" != "1" ]; then
    log "deleting kind cluster ${CLUSTER}"
    kind delete cluster --name "${CLUSTER}" >/dev/null 2>&1 || true
  else
    log "KEEP=1 set; leaving cluster ${CLUSTER} up"
  fi
  exit "$ec"
}
trap cleanup EXIT

dump_diagnostics() {
  log "--- diagnostics ---"
  kubectl --context "$CTX" get pods -A 2>&1 | grep -iE "gitea|runner" >&2 || true
  kubectl --context "$CTX" -n "$CONTROLLER_NS" logs deploy/gitea-runner-controller --tail=40 2>&1 >&2 || true
  kubectl --context "$CTX" -n "$CONTROLLER_NS" logs deploy/gitea-listener --tail=20 2>&1 >&2 || true
}

# Poll `cmd` until it prints the expected value or the budget runs out.
# wait_for <description> <expected> <interval> <cmd...>
wait_for() {
  local desc="$1" expected="$2" interval="$3"; shift 3
  local deadline=$(( SECONDS + TIMEOUT )) got
  while [ "$SECONDS" -lt "$deadline" ]; do
    got="$("$@" 2>/dev/null || true)"
    if [ "$got" = "$expected" ]; then
      log "OK: ${desc} (=${expected})"
      return 0
    fi
    sleep "$interval"
  done
  fail "timed out waiting for ${desc} (wanted ${expected}, last saw '${got:-}')"
}

kctl() { kubectl --context "$CTX" "$@"; }

# ---- current-state probes (used by wait_for) ----
runner_rows()  { curl -s -H "Authorization: token ${TOKEN}" "${API}/orgs/${ORG}/actions/runners" \
                   | python3 -c "import sys,json;d=json.load(sys.stdin);print(len(d if isinstance(d,list) else d.get('runners',[])))"; }
runner_pods()  { kctl -n gitea-runners get pods --no-headers 2>/dev/null | grep -c . || echo 0; }
runner_crs()   { kctl get ephemeralrunners -A --no-headers 2>/dev/null | grep -c . || echo 0; }

# ================================================================= cluster
if ! kind get clusters 2>/dev/null | grep -qx "${CLUSTER}"; then
  log "creating kind cluster ${CLUSTER}"
  kind create cluster --name "${CLUSTER}" --wait 120s
else
  log "reusing existing kind cluster ${CLUSTER}"
fi

# ================================================================= Gitea
log "installing Gitea (chart ${GITEA_CHART_VERSION}, appVersion 1.26.1)"
helm repo add gitea-charts https://dl.gitea.com/charts/ >/dev/null 2>&1 || true
helm repo update gitea-charts >/dev/null
helm --kube-context "$CTX" upgrade --install gitea gitea-charts/gitea \
  --version "${GITEA_CHART_VERSION}" -n gitea --create-namespace \
  -f "${REPO_ROOT}/dev/gitea/values.yaml" --wait --timeout "${TIMEOUT}s"
kctl -n gitea wait --for=condition=ready pod \
  -l app.kubernetes.io/name=gitea --timeout "${TIMEOUT}s"

# ================================================================= seed
log "seeding org + token + workflow repo"
CTX="$CTX" "${REPO_ROOT}/dev/gitea/seed.sh"
TOKEN="$(cat "${REPO_ROOT}/dev/gitea/.org-token")"
[ -n "$TOKEN" ] || fail "seed produced no org token"

# port-forward for the rest of the run (assertions talk to the Gitea API locally)
kctl -n gitea port-forward svc/gitea-http "${LOCAL_PORT}:3000" >/tmp/garc-e2e-pf.log 2>&1 &
PF_PID=$!
for _ in $(seq 1 30); do curl -sf "${API}/version" >/dev/null 2>&1 && break; sleep 1; done

# ================================================================= operator
log "building + loading controller image"
# SKIP_IMAGE_BUILD=1 lets CI build the image its own way (plain docker, no mise/zsh)
# and load it before calling this script. Locally we chain the Makefile target.
if [ "${SKIP_IMAGE_BUILD:-0}" != "1" ]; then
  make -C "${REPO_ROOT}" docker-load >/dev/null
fi

log "applying CRDs + operator + listener"
kctl apply -f "${REPO_ROOT}/config/crd/"
kctl apply -f "${REPO_ROOT}/config/manager/manager.yaml"
kctl apply -f "${REPO_ROOT}/config/manager/listener-deployment.yaml"

# Populate the teardown credential with the real org token (manifest ships a placeholder).
log "populating teardown credential secret"
kctl -n "$CONTROLLER_NS" create secret generic gitea-teardown-credential \
  --from-literal=token="${TOKEN}" --dry-run=client -o yaml | kctl apply -f -
kctl -n "$CONTROLLER_NS" rollout restart deploy/gitea-runner-controller deploy/gitea-listener
kctl -n "$CONTROLLER_NS" rollout status deploy/gitea-runner-controller --timeout "${TIMEOUT}s"
kctl -n "$CONTROLLER_NS" rollout status deploy/gitea-listener --timeout "${TIMEOUT}s"

# ================================================================= runner set
log "creating GiteaRunnerSet + registration credential"
kctl create namespace gitea-runners --dry-run=client -o yaml | kctl apply -f -
kctl -n gitea-runners create serviceaccount gitea-runner \
  --dry-run=client -o yaml | kctl apply -f -
kctl -n gitea-runners create secret generic gitea-runner-creds \
  --from-literal=token="${TOKEN}" --dry-run=client -o yaml | kctl apply -f -
cat <<YAML | kctl apply -f -
apiVersion: giteaactions.blackrabbit.dev/v1alpha1
kind: GiteaRunnerSet
metadata:
  name: test-set
  namespace: gitea-runners
spec:
  giteaConfigUrl: http://gitea-http.gitea.svc.cluster.local:3000
  giteaConfigSecretRef:
    name: gitea-runner-creds
    key: token
  orgName: ${ORG}
  runnerScope: org
  minRunners: 0
  maxRunners: 10
  labels: ["ubuntu-latest"]
  template:
    spec:
      serviceAccountName: gitea-runner
      restartPolicy: Never
      containers:
        - name: act-runner
          image: gitea/act_runner:0.2.13
YAML

# baseline must be clean before we trigger
wait_for "baseline runner rows == 0" 0 2 runner_rows

# ================================================================= trigger
log "triggering a workflow run (push a commit)"
STAMP="e2e-$(date +%s 2>/dev/null || echo run)"
CONTENT="$(printf '%s' "$STAMP" | base64)"
# create-or-update a tracking file on main -> fires the push-triggered ci workflow
EXIST_SHA="$(curl -s -u "${ADMIN_USER}:${ADMIN_PASS}" \
  "${API}/repos/${ORG}/${GH_REPO}/contents/e2e.txt?ref=main" \
  | python3 -c "import sys,json;print(json.load(sys.stdin).get('sha',''))" 2>/dev/null || true)"
BODY="{\"content\":\"${CONTENT}\",\"message\":\"e2e trigger ${STAMP}\",\"branch\":\"main\""
[ -n "$EXIST_SHA" ] && BODY="${BODY},\"sha\":\"${EXIST_SHA}\""
BODY="${BODY}}"
METHOD=POST; [ -n "$EXIST_SHA" ] && METHOD=PUT
curl -sf -u "${ADMIN_USER}:${ADMIN_PASS}" -X "$METHOD" \
  -H 'Content-Type: application/json' \
  "${API}/repos/${ORG}/${GH_REPO}/contents/e2e.txt" -d "$BODY" >/dev/null \
  || fail "failed to push trigger commit"

# ================================================================= assertions
# 1. scale-up: at least one runner registers in Gitea
log "asserting scale-up"
deadline=$(( SECONDS + TIMEOUT )); peak=0
while [ "$SECONDS" -lt "$deadline" ]; do
  n="$(runner_rows)"; [ "${n:-0}" -gt "$peak" ] && peak="$n"
  [ "${peak:-0}" -ge 1 ] && break
  sleep 2
done
[ "${peak:-0}" -ge 1 ] || fail "no runner ever registered (scale-up failed)"
log "OK: scaled up (peak rows=${peak})"

# 2. teardown: everything returns to zero (pods, CRs, Gitea rows)
log "asserting graceful teardown to zero"
wait_for "runner rows drain to 0" 0 3 runner_rows
wait_for "runner pods drain to 0"  0 3 runner_pods
wait_for "ephemeralrunner CRs drain to 0" 0 3 runner_crs

# 3. the job actually succeeded (not orphaned mid-flight)
log "asserting job success"
STATUS="$(curl -s -u "${ADMIN_USER}:${ADMIN_PASS}" \
  "${API}/repos/${ORG}/${GH_REPO}/actions/tasks" | python3 -c "
import sys,json
d=json.load(sys.stdin); tasks=d.get('workflow_runs') or d.get('tasks') or []
lr=max((t.get('run_number',0) for t in tasks), default=0)
recent=[t for t in tasks if t.get('run_number')==lr]
print('ok' if recent and all(t.get('status')=='success' for t in recent) else 'bad')
")"
[ "$STATUS" = "ok" ] || fail "latest workflow run did not all succeed"
log "OK: latest workflow run succeeded"

# 4. no controller errors during the run
log "checking controller logs for errors"
ERRS="$(kctl -n "$CONTROLLER_NS" logs deploy/gitea-runner-controller --tail=500 2>/dev/null \
  | grep -cE 'the object has been modified|Reconciler error' || true)"
[ "${ERRS:-0}" -eq 0 ] || fail "controller logged ${ERRS} reconcile error(s)"
log "OK: no reconcile errors"

log "E2E PASSED: scale-up -> job success -> graceful teardown -> zero, no errors"
