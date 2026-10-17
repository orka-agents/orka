package api

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"

	"net/http"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/orka-agents/orka/internal/gateway/workerclient"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/tools"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const apiMeetingDraft = `{"subject":"Planning","agenda":["Next steps"],"attendees":{"mode":"named","names":["Alice Smith"]},"suggestedStart":"2026-10-12T09:00:00+12:00"}`

func setAPIMeetingCapability(t *testing.T, reader client.Client, namespace string, enabled bool) {
	t.Helper()
	g := &gatewayv1alpha1.Gateway{}
	require.NoError(t, reader.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: "chat"}, g))
	// JSON exercises the additive API representation as well as service admission.
	data := `{"interimDelivery":true,"extensions":` + map[bool]string{true: `["meetingDraft.v1"]`, false: `[]`}[enabled] + `}`
	require.NoError(t, json.Unmarshal([]byte(data), &g.Status.ObservedCapabilities.Capabilities))
	require.NoError(t, reader.Status().Update(t.Context(), g))
}

func TestInternalGatewayMeetingDraftAdmissionReplayAndBoundary(t *testing.T) {
	f := newGatewayMessageAPIFixture(t)
	body := `{"content":"draft above","requestID":"meeting","meetingDraft":` + apiMeetingDraft + `}`
	status, result := f.request(t, []byte(body))
	require.Equal(t, http.StatusConflict, status, result)
	require.Equal(t, "meeting_drafts_unsupported", result["error"].(map[string]any)["code"])
	rows, err := f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Empty(t, rows)
	setAPIMeetingCapability(t, f.service.Client, f.task.Namespace, true)
	f.service.Config.InterimMessagesPerTask = 1
	status, result = f.request(t, []byte(body))
	require.Equal(t, http.StatusAccepted, status, result)
	id := result["deliveryID"].(string)
	row, err := f.db.GetGatewayDelivery(t.Context(), f.task.Namespace, id)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"meeting.version": "1", "meeting.subject": "Planning", "meeting.durationMinutes": "30", "meeting.attendees": "named", "meeting.name.0": "Alice Smith", "meeting.agenda.0": "Next steps", "meeting.suggestedStart": "2026-10-12T09:00:00+12:00"}, row.Metadata)
	status, result = f.request(t, []byte(strings.Replace(body, `"subject":`, `"durationMinutes":30,"subject":`, 1)))
	require.Equal(t, http.StatusOK, status, result)
	require.Equal(t, id, result["deliveryID"])
	for _, changed := range []string{strings.Replace(body, "Next steps", "Different agenda", 1), strings.Replace(body, "Alice Smith", "Bob", 1), strings.Replace(body, "09:00:00", "10:00:00", 1), `{"content":"draft above","requestID":"meeting"}`} {
		status, result = f.request(t, []byte(changed))
		require.Equal(t, http.StatusConflict, status, result)
	}
	for _, bad := range []string{strings.Replace(body, `"subject":`, `"url":"https://example.com","subject":`, 1), strings.Replace(body, "Alice Smith", "alice@example.com", 1), strings.Replace(body, `"content":`, `"content":"other","content":`, 1), strings.Replace(body, `"meetingDraft":`, `"MeetingDraft":`, 1), strings.Replace(body, `"subject":`, `"subject":"Other","subject":`, 1), strings.Replace(body, `"requestID":`, `"metadata":{"meeting.version":"1"},"requestID":`, 1), strings.Replace(body, apiMeetingDraft, "null", 1)} {
		status, result = f.request(t, []byte(bad))
		require.Equal(t, http.StatusBadRequest, status, result)
	}
	setAPIMeetingCapability(t, f.service.Client, f.task.Namespace, false)
	status, result = f.request(t, []byte(body))
	require.Equal(t, http.StatusOK, status, result)
	require.Equal(t, id, result["deliveryID"])
	rows, err = f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
}

