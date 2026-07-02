/*
Copyright 2026.

Contract tests: decode REAL captured Gitea 1.26.1 API response bodies into the client's
Go structs and assert the fields the operator depends on actually populate. These guard the
live-API boundary -- the class of bug where a wrong json tag (e.g. "data" vs "jobs") decodes
to an empty slice with no error and silently breaks scaling. Fixtures are verbatim captures
from GET against the live dev Gitea (org garc-dev), trimmed only for length.
*/

package gitea

import (
	"encoding/json"
	"testing"
)

// queuedJobsFixture is a verbatim body from
// GET /api/v1/orgs/{org}/actions/jobs?status=queued on Gitea 1.26.1 (2026-07-01).
// Top-level key is "jobs" (NOT "data"); job labels are bare strings.
const queuedJobsFixture = `{
  "jobs": [
    {
      "id": 9, "run_id": 5, "name": "hello",
      "labels": ["ubuntu-latest"],
      "status": "queued", "runner_id": 0,
      "created_at": "2026-07-01T20:51:15Z",
      "started_at": "1970-01-01T00:00:00Z"
    },
    {
      "id": 10, "run_id": 5, "name": "build",
      "labels": ["ubuntu-latest", "self-hosted"],
      "status": "queued", "runner_id": 0,
      "created_at": "2026-07-01T20:51:16Z",
      "started_at": "1970-01-01T00:00:00Z"
    }
  ],
  "total_count": 2
}`

// orgRunnersFixture is a verbatim body from
// GET /api/v1/orgs/{org}/actions/runners on Gitea 1.26.1. Note: NO created_at on rows,
// labels are objects {id,name,type}, and the collection key is "runners".
const orgRunnersFixture = `{
  "runners": [
    {
      "id": 9, "name": "orphan-test", "status": "offline",
      "busy": false, "disabled": false, "ephemeral": true,
      "labels": [{"id": 0, "name": "ubuntu-latest", "type": "custom"}]
    }
  ],
  "total_count": 1
}`

func TestDecodeQueuedJobsContract(t *testing.T) {
	var got ListOrgQueuedJobsResponse
	if err := json.Unmarshal([]byte(queuedJobsFixture), &got); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	// The bug this guards: a wrong top-level tag yields an empty slice with no error.
	if len(got.Jobs) != 2 {
		t.Fatalf("expected 2 jobs decoded from the real body, got %d -- likely a wrong json tag on the jobs field", len(got.Jobs))
	}
	if got.Jobs[0].Name != "hello" || got.Jobs[0].Status != "queued" {
		t.Errorf("job 0 fields not populated: %+v", got.Jobs[0])
	}
	// Labels must decode as bare strings the listener can bucket on.
	if len(got.Jobs[0].Labels) != 1 || got.Jobs[0].Labels[0] != "ubuntu-latest" {
		t.Errorf("job 0 labels not parsed as bare strings: %#v", got.Jobs[0].Labels)
	}
	if len(got.Jobs[1].Labels) != 2 {
		t.Errorf("job 1 (multi-label) labels not parsed: %#v", got.Jobs[1].Labels)
	}
	if got.Jobs[0].RunnerID != 0 {
		t.Errorf("queued job should have runner_id 0 (unclaimed sentinel), got %d", got.Jobs[0].RunnerID)
	}
}

func TestDecodeOrgRunnersContract(t *testing.T) {
	var got ListOrgRunnersResponse
	if err := json.Unmarshal([]byte(orgRunnersFixture), &got); err != nil {
		t.Fatalf("decode failed: %v", err)
	}
	if got.TotalCount != 1 || len(got.Runners) != 1 {
		t.Fatalf("expected 1 runner row, got total=%d len=%d", got.TotalCount, len(got.Runners))
	}
	r := got.Runners[0]
	if r.Name != "orphan-test" || !r.Ephemeral || r.Status != "offline" {
		t.Errorf("runner row fields not populated: %+v", r)
	}
	// Documented API gap: runner rows carry NO created_at. If a future Gitea adds it,
	// this test still passes; the operator must not DEPEND on it (see sweep design).
}
