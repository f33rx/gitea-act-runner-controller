#!/usr/bin/env bash
# Seed the dev Gitea (garc-dev kind cluster) with an org, an org-scoped access token,
# and a repo carrying a sample Actions workflow -- the target for garc integration work.
#
# Idempotent + non-interactive: safe to re-run. Talks to the API over a port-forward
# the script sets up and tears down itself.
#
# Aligns with ratified ADR 0006 (org-scoped default): the token carries
# read+write:organization scope so both the demand listener (read) and the teardown
# controller (write) can use it against this org. Registration tokens for act_runner
# come from the org runners endpoint under the same scope.
#
# Usage:  dev/gitea/seed.sh
# Env overrides: NS, ADMIN_USER, ADMIN_PASS, ORG, REPO, TOKEN_NAME, LOCAL_PORT
set -euo pipefail

NS="${NS:-gitea}"
SVC="${SVC:-gitea-http}"
LOCAL_PORT="${LOCAL_PORT:-3000}"
ADMIN_USER="${ADMIN_USER:-gitea_admin}"
ADMIN_PASS="${ADMIN_PASS:-gitea_admin_pw_dev}"
ORG="${ORG:-garc-dev}"
REPO="${REPO:-hello-actions}"
TOKEN_NAME="${TOKEN_NAME:-garc-operator}"
API="http://localhost:${LOCAL_PORT}/api/v1"
KUBECTL="${KUBECTL:-kubectl}"
CTX="${CTX:-kind-garc-dev}"
OUT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
TOKEN_FILE="${OUT_DIR}/.org-token"     # gitignored; consumed by dev tooling

log() { printf '[seed] %s\n' "$*" >&2; }

# --- port-forward, auto-torn-down on exit ---
log "port-forwarding svc/${SVC} -> localhost:${LOCAL_PORT}"
"$KUBECTL" --context "$CTX" -n "$NS" port-forward "svc/${SVC}" "${LOCAL_PORT}:3000" \
  >/tmp/garc-gitea-seed-pf.log 2>&1 &
PF_PID=$!
cleanup() { kill "$PF_PID" 2>/dev/null || true; }
trap cleanup EXIT

# wait for the API to answer
for i in $(seq 1 30); do
  if curl -sf "${API}/version" >/dev/null 2>&1; then break; fi
  sleep 1
  [ "$i" = 30 ] && { log "ERROR: Gitea API did not come up on ${API}"; exit 1; }
done
log "Gitea $(curl -s "${API}/version" | sed 's/[{}\"]//g')"

AUTH=(-u "${ADMIN_USER}:${ADMIN_PASS}")
jqget() { python3 -c "import sys,json; d=json.load(sys.stdin); print(d.get('$1',''))"; }

# --- org (idempotent) ---
if curl -sf "${AUTH[@]}" "${API}/orgs/${ORG}" >/dev/null 2>&1; then
  log "org '${ORG}' already exists"
else
  log "creating org '${ORG}'"
  curl -sf "${AUTH[@]}" -X POST "${API}/orgs" \
    -H 'Content-Type: application/json' \
    -d "{\"username\":\"${ORG}\",\"visibility\":\"private\"}" >/dev/null
fi

# --- org-scoped access token ---
# Gitea tokens are user-scoped with named scopes; write:organization + read:organization
# on an admin who owns the org gives the operator org-level read+write on runners/jobs.
# Recreate cleanly so the scope set is deterministic (delete-if-exists, then create).
log "(re)creating access token '${TOKEN_NAME}' with read+write:organization"
curl -s "${AUTH[@]}" -X DELETE "${API}/users/${ADMIN_USER}/tokens/${TOKEN_NAME}" >/dev/null 2>&1 || true
TOKEN=$(curl -sf "${AUTH[@]}" -X POST "${API}/users/${ADMIN_USER}/tokens" \
  -H 'Content-Type: application/json' \
  -d "{\"name\":\"${TOKEN_NAME}\",\"scopes\":[\"read:organization\",\"write:organization\"]}" \
  | jqget sha1)
if [ -z "$TOKEN" ]; then log "ERROR: token creation returned no sha1"; exit 1; fi
umask 077
printf '%s' "$TOKEN" > "$TOKEN_FILE"
log "token written to ${TOKEN_FILE} (gitignored)"

# --- repo under the org, with a sample workflow (idempotent) ---
if curl -sf "${AUTH[@]}" "${API}/repos/${ORG}/${REPO}" >/dev/null 2>&1; then
  log "repo '${ORG}/${REPO}' already exists"
else
  log "creating repo '${ORG}/${REPO}'"
  curl -sf "${AUTH[@]}" -X POST "${API}/orgs/${ORG}/repos" \
    -H 'Content-Type: application/json' \
    -d "{\"name\":\"${REPO}\",\"auto_init\":true,\"private\":true,\"default_branch\":\"main\"}" \
    >/dev/null
fi

# sample workflow: a trivial job that just echoes, so a registered runner has something
# to claim. Base64-put via the contents API (create-or-update).
WF_PATH=".gitea/workflows/ci.yml"
WF_CONTENT=$(cat <<'YAML'
name: ci
on: [push, workflow_dispatch]
jobs:
  hello:
    runs-on: ubuntu-latest
    steps:
      - run: echo "hello from garc dev runner"
      - run: echo "job=${{ github.job }} sha=${{ github.sha }}"
YAML
)
WF_B64=$(printf '%s' "$WF_CONTENT" | base64)
# does the file exist already? -> update with its sha; else create
EXIST_SHA=$(curl -s "${AUTH[@]}" "${API}/repos/${ORG}/${REPO}/contents/${WF_PATH}?ref=main" | jqget sha)
if [ -n "$EXIST_SHA" ]; then
  log "workflow ${WF_PATH} exists; updating"
  curl -sf "${AUTH[@]}" -X PUT "${API}/repos/${ORG}/${REPO}/contents/${WF_PATH}" \
    -H 'Content-Type: application/json' \
    -d "{\"content\":\"${WF_B64}\",\"message\":\"update ci workflow\",\"sha\":\"${EXIST_SHA}\",\"branch\":\"main\"}" \
    >/dev/null
else
  log "adding workflow ${WF_PATH}"
  curl -sf "${AUTH[@]}" -X POST "${API}/repos/${ORG}/${REPO}/contents/${WF_PATH}" \
    -H 'Content-Type: application/json' \
    -d "{\"content\":\"${WF_B64}\",\"message\":\"add ci workflow\",\"branch\":\"main\"}" \
    >/dev/null
fi

# --- sanity: org runners endpoint reachable under the new token (ADR 0006 path) ---
RUNNERS_HTTP=$(curl -s -o /dev/null -w '%{http_code}' \
  -H "Authorization: token ${TOKEN}" \
  "${API}/orgs/${ORG}/actions/runners")
log "org runners endpoint under org token -> HTTP ${RUNNERS_HTTP} (200 = scope OK)"

cat >&2 <<EOF
[seed] DONE.
  Gitea:  http://localhost:${LOCAL_PORT}/  (admin: ${ADMIN_USER} / ${ADMIN_PASS})
  Org:    ${ORG}   Repo: ${ORG}/${REPO}   Workflow: ${WF_PATH}
  Token:  ${TOKEN_FILE}  (read+write:organization; use as the operator credential)
  Access: kubectl --context ${CTX} -n ${NS} port-forward svc/${SVC} ${LOCAL_PORT}:3000
EOF
