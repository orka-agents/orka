package tools

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type replySenderStub struct {
	budget     GatewayReplyBudget
	err        error
	enqueueErr error
	requests   []string
	contents   []string
}

func (s *replySenderStub) Budget(_ context.Context, id string) (GatewayReplyBudget, error) {
	return s.budget, s.err
}
func (s *replySenderStub) Enqueue(_ context.Context, id, content string) (GatewayReplyReceipt, error) {
	s.requests = append(s.requests, id)
	s.contents = append(s.contents, content)
	return GatewayReplyReceipt{DeliveryID: "gdm-receipt", Status: "Pending", Created: true}, s.enqueueErr
}
func replyContext(s GatewayReplySender) context.Context {
	return WithToolContext(context.Background(), &ToolContext{Namespace: "default", TaskID: "task", TaskUID: "task-uid", OperationID: "logical-call", GatewayReplySender: s})
}

func TestReplyInConversationStrictContent(t *testing.T) {
	tool := NewReplyInConversationTool()
	var schema map[string]any
	require.NoError(t, json.Unmarshal(tool.Parameters(), &schema))
	require.Equal(t, false, schema["additionalProperties"])
	require.Equal(t, []any{"content"}, schema["required"])
	properties := schema["properties"].(map[string]any)
	require.Len(t, properties, 1)
	require.Equal(t, float64(1), properties["content"].(map[string]any)["minLength"])
	require.Equal(t, float64(16384), properties["content"].(map[string]any)["maxLength"])
	for _, raw := range []string{`{}`, `null`, `[]`, `{"content":null}`, `{"content":7}`, `{"Content":"x"}`, `{"content":"x","target":"room"}`, `{"content":"x","requestID":"x"}`, `{"content":"x"}{}`, `{"content":"x","content":"y"}`, `{"content":""}`, `{"content":" \n\t"}`, `{"content":"\u0000"}`, `{"content":"\ud800"}`, `{"content":"\udc00"}`, "{\"content\":\"\xff\"}", `{"content":"` + strings.Repeat("x", 16385) + `"}`, `{"content":"` + strings.Repeat("é", 8193) + `"}`} {
		t.Run(raw[:min(32, len(raw))], func(t *testing.T) {
			s := &replySenderStub{budget: GatewayReplyBudget{Limit: 10}}
			result, err := tool.Execute(replyContext(s), json.RawMessage(raw))
			require.Error(t, err)
			require.Empty(t, result)
			require.Empty(t, s.contents)
		})
	}
	for _, content := range []string{strings.Repeat("x", 16384), strings.Repeat("é", 8192), "hello \U0001f30d"} {
		s := &replySenderStub{budget: GatewayReplyBudget{Limit: 10}}
		raw, err := json.Marshal(map[string]string{"content": content})
		require.NoError(t, err)
		result, err := tool.Execute(replyContext(s), raw)
		require.NoError(t, err)
		require.Equal(t, []string{content}, s.contents)
		require.NotContains(t, result, content)
		require.Contains(t, result, "gdm-receipt")
	}
}

func TestReplyInConversationBudgetIdentityAndSafeErrors(t *testing.T) {
	tool := NewReplyInConversationTool()
	args := json.RawMessage(`{"content":"private text"}`)
	for _, budget := range []GatewayReplyBudget{{Limit: 1, Accepted: 1}, {Limit: 12, Accepted: 12}, {Limit: 0}, {Limit: 10, Accepted: -1}} {
		s := &replySenderStub{budget: budget}
		_, err := tool.Execute(replyContext(s), args)
		require.Error(t, err)
		require.Empty(t, s.contents)
	}
	for _, budget := range []GatewayReplyBudget{{Limit: 1, Accepted: 0}, {Limit: 12, Accepted: 11}} {
		s := &replySenderStub{budget: budget}
		_, err := tool.Execute(replyContext(s), args)
		require.NoError(t, err)
		require.Len(t, s.contents, 1)
	}
	s := &replySenderStub{budget: GatewayReplyBudget{Limit: 1, Accepted: 1, RequestExists: true}}
	_, err := tool.Execute(replyContext(s), args)
	require.NoError(t, err)
	_, err = tool.Execute(replyContext(s), args)
	require.NoError(t, err)
	require.Equal(t, s.requests[0], s.requests[1])
	require.LessOrEqual(t, len(s.requests[0]), 256)
	tc := *GetToolContext(replyContext(s))
	tc.OperationID = "different-call"
	_, err = tool.Execute(WithToolContext(t.Context(), &tc), args)
	require.NoError(t, err)
	require.NotEqual(t, s.requests[0], s.requests[2])
	tc.OperationID = ""
	_, err = tool.Execute(WithToolContext(t.Context(), &tc), args)
	require.Error(t, err)
	_, err = tool.Execute(replyContext(nil), args)
	require.Error(t, err)
	_, err = tool.Execute(t.Context(), args)
	require.Error(t, err)
	s.err = errors.New("private token and routing diagnostic")
	_, err = tool.Execute(replyContext(s), args)
	require.Error(t, err)
	require.NotContains(t, err.Error(), "private")
	require.ErrorIs(t, err, s.err, "execution hosts must retain typed failure classification without exposing diagnostics")
	s.enqueueErr, s.err = s.err, nil
	_, err = tool.Execute(replyContext(s), args)
	require.ErrorIs(t, err, s.enqueueErr)
	require.NotContains(t, err.Error(), "private")
}

func TestReplyInConversationRegistered(t *testing.T) {
	RegisterBuiltinTools()
	definitions := DefaultRegistry.ToLLMTools([]string{"reply_in_conversation"})
	require.Len(t, definitions, 1)
}
