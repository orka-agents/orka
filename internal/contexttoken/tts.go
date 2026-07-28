/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package contexttoken

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/tokenexchange"
	"github.com/orka-agents/orka/internal/transactiontoken"
)

const (
	// HeaderName is the default HTTP header used by transaction tokens.
	HeaderName = transactiontoken.HeaderName

	// TTSTokenSourceNone disables Orka-initiated token exchanges.
	TTSTokenSourceNone = "none"
	// TTSTokenSourceServiceAccount uses Orka's ServiceAccount identity for exchanges.
	TTSTokenSourceServiceAccount = "serviceAccount"
	// TTSTokenSourceIncoming uses the incoming subject token for exchanges.
	TTSTokenSourceIncoming = "incoming"
)

const (
	metricResultFailure        = "failure"
	metricReasonInvalidRequest = "invalid_request"

	defaultTTSTimeout     = 5 * time.Second
	defaultChildTokenTTL  = 5 * time.Minute
	defaultToolTokenTTL   = 2 * time.Minute
	defaultTTSTokenSource = TTSTokenSourceServiceAccount
)

// TTSConfig controls optional vendor-neutral transaction-token service integration.
type TTSConfig struct {
	Endpoint      string
	Audience      string
	Timeout       time.Duration
	TokenSource   string
	ChildTokenTTL time.Duration
	ToolTokenTTL  time.Duration
}

// Enabled reports whether Orka should perform TTS exchanges.
func (c TTSConfig) Enabled() bool {
	return c.Endpoint != "" && c.TokenSource != TTSTokenSourceNone
}

// NewTTSConfig builds TTS integration config from flag/env values. Endpoint is
// the exact OAuth token endpoint; Orka never appends a provider-specific path.
func NewTTSConfig(endpoint, audience, timeout, tokenSource, childTTL, toolTTL string) (TTSConfig, error) {
	rawEndpoint := endpoint
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
		if tokenSource == TTSTokenSourceNone {
			return TTSConfig{TokenSource: TTSTokenSourceNone}, nil
		}
		if audience != "" || timeout != "" || childTTL != "" || toolTTL != "" || tokenSource != defaultTTSTokenSource {
			return TTSConfig{}, errors.New("context-token-tts-endpoint is required when TTS settings are provided")
		}
		return TTSConfig{TokenSource: TTSTokenSourceNone}, nil
	}
	if endpoint != rawEndpoint {
		return TTSConfig{}, errors.New("context-token-tts-endpoint must not contain surrounding whitespace")
	}
	parsedEndpoint, err := url.Parse(endpoint)
	if err != nil || parsedEndpoint.Host == "" || (parsedEndpoint.Scheme != "http" && parsedEndpoint.Scheme != "https") ||
		parsedEndpoint.User != nil || parsedEndpoint.Fragment != "" || strings.Contains(endpoint, "#") || parsedEndpoint.String() != endpoint {
		return TTSConfig{}, errors.New("context-token-tts-endpoint must be an absolute HTTP(S) URL without userinfo or fragment")
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
		Endpoint:      endpoint,
		Audience:      audience,
		Timeout:       timeoutDuration,
		TokenSource:   tokenSource,
		ChildTokenTTL: childTTLDuration,
		ToolTokenTTL:  toolTTLDuration,
	}, nil
}

// SubjectTokenTypeForSource returns the default RFC 8693 subject token type
// for an Orka TTS token source.
func SubjectTokenTypeForSource(tokenSource string) string {
	switch tokenSource {
	case TTSTokenSourceServiceAccount:
		return transactiontoken.SubjectTokenTypeAccessToken
	default:
		return transactiontoken.SubjectTokenTypeTransactionToken
	}
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
	RequestedTTL     time.Duration
	RequestDetails   map[string]any
	RequestContext   map[string]any
}

// Exchanger exchanges subject tokens for transaction tokens.
type Exchanger interface {
	Exchange(ctx context.Context, req ExchangeRequest) (string, error)
}

// TTSClient is a strict RFC 8693 transaction-token service client.
type TTSClient struct {
	endpoint  string
	audience  string
	timeout   time.Duration
	exchanger tokenexchange.Exchanger
}

