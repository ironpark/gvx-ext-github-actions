package github

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const DefaultAPIBaseURL = "https://api.github.com"

// Client is a minimal stdlib-only GitHub REST API client for Self-Hosted Runner
// JIT config generation and runner inspection.
type Client struct {
	BaseURL    string
	HTTPClient *http.Client
}

// NewClient constructs a GitHub API client targeting DefaultAPIBaseURL.
func NewClient() *Client {
	return &Client{
		BaseURL: DefaultAPIBaseURL,
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// JITConfigRequest configures POST .../actions/runners/generate-jitconfig.
type JITConfigRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int64    `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder,omitempty"`
}

// RemoteRunnerLabel describes one label attached to a GitHub runner.
type RemoteRunnerLabel struct {
	ID   int64  `json:"id,omitempty"`
	Name string `json:"name"`
	Type string `json:"type,omitempty"`
}

// RemoteRunner describes a GitHub Actions self-hosted runner record.
type RemoteRunner struct {
	ID     int64               `json:"id"`
	Name   string              `json:"name"`
	OS     string              `json:"os"`
	Status string              `json:"status"`
	Busy   bool                `json:"busy"`
	Labels []RemoteRunnerLabel `json:"labels"`
}

// JITConfigResponse is returned by GitHub's generate-jitconfig endpoint.
type JITConfigResponse struct {
	Runner           RemoteRunner `json:"runner"`
	EncodedJITConfig string       `json:"encoded_jit_config"`
}

// ParseTarget validates scope ("repo" or "org") and target ("owner/repo" or "org").
func ParseTarget(scope, target string) (owner, repo string, err error) {
	scope = strings.ToLower(strings.TrimSpace(scope))
	target = strings.TrimSpace(target)
	target = strings.TrimPrefix(target, "https://github.com/")
	target = strings.TrimPrefix(target, "http://github.com/")
	target = strings.TrimSuffix(target, ".git")
	target = strings.Trim(target, "/")

	switch scope {
	case "", "repo":
		o, r, ok := strings.Cut(target, "/")
		if !ok || o == "" || r == "" || strings.Contains(r, "/") {
			return "", "", fmt.Errorf("repository target %q must be formatted as owner/repo", target)
		}
		return o, r, nil
	case "org":
		if target == "" || strings.Contains(target, "/") {
			return "", "", fmt.Errorf("organization target %q must be a single org slug", target)
		}
		return target, "", nil
	default:
		return "", "", fmt.Errorf("unsupported runner scope %q (want repo or org)", scope)
	}
}

func (c *Client) apiBase() string {
	b := strings.TrimRight(strings.TrimSpace(c.BaseURL), "/")
	if b == "" {
		return DefaultAPIBaseURL
	}
	return b
}

func (c *Client) httpClient() *http.Client {
	if c != nil && c.HTTPClient != nil {
		return c.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

func (c *Client) runnersPath(scope, target string) (string, error) {
	owner, repo, err := ParseTarget(scope, target)
	if err != nil {
		return "", err
	}
	if strings.EqualFold(strings.TrimSpace(scope), "org") {
		return fmt.Sprintf("/orgs/%s/actions/runners", url.PathEscape(owner)), nil
	}
	return fmt.Sprintf("/repos/%s/%s/actions/runners", url.PathEscape(owner), url.PathEscape(repo)), nil
}

// GenerateJITConfig calls GitHub's generate-jitconfig API to mint a single-use
// ephemeral runner configuration.
func (c *Client) GenerateJITConfig(ctx context.Context, scope, target, token string, req JITConfigRequest) (JITConfigResponse, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return JITConfigResponse{}, errors.New("GitHub token is empty")
	}
	basePath, err := c.runnersPath(scope, target)
	if err != nil {
		return JITConfigResponse{}, err
	}
	if req.RunnerGroupID <= 0 {
		req.RunnerGroupID = 1
	}
	if strings.TrimSpace(req.WorkFolder) == "" {
		req.WorkFolder = "_work"
	}
	if len(req.Labels) == 0 {
		req.Labels = []string{"self-hosted", "linux", "gvx"}
	}

	var out JITConfigResponse
	if err := c.doJSON(ctx, http.MethodPost, basePath+"/generate-jitconfig", token, req, &out); err != nil {
		return JITConfigResponse{}, err
	}
	if strings.TrimSpace(out.EncodedJITConfig) == "" {
		return JITConfigResponse{}, errors.New("GitHub API returned an empty encoded_jit_config")
	}
	return out, nil
}

// ListRunners returns all registered self-hosted runners for the repo or org.
func (c *Client) ListRunners(ctx context.Context, scope, target, token string) ([]RemoteRunner, error) {
	basePath, err := c.runnersPath(scope, target)
	if err != nil {
		return nil, err
	}
	var resp struct {
		Runners []RemoteRunner `json:"runners"`
	}
	if err := c.doJSON(ctx, http.MethodGet, basePath, token, nil, &resp); err != nil {
		return nil, err
	}
	return resp.Runners, nil
}

// DeleteRunner removes a registered runner by numeric ID (best-effort cleanup).
func (c *Client) DeleteRunner(ctx context.Context, scope, target, token string, runnerID int64) error {
	if runnerID <= 0 {
		return nil
	}
	basePath, err := c.runnersPath(scope, target)
	if err != nil {
		return err
	}
	return c.doJSON(ctx, http.MethodDelete, fmt.Sprintf("%s/%d", basePath, runnerID), token, nil, nil)
}

func (c *Client) doJSON(ctx context.Context, method, reqPath, token string, body, dst any) error {
	var bodyReader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		bodyReader = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.apiBase()+reqPath, bodyReader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "gvx-ext-github-actions/0.1.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var ghErr struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(raw, &ghErr)
		msg := strings.TrimSpace(ghErr.Message)
		if msg == "" {
			msg = strings.TrimSpace(string(raw))
		}
		switch resp.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Errorf("GitHub API 401 Unauthorized (%s): verify your PAT is valid and not expired", msg)
		case http.StatusForbidden:
			return fmt.Errorf("GitHub API 403 Forbidden (%s): PAT requires Administration: Read and write (repo) or Self-hosted runners: Read and write (org)", msg)
		case http.StatusNotFound:
			return fmt.Errorf("GitHub API 404 Not Found (%s): check target repository/org and token access", msg)
		default:
			return fmt.Errorf("GitHub API %d: %s", resp.StatusCode, msg)
		}
	}
	if dst != nil && len(bytes.TrimSpace(raw)) > 0 {
		if err := json.Unmarshal(raw, dst); err != nil {
			return fmt.Errorf("decode GitHub API response: %w", err)
		}
	}
	return nil
}
