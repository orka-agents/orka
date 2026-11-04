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

// Connector custody
//
// A Connection's OAuth token material, its pending consents and its parked
// completions are protected records. Orka seals them under the same key as
// execution snapshots before a provider sees them: current and retired
// custody under a fresh per-record data key that is itself sealed under
// Orka's key, consent verifiers and completion payloads under Orka's key
// directly. Each seal binds the record's identity, and for consents and
// completions every plain field, through its associated data. A provider
// stores the nonces and ciphertexts as opaque bytes and never receives a
// token, a verifier or a key.
//
// Decisions that need the plaintext, such as whether a parked completion was
// already committed or whether a replaced credential still needs revoking,
// are made in Orka. Orka reads a Connection's custody ([ConnectorCustody]),
// decides and seals, then sends a conditional command that names what it
// read: the custody version it replaces, the next grant or consent sequence,
// and the digests of the parked completions it judged. The provider commits
// only if those still hold and returns [ErrConflict], changing nothing, when
// they do not; Orka then reads again. A provider enforces the rules it can
// decide from plain fields itself: tombstones, consent order, sequence
// counters, expiry at the request's time and the bounds on retired grants.

// MaxRetiredConnectorConsentGrants bounds the grants a re-consent replaced
// that one Connection keeps for revocation at disconnect. A commit past the
// bound returns ErrConnectorRetiredLimit.
const MaxRetiredConnectorConsentGrants = 16

// MaxRetiredConnectorRefreshGrants bounds the grants a refresh rotated away
// that one Connection keeps until disconnect. The oldest are dropped first.
const MaxRetiredConnectorRefreshGrants = 16

// ConnectorTombstoneRetention is how long a provider keeps a disconnect
// tombstone. A tombstone only has to fence consents and completions that were
// in flight at disconnect, which expire after minutes, and a Connection UID is
// never reused. A provider drops the Connection's sequence counters with its
// tombstone.
const ConnectorTombstoneRetention = 24 * time.Hour

// ErrConnectorCustodyTombstoned is returned when material is written for a
// Connection UID that was already disconnected. Deletion is permanent per UID
// so a late consent completion can never recreate custody.
var ErrConnectorCustodyTombstoned = errors.New("connector custody for this connection was deleted")

// ErrConnectorConsentSuperseded reports a completion from a consent older
// than one that was already parked or committed for the same Connection; an
// older consent's tokens never replace a newer grant.
var ErrConnectorConsentSuperseded = errors.New("a newer consent for this connection superseded this one")

// ErrConnectorRetiredLimit reports a Connection that already retains the
// maximum number of superseded grants; disconnecting revokes them all.
var ErrConnectorRetiredLimit = errors.New("this connection retains too many superseded grants; disconnect and link again")

// SealedValueDigest identifies one sealed value, its nonce and ciphertext, in
// a conditional command. It is the canonical sha256 digest of the nonce length
// as an 8-byte big-endian integer, the nonce and the ciphertext.
func SealedValueDigest(nonce, ciphertext []byte) string {
	hash := sha256.New()
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(nonce)))
	hash.Write(length[:])
	hash.Write(nonce)
	hash.Write(ciphertext)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

// SealedConnectorCredential is a Connection's current token custody as a
// provider stores it. The plain fields repeat facts Orka needs to find and
// judge the record; Orka binds the identity fields into the seal and checks
// the grant sequence against the plaintext when it opens the record.
type SealedConnectorCredential struct {
	ConnectionUID string
	Namespace     string
	Name          string
	SubjectDigest string
	Provider      string
	// DataKeyNonce and DataKeyCiphertext are the record's data key, sealed
	// under Orka's key.
	DataKeyNonce      []byte
	DataKeyCiphertext []byte
	// Nonce and Ciphertext are the token material, sealed under the data key.
	Nonce      []byte
	Ciphertext []byte
	// ExpiresAt is the access token's expiry, or zero when the provider
	// reported none.
	ExpiresAt time.Time
	// GrantSequence identifies the consent grant the material belongs to.
	GrantSequence int64
	// Version and UpdatedAt are set by the provider when it stores the
	// record: Version from a per-Connection counter that never repeats, even
	// after the record is shredded, and UpdatedAt from the request's time.
	// Values in a request are ignored.
	Version   int64
	UpdatedAt time.Time
}

// Validate rejects an incomplete sealed credential.
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

