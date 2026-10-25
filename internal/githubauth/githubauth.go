/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package githubauth holds the GitHub calls behind signing people in with
// GitHub (ADR 0037): the device flow, the app-authenticated check that a token
// was issued to Orka's own app, and org and team membership.
package githubauth

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

const (
	// Issuer is the identity issuer of people signed in with GitHub. Their
	// subject is their numeric user ID, which survives a login rename.
	Issuer = "https://github.com"

	defaultWebURL = "https://github.com"
	defaultAPIURL = "https://api.github.com"
	maxBodyBytes  = 1 << 20
)

// ErrNotMember reports a person who is in none of the allowed orgs or teams.
var ErrNotMember = errors.New("not an active member of an allowed GitHub org or team")

// ErrInvalidToken reports a token GitHub does not know as issued to the app.
var ErrInvalidToken = errors.New("the GitHub token was not issued to this app")

// Client calls GitHub. Its zero value talks to github.com.
type Client struct {
	HTTP *http.Client
	// WebURL serves the device flow; APIURL the REST API. Empty means
	// github.com and api.github.com.
	WebURL string
	APIURL string
	// MinPollInterval is the device flow's shortest wait between polls;
	// zero means GitHub's five seconds.
	MinPollInterval time.Duration
}

// http never follows redirects: every call carries a token or device code,
// in its body or headers, that a redirect would send elsewhere.
func (c Client) http() *http.Client {
	client := &http.Client{Timeout: 15 * time.Second}
	if c.HTTP != nil {
		copied := *c.HTTP
		client = &copied
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return client
}

func (c Client) webURL() string {
	if c.WebURL != "" {
		return strings.TrimRight(c.WebURL, "/")
	}
	return defaultWebURL
}

func (c Client) apiURL() string {
	if c.APIURL != "" {
		return strings.TrimRight(c.APIURL, "/")
	}
	return defaultAPIURL
}

// User is a GitHub account.
type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

// CheckToken confirms with GitHub that token was issued to the app with
// clientID, and returns its user. It authenticates as the app, so a token
// another app obtained never passes.
func (c Client) CheckToken(ctx context.Context, clientID, clientSecret, token string) (User, error) {
	body, err := json.Marshal(map[string]string{"access_token": token})
	if err != nil {
		return User{}, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.apiURL()+"/applications/"+url.PathEscape(clientID)+"/token", bytes.NewReader(body))
	if err != nil {
		return User{}, err
	}
	request.SetBasicAuth(clientID, clientSecret)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Content-Type", "application/json")
	var checked struct {
		User User `json:"user"`
		App  struct {
			ClientID string `json:"client_id"`
		} `json:"app"`
	}
	status, _, err := c.do(request, &checked)
	if err != nil {
		return User{}, err
	}
	if status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
		return User{}, ErrInvalidToken
	}
	if status != http.StatusOK {
		return User{}, fmt.Errorf("check the GitHub token: HTTP %d", status)
	}
	if checked.App.ClientID != clientID || checked.User.ID <= 0 || checked.User.Login == "" {
		return User{}, ErrInvalidToken
	}
	return checked.User, nil
}

// DeleteToken revokes token, authenticating as the app that issued it.
func (c Client) DeleteToken(ctx context.Context, clientID, clientSecret, token string) error {
	body, err := json.Marshal(map[string]string{"access_token": token})
	if err != nil {
		return err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodDelete,
		c.apiURL()+"/applications/"+url.PathEscape(clientID)+"/token", bytes.NewReader(body))
	if err != nil {
		return err
	}
	request.SetBasicAuth(clientID, clientSecret)
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Content-Type", "application/json")
	status, _, err := c.do(request, nil)
	if err != nil {
		return err
	}
	if status != http.StatusNoContent && status != http.StatusNotFound {
		return fmt.Errorf("revoke the GitHub token: HTTP %d", status)
	}
	return nil
}

