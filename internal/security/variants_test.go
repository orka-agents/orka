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
}

func TestDistillFindingRule(t *testing.T) {
	finding := &store.Finding{
		ID:        "fnd_xyz789",
		Title:     "Archive path traversal",
		Category:  "path-traversal",
		Severity:  "high",
		RootCause: "Untrusted tar entry path joined without containment check",
		FilePath:  "internal/archive/extract.go",
		Evidence: []store.FindingEvidenceRef{
			{Kind: "file", Path: "internal/archive/extract.go", StartLine: 42, EndLine: 50, Role: "sink", Symbol: "filepath.Join"},
		},
	}

	rule, err := DistillFindingRule(finding)
	if err != nil {
		t.Fatalf("DistillFindingRule() error = %v", err)
	}
	if rule.SinkSymbol != "filepath.Join" {
		t.Fatalf("SinkSymbol = %q, want filepath.Join", rule.SinkSymbol)
	}
	if !strings.Contains(rule.SemgrepYAML, "languages: [go]") || !strings.Contains(rule.SemgrepYAML, "severity: ERROR") || !strings.Contains(rule.SemgrepYAML, "filepath.Join(...)") {
		t.Fatalf("unexpected Semgrep YAML:\n%s", rule.SemgrepYAML)
	}
	if !strings.Contains(rule.ScanPolicyHint, "Archive path traversal") {
		t.Fatalf("unexpected ScanPolicyHint: %q", rule.ScanPolicyHint)
	}

	noSymbolFinding := &store.Finding{
		ID:        "fnd_nosym",
		Title:     "Missing authz check",
		Category:  "authz",
		Severity:  "medium",
		RootCause: "Handler does not verify tenant ownership",
		FilePath:  "internal/api/handler.go",
	}
	noSymRule, err := DistillFindingRule(noSymbolFinding)
	if err != nil {
		t.Fatalf("DistillFindingRule(noSymbol) error = %v", err)
	}
	if noSymRule.SemgrepYAML != "" {
		t.Fatalf("expected empty SemgrepYAML when no concrete sink symbol exists, got:\n%s", noSymRule.SemgrepYAML)
	}
	if !strings.Contains(noSymRule.ScanPolicyHint, "Missing authz check") {
		t.Fatalf("unexpected ScanPolicyHint for symbol-less finding: %q", noSymRule.ScanPolicyHint)
	}
}
