/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package gitea

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// Client is a minimal Gitea API client for runner teardown operations.
type Client struct {
	baseURL string
	token   string
	client  *http.Client
}

// NewClient creates a new Gitea API client.
func NewClient(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		client:  &http.Client{},
	}
}

// DeregisterOrgRunner deletes a runner registration from an organization. 204 and 404
// both return nil: every caller wants "not registered" as the end state. Gitea answers
// 404 not only for an absent runner but also for an unknown org and for a runner that
// exists but belongs to another org or a repo (ADR 0006 hit that last case live), so
// callers must only pass IDs obtained from ListOrgRunners on the same org and token;
// any other ID can be reported as deregistered while the row remains. A token without
// org-owner rights is 403 and surfaces as an error, as does any other non-204 status,
// with Gitea's response body carried in the error.
func (c *Client) DeregisterOrgRunner(ctx context.Context, org string, runnerID int64) error {
	url := fmt.Sprintf("%s/api/v1/orgs/%s/actions/runners/%d", c.baseURL, org, runnerID)

	req, err := http.NewRequestWithContext(ctx, "DELETE", url, nil)
	if err != nil {
		return err
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", c.token))
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	// Read the body: it drains the connection for reuse and, on an unexpected status,
	// carries Gitea's reason (e.g. which scope the token lacks).
	body, _ := io.ReadAll(resp.Body)

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusNotFound:
		return nil
	default:
		return fmt.Errorf("deregister runner %d in org %s failed with status %d: %s", runnerID, org, resp.StatusCode, string(body))
	}
}

// ListOrgRunners fetches the list of runners in an organization.
type Runner struct {
	ID        int64  `json:"id"`
	Name      string `json:"name"`
	Status    string `json:"status"`
	Busy      bool   `json:"busy"`
	Ephemeral bool   `json:"ephemeral"`
}

// ListOrgRunnersResponse is the API response structure.
type ListOrgRunnersResponse struct {
	Runners    []Runner `json:"runners"`
	TotalCount int      `json:"total_count"`
}

// ListOrgRunners fetches all runners in an organization.
func (c *Client) ListOrgRunners(ctx context.Context, org string) ([]Runner, error) {
	url := fmt.Sprintf("%s/api/v1/orgs/%s/actions/runners", c.baseURL, org)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", c.token))
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list runners failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result ListOrgRunnersResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse runners response: %w", err)
	}

	return result.Runners, nil
}

// Job represents a job from Gitea (queued or in-progress).
type Job struct {
	ID     int64  `json:"id"`
	URL    string `json:"url"`
	Name   string `json:"name"`
	Status string `json:"status"`
	// Conclusion is set once Status is "completed": success, failure, cancelled, skipped.
	Conclusion string `json:"conclusion"`
	RunnerID   int64  `json:"runner_id"`
	// RunnerName is the name act_runner registered under, which the operator sets to
	// the EphemeralRunner name; the operator never learns runner_id, so this is how an
	// in-progress job is matched back to its runner.
	RunnerName string   `json:"runner_name"`
	Labels     []string `json:"labels"`
	StartedAt  string   `json:"started_at"`
}

// ListOrgQueuedJobsResponse is the API response for queued jobs.
// The Gitea API returns the jobs under the "jobs" key (verified live against
// 1.26.1: GET /orgs/{org}/actions/jobs?status=queued -> {"jobs": [...], "total_count": N}).
type ListOrgQueuedJobsResponse struct {
	Jobs       []Job `json:"jobs"`
	TotalCount int   `json:"total_count"`
}

// ListOrgQueuedJobs fetches queued jobs for an organization.
// Per live-probe, the Gitea API returns job labels as an array of strings.
func (c *Client) ListOrgQueuedJobs(ctx context.Context, org string) ([]Job, int, error) {
	url := fmt.Sprintf("%s/api/v1/orgs/%s/actions/jobs?status=queued&limit=100", c.baseURL, org)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, 0, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", c.token))
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, 0, fmt.Errorf("list queued jobs failed with status %d: %s", resp.StatusCode, string(body))
	}

	// Read X-Total-Count header for fast queue depth.
	totalCount := 0
	if xTotalCount := resp.Header.Get("X-Total-Count"); xTotalCount != "" {
		_, _ = fmt.Sscanf(xTotalCount, "%d", &totalCount) // #nosec G104 - Sscanf error is benign (use 0 as default)
	}

	var result ListOrgQueuedJobsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, 0, fmt.Errorf("failed to parse jobs response: %w", err)
	}

	return result.Jobs, totalCount, nil
}

