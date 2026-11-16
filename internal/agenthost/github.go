/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package agenthost

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// maxGitHubUserBytes bounds a GitHub user document.
const maxGitHubUserBytes = 1 << 20

// GitHubAccounts reads the GitHub account a token belongs to.
type GitHubAccounts struct {
	Client *http.Client
	// BaseURL is the GitHub API; empty means https://api.github.com.
	BaseURL string
}

// Account returns the account token belongs to, from GET /user.
func (g GitHubAccounts) Account(ctx context.Context, token string) (corev1alpha1.AgentHostGitHubAccount, error) {
	client := g.Client
	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}
	base := strings.TrimRight(g.BaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/user", nil)
	if err != nil {
		return corev1alpha1.AgentHostGitHubAccount{}, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	response, err := client.Do(request)
	if err != nil {
		return corev1alpha1.AgentHostGitHubAccount{}, fmt.Errorf("read the GitHub account: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return corev1alpha1.AgentHostGitHubAccount{}, fmt.Errorf("read the GitHub account: HTTP %d", response.StatusCode)
	}
	var user struct {
		ID    int64  `json:"id"`
		Login string `json:"login"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, maxGitHubUserBytes)).Decode(&user); err != nil {
		return corev1alpha1.AgentHostGitHubAccount{}, fmt.Errorf("decode the GitHub account: %w", err)
	}
	if user.ID <= 0 || user.Login == "" || len(user.Login) > 39 {
		return corev1alpha1.AgentHostGitHubAccount{}, fmt.Errorf("the GitHub account has no valid ID and login")
	}
	return corev1alpha1.AgentHostGitHubAccount{ID: user.ID, Login: user.Login}, nil
}
