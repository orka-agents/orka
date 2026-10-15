package enforcementprovider

import (
	"bytes"
	"errors"
	"strings"
	"testing"
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
