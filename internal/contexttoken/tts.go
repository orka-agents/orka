/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package contexttoken

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	kontxttoken "github.com/aramase/kontxt/pkg/token"

	"github.com/sozercan/orka/internal/metrics"
)

const (
	// TTSTokenSourceNone disables Orka-initiated token exchanges.
	TTSTokenSourceNone = "none"
	// TTSTokenSourceServiceAccount uses Orka's ServiceAccount identity for exchanges.
	TTSTokenSourceServiceAccount = "serviceAccount"
	// TTSTokenSourceIncoming uses the incoming subject token for exchanges.
	TTSTokenSourceIncoming = "incoming"
)

const (
	defaultTTSTimeout     = 5 * time.Second
	defaultChildTokenTTL  = 5 * time.Minute
	defaultToolTokenTTL   = 2 * time.Minute
	defaultTTSTokenSource = TTSTokenSourceServiceAccount
)

// TTSConfig controls optional kontxt TTS token exchange integration.
type TTSConfig struct {
	URL           string
	Audience      string
	Timeout       time.Duration
	TokenSource   string
	ChildTokenTTL time.Duration
	ToolTokenTTL  time.Duration
}

// Enabled reports whether Orka should perform TTS exchanges.
func (c TTSConfig) Enabled() bool {
	return c.URL != "" && c.TokenSource != TTSTokenSourceNone
}

// NewTTSConfig builds TTS integration config from flag/env values.
func NewTTSConfig(endpoint, audience, timeout, tokenSource, childTTL, toolTTL string) (TTSConfig, error) {
	endpoint = strings.TrimSpace(endpoint)
	audience = strings.TrimSpace(audience)
	timeout = strings.TrimSpace(timeout)
	tokenSource = strings.TrimSpace(tokenSource)
	childTTL = strings.TrimSpace(childTTL)
	toolTTL = strings.TrimSpace(toolTTL)

	if tokenSource == "" {
		tokenSource = defaultTTSTokenSource
	}
	if endpoint == "" {
		if audience != "" || timeout != "" || childTTL != "" || toolTTL != "" || tokenSource != defaultTTSTokenSource {
			return TTSConfig{}, errors.New("context-token-tts-url is required when TTS settings are provided")
		}
		return TTSConfig{TokenSource: TTSTokenSourceNone}, nil
	}

	timeoutDuration, err := parseOptionalDuration(timeout, defaultTTSTimeout, "context-token-tts-timeout")
	if err != nil {
		return TTSConfig{}, err
	}
	childTTLDuration, err := parseOptionalDuration(childTTL, defaultChildTokenTTL, "context-token-child-token-ttl")
	if err != nil {
		return TTSConfig{}, err
	}
	toolTTLDuration, err := parseOptionalDuration(toolTTL, defaultToolTokenTTL, "context-token-tool-token-ttl")
	if err != nil {
		return TTSConfig{}, err
	}
	switch tokenSource {
	case TTSTokenSourceServiceAccount, TTSTokenSourceIncoming, TTSTokenSourceNone:
	default:
		return TTSConfig{}, fmt.Errorf("unsupported context-token TTS token source %q", tokenSource)
	}
	return TTSConfig{
		URL:           strings.TrimRight(endpoint, "/"),
		Audience:      audience,
		Timeout:       timeoutDuration,
		TokenSource:   tokenSource,
		ChildTokenTTL: childTTLDuration,
		ToolTokenTTL:  toolTTLDuration,
	}, nil
}

func parseOptionalDuration(value string, fallback time.Duration, name string) (time.Duration, error) {
	if value == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		return 0, fmt.Errorf("invalid %s %q", name, value)
	}
	return d, nil
}

// ExchangeRequest describes a TTS exchange or replacement request.
type ExchangeRequest struct {
	SubjectToken     string
	SubjectTokenType string
	Scope            string
	RequestDetails   map[string]any
	RequestContext   map[string]any
}

