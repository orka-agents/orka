package v1

import (
	"context"
)

type SessionSoulState struct {
	Established    bool
	Digest         string
	MessageCount   int
	FirstMessageID string
	SessionType    string
}

type SessionSoulReader interface {
	ReadSessionSoul(context.Context, string, string, string, string) (SessionSoulState, error)
}

type SessionSoulWriter interface {
	EnsureSessionSoulWithLock(ctx context.Context, owner SessionOwnerWrite, digest string) error
}

const SessionSoulDigestMetadata = "orka.ai/soul-configuration-digest"

const SessionSoulUnboundMetadata = "orka.ai/soul-unbound"

const SessionSoulAnchorSource = "soul-context"