// ListOrgInProgressJobsResponse is the API response for in-progress jobs.
type ListOrgInProgressJobsResponse struct {
	Jobs []Job `json:"jobs"`
}

// ListOrgInProgressJobs fetches the org's currently in-progress jobs (ADR 0008:
// job-log liveness). One org-scoped call surfaces every running job's Gitea job URL
// (used to build the /logs URL) and its claiming runner_id/runner_name, avoiding a
// per-repo enumeration to find which job a given EphemeralRunner claimed.
func (c *Client) ListOrgInProgressJobs(ctx context.Context, org string) ([]Job, error) {
	url := fmt.Sprintf("%s/api/v1/orgs/%s/actions/jobs?status=in_progress&limit=100", c.baseURL, org)

	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", c.token))
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("list in-progress jobs failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result ListOrgInProgressJobsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse in-progress jobs response: %w", err)
	}

	return result.Jobs, nil
}

// JobLogSize returns the Content-Length of a job's log download (jobURL + "/logs"),
// used purely as a liveness signal (has the log grown since the last check) -- the
// body is never read. ADR 0008: this is the real job-log progress signal (verified
// live: act_runner streams step output to Gitea via UpdateLog/gRPC independent of the
// runner container's own stdout, which does NOT carry step output).
func (c *Client) JobLogSize(ctx context.Context, jobURL string) (int64, error) {
	// Gitea renders job.url from its ROOT_URL, which is the browser-facing address and
	// need not be reachable from inside the cluster (dev: http://localhost:3000). Keep
	// only the path and issue the request against the base URL this client was built
	// with.
	parsed, err := url.Parse(jobURL)
	if err != nil {
		return 0, fmt.Errorf("invalid job url %q: %w", jobURL, err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+parsed.Path+"/logs", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("token %s", c.token))
	// Without this the transport advertises gzip on our behalf; when the reply comes
	// back compressed it strips Content-Length and reports -1, which the caller cannot
	// distinguish from "log did not grow".
	req.Header.Set("Accept-Encoding", "identity")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("job log request failed with status %d", resp.StatusCode)
	}
	if resp.ContentLength < 0 {
		return 0, fmt.Errorf("job log response has unknown length (Content-Encoding %q)", resp.Header.Get("Content-Encoding"))
	}
	return resp.ContentLength, nil
}

// GetJob reads a job by its API URL (Job.URL), against this client's base URL for the
// same reason as JobLogSize.
func (c *Client) GetJob(ctx context.Context, jobURL string) (*Job, error) {
	parsed, err := url.Parse(jobURL)
	if err != nil {
		return nil, fmt.Errorf("invalid job url %q: %w", jobURL, err)
	}
	req, err := http.NewRequestWithContext(ctx, "GET", c.baseURL+parsed.Path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("token %s", c.token))
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get job failed with status %d", resp.StatusCode)
	}
	var job Job
	if err := json.NewDecoder(resp.Body).Decode(&job); err != nil {
		return nil, fmt.Errorf("decode job: %w", err)
	}
	return &job, nil
}

// RegistrationToken represents a registration token response.
type RegistrationToken struct {
	Token string `json:"token"`
}

// GetOrgRegistrationToken fetches a fresh registration token for an organization.
// Returns the token string.
func (c *Client) GetOrgRegistrationToken(ctx context.Context, org string) (string, error) {
	url := fmt.Sprintf("%s/api/v1/orgs/%s/actions/runners/registration-token", c.baseURL, org)

	req, err := http.NewRequestWithContext(ctx, "POST", url, nil)
	if err != nil {
		return "", err
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", c.token))
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("get registration token failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result RegistrationToken
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("failed to parse registration token response: %w", err)
	}

	return result.Token, nil
}
