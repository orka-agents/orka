package enforcementprovider

import (
	"bytes"
	"errors"
)

var (
	errAuthorizationProviderBinding = errors.New("authorization provider is not bound to authenticated descriptor")
	errAuthorizationEpochBinding    = errors.New("authorization epoch is not bound to authenticated descriptor")
)

// equalContext compares every immutable request field. A matching attempt ID is
// not sufficient for an idempotent retry.
func equalContext(left, right Context) bool {
	return left.RequestID == right.RequestID &&
		left.AttemptID == right.AttemptID &&
		left.WorkloadID == right.WorkloadID &&
		left.ProviderID == right.ProviderID &&
		left.Audience == right.Audience &&
		left.Challenge == right.Challenge &&
		left.IdempotencyKey == right.IdempotencyKey &&
		left.PolicyDigest == right.PolicyDigest &&
		bytes.Equal(left.PolicyBytes, right.PolicyBytes) &&
		equalStringSlice(left.RequiredCapabilities, right.RequiredCapabilities)
}

func equalAuthorization(left, right Authorization) bool {
	return left.ID == right.ID &&
		equalContext(left.Context, right.Context) &&
		left.IssuedAt.Equal(right.IssuedAt) &&
		left.NotBefore.Equal(right.NotBefore) &&
		left.ExpiresAt.Equal(right.ExpiresAt) &&
		left.Epoch == right.Epoch
}

func bindAuthorizationToDescriptor(authorization Authorization, descriptor Descriptor, trust TrustProfile) error {
	if authorization.Context.ProviderID != descriptor.ProviderID || descriptor.ProviderID != trust.ProviderID {
		return errAuthorizationProviderBinding
	}
	if authorization.Epoch != descriptor.Epoch {
		return errAuthorizationEpochBinding
	}
	return nil
}
