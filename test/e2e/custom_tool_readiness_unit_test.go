//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
)

func TestCustomToolReceiverHTTPProbe(t *testing.T) {
	python, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("python3 is required to exercise the echo fixture readiness probe")
	}
	for _, tc := range []struct {
		name   string
		status int
		body   string
		closed bool
		ready  bool
	}{
		{name: "echo ready", status: http.StatusOK, body: `{"echo":"{}"}`, ready: true},
		{name: "HTTP unavailable", status: http.StatusServiceUnavailable, body: `unavailable`},
		{name: "wrong response", status: http.StatusOK, body: `{"echo":"wrong"}`},
		{name: "connection refused", closed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if err != nil || r.Method != http.MethodPost || string(body) != "{}" {
					http.Error(w, "expected echo POST", http.StatusBadRequest)
					return
				}
				w.WriteHeader(tc.status)
				_, _ = io.WriteString(w, tc.body)
			}))
			t.Cleanup(server.Close)
			if tc.closed {
				server.Close()
			}
			output, err := exec.Command(python, "-c", customToolReceiverHTTPProbe, server.URL).CombinedOutput()
			if tc.ready {
				if err != nil || strings.TrimSpace(string(output)) != "ready" {
					t.Fatalf("ready echo receiver rejected: %v\n%s", err, output)
				}
			} else if err == nil {
				t.Fatalf("unready receiver accepted: %s", output)
			}
		})
	}
}
