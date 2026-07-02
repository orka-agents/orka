package harness

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/orka-agents/orka/internal/store"
)

const mappedFrameIdentityKeySeparator = "\x00"

// MappedFrameIdentity is the stable harness identity persisted in
// ExecutionEvent.Content under the "harness" key by MapFrameToExecutionEvent.
//
// The shape is intentionally owned by the harness package because controller
// restart dedupe depends on the same identity that the mapper writes.
type MappedFrameIdentity struct {
	Version          string           `json:"version,omitempty"`
	FrameType        FrameType        `json:"frameType,omitempty"`
	RuntimeSessionID RuntimeSessionID `json:"runtimeSessionID"`
	TurnID           HarnessTurnID    `json:"turnID"`
	CorrelationID    string           `json:"correlationID"`
	Seq              int64            `json:"seq"`
}

// MappedFrameIdentityFromFrame returns the persisted identity for a streamed
// harness frame. Callers should validate the frame before persisting it.
func MappedFrameIdentityFromFrame(frame HarnessEventFrame) MappedFrameIdentity {
	return MappedFrameIdentity{
		Version:          frame.Version,
		FrameType:        frame.Type,
		RuntimeSessionID: frame.RuntimeSessionID,
		TurnID:           frame.TurnID,
		CorrelationID:    frame.CorrelationID,
		Seq:              frame.Seq,
	}
}

// MappedFrameKey returns the stable dedupe key for a streamed harness frame.
func MappedFrameKey(frame HarnessEventFrame) string {
	return MappedFrameIdentityFromFrame(frame).Key()
}

// Key returns the stable dedupe key for this mapped frame identity.
func (i MappedFrameIdentity) Key() string {
	return strings.Join([]string{
		string(i.RuntimeSessionID),
		string(i.TurnID),
		i.CorrelationID,
		strconv.FormatInt(i.Seq, 10),
	}, mappedFrameIdentityKeySeparator)
}

// Valid reports whether this identity carries enough data to dedupe a mapped
// harness frame. Version and FrameType are deliberately optional so older
// persisted events that only carried the original four identity fields remain
// recoverable across upgrades.
func (i MappedFrameIdentity) Valid() bool {
	return strings.TrimSpace(string(i.RuntimeSessionID)) != "" &&
		strings.TrimSpace(string(i.TurnID)) != "" &&
		strings.TrimSpace(i.CorrelationID) != "" &&
		i.Seq > 0
}

// HasTurnID reports whether the identity belongs to the supplied turn ID.
func (i MappedFrameIdentity) HasTurnID(turnID HarnessTurnID) bool {
	return strings.TrimSpace(string(i.TurnID)) == strings.TrimSpace(string(turnID))
}

// MappedFrameIdentityFromEvent extracts the mapped harness identity from a
// store event produced by MapFrameToExecutionEvent. Non-harness events,
// malformed content, and incomplete identities are ignored.
func MappedFrameIdentityFromEvent(event store.ExecutionEvent) (MappedFrameIdentity, bool) {
	return MappedFrameIdentityFromContent(event.Content)
}

// MappedFrameIdentityFromContent extracts the mapped harness identity from the
// persisted ExecutionEvent.Content JSON object.
func MappedFrameIdentityFromContent(raw json.RawMessage) (MappedFrameIdentity, bool) {
	if len(raw) == 0 {
		return MappedFrameIdentity{}, false
	}
	var content struct {
		Harness MappedFrameIdentity `json:"harness"`
	}
	if err := json.Unmarshal(raw, &content); err != nil {
		return MappedFrameIdentity{}, false
	}
	if !content.Harness.Valid() {
		return MappedFrameIdentity{}, false
	}
	return content.Harness, true
}
