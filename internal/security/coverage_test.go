package security

import (
	"slices"
	"testing"
)

func coverageTestManifest() ReviewContextManifest {
	return ReviewContextManifest{IncludedFiles: []ReviewContextIncludedFile{
		{Path: "api/handler.go", Role: "owned", Readable: true},
		{Path: "api/auth.go", Role: "owned", Readable: true},
		{Path: "api/types.go", Role: "context", Readable: true},
		{Path: "api/handler_test.go", Role: "test", Readable: true},
		{Path: "api/generated.go", Role: "owned", Readable: false},
	}}
}

// coverageTestQuestions returns settled questions with a resolution followed
// by unanswered ones without, as the lenient result decoder can produce.
func coverageTestQuestions(settled, unanswered int) []FindingsV2ResolvedQuestion {
	var questions []FindingsV2ResolvedQuestion
	for range settled {
		questions = append(questions, FindingsV2ResolvedQuestion{Question: "Is this operation authorized?", Resolution: "Yes: the handler calls requireOwner first."})
	}
	for range unanswered {
		questions = append(questions, FindingsV2ResolvedQuestion{Question: "Is this operation authorized?", Resolution: " "})
	}
	return questions
}

func TestAssessReviewCoverage(t *testing.T) {
	tests := []struct {
		name         string
		kind         string
		claimed      []string
		evidence     []string
		questions    int
		unanswered   int
		wantCoverage string
		wantFiles    []string
	}{
		{
			name: "an echoed empty template corroborates nothing", kind: "package",
			wantCoverage: ReviewCoverageNone,
		},
		{
			name: "files outside the trusted context corroborate nothing", kind: "package",
			claimed: []string{"README.md", "../api/handler.go", "/api/handler.go"}, questions: 2,
			wantCoverage: ReviewCoverageNone,
		},
		{
			name: "an unreadable file is not corroborated", kind: "package",
			claimed: []string{"api/generated.go"}, wantCoverage: ReviewCoverageNone,
		},
		{
			name: "every readable owned file is a legitimate zero", kind: "package",
			claimed:      []string{"api/auth.go", " api/handler.go", "api/handler.go", "README.md"},
			wantCoverage: ReviewCoverageComplete, wantFiles: []string{"api/auth.go", "api/handler.go"},
		},
		{
			name: "an omitted owned file is partial", kind: "package",
			claimed:      []string{"api/handler.go", "api/types.go"},
			wantCoverage: ReviewCoveragePartial, wantFiles: []string{"api/handler.go", "api/types.go"},
		},
		{
			name: "cited evidence corroborates the files it cites", kind: "package",
			claimed: []string{"api/auth.go"}, evidence: []string{"api/handler.go"},
			wantCoverage: ReviewCoverageComplete, wantFiles: []string{"api/auth.go", "api/handler.go"},
		},
		{
			name: "context files alone do not cover what a component slice owns", kind: "package",
			claimed: []string{"api/types.go", "api/handler_test.go"}, questions: 3,
			wantCoverage: ReviewCoveragePartial, wantFiles: []string{"api/handler_test.go", "api/types.go"},
		},
		{
			name: "an investigation that settles a question is complete", kind: SliceKindInvestigation,
			claimed: []string{"api/types.go"}, questions: 1,
			wantCoverage: ReviewCoverageComplete, wantFiles: []string{"api/types.go"},
		},
		{
			name: "an investigation that reports a finding is complete", kind: SliceKindInvestigation,
			evidence:     []string{"api/auth.go"},
			wantCoverage: ReviewCoverageComplete, wantFiles: []string{"api/auth.go"},
		},
		{
			name: "an investigation without a settled question is partial", kind: SliceKindInvestigation,
			claimed:      []string{"api/handler.go", "api/auth.go"},
			wantCoverage: ReviewCoveragePartial, wantFiles: []string{"api/auth.go", "api/handler.go"},
		},
		{
			name: "an investigation that only lists questions is partial", kind: SliceKindInvestigation,
			claimed: []string{"api/types.go"}, unanswered: 2,
			wantCoverage: ReviewCoveragePartial, wantFiles: []string{"api/types.go"},
		},
		{
			name: "a baseline that settles a question is complete", kind: SliceKindBaseline,
			claimed: []string{"api/handler.go"}, questions: 1,
			wantCoverage: ReviewCoverageComplete, wantFiles: []string{"api/handler.go"},
		},
		{
			name: "settled questions cannot stand in for any trusted file", kind: SliceKindInvestigation,
			questions: 4, wantCoverage: ReviewCoverageNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := AssessReviewCoverage(tt.kind, coverageTestManifest(), tt.claimed, tt.evidence, coverageTestQuestions(tt.questions, tt.unanswered))
			if got.Disposition != tt.wantCoverage {
				t.Fatalf("Disposition = %q, want %q", got.Disposition, tt.wantCoverage)
			}
			if !slices.Equal(got.ReviewedFiles, tt.wantFiles) {
				t.Fatalf("ReviewedFiles = %#v, want %#v", got.ReviewedFiles, tt.wantFiles)
			}
		})
	}
}

// A component slice that owns no readable file falls back to the
// cross-component rule rather than calling any corroborated read complete.
func TestAssessReviewCoverageWithoutOwnedFiles(t *testing.T) {
	manifest := ReviewContextManifest{IncludedFiles: []ReviewContextIncludedFile{
		{Path: "deploy/values.yaml", Role: "context", Readable: true},
	}}
	if got := AssessReviewCoverage("config", manifest, []string{"deploy/values.yaml"}, nil, nil); got.Disposition != ReviewCoveragePartial {
		t.Fatalf("Disposition without a settled question = %q, want partial", got.Disposition)
	}
	if got := AssessReviewCoverage("config", manifest, []string{"deploy/values.yaml"}, nil, coverageTestQuestions(1, 0)); got.Disposition != ReviewCoverageComplete {
		t.Fatalf("Disposition with a settled question = %q, want complete", got.Disposition)
	}
}
