// Copyright (c) Orka contributors. Licensed under the MIT License.
// Package enforcementprovider defines enforcement-provider orchestration.
package enforcementprovider

import (
	"errors"
	"time"
)

const (
	// ProtocolVersion identifies the incompatible canonical transcript and wire contract.
	ProtocolVersion       = "orka.enforcement-provider/v2"
	transportFrameVersion = 2
)

type Descriptor struct {
	ProviderID          string
	EndpointID          string
	Algorithm           string
	ProtocolVersions    []string
	Capabilities        []string
	Epoch               uint64
	IssuedAt, ExpiresAt time.Time
	KeyID               string
	PublicKey           []byte
	Signature           []byte
}

// TrustProfile is operator-controlled provider trust configuration.
// Descriptor-supplied keys are never accepted unless they exactly match this profile.
type TrustProfile struct {
	ProviderID, EndpointID, Algorithm, KeyID string
	PublicKey                                []byte
	MinimumEpoch                             uint64
}
type Context struct {
	RequestID, AttemptID, WorkloadID, ProviderID, Audience, Challenge, IdempotencyKey, PolicyDigest string
	PolicyBytes                                                                                     []byte
	RequiredCapabilities                                                                            []string
}
type Authorization struct {
	ID                             string
	Context                        Context
	IssuedAt, NotBefore, ExpiresAt time.Time
	Epoch                          uint64
}
type ReadBack struct{ ActivationID, WorkloadID, PolicyDigest, ConfigurationDigest string }
type Proof struct {
	ID, AuthorizationID, ActivationID, WorkloadID, ProviderID, Audience, Challenge, PolicyDigest, ReadBackDigest, AuthorizationDigest, KeyID string
	Epoch                                                                                                                                    uint64
	IssuedAt, ExpiresAt                                                                                                                      time.Time
	Signature                                                                                                                                []byte
}
type Status struct{ State, Detail string }

type PrepareRequest struct{ Authorization Authorization }
type ActivateRequest struct {
	Authorization Authorization
	PreparationID string
}
type ReleaseRequest struct{ ActivationID, AuthorizationID, ProofID, ReleaseID string }
type ReconcileRequest struct{ AttemptID string }
type CleanupRequest struct{ AttemptID, ActivationID, CleanupID string }

var ErrProviderNotFound = errors.New("provider resource not found")

type ProviderError struct{ Code, Message string }

func (e *ProviderError) Error() string { return "provider " + e.Code + ": " + e.Message }
func (e *ProviderError) Is(target error) bool {
	return target == ErrProviderNotFound && e.Code == "not_found"
}

type Provider interface {
	DiscoverDescriptor() (Descriptor, error)
	// Prepare and Activate must be idempotent for the same authorization and
	// preparation ID so recovery can close provider-success/store-failure gaps.
	Prepare(PrepareRequest) (string, error)
	Activate(ActivateRequest) (string, error)
	ReadBack(string) (ReadBack, error)
	GetProof(string) (Proof, error)
	// Release must be idempotent for an unchanged ReleaseID.
	Release(ReleaseRequest) error
	GetStatus(string) (Status, error)
	Reconcile(ReconcileRequest) (Status, error)
	// Cleanup must be idempotent for an unchanged CleanupID and treat an already-absent resource as success.
	Cleanup(CleanupRequest) error
}
