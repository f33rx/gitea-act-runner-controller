# Dev Gitea (garc-dev kind cluster)

A persistent, single-node **Gitea 1.26.1** on the local `garc-dev` kind cluster, used as
a development and integration-test target for the operator (register ephemeral runners,
poll the demand queue, exercise teardown).

This is **not** the deferred `garc-nom` hermetic CI harness. That harness stands up its
own throwaway Gitea per test run; this is a stable instance you develop against by hand.

## Prerequisites

- `garc-dev` kind cluster running (`kind get clusters` shows `garc-dev`).
- `helm`, `kubectl`, `docker`, `curl`, `python3` on PATH.
- Gitea Helm repo added once:
  ```
  helm repo add gitea-charts https://dl.gitea.com/charts/ && helm repo update
  ```

## Install

```
helm upgrade --install gitea gitea-charts/gitea --version 12.6.0 \
  -n gitea --create-namespace -f dev/gitea/values.yaml
kubectl -n gitea wait --for=condition=ready pod -l app.kubernetes.io/name=gitea --timeout=180s
```

Chart `12.6.0` == appVersion `1.26.1` (the garc pinned target). The values profile is
lean: SQLite + in-memory session/cache + LevelDB queue, all bundled subcharts
(PostgreSQL-HA, valkey) disabled, one 5Gi PVC on the default `standard` (local-path)
StorageClass, Actions enabled.

## Seed

```
dev/gitea/seed.sh
```

Idempotent. Creates the `garc-dev` org, an **org-scoped read+write access token**
(matching ratified ADR 0006's org-scoped default), and a `hello-actions` repo with a
sample `.gitea/workflows/ci.yml` (which enqueues a `queued` job on push). The token is
written to `dev/gitea/.org-token` (gitignored) for local tooling.

## Access

kind maps no extra host ports on `garc-dev`, so reach Gitea via port-forward:

```
kubectl --context kind-garc-dev -n gitea port-forward svc/gitea-http 3000:3000
# UI:  http://localhost:3000/   admin: gitea_admin / gitea_admin_pw_dev
```

Key API paths (org-scoped, the operator's paths):

```
TOKEN=$(cat dev/gitea/.org-token)
# demand queue (listener reads this; ADR 0007):
curl -H "Authorization: token $TOKEN" \
  'http://localhost:3000/api/v1/orgs/garc-dev/actions/jobs?status=queued'
# registered runners (teardown controller; ADR 0006):
curl -H "Authorization: token $TOKEN" \
  'http://localhost:3000/api/v1/orgs/garc-dev/actions/runners'
# an ephemeral runner registration token:
curl -X POST -H "Authorization: token $TOKEN" \
  'http://localhost:3000/api/v1/orgs/garc-dev/actions/runners/registration-token'
```

## Teardown

```
helm uninstall gitea -n gitea
kubectl delete ns gitea         # also removes the PVC / all data
```

## Verified against this instance (2026-07-01)

- `GET .../orgs/garc-dev/actions/jobs?status=queued` -> 1 `queued` job, `labels:
  ['ubuntu-latest']`, sentinels `runner_id: 0` / `started_at: 1970-01-01`.
- `X-Total-Count: 1` header present (fast queue-depth gate).
- Org runners endpoint returns HTTP 200 under the org-scoped token (ADR 0006 path OK).
