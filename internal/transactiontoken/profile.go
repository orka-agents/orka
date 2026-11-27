/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package transactiontoken

const (
	ProfileName = "transaction-token"
	HeaderName  = "Txn-Token"
	JWTType     = "txntoken+jwt"

	GrantTypeTokenExchange           = "urn:ietf:params:oauth:grant-type:token-exchange"
	RequestedTokenType               = "urn:ietf:params:oauth:token-type:txn_token"
	SubjectTokenTypeTransactionToken = RequestedTokenType
	SubjectTokenTypeAccessToken      = "urn:ietf:params:oauth:token-type:access_token"
	ResponseTokenType                = "N_A"
)

var RequiredClaims = []string{"sub", "exp", "iat", "txn", "scope", "req_wl"}

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
