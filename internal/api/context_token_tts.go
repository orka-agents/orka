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

// ContextTokenTTSConfig controls optional transaction-token TTS integration.
type ContextTokenTTSConfig = contexttoken.TTSConfig

// NewContextTokenTTSConfig builds TTS integration config from flag/env values.
func NewContextTokenTTSConfig(endpoint, audience, timeout, tokenSource, childTTL, toolTTL string) (ContextTokenTTSConfig, error) {
	return contexttoken.NewTTSConfig(endpoint, audience, timeout, tokenSource, childTTL, toolTTL)
}

// ContextTokenExchangeRequest describes a TTS exchange or replacement request.
type ContextTokenExchangeRequest = contexttoken.ExchangeRequest

// ContextTokenExchanger exchanges subject tokens for transaction tokens.
type ContextTokenExchanger = contexttoken.Exchanger

// ContextTokenTTSClient is a strict transaction-token TTS client.
type ContextTokenTTSClient = contexttoken.TTSClient

// NewContextTokenTTSClient creates a TTS client for the configured endpoint.
func NewContextTokenTTSClient(cfg ContextTokenTTSConfig) (*ContextTokenTTSClient, error) {
	return contexttoken.NewTTSClient(cfg)
}
