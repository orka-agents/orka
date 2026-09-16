package supervisor

import (
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	// Cover the admitted turn ceiling, distinct text/thought identities, and
	// bounded housekeeping without imposing a smaller hidden turn limit.
	maxAssistantResultMessageIDs     = 4 * int(harnessv2.MaxAgentMaxTurns)
	maxAssistantResultMessageIDBytes = 512
)

// Only these pinned producers provide the message identity contract consumed
// here. Codex retains its final_answer phase projection; Copilot is unchanged.
func usesNativeAssistantMessageIdentity(provider string) bool {
	return provider == providerKindOpencode || provider == providerKindClaude
}

// assistantMessageResult keeps the newest distinct, named assistant message as
// the terminal answer. Pinned OpenCode and Claude ACP adapters expose stable
// messageId values for their assistant messages. Concatenating distinct messages
// exposes progress or compaction notes as part of the answer. Identity, never a
// text/heading heuristic, selects the final message.
// Earlier messages still travel through the normal execution-event stream.
//
// First observation determines message order. A late chunk for a previously seen
// older message must not make that message current again. State is bounded and
// belongs to one prompt, so repeated native IDs in other prompts are independent.
// Callers serialize access with the supervisor mutex.
type assistantMessageResult struct {
	messageID string
	seen      map[string]struct{}
	text      strings.Builder
	overflow  bool
	failure   error
}

func (r *assistantMessageResult) append(messageID, text string, limit int) error {
	if r.failure != nil {
		return r.failure
	}
	if messageID == "" {
		if r.messageID != "" {
			return r.invalidate(errors.New("identified assistant stream lost its message identity"))
		}
		// Retain the existing aggregation for entirely anonymous legacy streams.
		// Once named messages appear, a missing identity must fail closed rather
		// than silently merging unknown content into a selected final answer.
		return nil
	}
	if len(messageID) > maxAssistantResultMessageIDBytes {
		return r.invalidate(errors.New("assistant message identity exceeds the supported limit"))
	}
	if _, seen := r.seen[messageID]; !seen {
		if len(r.seen) >= maxAssistantResultMessageIDs {
			return r.invalidate(errors.New("assistant message identity count exceeds the supported limit"))
		}
		if r.seen == nil {
			r.seen = make(map[string]struct{})
		}
		r.seen[messageID] = struct{}{}
		r.messageID = messageID
		r.text.Reset()
		r.overflow = false
	}
	if messageID == r.messageID {
		appendBoundedPromptText(&r.text, &r.overflow, text, limit)
	}
	return nil
}

// Keep identity failures sticky through settlement and duplicate admission. The
// native provider can race cancellation with end_turn; rejecting the wire stream
// alone must not leave a replayable successful result for the invalid prompt.
func (r *assistantMessageResult) invalidate(err error) error {
	if r.failure == nil {
		r.failure = err
	}
	return r.failure
}

// nativeAssistantMessageIdentity observes only identity metadata. Thought
// content stays ignored: a newer reasoning-only assistant must invalidate an
// older text candidate, but its reasoning must never become a visible answer.
func nativeAssistantMessageIdentity(notification *acp.SessionNotification) (string, bool, error) {
	if notification == nil {
		return "", false, nil
	}
	var envelope struct {
		SessionUpdate string          `json:"sessionUpdate"`
		MessageID     json.RawMessage `json:"messageId"`
	}
	if err := json.Unmarshal(notification.Update, &envelope); err != nil {
		return "", false, errors.New("invalid assistant identity envelope")
	}
	if envelope.SessionUpdate != acpUpdateAgentMessageChunk && envelope.SessionUpdate != acpUpdateAgentThoughtChunk {
		return "", false, nil
	}
	var messageID string
	if len(envelope.MessageID) != 0 {
		if err := json.Unmarshal(envelope.MessageID, &messageID); err != nil {
			return "", false, errors.New("invalid assistant message identity")
		}
		if !wellFormedIdentityUnicode(envelope.MessageID) {
			return "", false, errors.New("invalid assistant message identity Unicode")
		}
	}
	return messageID, true, nil
}

// encoding/json repairs invalid UTF-8 and unpaired UTF-16 escapes. Identity
// must never use that lossy repair: distinct malformed IDs would otherwise
// collapse to the same map key. The caller already validates JSON syntax and
// the string/null shape; inspect the original bytes before using the value.
func wellFormedIdentityUnicode(raw json.RawMessage) bool {
	if !utf8.Valid(raw) {
		return false
	}
	for i := 0; i < len(raw); i++ {
		if raw[i] != '\\' {
			continue
		}
		i++
		if i >= len(raw) {
			return false
		}
		if raw[i] != 'u' {
			continue // Includes an escaped literal backslash before "u...".
		}
		if i+4 >= len(raw) {
			return false
		}
		first, err := strconv.ParseUint(string(raw[i+1:i+5]), 16, 16)
		if err != nil {
			return false
		}
		i += 4
		if first < 0xd800 || first > 0xdfff {
			continue
		}
		if first > 0xdbff || i+6 >= len(raw) || raw[i+1] != '\\' || raw[i+2] != 'u' {
			return false
		}
		second, err := strconv.ParseUint(string(raw[i+3:i+7]), 16, 16)
		if err != nil || second < 0xdc00 || second > 0xdfff {
			return false
		}
		i += 6
	}
	return true
}
