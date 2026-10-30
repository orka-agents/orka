/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

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

// AHPJournalLimits bound how much of a user's journal is kept for replay.
type AHPJournalLimits struct {
	MaxEntries int
	MaxBytes   int64
}

// AHPStore persists Agent Host Protocol tokens, client records and each
// user's sequence, replay journal, channel states and session catalog.
// Everything except tokens is keyed by an opaque user key, so one user never
// reads another's state.
type AHPStore interface {
	CreateAHPToken(ctx context.Context, token AHPToken) error
	// GetAHPTokenByHash returns ErrNotFound for an unknown hash.
	GetAHPTokenByHash(ctx context.Context, tokenHash string) (AHPToken, error)
	ListAHPTokens(ctx context.Context, userKey string) ([]AHPToken, error)
	// RevokeAHPToken returns ErrNotFound unless the user owns the token.
	RevokeAHPToken(ctx context.Context, userKey, id string, at time.Time) error

	// GetAHPClient returns ErrNotFound for an unknown clientId.
	GetAHPClient(ctx context.Context, clientID string) (AHPClient, error)
	// PutAHPClient records or updates a client. It returns ErrConflict when the
	// clientId already belongs to another user.
	PutAHPClient(ctx context.Context, client AHPClient) error

	// AHPServerSeq returns the user's last assigned sequence number, or 0.
	AHPServerSeq(ctx context.Context, userKey string) (int64, error)
	// AppendAHPJournal atomically appends entries numbered previousSeq+1..,
	// stores channel states, and prunes the journal to limits. It returns
	// ErrConflict if the user's sequence is no longer previousSeq.
	AppendAHPJournal(ctx context.Context, userKey string, previousSeq int64, entries []AHPJournalEntry,
		states []AHPChannelState, limits AHPJournalLimits) error
	// ReadAHPJournal returns the entries after afterSeq. complete is false when
	// some of them were already pruned.
	ReadAHPJournal(ctx context.Context, userKey string, afterSeq int64) (entries []AHPJournalEntry, complete bool, err error)
	// GetAHPChannelState returns ErrNotFound for a channel the user has no state for.
	GetAHPChannelState(ctx context.Context, userKey, channel string) (AHPChannelState, error)
	DeleteAHPChannelStates(ctx context.Context, userKey string, channels []string) error
	// ListAHPChannels returns the user's channels with state whose URI starts with prefix.
	ListAHPChannels(ctx context.Context, userKey, prefix string) ([]string, error)

	PutAHPSession(ctx context.Context, session AHPSession) error
	// GetAHPSession returns ErrNotFound for a session the user does not have.
	GetAHPSession(ctx context.Context, userKey, session string) (AHPSession, error)
	// ListAHPSessions returns the user's sessions, most recently modified first.
	ListAHPSessions(ctx context.Context, userKey string) ([]AHPSession, error)
	DeleteAHPSession(ctx context.Context, userKey, session string) error

	PutAHPRecord(ctx context.Context, record AHPRecord) error
	// GetAHPRecord returns ErrNotFound for a record the user does not have.
	GetAHPRecord(ctx context.Context, userKey, kind, key string) (AHPRecord, error)
	ListAHPRecords(ctx context.Context, userKey, kind string) ([]AHPRecord, error)
	// DeleteAHPRecords removes the user's records of kind whose key starts with prefix.
	DeleteAHPRecords(ctx context.Context, userKey, kind, prefix string) error
	// DeleteAHPRecord removes one record; a missing record is not an error.
	DeleteAHPRecord(ctx context.Context, userKey, kind, key string) error
}