func TestNativeGatewayMeetingDraftToolAPIAndSQLite(t *testing.T) {
	f := newGatewayMessageAPIFixture(t)
	setAPIMeetingCapability(t, f.service.Client, f.task.Namespace, true)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		response, err := f.app.Test(r, fiber.TestConfig{Timeout: 0})
		require.NoError(t, err)
		defer response.Body.Close() //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(response.StatusCode)
		_, err = io.Copy(w, response.Body)
		require.NoError(t, err)
	}))
	defer server.Close()
	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(f.token), 0600))
	sender, err := workerclient.New(workerclient.Config{ControllerURL: server.URL, Namespace: f.task.Namespace, TaskName: f.task.Name, TaskUID: string(f.task.UID), TokenFile: tokenFile})
	require.NoError(t, err)
	tc := &tools.ToolContext{Namespace: f.task.Namespace, TaskID: f.task.Name, TaskUID: string(f.task.UID), OperationID: "meeting-operation", GatewayReplySender: sender}
	ctx := tools.WithToolContext(t.Context(), tc)
	tool := tools.NewReplyInConversationTool()
	args := json.RawMessage(`{"content":"Draft above","meetingDraft":` + apiMeetingDraft + `}`)
	result, err := tool.Execute(ctx, args)
	require.NoError(t, err)
	require.Contains(t, result, `"created":true`)
	require.NotContains(t, result, "Alice Smith")
	result, err = tool.Execute(ctx, args)
	require.NoError(t, err)
	require.Contains(t, result, `"created":false`)
	_, err = tool.Execute(ctx, json.RawMessage(strings.Replace(string(args), "Alice Smith", "Bob", 1)))
	require.ErrorContains(t, err, "conflicts")
	require.NotContains(t, err.Error(), "unknown")
	setAPIMeetingCapability(t, f.service.Client, f.task.Namespace, false)
	tc.OperationID = "unsupported-meeting"
	_, err = tool.Execute(ctx, args)
	require.ErrorContains(t, err, "does not support meeting drafts")
	require.NotContains(t, err.Error(), "unknown")
	require.ErrorAs(t, err, new(*tools.GatewayReplyRejection))
	rows, err := f.db.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "30", rows[0].Metadata["meeting.durationMinutes"])
}

func TestBrokeredGatewayMeetingDraftUnsupportedIsDefinitive(t *testing.T) {
	f, _ := newBrokeredReplyFixture(t)
	f.request.Call.Arguments = json.RawMessage(`{"content":"Draft above","meetingDraft":` + apiMeetingDraft + `}`)
	replyRequestDigest(t, f)
	result := f.call(t)
	require.Equal(t, http.StatusOK, result.Code, result.Body.String())
	require.Contains(t, result.Body.String(), "does not support meeting drafts")
	require.NotContains(t, result.Body.String(), "unknown")
	rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Empty(t, rows)
}

