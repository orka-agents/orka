//go:build e2e

package e2e

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestPostLiveChatSSECancelsFailedSession(t *testing.T) {
	for _, tc := range []struct {
		name          string
		stream        string
		waitForClose  bool
		cancelStatus  int
		wantErr       string
		wantCancelled bool
	}{
		{name: "timeout", waitForClose: true, cancelStatus: http.StatusNoContent, wantErr: "context deadline exceeded", wantCancelled: true},
		{name: "incomplete stream", stream: "", cancelStatus: http.StatusNoContent, wantErr: "did not emit a done event", wantCancelled: true},
		{name: "server error", stream: "event: error\ndata: inference failed\n\n", cancelStatus: http.StatusNoContent, wantErr: "received SSE error event", wantCancelled: true},
		{name: "cancellation failure is preserved", cancelStatus: http.StatusServiceUnavailable, wantErr: "cancellation returned HTTP 503", wantCancelled: true},
		{name: "completed stream", stream: "event: message\ndata: {\"content\":\"OK\"}\n\nevent: done\ndata: {\"usage\":{\"llmCalls\":1}}\n\n", wantCancelled: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var deletes atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch {
				case r.Method == http.MethodPost && r.URL.Path == "/api/v1/chat":
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "event: status\ndata: {\"sessionId\":\"e2e-failed-chat\"}\n\n"+tc.stream)
					w.(http.Flusher).Flush()
					if tc.waitForClose {
						<-r.Context().Done()
					}
				case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/chat/e2e-failed-chat":
					deletes.Add(1)
					w.WriteHeader(tc.cancelStatus)
				default:
					t.Errorf("unexpected method/path: %s %s", r.Method, r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			sessionID, content, _, _, err := postLiveChatSSEWithTimeout(server.URL, "test-only", "provider", "model", "OK", 100*time.Millisecond)
			if sessionID != "e2e-failed-chat" {
				t.Fatalf("observed Session identity was lost: %q", sessionID)
			}
			if tc.wantErr == "" {
				if err != nil || content != "OK" {
					t.Fatalf("successful stream failed: content=%q error=%v", content, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("expected %q, got %v", tc.wantErr, err)
			}
			wantDeletes := int32(0)
			if tc.wantCancelled {
				wantDeletes = 1
			}
			if deletes.Load() != wantDeletes {
				t.Fatalf("cancellation calls = %d, want %d", deletes.Load(), wantDeletes)
			}
		})
	}
}

func TestParseLiveChatSSEKeepsSessionIdentityOnFailure(t *testing.T) {
	for _, tail := range []string{"", "event: error\ndata: failure\n\n", "event: done\ndata: not-json\n\n"} {
		id, _, _, _, err := parseLiveChatSSE(strings.NewReader("event: status\ndata: {\"sessionId\":\"known-session\"}\n\n" + tail))
		if id != "known-session" || err == nil {
			t.Fatalf("expected failed stream to retain Session identity, got %q, %v", id, err)
		}
	}
}
