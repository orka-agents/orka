package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"

	"github.com/orka-agents/orka/internal/gateway/protocol"
)

// GatewayReplySender is bound to an authenticated Task by its execution host.
// Budget is only a replay-aware snapshot, never an admission grant. Enqueue must
// independently authorize and enforce the durable lifetime quota atomically.
type GatewayReplySender interface {
	Budget(context.Context, string) (GatewayReplyBudget, error)
	Enqueue(context.Context, string, string) (GatewayReplyReceipt, error)
}

// GatewayMeetingDraftSender is optional so text-only execution hosts remain compatible.
type GatewayMeetingDraftSender interface {
	EnqueueMeetingDraft(context.Context, string, string, protocol.MeetingDraft) (GatewayReplyReceipt, error)
}

type GatewayReplyBudget struct {
	Accepted      int  `json:"accepted"`
	Limit         int  `json:"limit"`
	RequestExists bool `json:"requestExists"`
}

type GatewayReplyReceipt struct {
	DeliveryID string `json:"deliveryID"`
	Status     string `json:"status"`
	Created    bool   `json:"created"`
}

// Keep typed host failures (notably ambiguous outcomes) inspectable while the
// model and tracing receive only the safe message, never backend diagnostics.
type gatewayReplyError struct {
	message string
	cause   error
}

func (e *gatewayReplyError) Error() string { return e.message }
func (e *gatewayReplyError) Unwrap() error { return e.cause }

type ReplyInConversationTool struct{}

func NewReplyInConversationTool() *ReplyInConversationTool { return &ReplyInConversationTool{} }
func (*ReplyInConversationTool) Name() string              { return "reply_in_conversation" }
func (*ReplyInConversationTool) Description() string {
	return "Send a nonterminal message to this Task's originating gateway conversation and continue working. Content must be nonempty UTF-8, at most 16 KiB (16384 bytes); the controller enforces a configurable lifetime message limit (default 10). The receipt confirms durable acceptance, not delivery. No destination can be selected. Optional meetingDraft proposes an unscheduled meeting on capable adapters; use display names, never addresses. It does not book a calendar event."
}
func (*ReplyInConversationTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","minLength":1,"maxLength":16384,"description":"Message text, at most 16384 UTF-8 bytes; never truncated."},"meetingDraft":{"type":"object","additionalProperties":false,"required":["subject","agenda","attendees"],"properties":{"subject":{"type":"string","minLength":1,"description":"At most 200 UTF-8 bytes; no controls."},"agenda":{"type":"array","minItems":1,"maxItems":5,"items":{"type":"string","minLength":1,"description":"At most 256 UTF-8 bytes; no controls."}},"durationMinutes":{"type":"integer","minimum":1,"maximum":480,"default":30},"attendees":{"oneOf":[{"type":"object","additionalProperties":false,"required":["mode"],"properties":{"mode":{"type":"string","enum":["everyone"]}}},{"type":"object","additionalProperties":false,"required":["mode","names"],"properties":{"mode":{"type":"string","enum":["named"]},"names":{"type":"array","minItems":1,"maxItems":10,"items":{"type":"string","minLength":1,"pattern":"^[^@]+$","description":"Display name, at most 128 UTF-8 bytes; no controls or addresses."}}}}]},"preferredWindow":{"type":"string","minLength":1,"description":"At most 256 UTF-8 bytes; no controls."},"suggestedStart":{"type":"string","format":"date-time","description":"Valid ISO timestamp with explicit offset; end derives from duration."}}}},"required":["content"],"additionalProperties":false}`)
}
func (*ReplyInConversationTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	content, draft, err := replyArguments(args)
	if err != nil {
		return "", err
	}
	tc := GetToolContext(ctx)
	if tc == nil || tc.GatewayReplySender == nil || strings.TrimSpace(tc.OperationID) == "" || tc.TaskUID == "" || tc.Namespace == "" || tc.TaskID == "" {
		return "", errors.New("gateway reply is unavailable without authenticated task and operation identity")
	}
	var draftSender GatewayMeetingDraftSender
	if draft != nil {
		var ok bool
		draftSender, ok = tc.GatewayReplySender.(GatewayMeetingDraftSender)
		if !ok {
			return "", NewGatewayReplyRejection("meeting_drafts_unsupported", nil)
		}
	}
	identity, _ := json.Marshal([]string{"orka.gateway.reply.v1", tc.Namespace, tc.TaskUID, tc.OperationID})
	digest := sha256.Sum256(identity)
	requestID := "gr-" + hex.EncodeToString(digest[:])
	budget, err := tc.GatewayReplySender.Budget(ctx, requestID)
	if err != nil {
		return "", safeGatewayReplyError(err, "gateway reply budget is unavailable")
	}
	if budget.Limit <= 0 || budget.Accepted < 0 {
		return "", errors.New("gateway reply budget is invalid")
	}
	if !budget.RequestExists && budget.Accepted >= budget.Limit {
		return "", NewGatewayReplyRejection("limit_reached", nil)
	}
	var receipt GatewayReplyReceipt
	if draft != nil {
		receipt, err = draftSender.EnqueueMeetingDraft(ctx, requestID, content, *draft)
	} else {
		receipt, err = tc.GatewayReplySender.Enqueue(ctx, requestID, content)
	}
	if err != nil {
		return "", safeGatewayReplyError(err, "gateway reply admission failed; outcome may be unknown, do not regenerate the call to retry")
	}
	return ChatToolSuccess(receipt)
}

func replyArguments(args json.RawMessage) (string, *protocol.MeetingDraft, error) {
	invalid := NewGatewayReplyRejection("invalid_request", nil)
	if len(args) > 6*protocol.MaxInterimTextBytes+protocol.MaxMeetingDraftJSONBytes+1024 {
		return "", nil, invalid
	}
	fields, err := protocol.DecodeClosedObject(args, "content", "meetingDraft")
	if err != nil {
		return "", nil, invalid
	}
	var content string
	if json.Unmarshal(fields["content"], &content) != nil || len(content) > protocol.MaxInterimTextBytes || strings.TrimSpace(protocol.SanitizeMessage(content, 0)) == "" {
		return "", nil, invalid
	}
	var draft *protocol.MeetingDraft
	if raw, ok := fields["meetingDraft"]; ok {
		draft = new(protocol.MeetingDraft)
		if json.Unmarshal(raw, draft) != nil {
			return "", nil, invalid
		}
	}
	return content, draft, nil
}
