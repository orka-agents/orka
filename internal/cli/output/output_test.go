/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package output

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestTable_Render(t *testing.T) {
	tbl := NewTable("Name", "Status", "Age")
	tbl.AddRow("task-1", "Running", "5m")
	tbl.AddRow("task-2", "Succeeded", "1h")

	var buf bytes.Buffer
	if err := tbl.Render(&buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines (header + 2 rows), got %d: %q", len(lines), output)
	}

	// Headers should be uppercase
	if !strings.Contains(lines[0], "NAME") {
		t.Fatalf("expected header NAME, got %s", lines[0])
	}
	if !strings.Contains(lines[0], "STATUS") {
		t.Fatalf("expected header STATUS, got %s", lines[0])
	}
	if !strings.Contains(lines[0], "AGE") {
		t.Fatalf("expected header AGE, got %s", lines[0])
	}

	// Rows should contain data
	if !strings.Contains(lines[1], "task-1") {
		t.Fatalf("expected row to contain task-1, got %s", lines[1])
	}
	if !strings.Contains(lines[2], "task-2") {
		t.Fatalf("expected row to contain task-2, got %s", lines[2])
	}
}

func TestTable_Empty(t *testing.T) {
	tbl := NewTable("Name", "Status")

	var buf bytes.Buffer
	if err := tbl.Render(&buf); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	lines := strings.Split(strings.TrimRight(output, "\n"), "\n")
	if len(lines) != 1 {
		t.Fatalf("expected 1 line (header only), got %d: %q", len(lines), output)
	}
	if !strings.Contains(lines[0], "NAME") {
		t.Fatalf("expected header NAME, got %s", lines[0])
	}
}

func TestJSON(t *testing.T) {
	data := map[string]string{"name": "task-1", "status": "Running"}
	var buf bytes.Buffer
	if err := JSON(&buf, data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := buf.String()
	// Should be pretty-printed (indented)
	if !strings.Contains(output, "  ") {
		t.Fatal("expected indented JSON output")
	}
	if !strings.Contains(output, `"name": "task-1"`) {
		t.Fatalf("expected name field in output, got %s", output)
	}
	if !strings.Contains(output, `"status": "Running"`) {
		t.Fatalf("expected status field in output, got %s", output)
	}
}

func TestJSONCompact(t *testing.T) {
	data := map[string]string{"name": "task-1"}
	var buf bytes.Buffer
	if err := JSONCompact(&buf, data); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := strings.TrimSpace(buf.String())
	if output != `{"name":"task-1"}` {
		t.Fatalf("expected compact JSON, got %s", output)
	}
}

func TestParseFormat(t *testing.T) {
	tests := []struct {
		input   string
		want    Format
		wantErr bool
	}{
		{"table", FormatTable, false},
		{"json", FormatJSON, false},
		{"yaml", FormatYAML, false},
		{"xml", "", true},
		{"", "", true},
		{"TABLE", "", true}, // case-sensitive
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseFormat(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Fatalf("expected %s, got %s", tt.want, got)
			}
		})
	}
}

func TestFormatDuration(t *testing.T) {
	tests := []struct {
		name     string
		duration time.Duration
		want     string
	}{
		{"zero", 0, "0s"},
		{"seconds", 5 * time.Second, "5s"},
		{"minutes", 2*time.Minute + 30*time.Second, "2m30s"},
		{"minutes_even", 3 * time.Minute, "3m"},
		{"hours", 1*time.Hour + 5*time.Minute, "1h5m"},
		{"hours_even", 2 * time.Hour, "2h"},
		{"days", 72 * time.Hour, "3d"},
		{"negative", -5 * time.Second, "5s"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := FormatDuration(tt.duration)
			if got != tt.want {
				t.Fatalf("FormatDuration(%v) = %s, want %s", tt.duration, got, tt.want)
			}
		})
	}
}

func TestFormatAge(t *testing.T) {
	// FormatAge uses time.Since, so just verify it returns a non-empty string
	result := FormatAge(time.Now().Add(-5 * time.Minute))
	if result == "" {
		t.Fatal("expected non-empty age string")
	}
	// Should contain "m" for minutes
	if !strings.Contains(result, "m") {
		t.Fatalf("expected result to contain 'm' for minutes, got %s", result)
	}
}

func TestPrintResult_JSON(t *testing.T) {
	var buf bytes.Buffer
	data := map[string]string{"key": "value"}
	err := PrintResult(&buf, FormatJSON, data)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(buf.String(), `"key"`) {
		t.Fatalf("expected JSON output, got %s", buf.String())
	}
}

func TestPrintResult_TableError(t *testing.T) {
	var buf bytes.Buffer
	err := PrintResult(&buf, FormatTable, nil)
	if err == nil {
		t.Fatal("expected error for table format via PrintResult")
	}
}

func TestPrintResult_YAMLNotSupported(t *testing.T) {
	var buf bytes.Buffer
	err := PrintResult(&buf, FormatYAML, nil)
	if err == nil {
		t.Fatal("expected error for unsupported yaml format")
	}
}
