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
	"net/url"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/tokenexchange"
	"github.com/orka-agents/orka/internal/transactiontoken"
)

const (
	HeaderName = transactiontoken.HeaderName

	TTSTokenSourceNone           = "none"
	TTSTokenSourceServiceAccount = "serviceAccount"
	TTSTokenSourceIncoming       = "incoming"
)

const (
	metricResultFailure        = "failure"
	metricReasonInvalidRequest = "invalid_request"

	defaultTTSTimeout     = 5 * time.Second
	defaultChildTokenTTL  = 5 * time.Minute
	defaultToolTokenTTL   = 2 * time.Minute
	defaultTTSTokenSource = TTSTokenSourceServiceAccount
)

type TTSConfig struct {
	Endpoint      string
	Audience      string
	Timeout       time.Duration
	TokenSource   string
	ChildTokenTTL time.Duration
	ToolTokenTTL  time.Duration
}

func (c TTSConfig) Enabled() bool {
	return c.Endpoint != "" && c.TokenSource != TTSTokenSourceNone
}

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

type ExchangeRequest struct {
	SubjectToken     string
	SubjectTokenType string
	Scope            string
	RequestedTTL     time.Duration
	RequestDetails   map[string]any
	RequestContext   map[string]any
}

type Exchanger interface {
	Exchange(ctx context.Context, req ExchangeRequest) (string, error)
}

type TTSClient struct {
	endpoint  string
	audience  string
	timeout   time.Duration
	exchanger tokenexchange.Exchanger
}

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
		SubjectExpiresAt:        tokenexchange.UnverifiedJWTExpiry(req.SubjectToken),
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
