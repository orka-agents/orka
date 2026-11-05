package v1

import (
	"context"
	"time"
)

// AHPToken is a minted Agent Host Protocol connection token. Only a hash of
// the secret is stored, never the token itself.
type AHPToken struct {
	ID          string
	TokenHash   string
	UserKey     string
	Issuer      string
	Subject     string
	Username    string
	Namespace   string
	Description string
	CreatedAt   time.Time
	ExpiresAt   time.Time
	RevokedAt   *time.Time
}

// AHPClient binds an AHP clientId to the user that first used it and records
// what that connection negotiated, so a reconnect restores it.
type AHPClient struct {
	ClientID        string
	UserKey         string
	ProtocolVersion string
	Dialect         string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AHPJournalEntry is one serialized action envelope in a user's journal.
type AHPJournalEntry struct {
	Seq      int64
	Channel  string
	Envelope []byte
}

// AHPChannelState is the reduced state of one channel as of FromSeq.
type AHPChannelState struct {
	Channel string
	State   []byte
	FromSeq int64
}

// AHPSession is one entry in a user's session catalog.
type AHPSession struct {
	UserKey    string
	Session    string
	Backend    string
	Summary    []byte
	CreatedAt  time.Time
	ModifiedAt time.Time
}

// AHPRecord is a session backend's own record for a user, such as a project
// or the Orka objects behind a session. Clients never see records directly.
type AHPRecord struct {
	UserKey   string
	Kind      string
	Key       string
	Value     []byte
	UpdatedAt time.Time
}

// AHPJournalLimits bound how much of a user's journal is kept for replay. A
// zero limit keeps the journal unbounded by that measure.
type AHPJournalLimits struct {
	MaxEntries int
	MaxBytes   int64
}

// AHPStore persists the Agent Host Protocol host's state: connection tokens,
// client records, and each user's sequence, replay journal, channel states,
// session catalog and session backend records. Everything except tokens and
// clients is keyed by an opaque user key, and a method that takes one reads
// or changes only that user's state, so one user never reads another's.
// Byte values are opaque to the provider and are returned exactly as stored.
type AHPStore interface {
	// CreateAHPToken stores a new token. It returns ErrConflict when the ID or
	// the hash is already stored.
	CreateAHPToken(ctx context.Context, token AHPToken) error
	// GetAHPTokenByHash returns ErrNotFound for an unknown hash.
	GetAHPTokenByHash(ctx context.Context, tokenHash string) (AHPToken, error)
	// ListAHPTokens returns the user's tokens, revoked ones included, newest
	// first.
	ListAHPTokens(ctx context.Context, userKey string) ([]AHPToken, error)
	// RevokeAHPToken records when the user's token was revoked. It returns
	// ErrNotFound unless the user owns the token; revoking it again keeps the
	// first revocation time.
	RevokeAHPToken(ctx context.Context, userKey, id string, at time.Time) error

	// GetAHPClient returns ErrNotFound for an unknown clientId.
	GetAHPClient(ctx context.Context, clientID string) (AHPClient, error)
	// PutAHPClient records a client, created and updated at its UpdatedAt, or
	// updates the protocol version, dialect and update time of one the same
	// user recorded. It returns ErrConflict, changing nothing, when the
	// clientId already belongs to another user.
	PutAHPClient(ctx context.Context, client AHPClient) error

	// GetAHPServerSeq returns the user's last assigned sequence number, or 0.
	GetAHPServerSeq(ctx context.Context, userKey string) (int64, error)
	// AppendAHPJournal atomically appends entries numbered previousSeq+1..,
	// stores channel states, advances the user's sequence past the entries,
	// and prunes the journal to limits. It returns ErrConflict, changing
	// nothing, if the user's sequence is no longer previousSeq.
	AppendAHPJournal(ctx context.Context, userKey string, previousSeq int64, entries []AHPJournalEntry,
		states []AHPChannelState, limits AHPJournalLimits) error
	// ReadAHPJournal returns the entries after afterSeq, in sequence order,
	// from one snapshot. complete is false when some of them were already
	// pruned, and, with no entries, when afterSeq is negative or past the
	// user's sequence.
	ReadAHPJournal(ctx context.Context, userKey string, afterSeq int64) (entries []AHPJournalEntry, complete bool, err error)
	// GetAHPChannelState returns ErrNotFound for a channel the user has no
	// state for.
	GetAHPChannelState(ctx context.Context, userKey, channel string) (AHPChannelState, error)
	// DeleteAHPChannelStates removes the user's states of channels; a channel
	// with no state is not an error.
	DeleteAHPChannelStates(ctx context.Context, userKey string, channels []string) error
	// ListAHPChannels returns, in order, the user's channels with state whose
	// URI starts with prefix, matched literally.
	ListAHPChannels(ctx context.Context, userKey, prefix string) ([]string, error)

	// PutAHPSession records a session in the user's catalog, or updates the
	// summary and modification time of one already there, keeping its backend
	// and creation time.
	PutAHPSession(ctx context.Context, session AHPSession) error
	// GetAHPSession returns ErrNotFound for a session the user does not have.
	GetAHPSession(ctx context.Context, userKey, session string) (AHPSession, error)
	// ListAHPSessions returns the user's sessions, most recently modified first.
	ListAHPSessions(ctx context.Context, userKey string) ([]AHPSession, error)
	// DeleteAHPSession removes a session; a missing one is not an error.
	DeleteAHPSession(ctx context.Context, userKey, session string) error

	// PutAHPRecord records or replaces one of the user's records.
	PutAHPRecord(ctx context.Context, record AHPRecord) error
	// GetAHPRecord returns ErrNotFound for a record the user does not have.
	GetAHPRecord(ctx context.Context, userKey, kind, key string) (AHPRecord, error)
	// ListAHPRecords returns the user's records of kind, in key order.
	ListAHPRecords(ctx context.Context, userKey, kind string) ([]AHPRecord, error)
	// DeleteAHPRecords removes the user's records of kind whose key starts
	// with prefix, matched literally.
	DeleteAHPRecords(ctx context.Context, userKey, kind, prefix string) error
	// DeleteAHPRecord removes one record; a missing record is not an error.
	DeleteAHPRecord(ctx context.Context, userKey, kind, key string) error
}
