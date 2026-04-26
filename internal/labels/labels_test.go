/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package labels

import (
	"strings"
	"testing"
)

func TestSelectorValue_PreservesShortValidValues(t *testing.T) {
	value := "short-valid-task-name"

	if got := SelectorValue(value); got != value {
		t.Fatalf("SelectorValue(%q) = %q, want %q", value, got, value)
	}
}

func TestSelectorValue_ShortenLongValuesDeterministically(t *testing.T) {
	value := "demo-security-repository-20260424001139-initial-discovery-auth-secrets-privilege-1777014920-1"

	got := SelectorValue(value)
	if got == value {
		t.Fatalf("SelectorValue(%q) returned the original value, expected a shortened label-safe form", value)
	}
	if len(got) > maxLabelValueLength {
		t.Fatalf("len(SelectorValue(%q)) = %d, want <= %d", value, len(got), maxLabelValueLength)
	}
	if !isValidLabelValue(got) {
		t.Fatalf("SelectorValue(%q) = %q, want valid Kubernetes label value", value, got)
	}
	if !strings.HasSuffix(got, "-"+shortHash(value)) {
		t.Fatalf("SelectorValue(%q) = %q, want hash suffix %q", value, got, "-"+shortHash(value))
	}
	if got != SelectorValue(value) {
		t.Fatalf("SelectorValue(%q) is not deterministic", value)
	}
}

func TestParentTaskName_PrefersAnnotationAndFallsBackToLabel(t *testing.T) {
	labelOnly := ParentTaskName(
		map[string]string{LabelParentTask: "short-parent"},
		nil,
	)
	if labelOnly != "short-parent" {
		t.Fatalf("ParentTaskName(label only) = %q, want %q", labelOnly, "short-parent")
	}

	parentName := "very-long-parent-task-name-that-needs-more-than-a-label-can-hold-1234567890"
	got := ParentTaskName(
		map[string]string{LabelParentTask: SelectorValue(parentName)},
		map[string]string{AnnotationParentTaskName: parentName},
	)
	if got != parentName {
		t.Fatalf("ParentTaskName(annotation) = %q, want %q", got, parentName)
	}
}
