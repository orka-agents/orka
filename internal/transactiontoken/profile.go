/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package transactiontoken defines Orka's strict, vendor-neutral OAuth
// Transaction Token profile. It intentionally contains no provider SDK or
// provider-specific behavior.
package transactiontoken

import (
	"errors"
	"net/url"
	"strings"
	"time"
)

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

	// TokenSecretKey stores a task-bound transaction token in an owner-referenced Secret.
	TokenSecretKey = "token"
	// SubjectSecretKey stores the verified caller token in a controller-only renewal authority Secret.
	SubjectSecretKey = "subject-token"

	// WorkloadSecretPurpose labels the Task-annotated Secret mounted into the workload.
	WorkloadSecretPurpose = "task-token-workload"
	// AuthoritySecretPurpose labels the controller-only Secret that retains renewal authority.
	AuthoritySecretPurpose = "task-token-renewal"
	// PlaceholderSecretPurpose labels an ownerless delegated-child workload placeholder.
	PlaceholderSecretPurpose = "task-token-placeholder"
	// SubjectTokenTypeSecretKey persists the exact subject type used for every renewal.
	SubjectTokenTypeSecretKey = "subject-token-type"
	// RequestDetailsSecretKey persists safe delegated request_details for later controller exchange.
	RequestDetailsSecretKey = "request-details"
	// ParentUIDAnnotation binds an ownerless placeholder to the exact parent Task UID.
	ParentUIDAnnotation = "orka.ai/transaction-token-parent-uid"
	// ParentNamespaceAnnotation binds an ownerless placeholder to the exact parent Task namespace.
	ParentNamespaceAnnotation = "orka.ai/transaction-token-parent-namespace"
	// PlaceholderUIDAnnotation binds delegated cleanup to the exact placeholder Secret instance.
	PlaceholderUIDAnnotation = "orka.ai/transaction-token-placeholder-uid"

	// MinimumProjectedTokenRequestedTTL is the minimum requested TTS lifetime for tokens delivered through Kubernetes Secret volumes.
	MinimumProjectedTokenRequestedTTL = 5 * time.Minute
	// MinimumProjectedTokenRemainingLifetime reserves propagation time after exchange before a mounted token expires.
	MinimumProjectedTokenRemainingLifetime = 4 * time.Minute
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

// ValidateSubjectTokenType verifies the RFC 8693 subject token type is a
// non-empty absolute URI with no surrounding or embedded whitespace.
func ValidateSubjectTokenType(value string) error {
	if value == "" || strings.TrimSpace(value) != value || strings.ContainsAny(value, " \t\r\n") {
		return errors.New("transaction token subject token type is invalid")
	}
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" {
		return errors.New("transaction token subject token type is invalid")
	}
	return nil
}
