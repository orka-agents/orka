/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestParseTaskLogStream(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{in: "", want: ""},
		{in: "all", want: corev1.LogStreamAll},
		{in: "STDOUT", want: corev1.LogStreamStdout},
		{in: " stderr ", want: corev1.LogStreamStderr},
		{in: "invalid", wantErr: true},
	}
	for _, tc := range tests {
		got, err := parseTaskLogStream(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Fatalf("parseTaskLogStream(%q) succeeded with %q, want error", tc.in, got)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Fatalf("parseTaskLogStream(%q) = (%q, %v), want (%q, nil)", tc.in, got, err, tc.want)
		}
	}
}

func TestTailLogLines(t *testing.T) {
	if got := tailLogLines("a\nb\nc\nd\n", 2); got != "c\nd\n" {
		t.Fatalf("tailLogLines = %q, want %q", got, "c\nd\n")
	}
	if got := tailLogLines("a\nb", 5); got != "a\nb" {
		t.Fatalf("tailLogLines = %q, want %q", got, "a\nb")
	}
	if got := tailLogLines("a\nb\nc", 1); got != "c" {
		t.Fatalf("tailLogLines = %q, want %q", got, "c")
	}
}
