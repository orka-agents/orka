/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import "github.com/orka-agents/orka/internal/contexttoken"

const (
	ContextTokenTTSTokenSourceNone           = contexttoken.TTSTokenSourceNone
	ContextTokenTTSTokenSourceServiceAccount = contexttoken.TTSTokenSourceServiceAccount
	ContextTokenTTSTokenSourceIncoming       = contexttoken.TTSTokenSourceIncoming
)

type ContextTokenTTSConfig = contexttoken.TTSConfig

func NewContextTokenTTSConfig(endpoint, audience, timeout, tokenSource, childTTL, toolTTL string) (ContextTokenTTSConfig, error) {
	return contexttoken.NewTTSConfig(endpoint, audience, timeout, tokenSource, childTTL, toolTTL)
}

type ContextTokenExchangeRequest = contexttoken.ExchangeRequest

type ContextTokenExchanger = contexttoken.Exchanger

type ContextTokenTTSClient = contexttoken.TTSClient
