package client

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

type failingWriter struct{ err error }

func (w failingWriter) Write([]byte) (int, error) { return 0, w.err }

// serveOperatorExec answers one exec with a frame of stdout and one of stderr,
// then reports that the remote command exited 0. It closes streamEnded when
// the client lets go of the stream.
func serveOperatorExec(t *testing.T) (*httptest.Server, chan struct{}) {
	t.Helper()
	streamEnded := make(chan struct{})
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{operatoraccess.WebSocketSubprotocol}})
		if err != nil {
			return
		}
		defer close(streamEnded)
		defer conn.CloseNow() //nolint:errcheck
		ctx := r.Context()
		if _, _, err := conn.Read(ctx); err != nil {
			return
		}
		_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.Frame(operatoraccess.ChannelStdout, []byte("out")))
		_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.Frame(operatoraccess.ChannelStderr, []byte("err")))
		_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.StatusFrame(operatoraccess.Status{Reason: operatoraccess.ExitReasonExited}))
		for {
			if _, _, err := conn.Read(ctx); err != nil {
				return
			}
		}
	}))
	t.Cleanup(api.Close)
	return api, streamEnded
}

// "orka task exec my-task -- cat big > /dev/full" loses its output. The remote
// command still exits 0, so reporting that status would call a failed transfer
// a success.
func TestOperatorExecFailsWhenItsOutputCannotBeWrittenLocally(t *testing.T) {
	diskFull := errors.New("no space left on device")
	for name, streams := range map[string]OperatorExecStreams{
		"stdout": {Stdout: failingWriter{diskFull}, Stderr: io.Discard},
		"stderr": {Stdout: io.Discard, Stderr: failingWriter{diskFull}},
	} {
		t.Run(name, func(t *testing.T) {
			api, streamEnded := serveOperatorExec(t)
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			status, err := New(api.URL, "").OperatorExec(ctx, "my-task", operatoraccess.ExecOptions{Argv: []string{"cat", "big"}}, streams)
			if !errors.Is(err, diskFull) || !strings.Contains(err.Error(), "write command "+name) {
				t.Fatalf("exec with an unwritable %s = %+v, %v; want the write error", name, status, err)
			}
			select {
			case <-streamEnded:
			case <-time.After(10 * time.Second):
				t.Fatal("the stream stayed open after the local write failed, so the remote command keeps running")
			}
		})
	}
}

func TestOperatorExecReturnsTheRemoteStatusWhenOutputIsWritten(t *testing.T) {
	api, _ := serveOperatorExec(t)
	var stdout, stderr strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, err := New(api.URL, "").OperatorExec(ctx, "my-task", operatoraccess.ExecOptions{Argv: []string{"cat", "big"}},
		OperatorExecStreams{Stdout: &stdout, Stderr: &stderr})
	if err != nil || status.Reason != operatoraccess.ExitReasonExited || status.ExitCode != 0 {
		t.Fatalf("exec = %+v, %v", status, err)
	}
	if stdout.String() != "out" || stderr.String() != "err" {
		t.Fatalf("stdout %q stderr %q", stdout.String(), stderr.String())
	}
}
