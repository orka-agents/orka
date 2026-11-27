package v1

import (
	"context"
	"time"
)

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

const MaxAHPClientsPerUser = 256

const MaxAHPEndedTokensPerUser = 50

type AHPClient struct {
	ClientID        string
	UserKey         string
	ProtocolVersion string
	Dialect         string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

type AHPJournalEntry struct {
	Seq      int64
	Channel  string
	Envelope []byte
}

type AHPChannelState struct {
	Channel string
	State   []byte
	FromSeq int64
}

type AHPSession struct {
	UserKey    string
	Session    string
	Backend    string
	Summary    []byte
	CreatedAt  time.Time
	ModifiedAt time.Time
}

type AHPRecord struct {
	UserKey   string
	Kind      string
	Key       string
	Value     []byte
	UpdatedAt time.Time
}

type AHPJournalLimits struct {
	MaxEntries int
	MaxBytes   int64
}

type AHPStore interface {
	CreateAHPToken(ctx context.Context, token AHPToken) error
	GetAHPTokenByHash(ctx context.Context, tokenHash string) (AHPToken, error)
	ListAHPTokens(ctx context.Context, userKey string) ([]AHPToken, error)
	RevokeAHPToken(ctx context.Context, userKey, id string, at time.Time) error

	GetAHPClient(ctx context.Context, clientID string) (AHPClient, error)
	PutAHPClient(ctx context.Context, client AHPClient) error

	GetAHPServerSeq(ctx context.Context, userKey string) (int64, error)
	AppendAHPJournal(ctx context.Context, userKey string, previousSeq int64, entries []AHPJournalEntry,
		states []AHPChannelState, limits AHPJournalLimits) error
	ReadAHPJournal(ctx context.Context, userKey string, afterSeq int64) (entries []AHPJournalEntry, complete bool, err error)
	GetAHPChannelState(ctx context.Context, userKey, channel string) (AHPChannelState, error)
	DeleteAHPChannelStates(ctx context.Context, userKey string, channels []string) error
	ListAHPChannels(ctx context.Context, userKey, prefix string) ([]string, error)

	PutAHPSession(ctx context.Context, session AHPSession) error
	GetAHPSession(ctx context.Context, userKey, session string) (AHPSession, error)
	ListAHPSessions(ctx context.Context, userKey string) ([]AHPSession, error)
	DeleteAHPSession(ctx context.Context, userKey, session string) error

	PutAHPRecord(ctx context.Context, record AHPRecord) error
	GetAHPRecord(ctx context.Context, userKey, kind, key string) (AHPRecord, error)
	ListAHPRecords(ctx context.Context, userKey, kind string) ([]AHPRecord, error)
	DeleteAHPRecords(ctx context.Context, userKey, kind, prefix string) error
	DeleteAHPRecord(ctx context.Context, userKey, kind, key string) error
}
