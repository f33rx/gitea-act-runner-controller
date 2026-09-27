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

// ListOrgJobsResponse is the API response for an org's jobs. The Gitea API returns the
// jobs under the "jobs" key (verified live against 1.26.1).
type ListOrgJobsResponse struct {
	Jobs       []Job `json:"jobs"`
	TotalCount int   `json:"total_count"`
}

// jobsPageSize is Gitea's default MAX_RESPONSE_ITEMS; Gitea silently caps a larger limit,
// so paging stops on total_count or an empty page, never on page arithmetic.
const jobsPageSize = 50

// jobsMaxPages bounds one listing at 1000 jobs.
const jobsMaxPages = 20

// ListOrgJobs lists an org's jobs in any of the given statuses, reading every page.
// Statuses in one call come from one query, so a job moving from queued to in_progress
// appears exactly once. Pages have no defined order and can shift while being read, so
// jobs are deduplicated by ID and the result is best effort once past one page. The
// returned total is Gitea's count; fewer jobs than that means the listing is partial.
func (c *Client) ListOrgJobs(ctx context.Context, org string, statuses ...string) ([]Job, int, error) {
	q := url.Values{}
	for _, s := range statuses {
		q.Add("status", s)
	}
	q.Set("limit", fmt.Sprint(jobsPageSize))

	seen := map[int64]bool{}
	var jobs []Job
	total := 0
	for page := 1; page <= jobsMaxPages; page++ {
		q.Set("page", fmt.Sprint(page))
		result, err := c.listOrgJobsPage(ctx, fmt.Sprintf("%s/api/v1/orgs/%s/actions/jobs?%s", c.baseURL, org, q.Encode()))
		if err != nil {
			return nil, 0, err
		}
		total = result.TotalCount
		for _, j := range result.Jobs {
			if !seen[j.ID] {
				seen[j.ID] = true
				jobs = append(jobs, j)
			}
		}
		if len(result.Jobs) == 0 || len(seen) >= result.TotalCount {
			break
		}
	}
	return jobs, total, nil
}

func (c *Client) listOrgJobsPage(ctx context.Context, pageURL string) (*ListOrgJobsResponse, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", pageURL, nil)
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
		return nil, fmt.Errorf("list jobs failed with status %d: %s", resp.StatusCode, string(body))
	}
	var result ListOrgJobsResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("failed to parse jobs response: %w", err)
	}
	return &result, nil
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
