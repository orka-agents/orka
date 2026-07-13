package harness

import (
	"errors"
	"strings"
	"testing"
)

func TestTurnResourcePathBuildsEscapedSafePaths(t *testing.T) {
	got, err := EventStreamPath("turn 1")
	if err != nil {
		t.Fatalf("EventStreamPath() error = %v", err)
	}
	if got != "/v1/turns/turn%201/events" {
		t.Fatalf("EventStreamPath() = %q, want escaped path", got)
	}

	cancelPath, err := CancelTurnPath("turn-1")
	if err != nil {
		t.Fatalf("CancelTurnPath() error = %v", err)
	}
	if cancelPath != "/v1/turns/turn-1/cancel" {
		t.Fatalf("CancelTurnPath() = %q", cancelPath)
	}

	continuePath, err := ContinueTurnPath("turn-1")
	if err != nil {
		t.Fatalf("ContinueTurnPath() error = %v", err)
	}
	if continuePath != "/v1/turns/turn-1/continue" {
		t.Fatalf("ContinueTurnPath() = %q", continuePath)
	}
}

func TestTurnResourcePathRejectsUnsafeTurnIDs(t *testing.T) {
	for _, turnID := range []HarnessTurnID{"", ".", "..", " turn", "turn ", "turn/one", `turn\one`} {
		t.Run(string(turnID), func(t *testing.T) {
			_, err := EventStreamPath(turnID)
			if err == nil || !strings.Contains(err.Error(), "single safe path segment") && !strings.Contains(err.Error(), "required") {
				t.Fatalf("EventStreamPath(%q) error = %v, want unsafe segment rejection", turnID, err)
			}
		})
	}
}

func TestParseTurnResourcePath(t *testing.T) {
	turnID, resource, err := ParseTurnResourcePath("/v1/turns/turn%201/events")
	if err != nil {
		t.Fatalf("ParseTurnResourcePath() error = %v", err)
	}
	if turnID != "turn 1" || resource != TurnResourceEvents {
		t.Fatalf("ParseTurnResourcePath() = (%q, %q), want turn/resource", turnID, resource)
	}
}

func TestParseTurnResourcePathRejectsMissingAndUnsafePaths(t *testing.T) {
	for _, rawPath := range []string{"/v1/turns/turn-only", "/not-turns/turn/events", "/v1/turns/turn/events/extra"} {
		t.Run(rawPath, func(t *testing.T) {
			_, _, err := ParseTurnResourcePath(rawPath)
			if !errors.Is(err, ErrTurnPathNotFound) {
				t.Fatalf("ParseTurnResourcePath(%q) error = %v, want ErrTurnPathNotFound", rawPath, err)
			}
		})
	}

	_, _, err := ParseTurnResourcePath("/v1/turns/turn%2Fone/events")
	if err == nil || errors.Is(err, ErrTurnPathNotFound) || !strings.Contains(err.Error(), "single safe path segment") {
		t.Fatalf("ParseTurnResourcePath(encoded slash) error = %v, want unsafe segment rejection", err)
	}
}
