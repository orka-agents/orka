package security

import (
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

func TestBuildVariantInvestigation(t *testing.T) {
	finding := &store.Finding{
		ID:        "fnd_abc123",
		Title:     "Missing namespace authorization check",
		Category:  "authz-bypass",
		RootCause: "Handler reads namespace from query without RBAC verification",
		FilePath:  "internal/api/security_handlers.go",
		Line:      120,
		Evidence: []store.FindingEvidenceRef{
			{Kind: "file", Path: "internal/api/security_handlers.go", StartLine: 118, EndLine: 130},
			{Kind: "file", Path: "internal/api/external_authorization.go", StartLine: 180, EndLine: 200},
		},
	}

	inv, ok := BuildVariantInvestigation(finding)
	if !ok {
		t.Fatal("expected BuildVariantInvestigation to succeed")
	}
	if !strings.HasPrefix(inv.Title, "Variant hunt:") {
		t.Fatalf("unexpected variant title: %q", inv.Title)
	}
	if !strings.Contains(inv.Question, "authz-bypass") || !strings.Contains(inv.Question, "internal/api/security_handlers.go:120") {
		t.Fatalf("variant question missing key context: %q", inv.Question)
	}
	if len(inv.Paths) != 2 {
		t.Fatalf("variant paths = %#v, want 2 deduplicated repo-relative paths", inv.Paths)
	}

	secretFinding := &store.Finding{
		ID:        "fnd_secret",
		Title:     "Leaked token",
		RootCause: "Authorization: Bearer eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.abc123def456",
		FilePath:  "internal/api/auth.go",
	}
	if _, ok := BuildVariantInvestigation(secretFinding); ok {
		t.Fatal("expected BuildVariantInvestigation to reject credential-shaped text")
	}
}

func TestDistillFindingRule(t *testing.T) {
	finding := &store.Finding{
		ID:        "fnd_xyz789",
		Title:     "Archive path traversal",
		Category:  "path-traversal",
		Severity:  "high",
		RootCause: "Untrusted tar entry path joined without containment check",
		FilePath:  "scripts/entry.py",
		Evidence: []store.FindingEvidenceRef{
			{Kind: "file", Path: "scripts/entry.py", StartLine: 10, EndLine: 20, Role: "source", Symbol: "parse_input"},
			{Kind: "file", Path: "internal/archive/extract.go", StartLine: 42, EndLine: 50, Role: "sink", Symbol: "filepath.Join"},
		},
	}

	rule, err := DistillFindingRule(finding)
	if err != nil {
		t.Fatalf("DistillFindingRule() error = %v", err)
	}
	if rule.SinkSymbol != "filepath.Join" || !rule.CandidateOnly {
		t.Fatalf("SinkSymbol=%q CandidateOnly=%v, want filepath.Join + candidateOnly=true", rule.SinkSymbol, rule.CandidateOnly)
	}
	if !strings.Contains(rule.SemgrepYAML, "languages: [go]") || !strings.Contains(rule.SemgrepYAML, "candidatePattern: true") || !strings.Contains(rule.SemgrepYAML, "filepath.Join(...)") {
		t.Fatalf("unexpected Semgrep YAML:\n%s", rule.SemgrepYAML)
	}
	if !strings.Contains(rule.ScanPolicyHint, "Archive path traversal") {
		t.Fatalf("unexpected ScanPolicyHint: %q", rule.ScanPolicyHint)
	}

	sourceOnlyFinding := &store.Finding{
		ID:        "fnd_sourceonly",
		Title:     "Missing authz check",
		Category:  "authz",
		Severity:  "medium",
		RootCause: "Handler does not verify tenant ownership",
		FilePath:  "internal/api/handler.go",
		Evidence: []store.FindingEvidenceRef{
			{Kind: "file", Path: "internal/api/handler.go", StartLine: 10, EndLine: 20, Role: "source", Symbol: "handleRequest"},
		},
	}
	sourceOnlyRule, err := DistillFindingRule(sourceOnlyFinding)
	if err != nil {
		t.Fatalf("DistillFindingRule(sourceOnly) error = %v", err)
	}
	if sourceOnlyRule.SemgrepYAML != "" || sourceOnlyRule.CandidateOnly {
		t.Fatalf("expected empty SemgrepYAML when only non-sink symbols exist, got:\n%s", sourceOnlyRule.SemgrepYAML)
	}
	if !strings.Contains(sourceOnlyRule.ScanPolicyHint, "Missing authz check") {
		t.Fatalf("unexpected ScanPolicyHint for source-only finding: %q", sourceOnlyRule.ScanPolicyHint)
	}

	secretCatFinding := &store.Finding{
		ID:       "fnd_secretcat",
		Title:    "Auth bypass",
		Category: "ghp_1234567890abcdefghijklmnopqrstuvwxyz",
		Severity: "high",
	}
	if _, err := DistillFindingRule(secretCatFinding); err == nil {
		t.Fatal("expected DistillFindingRule to reject credential-shaped category")
	}
}
