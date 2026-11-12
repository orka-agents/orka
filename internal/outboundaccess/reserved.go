// Copyright (c) 2026. MIT License - see LICENSE file for details.

package outboundaccess

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// ReservedServiceAccount names a ServiceAccount no OutboundAccessPolicy may
// request tokens for. The zero value reserves nothing.
type ReservedServiceAccount struct {
	Namespace string
	Name      string
}

// Reserves reports whether the ServiceAccount namespace/name is reserved.
func (r ReservedServiceAccount) Reserves(namespace, name string) bool {
	return r.Name != "" && r.Namespace == namespace && r.Name == name
}

var errReservedServiceAccount = errors.New("direct token sources must not use the controller's own ServiceAccount")

// ValidateReservedServiceAccount reports a direct policy whose subject or
// actor token would be requested for the reserved ServiceAccount.
func ValidateReservedServiceAccount(policy *corev1alpha1.OutboundAccessPolicy, reserved ReservedServiceAccount) *Issue {
	if policy == nil || policy.Spec.Direct == nil {
		return nil
	}
	for _, source := range []*corev1alpha1.OutboundTokenSource{&policy.Spec.Direct.Subject, policy.Spec.Direct.Actor} {
		if source != nil && source.Source == corev1alpha1.OutboundTokenSourceServiceAccount && source.ServiceAccountRef != nil &&
			reserved.Reserves(policy.Namespace, source.ServiceAccountRef.Name) {
			return invalid(errReservedServiceAccount.Error())
		}
	}
	return nil
}

// ServiceAccountFromToken returns the ServiceAccount a Kubernetes
// ServiceAccount token was issued to, from its subject. The token is the
// process's own mounted token, so its signature is not checked.
func ServiceAccountFromToken(token string) (ReservedServiceAccount, error) {
	parts := strings.Split(strings.TrimSpace(token), ".")
	if len(parts) != 3 {
		return ReservedServiceAccount{}, errors.New("the ServiceAccount token is not a JWT")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ReservedServiceAccount{}, errors.New("the ServiceAccount token payload is not base64url")
	}
	var claims struct {
		Subject string `json:"sub"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ReservedServiceAccount{}, errors.New("the ServiceAccount token payload is not JSON")
	}
	fields := strings.Split(claims.Subject, ":")
	if len(fields) != 4 || fields[0] != "system" || fields[1] != "serviceaccount" || fields[2] == "" || fields[3] == "" {
		return ReservedServiceAccount{}, errors.New("the token was not issued to a ServiceAccount")
	}
	return ReservedServiceAccount{Namespace: fields[2], Name: fields[3]}, nil
}
