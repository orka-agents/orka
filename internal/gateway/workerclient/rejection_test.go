package workerclient

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/tools"
	"github.com/stretchr/testify/require"
)

func TestClientKnownRejectionCodes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		known  bool
	}{
		{"unsupported", 409, `{"error":{"code":"interim_delivery_unsupported","message":"private token"}}`, true},
		{"limit", 429, `{"error":{"code":"limit_reached","message":"private token"}}`, true},
		{"conflict", 409, `{"error":{"code":"conflict"}}`, true},
		{"unavailable", 503, `{"error":{"code":"unavailable","message":"private token"}}`, true},
		{"unknown", 409, `{"error":{"code":"private token"}}`, false},
		{"wrong status", 500, `{"error":{"code":"limit_reached"}}`, false},
		{"oversized", 409, `{"error":{"code":"conflict","message":"` + strings.Repeat("x", 4096) + `"}}`, false},
		{"trailing", 409, `{"error":{"code":"conflict"}}{}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(tc.status); _, _ = fmt.Fprint(w, tc.body) }))
			defer server.Close()
			sender, err := New(clientConfig(t, server.URL))
			require.NoError(t, err)
			for _, enqueue := range []bool{false, true} {
				if enqueue {
					_, err = sender.Enqueue(t.Context(), "id", "text")
				} else {
					_, err = sender.Budget(t.Context(), "id")
				}
				require.Error(t, err)
				var known *tools.GatewayReplyRejection
				require.Equal(t, tc.known, errors.As(err, &known))
				require.NotContains(t, err.Error(), "private")
			}
		})
	}
}
