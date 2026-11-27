/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"strings"
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

func TestReadBoundedTailLogLines(t *testing.T) {
	got, err := readBoundedTailLogLines(strings.NewReader("a\nb\nc\nd\n"), 2, 1024)
	if err != nil || got != "c\nd\n" {
		t.Fatalf("readBoundedTailLogLines = (%q, %v), want (%q, nil)", got, err, "c\nd\n")
	}
	got, err = readBoundedTailLogLines(strings.NewReader("a\nb"), 5, 1024)
	if err != nil || got != "a\nb" {
		t.Fatalf("readBoundedTailLogLines = (%q, %v), want (%q, nil)", got, err, "a\nb")
	}
	got, err = readBoundedTailLogLines(strings.NewReader("a\nb\nc"), 1, 1024)
	if err != nil || got != "c" {
		t.Fatalf("readBoundedTailLogLines = (%q, %v), want (%q, nil)", got, err, "c")
	}
	got, err = readBoundedTailLogLines(strings.NewReader("12345\n67890\nabcde\n"), 2, 12)
	if err != nil || got != "67890\nabcde\n" {
		t.Fatalf("readBoundedTailLogLines past byte cap = (%q, %v), want (%q, nil)", got, err, "67890\nabcde\n")
	}
	got, err = readBoundedTailLogLines(strings.NewReader(strings.Repeat("x", 100)+"tail\n"), 2, 10)
	if err != nil || got != "xxxxxtail\n" {
		t.Fatalf("readBoundedTailLogLines long line = (%q, %v), want (%q, nil)", got, err, "xxxxxtail\n")
	}
	got, err = readBoundedTailLogLines(strings.NewReader("aaaa\nbbbb\nc\nd\ne\n"), 5, 10)
	if err != nil || got != "c\nd\ne\n" {
		t.Fatalf("readBoundedTailLogLines eviction ordering = (%q, %v), want (%q, nil)", got, err, "c\nd\ne\n")
	}
}