func TestBrokeredGatewayMeetingDraftFrozenDescriptor(t *testing.T) {
	// Removing the descriptor fence would let a content-only frozen session
	// enqueue a draft under the current registry implementation.
	const contentOnlySchema = `{"type":"object","properties":{"content":{"type":"string","minLength":1,"maxLength":16384,"description":"Message text, at most 16384 UTF-8 bytes; never truncated."}},"required":["content"],"additionalProperties":false}`
	currentSchema := tools.NewReplyInConversationTool().Parameters()
	var schemaObject map[string]any
	require.NoError(t, json.Unmarshal(currentSchema, &schemaObject))
	reorderedSchema, err := json.MarshalIndent(schemaObject, "", "  ")
	require.NoError(t, err)
	for _, test := range []struct {
		name       string
		schema     json.RawMessage
		allowDraft bool
	}{
		{"old content-only", json.RawMessage(contentOnlySchema), false},
		{"draft property without exact schema", json.RawMessage(`{"type":"object","properties":{"content":{"type":"string"},"meetingDraft":{"type":"object"}},"required":["content"],"additionalProperties":false}`), false},
		{"current", currentSchema, true},
		{"current reordered JSON", reorderedSchema, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			f, service := newBrokeredReplyFixture(t)
			service.Config.InterimMessagesPerTask = 1
			g := &gatewayv1alpha1.Gateway{}
			require.NoError(t, service.Client.Get(t.Context(), client.ObjectKey{Namespace: f.task.Namespace, Name: "chat"}, g))
			g.Status.ObservedCapabilities.Capabilities.Extensions = []string{"meetingDraft.v1"}
			require.NoError(t, service.Client.Update(t.Context(), g))

			// Freeze the actual descriptor, including its digest, at the same
			// authenticated configuration boundary external ACP sessions use.
			f.request.Authorization.ToolPolicy.Tools[0].InputSchema = test.schema
			var err error
			f.request.Authorization.ToolPolicy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(f.request.Authorization.ToolPolicy.Tools)
			require.NoError(t, err)
			frozen := f.request.Authorization.Configuration()
			f.credentials.ExpectedMCPConfiguration = &frozen
			f.request.Call.Arguments = json.RawMessage(`{"content":"draft above","meetingDraft":` + apiMeetingDraft + `}`)
			replyRequestDigest(t, f)

			result := f.call(t)
			require.Equal(t, http.StatusOK, result.Code, result.Body.String())
			var response harnessv2.MCPBrokerCallResponse
			require.NoError(t, json.Unmarshal(result.Body.Bytes(), &response))
			require.Equal(t, !test.allowDraft, response.IsError, result.Body.String())
			require.NotContains(t, result.Body.String(), "unknown")
			rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
			require.NoError(t, err)
			if test.allowDraft {
				require.Len(t, rows, 1)
				require.Equal(t, "Planning", rows[0].Metadata["meeting.subject"])
				return
			}
			require.Contains(t, result.Body.String(), "does not support meeting drafts")
			require.Empty(t, rows, "rejected draft must not enter the outbox")
			identity := store.ExternalEffectIdentity{Kind: "acp-mcp-tool", Namespace: f.task.Namespace, AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID)}
			effect, err := f.control.GetExternalEffectByIdentity(t.Context(), identity)
			require.NoError(t, err)
			require.Equal(t, store.ExternalEffectSucceeded, effect.State, "definitive rejection must not leave an unknown operation")
			replay := f.call(t)
			require.Equal(t, http.StatusOK, replay.Code, replay.Body.String())
			require.NoError(t, json.Unmarshal(replay.Body.Bytes(), &response))
			require.True(t, response.IsError)
			require.True(t, response.Replayed)

			// The rejected call consumes none of the single-message quota, and
			// an old descriptor can still send ordinary content unchanged.
			f.request.Metadata.OperationID = "plain-content"
			f.request.Call.CallID = "plain-content"
			f.request.Call.Arguments = json.RawMessage(`{"content":"ordinary reply"}`)
			replyRequestDigest(t, f)
			plain := f.call(t)
			require.Equal(t, http.StatusOK, plain.Code, plain.Body.String())
			response = harnessv2.MCPBrokerCallResponse{}
			require.NoError(t, json.Unmarshal(plain.Body.Bytes(), &response))
			require.False(t, response.IsError, plain.Body.String())
			rows, err = f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
			require.NoError(t, err)
			require.Len(t, rows, 1)
			require.Equal(t, "ordinary reply", rows[0].Text)
			require.Empty(t, rows[0].Metadata)
		})
	}
}

func TestBrokeredGatewayMeetingDraftTransport(t *testing.T) {
	f, service := newBrokeredReplyFixture(t)
	g := &gatewayv1alpha1.Gateway{}
	require.NoError(t, service.Client.Get(t.Context(), client.ObjectKey{Namespace: f.task.Namespace, Name: "chat"}, g))
	require.NoError(t, json.Unmarshal([]byte(`{"interimDelivery":true,"extensions":["meetingDraft.v1"]}`), &g.Status.ObservedCapabilities.Capabilities))
	require.NoError(t, service.Client.Update(t.Context(), g))
	f.request.Call.Arguments = json.RawMessage(`{"content":"draft above","meetingDraft":` + apiMeetingDraft + `}`)
	replyRequestDigest(t, f)
	result := f.call(t)
	require.Equal(t, http.StatusOK, result.Code, result.Body.String())
	require.NotContains(t, result.Body.String(), "isError\":true")
	rows, err := f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, "Alice Smith", rows[0].Metadata["meeting.name.0"])
	require.Equal(t, "30", rows[0].Metadata["meeting.durationMinutes"])
	require.NotContains(t, result.Body.String(), "Alice Smith")
	require.Equal(t, http.StatusOK, f.call(t).Code)
	rows, err = f.data.ListGatewayDeliveries(t.Context(), store.GatewayDeliveryFilter{Namespace: f.task.Namespace})
	require.NoError(t, err)
	require.Len(t, rows, 1)
}
