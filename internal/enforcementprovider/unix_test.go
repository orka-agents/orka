package enforcementprovider

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReadTransportFrameBoundsBeforeNewline(t *testing.T) {
	if _, err := readTransportFrame(strings.NewReader(strings.Repeat("x", maxTransportFrame+1))); err == nil {
		t.Fatal("accepted oversized frame without newline")
	}
	if _, err := readTransportFrame(bytes.NewReader(append(bytes.Repeat([]byte{'x'}, maxTransportFrame), '\n'))); err == nil {
		t.Fatal("accepted frame whose newline exceeds the limit")
	}
	frame, err := readTransportFrame(strings.NewReader("{}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if string(frame) != "{}\n" {
		t.Fatalf("frame = %q", frame)
	}
}

func TestEncodeTransportFrameBoundsBeforeWrite(t *testing.T) {
	frame, err := encodeTransportFrame(strings.Repeat("x", maxTransportFrame-3))
	if err != nil {
		t.Fatal(err)
	}
	if len(frame) != maxTransportFrame {
		t.Fatalf("frame length = %d, want %d", len(frame), maxTransportFrame)
	}
	if _, err = encodeTransportFrame(strings.Repeat("x", maxTransportFrame-2)); !errors.Is(err, errTransportFrameTooLarge) {
		t.Fatalf("oversized frame error = %v", err)
	}
}

func TestUnixReconcileSendsAttemptID(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "socket")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "provider.sock")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = listener.Close() }()

	serverErr := make(chan error, 1)
	go func() {
		connection, acceptErr := listener.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr
			return
		}
		defer func() { _ = connection.Close() }()

		request, readErr := readTransportFrame(connection)
		if readErr != nil {
			serverErr <- readErr
			return
		}
		var frame requestFrame
		if decodeErr := strictDecodeJSON(request, &frame); decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		if frame.Operation != "Reconcile" {
			serverErr <- fmt.Errorf("operation = %q", frame.Operation)
			return
		}
		parameters, marshalErr := json.Marshal(frame.Parameters)
		if marshalErr != nil {
			serverErr <- marshalErr
			return
		}
		var decoded struct {
			AttemptID string `json:"attemptId"`
		}
		if decodeErr := strictDecodeJSON(parameters, &decoded); decodeErr != nil {
			serverErr <- decodeErr
			return
		}
		if decoded.AttemptID != "attempt-1" {
			serverErr <- fmt.Errorf("attempt ID = %q", decoded.AttemptID)
			return
		}
		_, writeErr := connection.Write([]byte(`{"version":2,"id":"1","ok":true,"result":{"state":"active"}}` + "\n"))
		serverErr <- writeErr
	}()

	provider := NewUnixProvider(path, uint32(os.Geteuid()), time.Second)
	status, err := provider.Reconcile(ReconcileRequest{AttemptID: "attempt-1"})
	if err != nil {
		t.Fatal(err)
	}
	if status.State != "active" {
		t.Fatalf("state = %q", status.State)
	}
	if err = <-serverErr; err != nil {
		t.Fatal(err)
	}
}
