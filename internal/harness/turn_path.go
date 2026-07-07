package harness

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const (
	TurnResourceEvents   = "events"
	TurnResourceContinue = "continue"
	TurnResourceCancel   = "cancel"
	TurnResourceOutput   = "output"
)

// ErrTurnPathNotFound reports that a request path is not a two-segment harness
// turn resource path under TurnsPath.
var ErrTurnPathNotFound = errors.New("harness turn path not found")

// ValidateTurnPathSegment validates the protocol invariant that turn IDs occupy
// exactly one URL path segment when used in turn resource paths.
func ValidateTurnPathSegment(turnID HarnessTurnID) error {
	raw := string(turnID)
	value := strings.TrimSpace(raw)
	if value == "" {
		return fmt.Errorf("turn id is required")
	}
	if value != raw || value == "." || value == ".." || strings.Contains(value, "/") || strings.Contains(value, "\\") {
		return fmt.Errorf("turn id must be a single safe path segment")
	}
	return nil
}

// TurnResourcePath builds the escaped path for a harness turn sub-resource.
func TurnResourcePath(turnID HarnessTurnID, resource string) (string, error) {
	if err := ValidateTurnPathSegment(turnID); err != nil {
		return "", err
	}
	resource = strings.Trim(strings.TrimSpace(resource), "/")
	if resource == "" || resource == "." || resource == ".." || strings.Contains(resource, "/") || strings.Contains(resource, "\\") {
		return "", fmt.Errorf("turn resource must be a single safe path segment")
	}
	return strings.TrimRight(TurnsPath, "/") + "/" + url.PathEscape(string(turnID)) + "/" + resource, nil
}

// EventStreamPath builds the escaped SSE event stream path for a turn.
func EventStreamPath(turnID HarnessTurnID) (string, error) {
	return TurnResourcePath(turnID, TurnResourceEvents)
}

// CancelTurnPath builds the escaped cancel path for a turn.
func CancelTurnPath(turnID HarnessTurnID) (string, error) {
	return TurnResourcePath(turnID, TurnResourceCancel)
}

// ContinueTurnPath builds the continuation path for a suspended turn.
func ContinueTurnPath(turnID HarnessTurnID) (string, error) {
	return TurnResourcePath(turnID, TurnResourceContinue)
}

// OutputTurnPath builds the escaped output fetch path for a turn.
func OutputTurnPath(turnID HarnessTurnID) (string, error) {
	return TurnResourcePath(turnID, TurnResourceOutput)
}

// ParseTurnResourcePath extracts the turn ID and resource from a request path
// previously produced by TurnResourcePath. Pass URL.EscapedPath() when parsing
// an HTTP request so encoded slashes are rejected after unescaping.
func ParseTurnResourcePath(rawPath string) (HarnessTurnID, string, error) {
	pathValue := strings.TrimSpace(rawPath)
	trimmed := strings.TrimPrefix(pathValue, strings.TrimRight(TurnsPath, "/")+"/")
	if trimmed == pathValue || trimmed == "" {
		return "", "", ErrTurnPathNotFound
	}
	parts := strings.Split(strings.Trim(trimmed, "/"), "/")
	if len(parts) != 2 {
		return "", "", ErrTurnPathNotFound
	}
	turnValue, err := url.PathUnescape(parts[0])
	if err != nil {
		return "", "", fmt.Errorf("decode turn id path segment: %w", err)
	}
	turnID := HarnessTurnID(turnValue)
	if err := ValidateTurnPathSegment(turnID); err != nil {
		return "", "", err
	}
	resource := strings.TrimSpace(parts[1])
	if resource == "" || strings.Contains(resource, "\\") {
		return "", "", ErrTurnPathNotFound
	}
	return turnID, resource, nil
}
