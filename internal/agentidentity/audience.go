// Copyright (c) 2026. MIT License - see LICENSE file for details.

package agentidentity

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

// requireAudience checks that a requester's token is addressed to the
// identity's blueprint, so a Connection linked for another client can never
// be exchanged. The token came from Orka's own custody, so only its claims
// are read here; the issuer verifies its signature during the exchange.
func requireAudience(token, blueprintClientID string) error {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return errors.New("the requester's token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return errors.New("the requester's token has an unreadable payload")
	}
	var claims struct {
		Audience json.RawMessage `json:"aud"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return errors.New("the requester's token has unreadable claims")
	}
	var audiences []string
	var single string
	if err := json.Unmarshal(claims.Audience, &single); err == nil {
		audiences = []string{single}
	} else if err := json.Unmarshal(claims.Audience, &audiences); err != nil {
		return errors.New("the requester's token has no audience")
	}
	blueprint := strings.ToLower(strings.TrimSpace(blueprintClientID))
	for _, audience := range audiences {
		audience = strings.ToLower(strings.TrimSpace(audience))
		if audience == blueprint || audience == "api://"+blueprint {
			return nil
		}
	}
	return errors.New("the requester's linked token is not addressed to the identity's blueprint; link the Connection through the blueprint's scope")
}
