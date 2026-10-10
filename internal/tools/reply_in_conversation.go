package tools

import (
	"bytes"
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
	return "Send a nonterminal message to this Task's originating gateway conversation and continue working. Content must be nonempty UTF-8, at most 16 KiB (16384 bytes); the controller enforces a configurable lifetime message limit (default 10). The receipt confirms durable acceptance, not delivery. No destination can be selected."
}
func (*ReplyInConversationTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{"content":{"type":"string","minLength":1,"maxLength":16384,"description":"Message text, at most 16384 UTF-8 bytes; never truncated."}},"required":["content"],"additionalProperties":false}`)
}
func (*ReplyInConversationTool) Execute(ctx context.Context, args json.RawMessage) (string, error) {
	content, err := replyContent(args)
	if err != nil {
		return "", err
	}
	tc := GetToolContext(ctx)
	if tc == nil || tc.GatewayReplySender == nil || strings.TrimSpace(tc.OperationID) == "" || tc.TaskUID == "" || tc.Namespace == "" || tc.TaskID == "" {
		return "", errors.New("gateway reply is unavailable without authenticated task and operation identity")
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
	receipt, err := tc.GatewayReplySender.Enqueue(ctx, requestID, content)
	if err != nil {
		return "", safeGatewayReplyError(err, "gateway reply admission failed; outcome may be unknown, do not regenerate the call to retry")
	}
	return ChatToolSuccess(receipt)
}

func replyContent(args json.RawMessage) (string, error) {
	invalid := NewGatewayReplyRejection("invalid_request", nil)
	if len(args) > 6*protocol.MaxInterimTextBytes+1024 || !protocol.ValidJSONUnicode(args) {
		return "", invalid
	}
	// Tokens enforce exact field spelling and reject duplicate keys, unlike struct
	// decoding (case-insensitive) or map decoding (last duplicate wins).
	d := json.NewDecoder(bytes.NewReader(args))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return "", invalid
	}
	token, err = d.Token()
	if err != nil || token != "content" {
		return "", invalid
	}
	var content *string
	if d.Decode(&content) != nil || content == nil || d.More() {
		return "", invalid
	}
	token, err = d.Token()
	if err != nil || token != json.Delim('}') {
		return "", invalid
	}
	if len(*content) > protocol.MaxInterimTextBytes || strings.TrimSpace(protocol.SanitizeMessage(*content, 0)) == "" {
		return "", invalid
	}
	return *content, nil
}