// Team names a team by its org and slug.
type Team struct {
	Org  string
	Slug string
}

// Allowed lists who may sign in: active members of any of Orgs, or of any of
// Teams. A team never admits its org's other members.
type Allowed struct {
	Orgs  []string
	Teams []Team
}

// Empty reports whether the list admits no one.
func (a Allowed) Empty() bool { return len(a.Orgs) == 0 && len(a.Teams) == 0 }

// ParseAllowed reads comma-separated org names and org/team slugs.
func ParseAllowed(orgs, teams string) (Allowed, error) {
	var allowed Allowed
	for org := range strings.SplitSeq(orgs, ",") {
		if org = strings.TrimSpace(org); org != "" {
			if strings.Contains(org, "/") {
				return Allowed{}, fmt.Errorf("allowed org %q must be an org name; name teams as org/team", org)
			}
			allowed.Orgs = append(allowed.Orgs, org)
		}
	}
	for team := range strings.SplitSeq(teams, ",") {
		if team = strings.TrimSpace(team); team == "" {
			continue
		}
		org, slug, ok := strings.Cut(team, "/")
		if !ok || org == "" || slug == "" || strings.Contains(slug, "/") {
			return Allowed{}, fmt.Errorf("allowed team %q must be org/team", team)
		}
		allowed.Teams = append(allowed.Teams, Team{Org: org, Slug: slug})
	}
	return allowed, nil
}

// RequireMember returns nil only when the person with token and login is an
// active member of an allowed org or team. It returns ErrNotMember when GitHub
// said so, and another error when GitHub could not be asked: a sign-in treats
// both as no, while a host refresh retries the second.
func (c Client) RequireMember(ctx context.Context, token, login string, allowed Allowed) error {
	if allowed.Empty() {
		return ErrNotMember
	}
	var failure error
	for _, org := range allowed.Orgs {
		active, err := c.active(ctx, token, "/user/memberships/orgs/"+url.PathEscape(org))
		if active {
			return nil
		}
		failure = errors.Join(failure, err)
	}
	for _, team := range allowed.Teams {
		active, err := c.active(ctx, token, "/orgs/"+url.PathEscape(team.Org)+"/teams/"+url.PathEscape(team.Slug)+
			"/memberships/"+url.PathEscape(login))
		if active {
			return nil
		}
		failure = errors.Join(failure, err)
	}
	if failure != nil {
		return fmt.Errorf("could not read GitHub memberships: %w", failure)
	}
	return ErrNotMember
}

// active reports whether a membership endpoint answers state active.
func (c Client) active(ctx context.Context, token, path string) (bool, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.apiURL()+path, nil)
	if err != nil {
		return false, err
	}
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Accept", "application/vnd.github+json")
	var membership struct {
		State string `json:"state"`
	}
	status, header, err := c.do(request, &membership)
	if err != nil {
		return false, err
	}
	// Only GitHub saying the person is not a member is a verdict. It answers
	// rate limits, including secondary ones without any header, with 403 or
	// 429, so those must never end a member's access.
	if status == http.StatusForbidden || status == http.StatusTooManyRequests {
		limited := ""
		if header.Get("Retry-After") != "" || header.Get("X-RateLimit-Remaining") == "0" {
			limited = ", rate-limited"
		}
		return false, fmt.Errorf("GitHub refused the membership check: HTTP %d%s", status, limited)
	}
	if status == http.StatusNotFound {
		return false, nil
	}
	if status != http.StatusOK {
		return false, fmt.Errorf("read a GitHub membership: HTTP %d", status)
	}
	// GitHub answers active or pending; anything else is no verdict.
	switch membership.State {
	case "active":
		return true, nil
	case "pending":
		return false, nil
	}
	return false, errors.New("GitHub answered a membership without a known state")
}

// DeviceCode is a started device flow.
type DeviceCode struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// Tokens are what a finished device flow grants.
type Tokens struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token,omitempty"`
	ExpiresIn             int    `json:"expires_in,omitempty"`
	RefreshTokenExpiresIn int    `json:"refresh_token_expires_in,omitempty"`
}