// SealedRetiredConnectorCredential is token material a later write replaced,
// or that could not become custody, kept sealed so disconnect can still
// revoke it. It is sealed like SealedConnectorCredential under its own data
// key.
type SealedRetiredConnectorCredential struct {
	ConnectionUID     string
	DataKeyNonce      []byte
	DataKeyCiphertext []byte
	Nonce             []byte
	Ciphertext        []byte
	// RevocableUntil is when nothing is left to revoke; zero keeps the record
	// until disconnect.
	RevocableUntil time.Time
	// ConsentGrant marks a whole grant a re-consent replaced. Only these count
	// toward MaxRetiredConnectorConsentGrants.
	ConsentGrant bool
	RetiredAt    time.Time
}

// SealedConnectorConsent is one in-flight OAuth authorization as a provider
// stores it. The PKCE verifier is sealed; its associated data binds every
// plain field.
type SealedConnectorConsent struct {
	// Nonce is the single-use identifier carried in the OAuth state.
	Nonce            string
	ConnectionUID    string
	Namespace        string
	Name             string
	SubjectDigest    string
	Provider         string
	Mode             string
	AuthorityDigest  string
	RevocationDigest string
	// Scopes are OAuth scope tokens, which never contain whitespace. A
	// provider returns them in order.
	Scopes []string
	// Sequence orders the Connection's consents. Orka assigns the next one
	// (IssuedConsentSequence + 1) when it creates the consent.
	Sequence           int64
	ExpiresAt          time.Time
	VerifierNonce      []byte
	VerifierCiphertext []byte
}

// Validate rejects an incomplete sealed consent.
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

// SealedConnectorCompletion is token material a consent callback obtained,
// parked until the Connection's owner commits it. The payload is sealed; its
// associated data binds every plain field. Whether the completion was
// committed is sealed inside the payload, so no plain field can flip it.
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

// Validate rejects an incomplete sealed completion.
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

// Ref names the completion and its sealed payload in a conditional command.
func (c SealedConnectorCompletion) Ref() SealedConnectorCompletionRef {
	return SealedConnectorCompletionRef{Nonce: c.Nonce, Digest: SealedValueDigest(c.PayloadNonce, c.Payload)}
}

// SealedConnectorCompletionRef identifies a parked completion as Orka read
// it: its nonce and the SealedValueDigest of its payload.
type SealedConnectorCompletionRef struct {
	Nonce  string
	Digest string
}

// ConnectorCustody is every custody record one Connection holds, read from
// one snapshot. Orka decides connector writes from it.
type ConnectorCustody struct {
	ConnectionUID string
	// Tombstoned reports a disconnect: no new material may be written.
	Tombstoned bool
	// Credential is the current custody, or nil.
	Credential *SealedConnectorCredential
	// Retired is the retained replaced material, oldest first.
	Retired []SealedRetiredConnectorCredential
	// Completions are the parked completions, expired ones included, ordered
	// by nonce.
	Completions []SealedConnectorCompletion
	// LastGrantSequence is the grant sequence the Connection's last grant
	// took, or zero. It outlives the credential record.
	LastGrantSequence int64
	// IssuedConsentSequence is the last consent sequence issued, and
	// CommittedConsentSequence the sequence of the newest consent whose
	// tokens became custody; both are zero before the first.
	IssuedConsentSequence    int64
	CommittedConsentSequence int64
}

// ConnectorRetirement keeps the custody record a write replaces sealed for
// revocation, exactly as stored. A commit retires it as a consent grant and a
// replacement as refresh material.
type ConnectorRetirement struct {
	// RevocableUntil is when nothing is left to revoke; zero keeps the record
	// until disconnect.
	RevocableUntil time.Time
}

// PutSealedConnectorCredentialRequest stores a new grant as a Connection's
// custody. Credential.GrantSequence must be LastGrantSequence + 1; otherwise
// another grant was taken since Orka read the custody and the provider
// returns ErrConflict.
type PutSealedConnectorCredentialRequest struct {
	Credential SealedConnectorCredential
	Now        time.Time
}

// ReplaceSealedConnectorCredentialRequest replaces the current custody of the
// same grant, as a refresh does. The provider commits only while the stored
// record is at ExpectedVersion, and Credential.GrantSequence must match it.
type ReplaceSealedConnectorCredentialRequest struct {
	Credential      SealedConnectorCredential
	ExpectedVersion int64
	// Retire, when set, keeps the replaced record for revocation.
	Retire *ConnectorRetirement
	Now    time.Time
}

// CreateSealedConnectorConsentRequest stores a pending consent and replaces
// any earlier pending consent of the Connection. Consent.Sequence must be
// IssuedConsentSequence + 1; otherwise the provider returns ErrConflict.
type CreateSealedConnectorConsentRequest struct {
	Consent SealedConnectorConsent
	// Now is the request's time; consents that expired before it are
	// dropped.
	Now time.Time
}

