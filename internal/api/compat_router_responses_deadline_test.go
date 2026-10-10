/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestCompatRouterResponsesBackpressureClosesDownstream(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		t.Run(contentType, func(t *testing.T) {
			finished := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", contentType)
				_, _ = io.WriteString(w, strings.Repeat("x", 1<<20))
				close(finished)
			}))
			t.Cleanup(upstream.Close)
			token := t.Name()
			router, err := NewCompatRouter(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:team-a:client"}), map[string]string{"team-a": upstream.URL})
			require.NoError(t, err)
			router.responsesWriteTimeout = 150 * time.Millisecond
			t.Cleanup(router.Close)
			entry := httptest.NewUnstartedServer(router)
			tracked := &responsesObservedListener{Listener: entry.Listener, accepted: make(chan *responsesObservedConn, 1)}
			entry.Listener = tracked
			entry.Start()
			t.Cleanup(entry.Close)
			conn, err := net.DialTimeout("tcp", entry.Listener.Addr().String(), time.Second)
			require.NoError(t, err)
			t.Cleanup(func() { _ = conn.Close() })
			require.NoError(t, conn.(*net.TCPConn).SetReadBuffer(1024))
			_, err = fmt.Fprintf(conn, "POST /OPENAI/v1/responses/ HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Length: 0\r\n\r\n", token)
			require.NoError(t, err)
			accepted := <-tracked.accepted
			t.Cleanup(func() { _ = accepted.Close() })
			select {
			case <-finished:
			case <-time.After(2 * time.Second):
				t.Fatal("installation did not finish its response")
			}
			require.Eventually(t, func() bool { return accepted.closed.Load() }, 2*time.Second, 10*time.Millisecond, "finished installation left the downstream socket blocked")
		})
	}
}

func TestCompatRouterResponsesWriteDeadlineDoesNotLimitInferenceOrKeepAlive(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(200 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"output":[]}`)
	}))
	t.Cleanup(upstream.Close)
	token := t.Name()
	router, err := NewCompatRouter(compatRouterTokenClient(t, map[string]string{token: "system:serviceaccount:team-a:client"}), map[string]string{"team-a": upstream.URL})
	require.NoError(t, err)
	router.responsesWriteTimeout = 50 * time.Millisecond
	t.Cleanup(router.Close)
	entry := httptest.NewServer(router)
	t.Cleanup(entry.Close)
	conn, err := net.DialTimeout("tcp", entry.Listener.Addr().String(), time.Second)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	require.NoError(t, conn.SetDeadline(time.Now().Add(3*time.Second)))
	reader := bufio.NewReader(conn)
	for _, path := range []string{"/openai/v1/responses", "/openai/v1/chat/completions"} {
		_, err = fmt.Fprintf(conn, "POST %s HTTP/1.1\r\nHost: localhost\r\nAuthorization: Bearer %s\r\nContent-Length: 0\r\n\r\n", path, token)
		require.NoError(t, err)
		response, err := http.ReadResponse(reader, nil)
		require.NoError(t, err)
		body, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		require.Equal(t, http.StatusOK, response.StatusCode)
		require.JSONEq(t, `{"output":[]}`, string(body))
	}
}

// ReverseProxy ignores some Flush errors before its next upstream read.
// The deadline wrapper must cancel that read rather than waiting for new data.
func TestCompatRouterResponsesFlushFailureCancelsUpstream(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	want := errors.New("fixture flush deadline")
	underlying := &responsesFailingFlushWriter{ResponseRecorder: httptest.NewRecorder(), err: want}
	writer := &responsesDeadlineWriter{ResponseWriter: underlying, timeout: time.Second, cancel: cancel}
	require.ErrorIs(t, writer.FlushError(), want)
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	require.False(t, underlying.deadline.IsZero(), "failed flush must retain its write deadline")
}

type responsesFailingFlushWriter struct {
	*httptest.ResponseRecorder
	deadline time.Time
	err      error
}

func (w *responsesFailingFlushWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	return nil
}

func (w *responsesFailingFlushWriter) FlushError() error { return w.err }