// StartDeviceFlow asks GitHub for a code the person enters at
// VerificationURI.
func (c Client) StartDeviceFlow(ctx context.Context, clientID string) (DeviceCode, error) {
	form := url.Values{"client_id": {clientID}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webURL()+"/login/device/code",
		strings.NewReader(form.Encode()))
	if err != nil {
		return DeviceCode{}, err
	}
	request.Header.Set("Accept", "application/json")
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	var code DeviceCode
	status, _, err := c.do(request, &code)
	if err != nil {
		return DeviceCode{}, err
	}
	if status != http.StatusOK || code.DeviceCode == "" || code.UserCode == "" {
		return DeviceCode{}, fmt.Errorf("start GitHub's device flow: HTTP %d; is the device flow enabled on the app?", status)
	}
	return code, nil
}

// ErrDeviceFlowDenied reports a person who refused, or a code that expired.
var ErrDeviceFlowDenied = errors.New("the GitHub device code was refused or expired")

// PollDeviceFlow waits for the person to approve code, at GitHub's pace,
// and returns the tokens.
func (c Client) PollDeviceFlow(ctx context.Context, clientID string, code DeviceCode) (Tokens, error) {
	floor := c.MinPollInterval
	if floor <= 0 {
		floor = 5 * time.Second
	}
	interval := max(time.Duration(code.Interval)*time.Second, floor)
	deadline := time.Now().Add(time.Duration(max(code.ExpiresIn, 60)) * time.Second)
	for {
		select {
		case <-ctx.Done():
			return Tokens{}, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return Tokens{}, ErrDeviceFlowDenied
		}
		form := url.Values{"client_id": {clientID}, "device_code": {code.DeviceCode},
			"grant_type": {"urn:ietf:params:oauth:grant-type:device_code"}}
		request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.webURL()+"/login/oauth/access_token",
			strings.NewReader(form.Encode()))
		if err != nil {
			return Tokens{}, err
		}
		request.Header.Set("Accept", "application/json")
		request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		var answer struct {
			Tokens
			Error    string `json:"error"`
			Interval int    `json:"interval"`
		}
		status, _, err := c.do(request, &answer)
		switch {
		// A call that failed, or a server error, is no verdict on the code:
		// polling goes on until GitHub gives one or the code expires.
		case err != nil || status >= http.StatusInternalServerError || status == http.StatusTooManyRequests:
		case status == http.StatusOK && answer.AccessToken != "":
			return answer.Tokens, nil
		case answer.Error == "authorization_pending":
		case answer.Error == "slow_down":
			interval = max(time.Duration(answer.Interval)*time.Second, interval+floor)
		case answer.Error == "expired_token" || answer.Error == "access_denied":
			return Tokens{}, ErrDeviceFlowDenied
		default:
			return Tokens{}, fmt.Errorf("GitHub's device flow failed: HTTP %d %s", status, answer.Error)
		}
	}
}

func (c Client) do(request *http.Request, into any) (int, http.Header, error) {
	response, err := c.http().Do(request)
	if err != nil {
		return 0, nil, fmt.Errorf("call GitHub: %w", err)
	}
	defer func() { _ = response.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(response.Body, maxBodyBytes))
	if err != nil {
		return 0, response.Header, fmt.Errorf("read GitHub's answer: %w", err)
	}
	if into != nil && len(raw) > 0 && response.StatusCode < 300 {
		if err := json.Unmarshal(raw, into); err != nil {
			return response.StatusCode, response.Header, fmt.Errorf("decode GitHub's answer: %w", err)
		}
	} else if into != nil && len(raw) > 0 {
		// Device-flow errors arrive as JSON with status 200 or 4xx.
		_ = json.Unmarshal(raw, into)
	}
	return response.StatusCode, response.Header, nil
}
