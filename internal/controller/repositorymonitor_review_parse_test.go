package controller

import (
	"encoding/json"
	"testing"
)

func TestParseRepositoryMonitorReviewResultFromACPOutput(t *testing.T) {
	review := `{"schemaVersion":"orka.prReview.v1","repo":"owner/repo","prNumber":23,"headSHA":"exact-head","verdict":"needs_changes","confidence":"high","repairable":true,"summary":"Restore addition.","findings":[],"security":{"status":"clear"},"tests":{"status":"failed"}}`
	wrapped, err := json.Marshal(map[string]any{"version": 1, "summary": review})
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name string
		raw  string
	}{
		{name: "bare review", raw: review},
		{name: "structured worker result", raw: string(wrapped)},
		{name: "fenced review", raw: "```json\n" + review + "\n```"},
		{
			name: "code braces in earlier ACP commentary",
			raw: "The diff changed `function add(a, b) { return a + b; }` to " +
				"`function add(a, b) { return a - b; }`. Tests failed.\n```json\n" + review + "\n```",
		},
		{name: "unrelated diagnostic object", raw: "Progress: {\"phase\":\"running\"}\n" + review},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := parseRepositoryMonitorReviewResult([]byte(tt.raw))
			if err != nil {
				t.Fatalf("parse review: %v", err)
			}
			if got.SchemaVersion != repositoryMonitorReviewSchemaVersion || got.Repo != "owner/repo" ||
				got.PRNumber != 23 || got.HeadSHA != "exact-head" || got.Verdict != "needs_changes" || got.Tests.Status != "failed" {
				t.Fatalf("review identity or verdict changed: %#v", got)
			}
		})
	}
}

func TestParseRepositoryMonitorReviewResultRejectsAmbiguousOrMissingReview(t *testing.T) {
	review := `{"schemaVersion":"orka.prReview.v1","verdict":"needs_changes"}`
	for name, raw := range map[string]string{
		"no object":             "not a review result",
		"only code":             "function add(a, b) { return a + b; }",
		"only unrelated JSON":   "Progress: {\"phase\":\"running\"}",
		"truncated review":      "Review: {\"schemaVersion\":\"orka.prReview.v1\",",
		"two review objects":    review + "\n" + review,
		"nested review example": "Example: {\"example\":" + review + "}",
	} {
		t.Run(name, func(t *testing.T) {
			if got, err := parseRepositoryMonitorReviewResult([]byte(raw)); err == nil {
				t.Fatalf("accepted ambiguous or missing review: %#v", got)
			}
		})
	}
}
