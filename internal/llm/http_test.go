/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package llm_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/llm"
	_ "github.com/orka-agents/orka/internal/llm/anthropic"
	_ "github.com/orka-agents/orka/internal/llm/openai"
	"github.com/stretchr/testify/require"
)

func TestFreshProvidersReuseConnectionsAndResolveCredentials(t *testing.T) {
	for _, kind := range []string{"openai", "anthropic"} {
		t.Run(kind, func(t *testing.T) {
			var mu sync.Mutex
			addresses := map[string]bool{}
			requests := 0
			credentialsCorrect := true
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				addresses[r.RemoteAddr] = true
				expected := fmt.Sprintf("synthetic-key-%d", requests)
				actual := r.Header.Get("X-Api-Key")
				if kind == "openai" {
					actual = r.Header.Get("Authorization")
					expected = "Bearer " + expected
				}
				credentialsCorrect = credentialsCorrect && actual == expected
				requests++
				mu.Unlock()
				w.Header().Set("Content-Type", "application/json")
				if kind == "openai" {
					_, _ = fmt.Fprint(w, `{"id":"resp_test","object":"response","model":"test-model","status":"completed","output":[{"id":"msg_test","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
				} else {
					_, _ = fmt.Fprint(w, `{"id":"msg_test","type":"message","role":"assistant","model":"test-model","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
				}
			}))
			defer server.Close()
			for i := range 12 {
				p, err := llm.NewProvider(kind, llm.ProviderConfig{APIKey: fmt.Sprintf("synthetic-key-%d", i), BaseURL: server.URL})
				require.NoError(t, err)
				response, err := p.Complete(t.Context(), &llm.CompletionRequest{Model: "test-model", Messages: []llm.Message{{Role: "user", Content: "hello"}}})
				require.NoError(t, err)
				require.Equal(t, "ok", response.Content)
			}
			mu.Lock()
			defer mu.Unlock()
			require.Equal(t, 12, requests)
			require.Len(t, addresses, 1, "request-scoped provider clients must share their connection pool")
			require.True(t, credentialsCorrect, "each request must use its freshly resolved credentials")
		})
	}
}

func TestSharedHTTPClientPreservesHeaderTimeoutAndCancellation(t *testing.T) {
	client := llm.SharedHTTPClient()
	require.Same(t, client, llm.SharedHTTPClient())
	transport, ok := client.Transport.(*http.Transport)
	require.True(t, ok)
	require.NotSame(t, http.DefaultTransport, transport)
	require.Equal(t, 10*time.Minute, transport.ResponseHeaderTimeout)
	require.Nil(t, client.Jar, "provider credentials must not acquire shared cookie state")
	started := make(chan struct{})
	cancelled := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/cancel" {
			close(started)
			<-r.Context().Done()
			close(cancelled)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/cancel", nil)
	require.NoError(t, err)
	done := make(chan error, 1)
	go func() {
		resp, requestErr := client.Do(req)
		if resp != nil {
			_ = resp.Body.Close()
		}
		done <- requestErr
	}()
	<-started
	cancel()
	require.ErrorIs(t, <-done, context.Canceled)
	select {
	case <-cancelled:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream did not observe cancellation")
	}
	req, err = http.NewRequestWithContext(t.Context(), http.MethodGet, server.URL+"/healthy", nil)
	require.NoError(t, err)
	resp, err := client.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusNoContent, resp.StatusCode)
}
