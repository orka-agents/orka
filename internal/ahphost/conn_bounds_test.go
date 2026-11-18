/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package ahphost

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
)

// A request id is echoed in its response, so an oversized one is refused
// rather than queued back to a client that may not read.
func TestARequestWithAnOversizedIDIsRefused(t *testing.T) {
	env := newTestEnv(t, store.AHPJournalLimits{})
	ctx := ctx5(t)
	ws, _, err := websocket.Dial(ctx, env.wsURL(env.mint("alice").Token), nil)
	require.NoError(t, err)
	defer func() { _ = ws.CloseNow() }()

	id, err := json.Marshal(strings.Repeat("x", maxRequestIDBytes))
	require.NoError(t, err)
	require.NoError(t, ws.Write(ctx, websocket.MessageText,
		[]byte(`{"jsonrpc":"2.0","id":`+string(id)+`,"method":"ping"}`)))
	_, data, err := ws.Read(ctx)
	require.NoError(t, err)
	var reply struct {
		ID    json.RawMessage `json:"id"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	require.NoError(t, json.Unmarshal(data, &reply))
	require.Equal(t, "null", string(reply.ID))
	require.NotNil(t, reply.Error)
	require.Contains(t, reply.Error.Message, "request id is too long")

	require.NoError(t, ws.Write(ctx, websocket.MessageText, []byte(`{"jsonrpc":"2.0","id":"short","method":"ping"}`)))
	_, data, err = ws.Read(ctx)
	require.NoError(t, err)
	require.Contains(t, string(data), `"id":"short"`, "a short id is answered")
}

// A client that does not read is closed once its unsent messages pass
// maxQueuedBytes, however few messages that takes.
func TestUnsentMessagesAreBoundedByBytes(t *testing.T) {
	accepted := make(chan *websocket.Conn, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ws, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		accepted <- ws
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
	require.NoError(t, err)
	defer func() { _ = client.CloseNow() }()
	c := &conn{ws: <-accepted, send: make(chan frame, 1024), done: make(chan struct{})}

	message := make([]byte, 4<<20)
	queued := 0
	for queued+len(message) <= maxQueuedBytes {
		c.enqueue(message)
		queued += len(message)
	}
	select {
	case <-c.done:
		t.Fatal("the connection closed within its byte bound")
	default:
	}
	require.EqualValues(t, queued, c.queued.Load())

	c.enqueue(message)
	select {
	case <-c.done:
	default:
		t.Fatal("the connection stayed open past its byte bound")
	}
	require.EqualValues(t, queued, c.queued.Load(), "a refused message reserves nothing")
	require.Len(t, c.send, queued/len(message))
}
