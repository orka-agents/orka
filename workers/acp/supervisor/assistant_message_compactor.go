package supervisor

import (
	"bytes"
	"encoding/json"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const assistantMessageCoalesceWindow = 25 * time.Millisecond

// assistantMessageCompactor batches only adjacent ACP assistant text deltas.
// It runs before harness identity assignment so emitted sequences remain
// contiguous while the protocol rate limiter still sees every meaningful
// lifecycle update.
type assistantMessageCompactor struct {
	maxBytes      int
	flushInterval time.Duration
	pending       acp.PromptEvent
	text          strings.Builder
	messageID     string
	meta          json.RawMessage
	deadline      time.Time
	timer         *time.Timer
}

func newAssistantMessageCompactor() *assistantMessageCompactor {
	return &assistantMessageCompactor{
		maxBytes:      harnessv2.MaxProtocolStringBytes,
		flushInterval: assistantMessageCoalesceWindow,
	}
}

func (c *assistantMessageCompactor) timerChannel() <-chan time.Time {
	if c.timer == nil {
		return nil
	}
	return c.timer.C
}

func (c *assistantMessageCompactor) push(event acp.PromptEvent, arrivedAt time.Time) []acp.PromptEvent {
	ready := make([]acp.PromptEvent, 0, 2)
	if c.text.Len() > 0 && !arrivedAt.Before(c.deadline) {
		ready = append(ready, c.flush())
	}

	chunk, isAssistantText := decodeAssistantMessageChunk(event)
	if !isAssistantText {
		if c.text.Len() > 0 {
			ready = append(ready, c.flush())
		}
		return append(ready, event)
	}
	text := chunk.Content.Text
	if text == "" {
		return ready
	}
	if c.text.Len() > 0 && (event.Update.SessionID != c.pending.Update.SessionID ||
		chunk.MessageID != c.messageID || !bytes.Equal(chunk.Meta, c.meta)) {
		ready = append(ready, c.flush())
	}

	for len(text) > 0 {
		if c.text.Len() == 0 {
			c.pending = event
			c.messageID = chunk.MessageID
			c.meta = append(c.meta[:0], chunk.Meta...)
			c.arm(arrivedAt)
		}
		remaining := c.maxBytes - c.text.Len()
		prefixBytes := utf8SafePrefixBytes(text, remaining)
		if prefixBytes == 0 {
			ready = append(ready, c.flush())
			continue
		}
		_, _ = c.text.WriteString(text[:prefixBytes])
		c.pending.Sequence = event.Sequence
		c.pending.Timestamp = event.Timestamp
		text = text[prefixBytes:]
		if c.text.Len() == c.maxBytes {
			ready = append(ready, c.flush())
		}
	}
	return ready
}

func (c *assistantMessageCompactor) flushPending() []acp.PromptEvent {
	if c.text.Len() == 0 {
		return nil
	}
	return []acp.PromptEvent{c.flush()}
}

func (c *assistantMessageCompactor) close() {
	c.stopTimer()
}

func (c *assistantMessageCompactor) arm(now time.Time) {
	c.deadline = now.Add(c.flushInterval)
	c.timer = time.NewTimer(c.flushInterval)
}

func (c *assistantMessageCompactor) flush() acp.PromptEvent {
	const acpUpdateAgentMessageChunk = "agent_message_chunk"

	event := c.pending
	encoded, err := json.Marshal(struct {
		SessionUpdate string `json:"sessionUpdate"`
		MessageID     string `json:"messageId,omitempty"`
		Content       struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		Meta json.RawMessage `json:"_meta,omitempty"`
	}{
		SessionUpdate: acpUpdateAgentMessageChunk,
		MessageID:     c.messageID,
		Content: struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}{Type: acpContentTypeText, Text: c.text.String()},
		Meta: c.meta,
	})
	if err != nil {
		panic(err) // json.Marshal cannot fail for this fixed string-only shape.
	}
	event.Update = &acp.SessionNotification{SessionID: event.Update.SessionID, Update: encoded}
	c.text.Reset()
	c.pending = acp.PromptEvent{}
	c.messageID = ""
	c.meta = nil
	c.deadline = time.Time{}
	c.stopTimer()
	return event
}

func (c *assistantMessageCompactor) stopTimer() {
	if c.timer == nil {
		return
	}
	if !c.timer.Stop() {
		select {
		case <-c.timer.C:
		default:
		}
	}
	c.timer = nil
}

func assistantMessageText(event acp.PromptEvent) (string, bool) {
	chunk, ok := decodeAssistantMessageChunk(event)
	return chunk.Content.Text, ok
}

type assistantMessageChunk struct {
	SessionUpdate string `json:"sessionUpdate"`
	MessageID     string `json:"messageId,omitempty"`
	Content       struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Meta json.RawMessage `json:"_meta,omitempty"`
}

func decodeAssistantMessageChunk(event acp.PromptEvent) (assistantMessageChunk, bool) {
	const acpUpdateAgentMessageChunk = "agent_message_chunk"

	var envelope assistantMessageChunk
	if event.Type != acp.PromptEventUpdate || event.Update == nil {
		return envelope, false
	}
	if json.Unmarshal(event.Update.Update, &envelope) != nil ||
		envelope.SessionUpdate != acpUpdateAgentMessageChunk || envelope.Content.Type != acpContentTypeText {
		return assistantMessageChunk{}, false
	}
	return envelope, true
}

func utf8SafePrefixBytes(value string, limit int) int {
	if limit <= 0 {
		return 0
	}
	if len(value) <= limit {
		return len(value)
	}
	prefix := limit
	for prefix > 0 && !utf8.RuneStart(value[prefix]) {
		prefix--
	}
	return prefix
}
