package store

import "context"

// SessionSoulDigestMetadata is controller-authored prompt revision metadata, not
// model instruction text. It follows the existing canonical transcript lifecycle.
const SessionSoulDigestMetadata = "orka.ai/soul-configuration-digest"

// SessionSoulAnchorSource identifies digest-only context retained for the Session lifetime.
const SessionSoulAnchorSource = "soul-context"

// SessionSoulState describes a pinned Task revision or the first canonical
// message revision (first assistant message for gateway-owned conversations).
type SessionSoulState struct {
	Established    bool
	Digest         string
	MessageCount   int
	FirstMessageID string
	SessionType    string
}

// SessionSoulReader reads only bounded revision metadata while checking the
// caller still owns the legacy Task lock. No schema or transcript content changes
// are needed to retain the revision across Task cleanup.
type SessionSoulReader interface {
	ReadSessionSoul(context.Context, string, string, string, string) (SessionSoulState, error)
}

// SessionSoulWriter durably pins ordinary AI Session identity under its exact
// Task lock before execution. It writes only digest metadata, never persona text.
type SessionSoulWriter interface {
	EnsureSessionSoulWithLock(context.Context, string, string, string, string, string) error
}
