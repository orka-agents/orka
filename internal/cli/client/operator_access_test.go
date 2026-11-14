package client

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// failingReader yields data once, then err on every later read.
type failingReader struct {
	data string
	err  error
}

func (r *failingReader) Read(p []byte) (int, error) {
	if r.data == "" {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	return n, nil
}

// serveOperatorExecReadingStdin behaves like "cat > file": it consumes stdin
// and exits 0 as soon as it is told the input ended. It reports whether that
// end-of-input message arrived and closes streamEnded when the client lets go.
func serveOperatorExecReadingStdin(t *testing.T) (*httptest.Server, *atomic.Bool, chan struct{}) {
	t.Helper()
	sawEOF := &atomic.Bool{}
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
		for {
			_, payload, err := conn.Read(ctx)
			if err != nil {
				return
			}
			var control operatoraccess.Control
			if len(payload) > 1 && payload[0] == operatoraccess.ChannelControl &&
				json.Unmarshal(payload[1:], &control) == nil && control.Type == operatoraccess.ControlEOF {
				sawEOF.Store(true)
				_ = conn.Write(ctx, websocket.MessageBinary, operatoraccess.StatusFrame(operatoraccess.Status{Reason: operatoraccess.ExitReasonExited}))
			}
		}
	}))
	t.Cleanup(api.Close)
	return api, sawEOF, streamEnded
}

// A read error on local stdin is not the end of the input. Announcing a clean
// end would let the remote command finish on truncated input and exit 0.
func TestOperatorExecFailsWhenItsStdinCannotBeRead(t *testing.T) {
	api, sawEOF, streamEnded := serveOperatorExecReadingStdin(t)
	broken := errors.New("input/output error")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, err := New(api.URL, "").OperatorExec(ctx, "my-task",
		operatoraccess.ExecOptions{Argv: []string{"tee", "out"}, Stdin: true},
		OperatorExecStreams{Stdin: &failingReader{data: "first half", err: broken}, Stdout: io.Discard, Stderr: io.Discard})
	if !errors.Is(err, broken) || !strings.Contains(err.Error(), "read command stdin") {
		t.Fatalf("exec with unreadable stdin = %+v, %v; want the read error", status, err)
	}
	select {
	case <-streamEnded:
	case <-time.After(10 * time.Second):
		t.Fatal("the stream stayed open after stdin failed, so the remote command keeps running")
	}
	if sawEOF.Load() {
		t.Fatal("the remote command was told its input ended cleanly")
	}
}

func TestOperatorExecAnnouncesARealEndOfStdin(t *testing.T) {
	api, sawEOF, _ := serveOperatorExecReadingStdin(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	status, err := New(api.URL, "").OperatorExec(ctx, "my-task",
		operatoraccess.ExecOptions{Argv: []string{"tee", "out"}, Stdin: true},
		OperatorExecStreams{Stdin: strings.NewReader("all of it"), Stdout: io.Discard, Stderr: io.Discard})
	if err != nil || status.Reason != operatoraccess.ExitReasonExited || status.ExitCode != 0 || !sawEOF.Load() {
		t.Fatalf("exec with complete stdin = %+v, %v (end of input seen: %v)", status, err, sawEOF.Load())
	}
}
