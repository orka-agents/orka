package workerclient

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func clientConfig(t *testing.T, url string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(path, []byte("projected-test-token"), 0600))
	return Config{ControllerURL: url, Namespace: "default", TaskName: "task", TaskUID: "task-uid", TokenFile: path}
}

func TestClientBoundRequestAndTokenRotation(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := requests.Add(1)
		want := "projected-test-token"
		if n > 1 {
			want = "rotated-test-token"
		}
		if r.Header.Get("Authorization") != "Bearer "+want {
			t.Error("incorrect projected token")
		}
		require.Equal(t, "/internal/v1/tasks/default/task/gateway-messages/budget", r.URL.Path)
		require.Equal(t, "logical-id", r.URL.Query().Get("requestID"))
		_, _ = fmt.Fprint(w, `{"accepted":3,"limit":12,"requestExists":true}`)
	}))
	defer server.Close()
	cfg := clientConfig(t, server.URL)
	c, err := New(cfg)
	require.NoError(t, err)
	b, err := c.Budget(t.Context(), "logical-id")
	require.NoError(t, err)
	require.Equal(t, 3, b.Accepted)
	require.Equal(t, 12, b.Limit)
	require.True(t, b.RequestExists)
	require.NoError(t, os.WriteFile(cfg.TokenFile, []byte("rotated-test-token"), 0600))
	_, err = c.Budget(t.Context(), "logical-id")
	require.NoError(t, err)
	require.NoError(t, os.Remove(cfg.TokenFile))
	_, err = c.Budget(t.Context(), "logical-id")
	require.Error(t, err)
	require.NotContains(t, err.Error(), cfg.TokenFile)
	require.Equal(t, int32(2), requests.Load())
}

func TestClientNoRedirectProxyOrUnsafeResponses(t *testing.T) {
	var leaked atomic.Bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { leaked.Store(true) }))
	defer target.Close()
	t.Setenv("HTTP_PROXY", target.URL)
	t.Setenv("HTTPS_PROXY", target.URL)
	for _, tc := range []struct {
		name   string
		status int
		body   string
	}{
		{"redirect", 302, ""}, {"private error", 500, `private content token route`}, {"oversize", 200, strings.Repeat("x", 5000)}, {"invalid unicode", 200, "{\"accepted\":1,\"limit\":2,\"private\":\"\xff\"}"}, {"trailing JSON", 200, `{"accepted":0,"limit":2,"requestExists":false}{}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Location", target.URL)
				w.WriteHeader(tc.status)
				_, _ = fmt.Fprint(w, tc.body)
			}))
			defer server.Close()
			c, err := New(clientConfig(t, server.URL))
			require.NoError(t, err)
			require.Nil(t, c.http.Transport.(*http.Transport).Proxy)
			require.LessOrEqual(t, c.http.Timeout, 15*time.Second)
			_, err = c.Budget(t.Context(), "id")
			require.Error(t, err)
			require.NotContains(t, err.Error(), "private")
			require.NotContains(t, err.Error(), "projected-test-token")
			require.False(t, leaked.Load())
		})
	}
}

func TestClientOriginRequiresExactIdentityAndSuccessfulRead(t *testing.T) {
	for _, test := range []struct {
		name      string
		status    int
		body      string
		wantError bool
	}{
		{"exact UID", 200, `{"taskUID":"task-uid"}`, false},
		{"other UID", 200, `{"taskUID":"other"}`, true},
		{"missing UID", 200, `{}`, true},
		{"budget not identity", 200, `{"accepted":0,"limit":10,"requestExists":false}`, true},
		{"202 not authenticated read", 202, `{"taskUID":"task-uid"}`, true},
		{"403 with forged successful body", 403, `{"taskUID":"task-uid"}`, true},
		{"409 with forged successful body", 409, `{"taskUID":"task-uid"}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Equal(t, http.MethodGet, r.Method)
				require.Empty(t, r.URL.RawQuery)
				w.WriteHeader(test.status)
				_, _ = fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			c, err := New(clientConfig(t, server.URL))
			require.NoError(t, err)
			err = c.AuthenticateOrigin(t.Context())
			if test.wantError {
				require.Error(t, err)
				require.NotErrorIs(t, err, ErrUnavailable)
			} else {
				require.NoError(t, err)
			}
			require.Equal(t, int32(1), calls.Load(), "explicit denial and invalid identity must not retry")
		})
	}
}

func TestClientFailsClosedConfigAndCancellation(t *testing.T) {
	for _, url := range []string{"", "ftp://controller", "http://fixture-user@controller", "http://controller?token=x", "http://controller#frag"} {
		_, err := New(clientConfig(t, url))
		require.Error(t, err)
	}
	cfg := clientConfig(t, "http://127.0.0.1:1")
	cfg.TaskUID = ""
	_, err := New(cfg)
	require.Error(t, err)
	cfg.TaskUID = "uid"
	c, err := New(cfg)
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.Enqueue(ctx, "logical-id", "content")
	require.Error(t, err)
	require.NotContains(t, err.Error(), "127.0.0.1")
}
