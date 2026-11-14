package protocol

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDelegatedTextRequiresReservedGrantReference(t *testing.T) {
	for _, tc := range []struct {
		kind, ref string
		valid     bool
	}{
		{"delegatedText", "account-generation-opaque", true}, {"delegatedText", "", false},
		{"delegatedText", "account-token:sender-generation-1", false}, {"delegatedText", "token=sender-generation-1", false},
		{"text", "account-generation-opaque", false}, {"text", "", true},
	} {
		event := &EventEnvelope{ProtocolVersion: Version, ExternalEventID: "e", EventType: tc.kind, AccountID: "a", ContextID: "c", Sender: Sender{ID: "s"}, Text: "hello"}
		if tc.ref != "" {
			event.Metadata = map[string]string{"gatewayGrantRef": tc.ref}
		}
		err := ValidateEvent(event)
		if tc.valid {
			require.NoError(t, err)
		} else {
			require.Error(t, err)
		}
	}
	require.Error(t, ValidateAllowedMetadata(map[string]string{"gatewayGrantRef": "opaque"}, nil))
}
