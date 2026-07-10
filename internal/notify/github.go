// Package notify delivers deployment feedback: GitHub commit statuses and
// ntfy push notifications.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Commit status states accepted by the GitHub Statuses API.
const (
	StatePending = "pending"
	StateSuccess = "success"
	StateFailure = "failure"
	StateError   = "error"
)

// GitHubClient posts commit statuses. A nil *GitHubClient is a valid no-op
// client, so callers don't need to branch on whether a token is configured.
type GitHubClient struct {
	token   string
	baseURL string
	hc      *http.Client
}

// NewGitHub returns nil when token is empty (statuses disabled).
func NewGitHub(token, baseURL string) *GitHubClient {
	if token == "" {
		return nil
	}
	if baseURL == "" {
		baseURL = "https://api.github.com"
	}
	return &GitHubClient{
		token:   token,
		baseURL: strings.TrimRight(baseURL, "/"),
		hc:      &http.Client{Timeout: 15 * time.Second},
	}
}

// SetCommitStatus posts state for repo@sha under the given status context
// (POST /repos/{owner}/{repo}/statuses/{sha}).
func (c *GitHubClient) SetCommitStatus(ctx context.Context, repo, sha, state, statusContext, description string) error {
	if c == nil {
		return nil
	}
	// The API rejects descriptions over 140 characters.
	if len(description) > 140 {
		description = description[:137] + "..."
	}
	payload, err := json.Marshal(map[string]string{
		"state":       state,
		"context":     statusContext,
		"description": description,
	})
	if err != nil {
		return err
	}

	url := fmt.Sprintf("%s/repos/%s/statuses/%s", c.baseURL, repo, sha)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Content-Type", "application/json")

	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github statuses API: %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	return nil
}