// CreateSealedConnectorCompletionRequest parks a completion. Orka judged the
// Connection's parked completions, ExpectedCompletions, and chose Replace,
// the uncommitted ones from older consents that the new completion replaces.
// The provider commits only if the Connection's completions are still
// exactly ExpectedCompletions.
type CreateSealedConnectorCompletionRequest struct {
	Completion          SealedConnectorCompletion
	ExpectedCompletions []SealedConnectorCompletionRef
	Replace             []string
	Now                 time.Time
}

// CommitSealedConnectorCompletionRequest turns a parked completion into the
// Connection's custody, as a new grant. Orka opened the completion, judged it
// uncommitted and re-sealed its payload as committed (PayloadNonce,
// Payload); it decided whether the replaced custody is retired, and which
// uncommitted completions from older consents are dropped. The provider
// commits only if the custody record is still at ExpectedCredentialVersion
// (zero for none), Credential.GrantSequence is LastGrantSequence + 1 and the
// Connection's completions are still exactly ExpectedCompletions.
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

// SealedConnectorCredentialPage is one page of sealed custody records.
type SealedConnectorCredentialPage struct {
	Credentials []SealedConnectorCredential
	// NextCursor continues the listing; it is empty after the last page.
	NextCursor string
}

// SealedRetiredConnectorCredentialPage is one page of retired custody.
type SealedRetiredConnectorCredentialPage struct {
	Retired    []SealedRetiredConnectorCredential
	NextCursor string
}

// SealedConnectorConsentPage is one page of pending consents.
type SealedConnectorConsentPage struct {
	Consents   []SealedConnectorConsent
	NextCursor string
}

// SealedConnectorCompletionPage is one page of parked completions.
type SealedConnectorCompletionPage struct {
	Completions []SealedConnectorCompletion
	NextCursor  string
}

// SealedConnectorCustodyStore persists sealed connector token custody.
type SealedConnectorCustodyStore interface {
	// ReadConnectorCustody returns every custody record of a Connection from
	// one snapshot. An unknown Connection has empty custody.
	ReadConnectorCustody(ctx context.Context, connectionUID string) (*ConnectorCustody, error)

	// GetSealedConnectorCredential returns the current custody or
	// ErrNotFound.
	GetSealedConnectorCredential(ctx context.Context, connectionUID string) (*SealedConnectorCredential, error)

	// PutSealedConnectorCredential stores a new grant and returns the record
	// as stored. A tombstoned Connection returns
	// ErrConnectorCustodyTombstoned.
	PutSealedConnectorCredential(ctx context.Context, request PutSealedConnectorCredentialRequest) (*SealedConnectorCredential, error)

	// ReplaceSealedConnectorCredential replaces the current custody's sealed
	// bytes and expiry, keeping its identity fields, and returns the record
	// as stored. A tombstoned Connection returns
	// ErrConnectorCustodyTombstoned, a missing record ErrNotFound and a
	// record at another version ErrConflict, in that order. Retired records
	// whose RevocableUntil is not after Now are dropped first; a record
	// retired until disconnect that is not a consent grant counts toward
	// MaxRetiredConnectorRefreshGrants.
	ReplaceSealedConnectorCredential(ctx context.Context, request ReplaceSealedConnectorCredentialRequest) (*SealedConnectorCredential, error)

	// RetireSealedConnectorCredential keeps material that cannot become
	// custody sealed for revocation. RetiredAt is the request's time and
	// ConsentGrant must be false. A tombstoned Connection returns
	// ErrConnectorCustodyTombstoned. Expired retired records are dropped and
	// the refresh bound applied as for ReplaceSealedConnectorCredential.
	RetireSealedConnectorCredential(ctx context.Context, retired SealedRetiredConnectorCredential) error

	// TombstoneConnectorCustody fences the Connection before its material is
	// read for revocation: later writes return ErrConnectorCustodyTombstoned
	// while the records stay readable. It is idempotent, and it drops
	// tombstones older than ConnectorTombstoneRetention by the provider's
	// clock.
	TombstoneConnectorCustody(ctx context.Context, connectionUID string) error

	// ShredConnectorCredential deletes the current custody while it is at
	// expectedVersion, without a tombstone, so the person may consent again.
	// A newer record returns ErrConflict and a missing one ErrNotFound.
	ShredConnectorCredential(ctx context.Context, connectionUID string, expectedVersion int64) error

	// DeleteConnectorCredential deletes current and retired custody and
	// tombstones the Connection. Missing records succeed.
	DeleteConnectorCredential(ctx context.Context, connectionUID string) error

	// ListSealedConnectorCredentials returns every current custody record
	// ordered by Connection UID, paged like ListSealedAgentExecutionSnapshots,
	// so Orka can authenticate retained custody with its key.
	ListSealedConnectorCredentials(ctx context.Context, cursor string, limit int) (*SealedConnectorCredentialPage, error)

	// ListSealedRetiredConnectorCredentials returns every retired record
	// ordered by Connection UID and retirement order, paged in the same way.
	ListSealedRetiredConnectorCredentials(ctx context.Context, cursor string, limit int) (*SealedRetiredConnectorCredentialPage, error)
}

