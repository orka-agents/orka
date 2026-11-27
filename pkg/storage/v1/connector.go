package v1

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"strings"
	"time"
	"unicode"
)

const MaxRetiredConnectorConsentGrants = 16

const MaxRetiredConnectorRefreshGrants = 16

const ConnectorTombstoneRetention = 24 * time.Hour

var ErrConnectorCustodyTombstoned = errors.New("connector custody for this connection was deleted")

var ErrConnectorConsentSuperseded = errors.New("a newer consent for this connection superseded this one")

var ErrConnectorRetiredLimit = errors.New("this connection retains too many superseded grants; disconnect and link again")

func SealedValueDigest(nonce, ciphertext []byte) string {
	hash := sha256.New()
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(nonce)))
	hash.Write(length[:])
	hash.Write(nonce)
	hash.Write(ciphertext)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

type SealedConnectorCredential struct {
	ConnectionUID     string
	Namespace         string
	Name              string
	SubjectDigest     string
	Provider          string
	DataKeyNonce      []byte
	DataKeyCiphertext []byte
	Nonce             []byte
	Ciphertext        []byte
	ExpiresAt         time.Time
	GrantSequence     int64
	Version           int64
	UpdatedAt         time.Time
}

func (c SealedConnectorCredential) Validate() error {
	if err := requireConnectorFields("connector credential",
		connectorIdentityFields(c.ConnectionUID, c.Namespace, c.Name, c.SubjectDigest, c.Provider)); err != nil {
		return err
	}
	if len(c.DataKeyNonce) == 0 || len(c.DataKeyCiphertext) == 0 || len(c.Nonce) == 0 || len(c.Ciphertext) == 0 {
		return ValidationErrorf("sealed connector credential requires a sealed data key and ciphertext")
	}
	if c.GrantSequence < 1 {
		return ValidationErrorf("connector credential grant sequence must be positive")
	}
	return nil
}

type SealedRetiredConnectorCredential struct {
	ConnectionUID     string
	DataKeyNonce      []byte
	DataKeyCiphertext []byte
	Nonce             []byte
	Ciphertext        []byte
	RevocableUntil    time.Time
	ConsentGrant      bool
	RetiredAt         time.Time
}

type SealedConnectorConsent struct {
	Nonce              string
	ConnectionUID      string
	Namespace          string
	Name               string
	SubjectDigest      string
	Provider           string
	Mode               string
	AuthorityDigest    string
	RevocationDigest   string
	Scopes             []string
	Sequence           int64
	ExpiresAt          time.Time
	VerifierNonce      []byte
	VerifierCiphertext []byte
}

func (c SealedConnectorConsent) Validate() error {
	if err := requireConnectorFields("connector consent",
		connectorPendingFields(c.Nonce, c.ConnectionUID, c.Namespace, c.Name, c.SubjectDigest, c.Provider, c.Mode)); err != nil {
		return err
	}
	for _, scope := range c.Scopes {
		if scope == "" || strings.IndexFunc(scope, unicode.IsSpace) >= 0 {
			return ValidationErrorf("connector consent scopes must be non-empty tokens without whitespace")
		}
	}
	if c.Sequence < 1 {
		return ValidationErrorf("connector consent sequence must be positive")
	}
	if c.ExpiresAt.IsZero() {
		return ValidationErrorf("connector consent expiry is required")
	}
	if len(c.VerifierNonce) == 0 || len(c.VerifierCiphertext) == 0 {
		return ValidationErrorf("sealed connector consent requires a sealed verifier")
	}
	return nil
}

type SealedConnectorCompletion struct {
	Nonce           string
	ConnectionUID   string
	Namespace       string
	Name            string
	SubjectDigest   string
	Provider        string
	Mode            string
	ConsentSequence int64
	ExpiresAt       time.Time
	PayloadNonce    []byte
	Payload         []byte
}

func (c SealedConnectorCompletion) Validate() error {
	if err := requireConnectorFields("connector completion",
		connectorPendingFields(c.Nonce, c.ConnectionUID, c.Namespace, c.Name, c.SubjectDigest, c.Provider, c.Mode)); err != nil {
		return err
	}
	if c.ConsentSequence < 0 {
		return ValidationErrorf("connector completion consent sequence must not be negative")
	}
	if c.ExpiresAt.IsZero() {
		return ValidationErrorf("connector completion expiry is required")
	}
	if len(c.PayloadNonce) == 0 || len(c.Payload) == 0 {
		return ValidationErrorf("sealed connector completion requires a sealed payload")
	}
	return nil
}

func (c SealedConnectorCompletion) Ref() SealedConnectorCompletionRef {
	return SealedConnectorCompletionRef{Nonce: c.Nonce, Digest: SealedValueDigest(c.PayloadNonce, c.Payload)}
}

type SealedConnectorCompletionRef struct {
	Nonce  string
	Digest string
}

