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
)

// ConnectorFieldNamespace names the namespace binding field in validation messages.
const ConnectorFieldNamespace = "namespace"

// ErrConnectorCustodyTombstoned is returned when material is written for a
// Connection UID that was already disconnected. Deletion is permanent per UID
// so a late consent completion can never recreate custody.
var ErrConnectorCustodyTombstoned = errors.New("connector custody for this connection was deleted")

// ErrConnectorCompletionCommitted reports a completion another commit already
// finished; the caller resumes it instead of committing again.
var ErrConnectorCompletionCommitted = errors.New("connector completion was already committed")

// ErrConnectorConsentSuperseded reports a completion from a consent older
// than one that was already parked or committed for the same Connection; an
// older consent's tokens never replace a newer grant.
var ErrConnectorConsentSuperseded = errors.New("a newer consent for this connection superseded this one")

// ErrConnectorRetiredLimit reports a Connection that already retains the
// maximum number of superseded grants; disconnecting revokes them all.
var ErrConnectorRetiredLimit = errors.New("this connection retains too many superseded grants; disconnect and link again")

// ConnectorCredentialRef binds sealed token material to exactly one
// Connection. Every field participates in the AEAD additional data, so a row
// copied to another Connection, subject, or provider fails to open.
type ConnectorCredentialRef struct {
	ConnectionUID string
	Namespace     string
	Name          string
	// SubjectDigest is a stable digest of the owning verified identity.
	SubjectDigest string
	Provider      string
}

// Validate reports whether every binding field is present.
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

// ConnectorCredential is the plaintext token material for one Connection. It
// exists only in controller memory; the store seals it at rest.
type ConnectorCredential struct {
	AccessToken  string
	RefreshToken string
	TokenType    string
	// ExpiresAt is zero when the provider reported no expiry.
	ExpiresAt time.Time
	Scopes    []string
	// AuthorityDigest is the provider OAuth issuer digest of the client that
	// issued the tokens. It is sealed with them, so refresh and revocation
	// always know which client the material belongs to regardless of
	// Connection status or later tool changes.
	AuthorityDigest string
	// RevocationDigest is the provider revocation digest (client and
	// revocation endpoint, without the token endpoint) the material can be
	// revoked against; see connectors.ProviderRevocationDigest.
	RevocationDigest string
	// GrantSequence identifies the consent that produced this material. The
	// store assigns it when a grant is committed (never on a refresh, which
	// carries the current grant forward), it rises monotonically for the
	// Connection's lifetime even across a shred, and the Connection status
	// mirrors it, so authority frozen under one grant can be refused against
	// custody that a later consent replaced.
	GrantSequence int64
	// UpdatedAt is set by the store on read.
	UpdatedAt time.Time
	// Version is the custody row version, set by the store on read. Refresh
	// writes are fenced against it so a concurrent re-consent is never
	// overwritten by material derived from an older refresh token.
	Version int64
}

// ConnectorCredentialStore seals per-Connection token material with one data
// key per Connection, itself wrapped by the controller key. Deleting a row
// deletes its wrapped key, which makes the ciphertext unrecoverable.
type ConnectorCredentialStore interface {
	// PutConnectorCredential replaces the material for ref, minting a fresh
	// data key so previous ciphertext can no longer be opened.
	PutConnectorCredential(ctx context.Context, ref ConnectorCredentialRef, credential ConnectorCredential) error
	// GetConnectorCredential opens the material bound to ref, or ErrNotFound.
	GetConnectorCredential(ctx context.Context, ref ConnectorCredentialRef) (ConnectorCredential, error)
	// ListRetiredConnectorCredentials opens the committed credentials a later
	// commit replaced. They stay sealed until disconnect revokes them.
	ListRetiredConnectorCredentials(ctx context.Context, ref ConnectorCredentialRef) ([]ConnectorCredential, error)
	// TombstoneConnectorCustody fences the UID before its material is read
	// for revocation: later commits fail with ErrConnectorCustodyTombstoned
	// while the rows stay readable until DeleteConnectorCredential.
	TombstoneConnectorCustody(ctx context.Context, connectionUID string) error
	// ReplaceConnectorCredential replaces the material only while the row is
	// still at expectedVersion, minting a fresh data key. A newer row returns
	// ErrConflict; a missing row returns ErrNotFound.
	ReplaceConnectorCredential(ctx context.Context, ref ConnectorCredentialRef, credential ConnectorCredential, expectedVersion int64) error
	// ShredConnectorCredential deletes the material without tombstoning the
	// UID, for a link the provider revoked that the person may re-consent to.
	// A newer row returns ErrConflict; a missing row returns ErrNotFound, after
	// the log truncation still runs, so callers treat it as already shredded.
	ShredConnectorCredential(ctx context.Context, connectionUID string, expectedVersion int64) error
	// RetireConnectorCredential keeps material that was obtained for ref
	// but cannot become its current row (a refresh that lost to a consent)
	// sealed for revocation at disconnect. A refresh token keeps the row
	// until disconnect; without one the row lives until the access token
	// expires. A tombstoned Connection refuses it.
	RetireConnectorCredential(ctx context.Context, ref ConnectorCredentialRef, credential ConnectorCredential) error
	// DeleteConnectorCredential removes the row and its wrapped key and
	// tombstones the UID so later writes fail with
	// ErrConnectorCustodyTombstoned. Missing rows succeed.
	DeleteConnectorCredential(ctx context.Context, connectionUID string) error
}