// SealedConnectorConsentStore persists sealed pending consents and parked
// completions.
type SealedConnectorConsentStore interface {
	// CreateSealedConnectorConsent stores a pending consent.
	CreateSealedConnectorConsent(ctx context.Context, request CreateSealedConnectorConsentRequest) error

	// ConsumeSealedConnectorConsent removes and returns the consent for
	// nonce, or ErrNotFound. Consents that expired before now are dropped
	// first; Orka judges the expiry of the one it receives.
	ConsumeSealedConnectorConsent(ctx context.Context, nonce string, now time.Time) (*SealedConnectorConsent, error)

	// CreateSealedConnectorCompletion parks a completion. A tombstoned
	// Connection returns ErrConnectorCustodyTombstoned, and a consent sequence
	// not after CommittedConsentSequence returns
	// ErrConnectorConsentSuperseded.
	CreateSealedConnectorCompletion(ctx context.Context, request CreateSealedConnectorCompletionRequest) error

	// GetSealedConnectorCompletion returns one parked completion, expired or
	// not, or ErrNotFound.
	GetSealedConnectorCompletion(ctx context.Context, nonce string) (*SealedConnectorCompletion, error)

	// ConsumeSealedConnectorCompletion removes and returns one parked
	// completion, expired or not, or ErrNotFound.
	ConsumeSealedConnectorCompletion(ctx context.Context, nonce string) (*SealedConnectorCompletion, error)

	// CommitSealedConnectorCompletion commits a completion in one
	// transaction: it retires the replaced custody when asked, stores the new
	// grant, replaces the completion's payload, records its consent as the
	// committed one and drops the completions in Drop. It returns the stored
	// custody. Its refusals, in order: ErrConflict when what Orka read
	// changed; ErrNotFound for a completion that expired at Now;
	// ErrConnectorConsentSuperseded when the completion's consent is not
	// after CommittedConsentSequence; ErrConnectorRetiredLimit when the
	// Connection already retains MaxRetiredConnectorConsentGrants consent
	// grants after dropping expired retired records; and
	// ErrConnectorCustodyTombstoned. A refused commit changes nothing.
	CommitSealedConnectorCompletion(ctx context.Context, request CommitSealedConnectorCompletionRequest) (*SealedConnectorCredential, error)

	// DeleteConnectorCompletion removes one parked completion. Missing
	// records succeed.
	DeleteConnectorCompletion(ctx context.Context, nonce string) error

	// DeleteConnectorConsentsForConnection drops every pending consent and
	// parked completion of a Connection.
	DeleteConnectorConsentsForConnection(ctx context.Context, connectionUID string) error

	// ListSealedConnectorConsents returns every pending consent, expired ones
	// included, ordered by nonce and paged like
	// ListSealedAgentExecutionSnapshots.
	ListSealedConnectorConsents(ctx context.Context, cursor string, limit int) (*SealedConnectorConsentPage, error)

	// ListSealedConnectorCompletions returns every parked completion ordered
	// by nonce, paged in the same way.
	ListSealedConnectorCompletions(ctx context.Context, cursor string, limit int) (*SealedConnectorCompletionPage, error)
}

// connectorNamespaceField names the namespace field in validation messages.
const connectorNamespaceField = "namespace"

// connectorField is one plain identity field of a connector record.
type connectorField struct {
	name, value string
}

// connectorIdentityFields are the identity fields every connector record
// carries.
func connectorIdentityFields(connectionUID, namespace, name, subjectDigest, provider string) []connectorField {
	return []connectorField{
		{"connection UID", connectionUID}, {connectorNamespaceField, namespace}, {"name", name},
		{"subject digest", subjectDigest}, {"provider", provider},
	}
}

// connectorPendingFields are the identity fields of a pending consent or a
// parked completion.
func connectorPendingFields(nonce, connectionUID, namespace, name, subjectDigest, provider, mode string) []connectorField {
	fields := append([]connectorField{{"nonce", nonce}}, connectorIdentityFields(connectionUID, namespace, name, subjectDigest, provider)...)
	return append(fields, connectorField{"mode", mode})
}

// requireConnectorFields rejects the first blank field.
func requireConnectorFields(record string, fields []connectorField) error {
	for _, field := range fields {
		if strings.TrimSpace(field.value) == "" {
			return ValidationErrorf("%s %s is required", record, field.name)
		}
	}
	return nil
}
