/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	sdktts "github.com/aramase/kontxt/sdk/tts"
)

const (
	// ContextTokenTTSTokenSourceNone disables Orka-initiated token exchanges.
	ContextTokenTTSTokenSourceNone = "none"
	// ContextTokenTTSTokenSourceServiceAccount uses Orka's ServiceAccount identity for exchanges.
	ContextTokenTTSTokenSourceServiceAccount = "serviceAccount"
	// ContextTokenTTSTokenSourceIncoming uses the incoming subject token for exchanges.
	ContextTokenTTSTokenSourceIncoming = "incoming"

	defaultContextTokenTTSTimeout     = 5 * time.Second
	defaultContextTokenChildTokenTTL  = 5 * time.Minute
	defaultContextTokenToolTokenTTL   = 2 * time.Minute
	defaultContextTokenTTSTokenSource = ContextTokenTTSTokenSourceServiceAccount
)

// ContextTokenTTSConfig controls optional kontxt TTS token exchange integration.
type ContextTokenTTSConfig struct {
	URL           string
	Audience      string
	Timeout       time.Duration
	TokenSource   string
	ChildTokenTTL time.Duration
	ToolTokenTTL  time.Duration
}

// Enabled reports whether Orka has a TTS endpoint configured.
func (c ContextTokenTTSConfig) Enabled() bool {
	return c.URL != "" && c.TokenSource != ContextTokenTTSTokenSourceNone
}

// NewContextTokenTTSConfig builds TTS integration config from flag/env values.
func NewContextTokenTTSConfig(url, audience, timeout, tokenSource, childTTL, toolTTL string) (ContextTokenTTSConfig, error) {
	url = strings.TrimSpace(url)
	audience = strings.TrimSpace(audience)
	tokenSource = strings.TrimSpace(tokenSource)
	if tokenSource == "" {
		tokenSource = defaultContextTokenTTSTokenSource
	}
	if url == "" {
		if audience != "" || timeout != "" || childTTL != "" || toolTTL != "" || tokenSource != defaultContextTokenTTSTokenSource {
			return ContextTokenTTSConfig{}, errors.New("context-token-tts-url is required when TTS settings are provided")
		}
		return ContextTokenTTSConfig{TokenSource: ContextTokenTTSTokenSourceNone}, nil
	}

	timeoutDuration, err := parseOptionalDuration(timeout, defaultContextTokenTTSTimeout, "context-token-tts-timeout")
	if err != nil {
		return ContextTokenTTSConfig{}, err
	}
	childTTLDuration, err := parseOptionalDuration(childTTL, defaultContextTokenChildTokenTTL, "context-token-child-token-ttl")
	if err != nil {
		return ContextTokenTTSConfig{}, err
	}
	toolTTLDuration, err := parseOptionalDuration(toolTTL, defaultContextTokenToolTokenTTL, "context-token-tool-token-ttl")
	if err != nil {
		return ContextTokenTTSConfig{}, err
	}

	switch tokenSource {
	case ContextTokenTTSTokenSourceServiceAccount, ContextTokenTTSTokenSourceIncoming, ContextTokenTTSTokenSourceNone:
	default:
		return ContextTokenTTSConfig{}, fmt.Errorf("unsupported context-token TTS token source %q", tokenSource)
	}

	return ContextTokenTTSConfig{
		URL:           strings.TrimRight(url, "/"),
		Audience:      audience,
		Timeout:       timeoutDuration,
		TokenSource:   tokenSource,
		ChildTokenTTL: childTTLDuration,
		ToolTokenTTL:  toolTTLDuration,
	}, nil
}

func parseOptionalDuration(value string, fallback time.Duration, name string) (time.Duration, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("invalid %s: %w", name, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("invalid %s: must be positive", name)
	}
	return duration, nil
}

// ContextTokenExchangeRequest describes a TTS exchange or replacement request.
type ContextTokenExchangeRequest struct {
	SubjectToken     string
	SubjectTokenType string
	Scope            string
	RequestDetails   map[string]any
	RequestContext   map[string]any
}

// ContextTokenExchanger exchanges subject tokens for kontxt TxTokens.
type ContextTokenExchanger interface {
	Exchange(ctx context.Context, req ContextTokenExchangeRequest) (string, error)
}

// KontxtTTSClient adapts the upstream kontxt TTS SDK behind Orka's interface.
type KontxtTTSClient struct {
	client *sdktts.Client
}

// NewKontxtTTSClient creates a TTS client for the configured endpoint.
func NewKontxtTTSClient(cfg ContextTokenTTSConfig) (*KontxtTTSClient, error) {
	if !cfg.Enabled() {
		return nil, errors.New("context-token TTS is not enabled")
	}
	return &KontxtTTSClient{client: sdktts.NewClient(cfg.URL)}, nil
}

// Exchange exchanges a subject token for a TxToken using kontxt TTS.
func (c *KontxtTTSClient) Exchange(ctx context.Context, req ContextTokenExchangeRequest) (string, error) {
	if c == nil || c.client == nil {
		return "", errors.New("context-token TTS client is not configured")
	}
	return c.client.Exchange(ctx, &sdktts.ExchangeRequest{
		SubjectToken:     req.SubjectToken,
		SubjectTokenType: req.SubjectTokenType,
		Scope:            req.Scope,
		RequestDetails:   req.RequestDetails,
		RequestContext:   req.RequestContext,
	})
}
