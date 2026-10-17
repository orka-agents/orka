package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/protocol"

	"github.com/stretchr/testify/require"
)

type meetingReplySender struct {
	replySenderStub
	draft protocol.MeetingDraft
}

func (s *meetingReplySender) EnqueueMeetingDraft(ctx context.Context, id, content string, draft protocol.MeetingDraft) (GatewayReplyReceipt, error) {
	s.draft = draft
	return s.Enqueue(ctx, id, content)
}

func TestReplyMeetingDraftSchemaUsesParserByteBounds(t *testing.T) {
	var schema map[string]any
	require.NoError(t, json.Unmarshal(NewReplyInConversationTool().Parameters(), &schema))
	properties := schema["properties"].(map[string]any)
	require.Equal(t, float64(16384), properties["content"].(map[string]any)["maxLength"])
	draft := properties["meetingDraft"].(map[string]any)["properties"].(map[string]any)
	named := draft["attendees"].(map[string]any)["oneOf"].([]any)[1].(map[string]any)["properties"].(map[string]any)
	for name, text := range map[string]map[string]any{
		"subject":          draft["subject"].(map[string]any),
		"agenda item":      draft["agenda"].(map[string]any)["items"].(map[string]any),
		"attendee name":    named["names"].(map[string]any)["items"].(map[string]any),
		"preferred window": draft["preferredWindow"].(map[string]any),
	} {
		t.Run(name, func(t *testing.T) {
			require.NotContains(t, text, "maxLength", "JSON Schema lengths count characters, not UTF-8 bytes")
			require.Equal(t, float64(1), text["minLength"])
		})
	}
}

func TestReplyMeetingDraftParserKeepsUTF8ByteBounds(t *testing.T) {
	for _, tc := range []struct {
		field string
		limit int
	}{
		{"subject", 200}, {"agenda", 256}, {"name", 128}, {"preferredWindow", 256},
	} {
		t.Run(tc.field, func(t *testing.T) {
			for _, extra := range []string{"", "x"} {
				text := strings.Repeat("é", tc.limit/2) + extra
				draft := map[string]any{"subject": "Planning", "agenda": []string{"Next steps"}, "attendees": map[string]any{"mode": "everyone"}}
				switch tc.field {
				case "agenda":
					draft[tc.field] = []string{text}
				case "name":
					draft["attendees"] = map[string]any{"mode": "named", "names": []string{text}}
				default:
					draft[tc.field] = text
				}
				raw, err := json.Marshal(map[string]any{"content": "Draft", "meetingDraft": draft})
				require.NoError(t, err)
				sender := &meetingReplySender{replySenderStub: replySenderStub{budget: GatewayReplyBudget{Limit: 10}}}
				_, err = NewReplyInConversationTool().Execute(replyContext(sender), raw)
				if extra == "" {
					require.NoError(t, err)
					require.Len(t, sender.contents, 1)
				} else {
					require.ErrorAs(t, err, new(*GatewayReplyRejection))
					require.Empty(t, sender.contents)
				}
			}
		})
	}
}

func TestReplyMeetingDraftValidationAndCanonicalDefault(t *testing.T) {
	s := &meetingReplySender{replySenderStub: replySenderStub{budget: GatewayReplyBudget{Limit: 10}}}
	tool := NewReplyInConversationTool()
	raw := `{"meetingDraft":{"subject":"Planning","agenda":["Next steps"],"attendees":{"mode":"everyone"}},"content":"draft above"}`
	result, err := tool.Execute(replyContext(s), json.RawMessage(raw))
	require.NoError(t, err)
	require.Contains(t, result, "gdm-receipt")
	require.NotContains(t, result, "Planning")
	require.Equal(t, 30, s.draft.DurationMinutes)
	_, err = tool.Execute(replyContext(s), json.RawMessage(strings.Replace(raw, `"subject":`, `"durationMinutes":30,"subject":`, 1)))
	require.NoError(t, err)
	require.Equal(t, s.requests[0], s.requests[1])
	before := len(s.contents)
	for _, bad := range []string{
		strings.Replace(raw, `"subject":`, `"subject":"Other","subject":`, 1),
		strings.Replace(raw, `"mode":"everyone"`, `"mode":"named","names":["alice@example.com"]`, 1),
		strings.Replace(raw, `"subject":`, `"url":"https://example.com","subject":`, 1),
		strings.Replace(raw, `"subject":`, `"suggestedStart":"2026-01-01T09:00:00","subject":`, 1),
		strings.Replace(raw, `"subject":`, `"durationMinutes":0,"subject":`, 1),
		strings.Replace(raw, `"subject":"Planning"`, `"subject":"\ud800"`, 1),
	} {
		_, err = tool.Execute(replyContext(s), json.RawMessage(bad))
		require.ErrorAs(t, err, new(*GatewayReplyRejection))
	}
	require.Len(t, s.contents, before)
}

// Removing the optional sender gate would incorrectly turn a structured action
// into ordinary text (or report an ambiguous write rather than non-admission).
func TestReplyMeetingDraftUnsupportedSender(t *testing.T) {
	s := &replySenderStub{budget: GatewayReplyBudget{Limit: 10}}
	_, err := NewReplyInConversationTool().Execute(replyContext(s), json.RawMessage(`{"content":"Draft","meetingDraft":{"subject":"Review","agenda":["Agree next steps"],"attendees":{"mode":"everyone"}}}`))
	require.Error(t, err)
	require.ErrorAs(t, err, new(*GatewayReplyRejection))
	require.Contains(t, err.Error(), "does not support meeting drafts")
	require.Empty(t, s.contents)
}