type ConnectorCustody struct {
	ConnectionUID            string
	Tombstoned               bool
	Credential               *SealedConnectorCredential
	Retired                  []SealedRetiredConnectorCredential
	Completions              []SealedConnectorCompletion
	LastGrantSequence        int64
	IssuedConsentSequence    int64
	CommittedConsentSequence int64
}

type ConnectorRetirement struct {
	RevocableUntil time.Time
}

type PutSealedConnectorCredentialRequest struct {
	Credential SealedConnectorCredential
	Now        time.Time
}

type ReplaceSealedConnectorCredentialRequest struct {
	Credential      SealedConnectorCredential
	ExpectedVersion int64
	Retire          *ConnectorRetirement
	Now             time.Time
}

type CreateSealedConnectorConsentRequest struct {
	Consent SealedConnectorConsent
	Now     time.Time
}

type CreateSealedConnectorCompletionRequest struct {
	Completion          SealedConnectorCompletion
	ExpectedCompletions []SealedConnectorCompletionRef
	Replace             []string
	Now                 time.Time
}

type CommitSealedConnectorCompletionRequest struct {
	Nonce                     string
	PayloadNonce              []byte
	Payload                   []byte
	Credential                SealedConnectorCredential
	ExpectedCredentialVersion int64
	Retire                    *ConnectorRetirement
	ExpectedCompletions       []SealedConnectorCompletionRef
	Drop                      []string
	Now                       time.Time
}

type SealedConnectorCredentialPage struct {
	Credentials []SealedConnectorCredential
	NextCursor  string
}

type SealedRetiredConnectorCredentialPage struct {
	Retired    []SealedRetiredConnectorCredential
	NextCursor string
}

type SealedConnectorConsentPage struct {
	Consents   []SealedConnectorConsent
	NextCursor string
}

type SealedConnectorCompletionPage struct {
	Completions []SealedConnectorCompletion
	NextCursor  string
}

type SealedConnectorCustodyStore interface {
	ReadConnectorCustody(ctx context.Context, connectionUID string) (*ConnectorCustody, error)

	GetSealedConnectorCredential(ctx context.Context, connectionUID string) (*SealedConnectorCredential, error)

	PutSealedConnectorCredential(ctx context.Context, request PutSealedConnectorCredentialRequest) (*SealedConnectorCredential, error)

	ReplaceSealedConnectorCredential(ctx context.Context, request ReplaceSealedConnectorCredentialRequest) (*SealedConnectorCredential, error)

	RetireSealedConnectorCredential(ctx context.Context, retired SealedRetiredConnectorCredential) error

	TombstoneConnectorCustody(ctx context.Context, connectionUID string) error

	ShredConnectorCredential(ctx context.Context, connectionUID string, expectedVersion int64) error

	DeleteConnectorCredential(ctx context.Context, connectionUID string) error

	ListSealedConnectorCredentials(ctx context.Context, cursor string, limit int) (*SealedConnectorCredentialPage, error)

	ListSealedRetiredConnectorCredentials(ctx context.Context, cursor string, limit int) (*SealedRetiredConnectorCredentialPage, error)
}

type SealedConnectorConsentStore interface {
	CreateSealedConnectorConsent(ctx context.Context, request CreateSealedConnectorConsentRequest) error

	ConsumeSealedConnectorConsent(ctx context.Context, nonce string, now time.Time) (*SealedConnectorConsent, error)

	CreateSealedConnectorCompletion(ctx context.Context, request CreateSealedConnectorCompletionRequest) error

	GetSealedConnectorCompletion(ctx context.Context, nonce string) (*SealedConnectorCompletion, error)

	ConsumeSealedConnectorCompletion(ctx context.Context, nonce string) (*SealedConnectorCompletion, error)

	CommitSealedConnectorCompletion(ctx context.Context, request CommitSealedConnectorCompletionRequest) (*SealedConnectorCredential, error)

	DeleteConnectorCompletion(ctx context.Context, nonce string) error

	DeleteConnectorConsentsForConnection(ctx context.Context, connectionUID string) error

	ListSealedConnectorConsents(ctx context.Context, cursor string, limit int) (*SealedConnectorConsentPage, error)

	ListSealedConnectorCompletions(ctx context.Context, cursor string, limit int) (*SealedConnectorCompletionPage, error)
}

const connectorNamespaceField = "namespace"

type connectorField struct {
	name, value string
}

func connectorIdentityFields(connectionUID, namespace, name, subjectDigest, provider string) []connectorField {
	return []connectorField{
		{"connection UID", connectionUID}, {connectorNamespaceField, namespace}, {"name", name},
		{"subject digest", subjectDigest}, {"provider", provider},
	}
}

func connectorPendingFields(nonce, connectionUID, namespace, name, subjectDigest, provider, mode string) []connectorField {
	fields := append([]connectorField{{"nonce", nonce}}, connectorIdentityFields(connectionUID, namespace, name, subjectDigest, provider)...)
	return append(fields, connectorField{"mode", mode})
}

func requireConnectorFields(record string, fields []connectorField) error {
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			return ValidationErrorf("%s %s is required", record, field.name)
		}
	}
	return nil
}
