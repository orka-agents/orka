/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"
)

func TestWrapTextBreaksAtSpacesAndKeepsLineBreaks(t *testing.T) {
	lines := wrapText("one two three four five\nsix", 9)
	want := []string{"one two", "three", "four five", "six"}
	if strings.Join(lines, "|") != strings.Join(want, "|") {
		t.Fatalf("wrapText = %q, want %q", lines, want)
	}
	long := wrapText("id abcdefghijkl end", 5)
	if strings.Join(long, "|") != "id|abcdefghijkl|end" {
		t.Fatalf("wrapText keeps a long token whole = %q", long)
	}
}

func TestTruncateToWidthCutsOnRuneBoundaryWithEllipsis(t *testing.T) {
	if got := truncateToWidth("héllo wörld", 6); got != "héllo…" {
		t.Fatalf("truncateToWidth = %q", got)
	}
	if got := truncateToWidth("short", 10); got != "short" {
		t.Fatalf("truncateToWidth short = %q", got)
	}
	if got := truncateToWidth("a\x1b[31mb\nc", 10); got != "a[31mb c" {
		t.Fatalf("truncateToWidth strips controls and newlines = %q", got)
	}
}

func TestPrintDescribeOmitsEmptyWrapsAndIndentsSections(t *testing.T) {
	t.Setenv("COLUMNS", "40")
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	err := printDescribe(cmd, []describeRow{
		{Label: "Name", Value: "demo"},
		{Label: "Empty", Value: ""},
		{Label: "Instructions", Value: "You are a careful reviewer who reads every line before commenting."},
		{Label: "Transaction", Children: []describeRow{{Label: "ID", Value: "txn-1"}, {Label: "None", Value: ""}}},
		{Label: "Nothing", Children: []describeRow{{Label: "None", Value: ""}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	text := out.String()
	if strings.Contains(text, "Empty") || strings.Contains(text, "Nothing") || strings.Contains(text, "None") {
		t.Fatalf("empty rows were printed:\n%s", text)
	}
	for line := range strings.SplitSeq(strings.TrimRight(text, "\n"), "\n") {
		if len([]rune(line)) > 40 {
			t.Fatalf("line longer than the terminal width: %q\n%s", line, text)
		}
	}
	if !strings.Contains(text, "Name:         demo") {
		t.Fatalf("labels are not aligned:\n%s", text)
	}
	if !strings.Contains(text, "Transaction:\n  ID: txn-1") {
		t.Fatalf("nested section not indented:\n%s", text)
	}
}

func TestPrintDescribeKeepsLineBreaksInValues(t *testing.T) {
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	if err := printDescribe(cmd, []describeRow{{Label: "Prompt", Value: "line1\nline2\x1b[31m\n\nline4"}}); err != nil {
		t.Fatal(err)
	}
	want := "Prompt: line1\n        line2[31m\n\n        line4\n"
	if out.String() != want {
		t.Fatalf("describe output = %q, want %q", out.String(), want)
	}
}

func TestPrintDescribeSanitizesLabels(t *testing.T) {
	cmd := &cobra.Command{}
	var out strings.Builder
	cmd.SetOut(&out)
	err := printDescribe(cmd, []describeRow{
		{Label: "Arguments", Children: []describeRow{{Label: "asset\x1b[2J\nname", Value: "pump-1"}}},
		{Label: "plain\x07", Value: "ok"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(out.String(), "\x1b\x07") {
		t.Fatalf("control characters reached the output: %q", out.String())
	}
	if !strings.Contains(out.String(), "asset[2J name: pump-1") {
		t.Fatalf("label not sanitized as expected:\n%s", out.String())
	}
}

func TestShortImageKeepsLastSegmentAndTag(t *testing.T) {
	for in, want := range map[string]string{
		"registry.example.com/team/golang:1.27": "golang:1.27",
		"golang:1.27":                           "golang:1.27",
		"busybox":                               "busybox",
		"ghcr.io/org/tool@sha256:abcdef":        "tool",
		"":                                      "",
	} {
		if got := shortImage(in); got != want {
			t.Errorf("shortImage(%q) = %q, want %q", in, got, want)
		}
	}
	if got := taskAgentLabel("coder", "golang:1.27"); got != "coder" {
		t.Fatalf("taskAgentLabel prefers the agent: %q", got)
	}
}

func TestKeyValuePairsSortsAndQuotes(t *testing.T) {
	got := keyValuePairs(map[string]any{
		"summary": "Inspect the pressure transmitter.",
		"asset":   "pump-1",
		"count":   float64(2),
		"nested":  map[string]any{"a": true},
	})
	want := `asset=pump-1 count=2 nested={"a":true} summary="Inspect the pressure transmitter."`
	if got != want {
		t.Fatalf("keyValuePairs = %q, want %q", got, want)
	}
}

func TestFormatUntil(t *testing.T) {
	now := time.Date(2026, 9, 23, 8, 25, 0, 0, time.UTC)
	if got := formatUntil("2026-09-23T08:34:29Z", now); got != "in 9m" {
		t.Fatalf("formatUntil = %q", got)
	}
	if got := formatUntil("2026-09-23T08:00:00Z", now); got != "expired" {
		t.Fatalf("formatUntil past = %q", got)
	}
	if got := formatUntil("", now); got != "" {
		t.Fatalf("formatUntil empty = %q", got)
	}
}

func TestParseSinceAcceptsDurationAndTimestamp(t *testing.T) {
	now := time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC)
	got, err := parseSince("10m", now)
	if err != nil || !got.Equal(now.Add(-10*time.Minute)) {
		t.Fatalf("parseSince(10m) = %v, %v", got, err)
	}
	got, err = parseSince("2026-09-23T08:00:00Z", now)
	if err != nil || !got.Equal(time.Date(2026, 9, 23, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("parseSince(timestamp) = %v, %v", got, err)
	}
	if _, err := parseSince("yesterday", now); err == nil || !strings.Contains(err.Error(), "invalid --since") {
		t.Fatalf("parseSince(yesterday) error = %v", err)
	}
	if got, err := parseSince("", now); err != nil || !got.IsZero() {
		t.Fatalf("parseSince(empty) = %v, %v", got, err)
	}
}
