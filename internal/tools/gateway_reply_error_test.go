package tools

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestReplyInConversationKnownRejections(t *testing.T) {
	for _, tc := range []struct{ code, message string }{
		{"interim_delivery_unsupported", "does not support interim delivery"},
		{"limit_reached", "lifetime message limit"},
		{"conflict", "conflicts"},
		{"forbidden", "not authorized"},
		{"unavailable", "temporarily unavailable"},
	} {
		t.Run(tc.code, func(t *testing.T) {
			cause := errors.New("secret backend diagnostics")
			rejection := NewGatewayReplyRejection(tc.code, cause)
			require.NotNil(t, rejection)
			for _, enqueue := range []bool{false, true} {
				sender := &replySenderStub{budget: GatewayReplyBudget{Limit: 10}}
				if enqueue {
					sender.enqueueErr = rejection
				} else {
					sender.err = rejection
				}
				_, err := NewReplyInConversationTool().Execute(replyContext(sender), []byte(`{"content":"private"}`))
				require.ErrorContains(t, err, tc.message)
				require.ErrorIs(t, err, cause)
				require.NotContains(t, err.Error(), "secret")
				require.NotContains(t, err.Error(), "unknown")
			}
		})
	}
	require.Nil(t, NewGatewayReplyRejection("untrusted backend code", nil))
}
