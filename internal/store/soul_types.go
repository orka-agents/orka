package store

import "context"

// SessionSoulDigestMetadata is controller-authored prompt revision metadata, not
// model instruction text. It follows the existing canonical transcript lifecycle.
const SessionSoulDigestMetadata = "orka.ai/soul-configuration-digest"

// SessionSoulUnboundMetadata marks a controller-authored Gateway AI error that
// failed before soul binding or execution. Only the literal "true" on a canonical
// error without digest metadata is unbound; absence preserves legacy identity.
const SessionSoulUnboundMetadata = "orka.ai/soul-unbound"

// SessionSoulAnchorSource identifies digest-only context retained for the Session lifetime.
const SessionSoulAnchorSource = "soul-context"

// SessionSoulState describes a pinned Task revision or the first canonical
// message revision (first assistant message for gateway-owned conversations).
// Established with an empty Digest means an explicitly no-soul conversation.
type SessionSoulState struct {
	Established bool
	Digest      string
	// MessageCount includes all visible messages, including unbound failures.
	MessageCount int
	// FirstMessageID excludes hidden anchors and attested unbound Gateway turns
	// so the controller can still require the current event to be first.
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
// An empty digest pins absence of a soul so an empty turn cannot permit later opt-in.
type SessionSoulWriter interface {
	EnsureSessionSoulWithLock(context.Context, string, string, string, string, string) error
}
