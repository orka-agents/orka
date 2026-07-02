/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import "github.com/orka-agents/orka/internal/contexttoken"

const (
	// ContextTokenTTSTokenSourceNone disables Orka-initiated token exchanges.
	ContextTokenTTSTokenSourceNone = contexttoken.TTSTokenSourceNone
	// ContextTokenTTSTokenSourceServiceAccount uses Orka's ServiceAccount identity for exchanges.
	ContextTokenTTSTokenSourceServiceAccount = contexttoken.TTSTokenSourceServiceAccount
	// ContextTokenTTSTokenSourceIncoming uses the incoming subject token for exchanges.
	ContextTokenTTSTokenSourceIncoming = contexttoken.TTSTokenSourceIncoming
)

// ContextTokenTTSConfig controls optional kontxt TTS token exchange integration.
type ContextTokenTTSConfig = contexttoken.TTSConfig

// NewContextTokenTTSConfig builds TTS integration config from flag/env values.
func NewContextTokenTTSConfig(url, audience, timeout, tokenSource, childTTL, toolTTL string) (ContextTokenTTSConfig, error) {
	return contexttoken.NewTTSConfig(url, audience, timeout, tokenSource, childTTL, toolTTL)
}

// ContextTokenExchangeRequest describes a TTS exchange or replacement request.
type ContextTokenExchangeRequest = contexttoken.ExchangeRequest

// ContextTokenExchanger exchanges subject tokens for kontxt TxTokens.
type ContextTokenExchanger = contexttoken.Exchanger

// KontxtTTSClient is an RFC 8693 Token Transaction Service client.
type KontxtTTSClient = contexttoken.KontxtTTSClient

// NewKontxtTTSClient creates a TTS client for the configured endpoint.
func NewKontxtTTSClient(cfg ContextTokenTTSConfig) (*KontxtTTSClient, error) {
	return contexttoken.NewKontxtTTSClient(cfg)
}