// ConnectorConsent is one in-flight OAuth authorization for a Connection. The
// PKCE verifier stays server-side, sealed, and single-use.
type ConnectorConsent struct {
	// Nonce is the random single-use identifier carried in the OAuth state.
	Nonce         string
	ConnectionUID string
	Namespace     string
	Name          string
	SubjectDigest string
	Provider      string
	Mode          string
	CodeVerifier  string
	// AuthorityDigest is the provider OAuth-authority digest the consent was
	// started against; the callback refuses to exchange the code with a
	// different authority.
	AuthorityDigest string
	// RevocationDigest is the provider revocation identity (client and
	// revocation endpoint) when consent started; the callback seals it with
	// the tokens, so a revocation endpoint moved during the consent window
	// is never handed the credential at disconnect.
	RevocationDigest string
	// Scopes are the scopes the consent requested. A token response that
	// omits scope is taken to grant exactly these, never a later configured
	// set.
	Scopes    []string
	ExpiresAt time.Time
	// Sequence orders the Connection's consents: the store assigns it when
	// the consent starts, and completions carry it so commits follow consent
	// order rather than callback arrival.
	Sequence int64
}

// ConnectorCompletion is the second half of a consent: token material the
// callback obtained, sealed and parked until the verified owner commits it
// with the one-time completion nonce the completing browser received.
type ConnectorCompletion struct {
	Nonce         string
	ConnectionUID string
	Namespace     string
	Name          string
	SubjectDigest string
	Provider      string
	Mode          string
	// Credential carries the parked tokens and, sealed with them, the
	// AuthorityDigest of the OAuth client that issued them.
	Credential ConnectorCredential
	ExpiresAt  time.Time
	// ConsentAuthorityDigest is the full provider authority digest (client
	// identity plus tool destinations) the consent was granted against,
	// sealed with the tokens; completion refuses a provider that changed it.
	ConsentAuthorityDigest string
	// Committed is sealed into the parked payload atomically with the custody
	// write, so a retry after a failed status update resumes that commit
	// instead of writing the parked tokens again over a newer commit, and no
	// plaintext column can flip it.
	Committed bool
	// ConsentSequence is the Sequence of the consent this completion came
	// from; a completion whose consent is older than one already parked or
	// committed for the Connection is refused.
	ConsentSequence int64
}

// ConnectorConsentStore holds pending consents and pending completions.
type ConnectorConsentStore interface {
	// CreateConnectorConsent stores a consent and drops expired ones.
	CreateConnectorConsent(ctx context.Context, consent ConnectorConsent) error
	// ConsumeConnectorConsent atomically removes and returns the consent for
	// nonce, or ErrNotFound. Expired consents are removed and reported as
	// ErrNotFound.
	ConsumeConnectorConsent(ctx context.Context, nonce string) (ConnectorConsent, error)
	// CreateConnectorCompletion parks sealed token material until the owner
	// commits it. A tombstoned UID fails with ErrConnectorCustodyTombstoned.
	CreateConnectorCompletion(ctx context.Context, completion ConnectorCompletion) error
	// ConsumeConnectorCompletion atomically removes and returns the parked
	// material for nonce, or ErrNotFound (also for expired entries).
	ConsumeConnectorCompletion(ctx context.Context, nonce string) (ConnectorCompletion, error)
	// CommitConnectorCompletion seals credential into custody for ref and
	// marks the completion committed in one transaction. It fails with
	// ErrConnectorCustodyTombstoned after a disconnect and ErrNotFound when
	// the completion no longer exists.
	// The committed material, with the grant sequence the store assigned,
	// is returned.
	CommitConnectorCompletion(ctx context.Context, nonce string, ref ConnectorCredentialRef, credential ConnectorCredential) (ConnectorCredential, error)
	// PeekConnectorCompletion returns the parked material for nonce without
	// removing it, so a commit that fails after the credential write can be
	// retried with the same token. Expired entries report ErrNotFound.
	PeekConnectorCompletion(ctx context.Context, nonce string) (ConnectorCompletion, error)
	// DeleteConnectorCompletion removes one parked completion. Missing rows
	// succeed.
	DeleteConnectorCompletion(ctx context.Context, nonce string) error
	// ListConnectorCompletionsForConnection opens every parked completion for
	// a Connection, expired ones included, so the reconciler can revoke tokens
	// that were never committed before dropping them.
	ListConnectorCompletionsForConnection(ctx context.Context, connectionUID string) ([]ConnectorCompletion, error)
	// DeleteConnectorConsentsForConnection drops every pending consent and
	// completion for a Connection, for example on disconnect.
	DeleteConnectorConsentsForConnection(ctx context.Context, connectionUID string) error
}