// Exchanger exchanges subject tokens for kontxt TxTokens.
type Exchanger interface {
	Exchange(ctx context.Context, req ExchangeRequest) (string, error)
}

// KontxtTTSClient is an RFC 8693 Token Transaction Service client.
type KontxtTTSClient struct {
	endpoint string
	audience string
	client   *http.Client
}

// NewKontxtTTSClient creates a TTS client for the configured endpoint.
func NewKontxtTTSClient(cfg TTSConfig) (*KontxtTTSClient, error) {
	if !cfg.Enabled() {
		return nil, errors.New("context token TTS is not configured")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTTSTimeout
	}
	return &KontxtTTSClient{
		endpoint: strings.TrimRight(cfg.URL, "/"),
		audience: strings.TrimSpace(cfg.Audience),
		client: &http.Client{
			Timeout: timeout,
		},
	}, nil
}

// Exchange exchanges the subject token for a kontxt transaction token.
func (c *KontxtTTSClient) Exchange(ctx context.Context, req ExchangeRequest) (token string, err error) {
	start := time.Now()
	result, reason := "success", "ok"
	defer func() {
		metrics.RecordContextTokenTTSExchange(result, reason, time.Since(start).Seconds())
	}()

	if c == nil || c.client == nil || strings.TrimSpace(c.endpoint) == "" {
		result, reason = "failure", "not_configured"
		return "", errors.New("context token TTS is not configured")
	}

	token, err = c.exchange(ctx, req)
	if err != nil {
		result, reason = "failure", "exchange_error"
		return "", err
	}
	return token, nil
}

func (c *KontxtTTSClient) exchange(ctx context.Context, req ExchangeRequest) (string, error) {
	subjectTokenType := strings.TrimSpace(req.SubjectTokenType)
	if subjectTokenType == "" {
		subjectTokenType = kontxttoken.SubjectTokenTypeTxnToken
	}

	form := url.Values{}
	form.Set("grant_type", "urn:ietf:params:oauth:grant-type:token-exchange")
	form.Set("requested_token_type", kontxttoken.SubjectTokenTypeTxnToken)
	form.Set("subject_token", req.SubjectToken)
	form.Set("subject_token_type", subjectTokenType)
	form.Set("scope", req.Scope)
	if c.audience != "" {
		form.Set("audience", c.audience)
	}
	if req.RequestDetails != nil {
		data, err := json.Marshal(req.RequestDetails)
		if err != nil {
			return "", fmt.Errorf("marshal request_details: %w", err)
		}
		form.Set("request_details", string(data))
	}
	if req.RequestContext != nil {
		data, err := json.Marshal(req.RequestContext)
		if err != nil {
			return "", fmt.Errorf("marshal request_context: %w", err)
		}
		form.Set("request_context", string(data))
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint+"/token_endpoint", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	httpReq.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(httpReq)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		var errorResp struct {
			Error            string `json:"error"`
			ErrorDescription string `json:"error_description"`
		}
		if err := json.Unmarshal(body, &errorResp); err == nil && errorResp.Error != "" {
			if errorResp.ErrorDescription != "" {
				return "", fmt.Errorf("TTS error: %s - %s", errorResp.Error, errorResp.ErrorDescription)
			}
			return "", fmt.Errorf("TTS error: %s", errorResp.Error)
		}
		return "", fmt.Errorf("TTS exchange failed with status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var tokenResp struct {
		AccessToken     string `json:"access_token"`
		IssuedTokenType string `json:"issued_token_type"`
		TokenType       string `json:"token_type"`
		ExpiresIn       int    `json:"expires_in,omitempty"`
	}
	if err := json.Unmarshal(body, &tokenResp); err != nil {
		return "", fmt.Errorf("decode TTS response: %w", err)
	}
	if tokenResp.AccessToken == "" {
		return "", errors.New("TTS response missing access_token")
	}
	return tokenResp.AccessToken, nil
}