// NewTTSClient creates a TTS client for the configured exact endpoint.
func NewTTSClient(cfg TTSConfig) (*TTSClient, error) {
	if !cfg.Enabled() {
		return nil, errors.New("context token TTS is not configured")
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = defaultTTSTimeout
	}
	return &TTSClient{
		endpoint:  strings.TrimSpace(cfg.Endpoint),
		audience:  strings.TrimSpace(cfg.Audience),
		timeout:   timeout,
		exchanger: tokenexchange.NewClient(tokenexchange.ClientOptions{}),
	}, nil
}

// NewTTSClientWithExchanger creates a TTS client using an injected exchange
// implementation. It is intended for conformance tests and internal adapters.
func NewTTSClientWithExchanger(cfg TTSConfig, exchanger tokenexchange.Exchanger) (*TTSClient, error) {
	client, err := NewTTSClient(cfg)
	if err != nil {
		return nil, err
	}
	if exchanger == nil {
		return nil, errors.New("token exchanger is required")
	}
	client.exchanger = exchanger
	return client, nil
}

// Exchange exchanges the subject token for a transaction token and requires
// the strict txn_token/N_A response profile.
func (c *TTSClient) Exchange(ctx context.Context, req ExchangeRequest) (token string, err error) {
	start := time.Now()
	result, reason := "success", "ok"
	defer func() {
		metrics.RecordContextTokenTTSExchange(result, reason, time.Since(start).Seconds())
	}()

	if c == nil || c.exchanger == nil || strings.TrimSpace(c.endpoint) == "" {
		result, reason = metricResultFailure, "not_configured"
		return "", errors.New("context token TTS is not configured")
	}
	if strings.TrimSpace(req.SubjectToken) == "" {
		result, reason = metricResultFailure, metricReasonInvalidRequest
		return "", errors.New("context token TTS subject token is required")
	}

	additional := make(map[string]string, 3)
	if req.RequestedTTL > 0 {
		ttlSeconds := int64((req.RequestedTTL + time.Second - 1) / time.Second)
		ttlSeconds = max(ttlSeconds, 1)
		additional["requested_expires_in"] = fmt.Sprintf("%d", ttlSeconds)
	}
	if req.RequestDetails != nil {
		data, marshalErr := json.Marshal(req.RequestDetails)
		if marshalErr != nil {
			result, reason = metricResultFailure, metricReasonInvalidRequest
			return "", fmt.Errorf("marshal request_details: %w", marshalErr)
		}
		additional["request_details"] = string(data)
	}
	if req.RequestContext != nil {
		data, marshalErr := json.Marshal(req.RequestContext)
		if marshalErr != nil {
			result, reason = metricResultFailure, metricReasonInvalidRequest
			return "", fmt.Errorf("marshal request_context: %w", marshalErr)
		}
		additional["request_context"] = string(data)
	}

	subjectTokenType := strings.TrimSpace(req.SubjectTokenType)
	if subjectTokenType == "" {
		subjectTokenType = transactiontoken.SubjectTokenTypeTransactionToken
	}
	audiences := []string(nil)
	if c.audience != "" {
		audiences = []string{c.audience}
	}
	exchangeResult, exchangeErr := c.exchanger.Exchange(ctx, tokenexchange.Request{
		Adapter:                 transactiontoken.ProfileName,
		Endpoint:                c.endpoint,
		Timeout:                 c.timeout,
		GrantType:               tokenexchange.GrantTypeTokenExchange,
		SubjectToken:            req.SubjectToken,
		SubjectTokenType:        subjectTokenType,
		SubjectExpiresAt:        unverifiedJWTExpiry(req.SubjectToken),
		Audiences:               audiences,
		Scopes:                  strings.Fields(req.Scope),
		RequestedTokenType:      transactiontoken.RequestedTokenType,
		AdditionalParameters:    additional,
		ExpectedIssuedTokenType: transactiontoken.RequestedTokenType,
		RequiredTokenType:       transactiontoken.ResponseTokenType,
		CacheNamespace:          transactiontoken.ProfileName,
	})
	if exchangeErr != nil {
		result, reason = metricResultFailure, "exchange_error"
		return "", exchangeErr
	}
	return exchangeResult.AccessToken, nil
}

func unverifiedJWTExpiry(token string) time.Time {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}
	}
	var claims struct {
		Expiration json.Number `json:"exp"`
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.UseNumber()
	if decoder.Decode(&claims) != nil || claims.Expiration == "" {
		return time.Time{}
	}
	seconds, err := claims.Expiration.Int64()
	if err != nil || seconds <= 0 {
		return time.Time{}
	}
	return time.Unix(seconds, 0)
}
