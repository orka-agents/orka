package workspacedelta

import "testing"

func TestPathDeniedMatchesPatternsIgnoringCase(t *testing.T) {
	patterns := []string{".github/**", "internal/controller/acp_mcp_broker*.go", "AGENTS.md", "internal/api/*authoriz*"}
	for _, denied := range []string{
		".github/workflows/ci.yml", ".GitHub/Workflows/ci.yml", "internal/controller/acp_mcp_broker.go",
		"internal/controller/acp_mcp_broker_proposer.go", "agents.md", "internal/api/external_authorization.go",
	} {
		if !PathDenied(patterns, denied) {
			t.Errorf("PathDenied(%q) = false, want true", denied)
		}
	}
	for _, allowed := range []string{"github/ci.yml", "internal/controller/acp_mcp_policy.go", "docs/AGENTS.md", "internal/api/sub/authorization.go"} {
		if PathDenied(patterns, allowed) {
			t.Errorf("PathDenied(%q) = true, want false", allowed)
		}
	}
	if PathDenied(nil, "anything") {
		t.Fatal("an empty denied list denied a path")
	}
}

func TestValidDeniedPathPatternRequiresCanonicalSegments(t *testing.T) {
	for _, valid := range []string{"internal/**", "./AGENTS.md", "internal/api/*authoriz*"} {
		if !ValidDeniedPathPattern(valid) {
			t.Errorf("ValidDeniedPathPattern(%q) = false, want true", valid)
		}
	}
	// Each of these is a valid pattern, but the matcher would never match
	// the canonical path it appears to protect.
	for _, invalid := range []string{"internal/./evals/**", "internal//evals/**", "docs/", "a/.", "internal/evals/..", "internal/**/.."} {
		if !ValidPathPattern(invalid) {
			t.Fatalf("ValidPathPattern(%q) = false; the test needs a pattern only the denied check rejects", invalid)
		}
		if ValidDeniedPathPattern(invalid) {
			t.Errorf("ValidDeniedPathPattern(%q) = true, want false", invalid)
		}
	}
	if PathDenied([]string{"internal/./evals/**"}, "internal/evals/gate.go") {
		t.Fatal("matcher now normalizes segments; revisit ValidDeniedPathPattern")
	}
}

func TestValidPathPattern(t *testing.T) {
	for _, valid := range []string{"internal/**", "./AGENTS.md", "internal/api/*authoriz*", "a/[bc]/d"} {
		if !ValidPathPattern(valid) {
			t.Errorf("ValidPathPattern(%q) = false, want true", valid)
		}
	}
	for _, invalid := range []string{"", " ", "/etc/passwd", "../x", "a/../b", "..", "a\\b", "a[", "x\x00"} {
		if ValidPathPattern(invalid) {
			t.Errorf("ValidPathPattern(%q) = true, want false", invalid)
		}
	}
}
