package v2

import (
	"errors"
	"strings"
)

// Credential bootstrap is the one-time seeding step for provider-hosted
// supervisors that boot credential-free: execution-workspace providers
// materialize the workload from a provider-visible template (and may build a
// golden snapshot by booting and checkpointing an instance), so no credential
// may exist in the process or its template until the controller seeds it. The
// nonce is public per-pool entropy that only fences which controller may seed
// the exact workload; the seeded values travel over the same cluster-trusted
// channel that carries every subsequent Authorization header.

const (
	// CredentialBootstrapPath is the one-time, idempotent seeding endpoint.
	CredentialBootstrapPath = "/v2/credential-bootstrap"
	// CredentialBootstrapNonceHeader carries the pool bootstrap nonce.
	CredentialBootstrapNonceHeader = "X-Orka-Credential-Bootstrap-Nonce"
)

// CredentialBootstrapRequest is the seeding payload. First write wins; an
// identical repeat is acknowledged; a different payload conflicts.
type CredentialBootstrapRequest struct {
	ControllerToken  string `json:"controllerToken"`
	CapabilitySecret string `json:"capabilitySecret"`
	ProviderToken    string `json:"providerToken"`
}

// Validate enforces the same minimum credential shapes as supervisor boot.
func (r CredentialBootstrapRequest) Validate() error {
	if len(strings.TrimSpace(r.ControllerToken)) < 32 {
		return errors.New("controllerToken must be at least 32 bytes")
	}
	if len(strings.TrimSpace(r.CapabilitySecret)) < MinCapabilitySecretBytes {
		return errors.New("capabilitySecret is too short")
	}
	if strings.TrimSpace(r.ProviderToken) == "" {
		return errors.New("providerToken is required")
	}
	return nil
}
