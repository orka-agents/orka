package v1

import (
	"context"
	"strings"
	"time"
)

type SessionLineage struct {
	Namespace         string
	SessionName       string
	NamespaceUID      string
	SessionUID        string
	ContractVersion   string
	LineageGeneration int64
	RuntimeIdentity   string
	ConfigDigest      string
	Version           int64
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

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

type ClaimSessionLineageRequest struct {
	Namespace         string
	SessionName       string
	NamespaceUID      string
	SessionUID        string
	ContractVersion   string
	LineageGeneration int64
	RuntimeIdentity   string
	ConfigDigest      string

	EstablishIfAbsent bool
}

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

type SessionLineageStore interface {
	ProjectSessionLineage(ctx context.Context, lineage SessionLineage) (*SessionLineage, error)

	GetSessionLineage(ctx context.Context, namespace, sessionName string) (*SessionLineage, error)
}
