package workerclient

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/protocol"
	"github.com/orka-agents/orka/internal/tools"
	"github.com/stretchr/testify/require"
)

func TestClientMeetingDraftTypedTransport(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		require.Equal(t, "Bearer projected-test-token", r.Header.Get("Authorization"))
		var body map[string]json.RawMessage
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Len(t, body, 3)
		require.JSONEq(t, `"logical-id"`, string(body["requestID"]))
		require.JSONEq(t, `{"subject":"Planning","agenda":["Next steps"],"durationMinutes":30,"attendees":{"mode":"everyone"}}`, string(body["meetingDraft"]))
		w.WriteHeader(http.StatusAccepted)
		_, _ = fmt.Fprint(w, `{"deliveryID":"gdm-receipt","status":"Pending","created":true}`)
	}))
	defer server.Close()
	c, err := New(clientConfig(t, server.URL))
	require.NoError(t, err)
	sender, ok := any(c).(tools.GatewayMeetingDraftSender)
	require.True(t, ok, "native sender must support typed drafts")
	draft := protocol.MeetingDraft{Subject: "Planning", Agenda: []string{"Next steps"}, Attendees: protocol.MeetingAttendees{Mode: "everyone"}}
	receipt, err := sender.EnqueueMeetingDraft(t.Context(), "logical-id", "Draft above", draft)
	require.NoError(t, err)
	require.Equal(t, "gdm-receipt", receipt.DeliveryID)
	draft.Attendees = protocol.MeetingAttendees{Mode: "named", Names: []string{"alice@example.com"}}
	_, err = sender.EnqueueMeetingDraft(t.Context(), "logical-id", "Draft above", draft)
	require.Error(t, err)
	require.Equal(t, 1, calls)
}
