/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package transactiontoken defines Orka's strict, vendor-neutral OAuth
// Transaction Token profile. It intentionally contains no provider SDK or
// provider-specific behavior.
package transactiontoken

const (
	// ProfileName is the only context-token profile accepted by Orka.
	ProfileName = "transaction-token"
	// HeaderName is the default HTTP header carrying a transaction token.
	HeaderName = "Txn-Token"
	// JWTType is the required JWT typ header for transaction tokens.
	JWTType = "txntoken+jwt"

	// GrantTypeTokenExchange is the RFC 8693 token exchange grant.
	GrantTypeTokenExchange = "urn:ietf:params:oauth:grant-type:token-exchange"
	// RequestedTokenType is the OAuth token type requested for transaction tokens.
	RequestedTokenType = "urn:ietf:params:oauth:token-type:txn_token"
	// SubjectTokenTypeTransactionToken identifies a transaction token subject.
	SubjectTokenTypeTransactionToken = RequestedTokenType
	// SubjectTokenTypeAccessToken identifies an OAuth access token subject.
	SubjectTokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
	// ResponseTokenType is the token_type returned by a transaction-token service.
	ResponseTokenType = "N_A"
)

// RequiredClaims are mandatory in every transaction token accepted by Orka.
var RequiredClaims = []string{"sub", "exp", "iat", "txn", "scope", "req_wl"}

// Claims is the vendor-neutral transaction-token claim set used by Orka's
// conformance fixtures and integrations.
type Claims struct {
	Issuer             string         `json:"iss"`
	Subject            string         `json:"sub"`
	Audience           string         `json:"aud"`
	Expiration         int64          `json:"exp"`
	IssuedAt           int64          `json:"iat"`
	NotBefore          int64          `json:"nbf,omitempty"`
	TransactionID      string         `json:"txn"`
	Scope              string         `json:"scope"`
	RequestingWorkload string         `json:"req_wl"`
	TransactionContext map[string]any `json:"tctx,omitempty"`
	RequesterContext   map[string]any `json:"rctx,omitempty"`
}
