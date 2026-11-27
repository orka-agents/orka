/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import (
	"context"
	"errors"
	"strings"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const ConnectorFieldNamespace = "namespace"

var ErrConnectorCustodyTombstoned = storagev1.ErrConnectorCustodyTombstoned

var ErrConnectorCompletionCommitted = errors.New("connector completion was already committed")

var ErrConnectorConsentSuperseded = storagev1.ErrConnectorConsentSuperseded

var ErrConnectorRetiredLimit = storagev1.ErrConnectorRetiredLimit

type ConnectorCredentialRef struct {
	ConnectionUID string
	Namespace     string
	Name          string
	SubjectDigest string
	Provider      string
}

func (r ConnectorCredentialRef) Validate() error {
	for _, field := range []struct{ name, value string }{
		{"connection UID", r.ConnectionUID},
		{ConnectorFieldNamespace, r.Namespace},
		{"name", r.Name},
		{"subject digest", r.SubjectDigest},
		{"provider", r.Provider},
	} {
		if strings.TrimSpace(field.value) == "" {
			return errors.New("connector credential " + field.name + " is required")
		}
	}
	return nil
}

type ConnectorCredential struct {
	AccessToken      string
	RefreshToken     string
	TokenType        string
	ExpiresAt        time.Time
	Scopes           []string
	AuthorityDigest  string
	RevocationDigest string
	GrantSequence    int64
	UpdatedAt        time.Time
	Version          int64
}

type ConnectorGrant struct {
	Sequence int64
	Scopes   []string
}

type ConnectorGrantReader interface {
	GetConnectorCredentialGrant(ctx context.Context, ref ConnectorCredentialRef) (ConnectorGrant, error)
}

type ConnectorCredentialStore interface {
	PutConnectorCredential(ctx context.Context, ref ConnectorCredentialRef, credential ConnectorCredential) error
	GetConnectorCredential(ctx context.Context, ref ConnectorCredentialRef) (ConnectorCredential, error)
	ListRetiredConnectorCredentials(ctx context.Context, ref ConnectorCredentialRef) ([]ConnectorCredential, error)
	TombstoneConnectorCustody(ctx context.Context, connectionUID string) error
	ReplaceConnectorCredential(ctx context.Context, ref ConnectorCredentialRef, credential ConnectorCredential, expectedVersion int64) error
	ShredConnectorCredential(ctx context.Context, connectionUID string, expectedVersion int64) error
	RetireConnectorCredential(ctx context.Context, ref ConnectorCredentialRef, credential ConnectorCredential) error
	DeleteConnectorCredential(ctx context.Context, connectionUID string) error
}

type ConnectorConsent struct {
	Nonce            string
	ConnectionUID    string
	Namespace        string
	Name             string
	SubjectDigest    string
	Provider         string
	Mode             string
	CodeVerifier     string
	AuthorityDigest  string
	RevocationDigest string
	Scopes           []string
	ExpiresAt        time.Time
	Sequence         int64
}

type ConnectorCompletion struct {
	Nonce                  string
	ConnectionUID          string
	Namespace              string
	Name                   string
	SubjectDigest          string
	Provider               string
	Mode                   string
	Credential             ConnectorCredential
	ExpiresAt              time.Time
	ConsentAuthorityDigest string
	Committed              bool
	ConsentSequence        int64
}

type ConnectorConsentStore interface {
	CreateConnectorConsent(ctx context.Context, consent ConnectorConsent) error
	ConsumeConnectorConsent(ctx context.Context, nonce string) (ConnectorConsent, error)
	CreateConnectorCompletion(ctx context.Context, completion ConnectorCompletion) error
	ConsumeConnectorCompletion(ctx context.Context, nonce string) (ConnectorCompletion, error)
	CommitConnectorCompletion(ctx context.Context, nonce string, ref ConnectorCredentialRef, credential ConnectorCredential) (ConnectorCredential, error)
	PeekConnectorCompletion(ctx context.Context, nonce string) (ConnectorCompletion, error)
	DeleteConnectorCompletion(ctx context.Context, nonce string) error
	ListConnectorCompletionsForConnection(ctx context.Context, connectionUID string) ([]ConnectorCompletion, error)
	DeleteConnectorConsentsForConnection(ctx context.Context, connectionUID string) error
}
