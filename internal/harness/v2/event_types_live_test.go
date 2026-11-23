package v2

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestValidateWorkspaceRelativePath(t *testing.T) {
	for _, valid := range []string{"a.go", "pkg/util/a.go", ".github/workflows/ci.yml", "a..b"} {
		if err := ValidateWorkspaceRelativePath(valid); err != nil {
			t.Errorf("%q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "/abs/a.go", "../a.go", "..", ".", "a/../b", "a//b", "a/", "./a", "a\x00b"} {
		if err := ValidateWorkspaceRelativePath(invalid); err == nil {
			t.Errorf("%q was accepted", invalid)
		}
	}
}

func TestPermissionRawInputMustBeBoundedJSON(t *testing.T) {
	at := time.Date(2026, 10, 27, 0, 0, 0, 0, time.UTC)
	event := PermissionRequestedEvent{
		RequestID: "permission-1", Title: "Write", ExpiresAt: at.Add(time.Minute),
		Options: []PermissionOption{{OptionID: "allow", Name: "Allow", Kind: PermissionOptionAllowOnce}},
	}
	for _, valid := range []json.RawMessage{nil, json.RawMessage(`{"file_path":"README.md"}`)} {
		event.RawInput = valid
		if err := event.Validate(at); err != nil {
			t.Errorf("%s: %v", valid, err)
		}
	}
	oversized := json.RawMessage(`"` + strings.Repeat("a", MaxToolCallRawInputBytes) + `"`)
	for _, invalid := range []json.RawMessage{json.RawMessage(`{"file_path":`), oversized} {
		event.RawInput = invalid
		if err := event.Validate(at); err == nil {
			t.Errorf("raw input of %d bytes was accepted", len(invalid))
		}
	}
}
