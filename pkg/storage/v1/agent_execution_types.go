package v1

import (
	"context"
	"strings"
	"time"
)

// SessionLineage durably records the execution protocol and runtime identity
// of one conversation Session. LineageGeneration is independent of the Session
// mutation-lease generation and any v2 RuntimeSession generation.
type SessionLineage struct {
	Namespace    string
	SessionName  string
	NamespaceUID string
	SessionUID   string
	// ContractVersion is orka.harness.v2.
	ContractVersion   string
	LineageGeneration int64
	// RuntimeIdentity is the built-in runtime type or the AgentRuntime UID.
	RuntimeIdentity string
	// ConfigDigest is the configuration/snapshot digest recorded when the
	// lineage was established.
	ConfigDigest string
	Version      int64
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Validate rejects an incomplete authoritative Session lineage.
func (l SessionLineage) Validate() error {
	claim := ClaimSessionLineageRequest{
		Namespace: l.Namespace, SessionName: l.SessionName, NamespaceUID: l.NamespaceUID,
		SessionUID: l.SessionUID, ContractVersion: l.ContractVersion,
		LineageGeneration: l.LineageGeneration, RuntimeIdentity: l.RuntimeIdentity, ConfigDigest: l.ConfigDigest,
	}
	if err := claim.Validate(); err != nil {
		return err
	}
	if l.LineageGeneration < 1 {
		return ValidationErrorf("session lineage generation must be at least 1")
	}
	if l.Version < 1 {
		return ValidationErrorf("session lineage projection version must be at least 1")
	}
	if l.CreatedAt.IsZero() || l.UpdatedAt.IsZero() {
		return ValidationErrorf("session lineage creation and update times are required")
	}
	return nil
}

// ClaimSessionLineageRequest atomically establishes or verifies a Session
// lineage. Callers must invoke it under the same serialization that acquires
// the Session mutation lease so two concurrent first-use Tasks cannot
// establish different runtime identities.
type ClaimSessionLineageRequest struct {
	Namespace       string
	SessionName     string
	NamespaceUID    string
	SessionUID      string
	ContractVersion string
	// LineageGeneration is independent of mutation-lease and runtime-session
	// generations. Ordinary first use establishes generation 1.
	LineageGeneration int64
	RuntimeIdentity   string
	ConfigDigest      string

	// EstablishIfAbsent permits creating the lineage row. It must be true only
	// when the caller has proven the Session is fresh or is a controller-owned
	// Gateway Session whose first message was admitted atomically.
	EstablishIfAbsent bool
}

// Validate rejects incomplete lineage claims.
func (r ClaimSessionLineageRequest) Validate() error {
	switch {
	case strings.TrimSpace(r.Namespace) == "":
		return ValidationErrorf("session lineage namespace is required")
	case strings.TrimSpace(r.SessionName) == "":
		return ValidationErrorf("session lineage session name is required")
	case strings.TrimSpace(r.NamespaceUID) == "":
		return ValidationErrorf("session lineage namespace UID is required")
	case strings.TrimSpace(r.SessionUID) == "":
		return ValidationErrorf("session lineage session UID is required")
	case r.ContractVersion != "orka.harness.v2":
		return ValidationErrorf("session lineage contract version %q must be orka.harness.v2", r.ContractVersion)
	case r.LineageGeneration < 1:
		return ValidationErrorf("session lineage generation must be at least 1")
	case strings.TrimSpace(r.RuntimeIdentity) == "":
		return ValidationErrorf("session lineage runtime identity is required")
	}
	if err := ValidateCanonicalDigest("session lineage config digest", r.ConfigDigest); err != nil {
		return err
	}
	return nil
}

// SessionLineageStore persists a payload projection of Kubernetes-authoritative
// Session protocol/runtime lineage. Projection failure may block dispatch but
// never changes or releases the Kubernetes lineage/Lease authority.
type SessionLineageStore interface {
	// ProjectSessionLineage idempotently stores the exact authoritative record.
	// Any mismatch returns ErrConflict and must be repaired explicitly; SQLite
	// never adjudicates which lineage owns a Session.
	ProjectSessionLineage(ctx context.Context, lineage SessionLineage) (*SessionLineage, error)

	// GetSessionLineage returns the lineage for one Session, or ErrNotFound.
	GetSessionLineage(ctx context.Context, namespace, sessionName string) (*SessionLineage, error)
}
