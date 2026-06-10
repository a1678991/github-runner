package github

import (
	"bytes"
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ErrNotFound is returned for 404 responses, e.g. deleting an
// already-deregistered runner.
var ErrNotFound = errors.New("not found")

type Client struct {
	BaseURL        string
	HTTP           *http.Client
	AppID          int64
	InstallationID int64
	Key            *rsa.PrivateKey
	Now            func() time.Time

	mu       sync.Mutex
	token    string
	tokenExp time.Time
}

func New(baseURL string, appID, installationID int64, key *rsa.PrivateKey) *Client {
	return &Client{
		BaseURL:        strings.TrimRight(baseURL, "/"),
		HTTP:           &http.Client{Timeout: 30 * time.Second},
		AppID:          appID,
		InstallationID: installationID,
		Key:            key,
		Now:            time.Now,
	}
}

type Runner struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
	Busy   bool   `json:"busy"`
}

type JITRequest struct {
	Name          string   `json:"name"`
	RunnerGroupID int64    `json:"runner_group_id"`
	Labels        []string `json:"labels"`
	WorkFolder    string   `json:"work_folder,omitempty"`
}

type JITResult struct {
	Runner           Runner `json:"runner"`
	EncodedJITConfig string `json:"encoded_jit_config"`
}

// CheckAuth verifies the App credentials by minting an installation token.
func (c *Client) CheckAuth(ctx context.Context) error {
	_, err := c.installationToken(ctx)
	return err
}

func (c *Client) installationToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && c.Now().Before(c.tokenExp.Add(-5*time.Minute)) {
		return c.token, nil
	}
	jwt, err := mintJWT(c.AppID, c.Key, c.Now())
	if err != nil {
		return "", err
	}
	var out struct {
		Token     string    `json:"token"`
		ExpiresAt time.Time `json:"expires_at"`
	}
	path := fmt.Sprintf("app/installations/%d/access_tokens", c.InstallationID)
	if err := c.do(ctx, http.MethodPost, path, jwt, nil, &out); err != nil {
		return "", fmt.Errorf("mint installation token: %w", err)
	}
	c.token, c.tokenExp = out.Token, out.ExpiresAt
	return c.token, nil
}

// api performs an installation-token-authenticated request.
func (c *Client) api(ctx context.Context, method, path string, in, out any) error {
	tok, err := c.installationToken(ctx)
	if err != nil {
		return err
	}
	return c.do(ctx, method, path, tok, in, out)
}

func (c *Client) do(ctx context.Context, method, path, bearer string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+"/"+path, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+bearer)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return fmt.Errorf("%s %s: %w", method, path, ErrNotFound)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 300))
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, msg)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}

func (c *Client) GenerateJITConfig(ctx context.Context, prefix string, req JITRequest) (*JITResult, error) {
	var out JITResult
	if err := c.api(ctx, http.MethodPost, prefix+"/actions/runners/generate-jitconfig", req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) GetRunner(ctx context.Context, prefix string, id int64) (*Runner, error) {
	var out Runner
	if err := c.api(ctx, http.MethodGet, fmt.Sprintf("%s/actions/runners/%d", prefix, id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) DeleteRunner(ctx context.Context, prefix string, id int64) error {
	return c.api(ctx, http.MethodDelete, fmt.Sprintf("%s/actions/runners/%d", prefix, id), nil, nil)
}

func (c *Client) ListRunners(ctx context.Context, prefix string) ([]Runner, error) {
	var all []Runner
	for page := 1; ; page++ {
		var out struct {
			Runners []Runner `json:"runners"`
		}
		path := fmt.Sprintf("%s/actions/runners?per_page=100&page=%d", prefix, page)
		if err := c.api(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Runners...)
		if len(out.Runners) < 100 {
			return all, nil
		}
	}
}

// RunnerGroupID resolves a runner-group name to its ID. Repo-level runners
// always belong to the default group (ID 1); the API has no repo-level
// runner-groups endpoint.
func (c *Client) RunnerGroupID(ctx context.Context, prefix, name string) (int64, error) {
	if strings.HasPrefix(prefix, "repos/") {
		return 1, nil
	}
	for page := 1; ; page++ {
		var out struct {
			RunnerGroups []struct {
				ID   int64  `json:"id"`
				Name string `json:"name"`
			} `json:"runner_groups"`
		}
		path := fmt.Sprintf("%s/actions/runner-groups?per_page=100&page=%d", prefix, page)
		if err := c.api(ctx, http.MethodGet, path, nil, &out); err != nil {
			return 0, err
		}
		for _, g := range out.RunnerGroups {
			if g.Name == name {
				return g.ID, nil
			}
		}
		if len(out.RunnerGroups) < 100 {
			return 0, fmt.Errorf("runner group %q not found in %s", name, prefix)
		}
	}
}
