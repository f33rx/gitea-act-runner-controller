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
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

// DeregisterOrgRunner deletes an ephemeral runner from an organization.
// Returns the HTTP status code. 204 indicates success.
func (c *Client) DeregisterOrgRunner(org string, runnerID int64) (int, error) {
	url := fmt.Sprintf("%s/api/v1/orgs/%s/actions/runners/%d", c.baseURL, org, runnerID)

	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return 0, err
	}

	req.Header.Set("Authorization", fmt.Sprintf("token %s", c.token))
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// Drain the response body to allow connection reuse
	_, _ = io.ReadAll(resp.Body)

	return resp.StatusCode, nil
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
func (c *Client) ListOrgRunners(org string) ([]Runner, error) {
	url := fmt.Sprintf("%s/api/v1/orgs/%s/actions/runners", c.baseURL, org)

	req, err := http.NewRequest("GET", url, nil)
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
	ID        int64    `json:"id"`
	URL       string   `json:"url"`
	Name      string   `json:"name"`
	Status    string   `json:"status"`
	RunnerID  int64    `json:"runner_id"`
	Labels    []string `json:"labels"`
	StartedAt string   `json:"started_at"`
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
func (c *Client) ListOrgQueuedJobs(org string) ([]Job, int, error) {
	url := fmt.Sprintf("%s/api/v1/orgs/%s/actions/jobs?status=queued&limit=100", c.baseURL, org)

	req, err := http.NewRequest("GET", url, nil)
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
func (c *Client) ListOrgInProgressJobs(org string) ([]Job, error) {
	url := fmt.Sprintf("%s/api/v1/orgs/%s/actions/jobs?status=in_progress&limit=100", c.baseURL, org)

	req, err := http.NewRequest("GET", url, nil)
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
func (c *Client) JobLogSize(jobURL string) (int64, error) {
	req, err := http.NewRequest("GET", jobURL+"/logs", nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Authorization", fmt.Sprintf("token %s", c.token))

	resp, err := c.client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("job log request failed with status %d", resp.StatusCode)
	}
	return resp.ContentLength, nil
}

// RegistrationToken represents a registration token response.
type RegistrationToken struct {
	Token string `json:"token"`
}

// GetOrgRegistrationToken fetches a fresh registration token for an organization.
// Returns the token string.
func (c *Client) GetOrgRegistrationToken(org string) (string, error) {
	url := fmt.Sprintf("%s/api/v1/orgs/%s/actions/runners/registration-token", c.baseURL, org)

	req, err := http.NewRequest("POST", url, nil)
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
