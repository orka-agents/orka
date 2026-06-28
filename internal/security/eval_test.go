package security

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/sozercan/orka/internal/store"
)

type scannerEvalCase struct {
	name    string
	finding *store.Finding
	keep    bool
}

func TestScannerEvalFilterCorpus(t *testing.T) {
	cases := []scannerEvalCase{
		{name: "path_traversal", keep: true, finding: evalFinding("Archive path traversal", "internal/archive/extract.go", "Attacker-controlled archive path crosses filesystem trust boundary to privileged write sink.")},
		{name: "auth_bypass", keep: true, finding: evalFinding("API auth bypass", "internal/api/auth.go", "Attacker-controlled request bypasses server-side authorization and exposes tenant data.")},
		{name: "secret_logging", keep: true, finding: evalFinding("TxToken secret logging", "internal/api/token.go", "Raw TxToken credential is logged and persisted.")},
		{name: "kubernetes_rbac_escape", keep: true, finding: evalFinding("Kubernetes RBAC escape", "config/rbac/role.yaml", "Tenant can cross Kubernetes RBAC privilege boundary to create pods.")},
		{name: "artifact_ingestion_trust_boundary", keep: true, finding: evalFinding("Artifact ingestion trust boundary", "internal/controller/artifacts.go", "Attacker-controlled artifact changes task status and privileged persisted result.")},
		{name: "prompt_tool_injection_privileged", keep: true, finding: evalFinding("Privileged prompt tool injection", "internal/api/chat.go", "Prompt injection can influence privileged tool use and patch generation.")},
		{name: "docs_only", keep: false, finding: evalFinding("Docs-only rate limit", "docs/security.md", "Documentation says rate limiting is missing.")},
		{name: "test_only", keep: false, finding: evalFinding("Test-only command injection", "internal/api/auth_test.go", "Test-only helper has command injection.")},
		{name: "generic_rate_limit", keep: false, finding: evalFinding("Missing rate limit", "internal/api/status.go", "Endpoint needs generic rate limiting.")},
		{name: "generic_dos", keep: false, finding: evalFinding("Generic DoS", "internal/api/server.go", "Generic denial of service due to memory exhaustion.")},
		{name: "dependency_version", keep: false, finding: evalFinding("Outdated dependency", "go.mod", "Dependency version has a CVE.")},
		{name: "client_side_auth_only", keep: false, finding: evalFinding("Client auth missing", "ui/src/App.tsx", "Client-side authorization check is missing.")},
		{name: "react_xss_without_unsafe_sink", keep: false, finding: evalFinding("React XSS", "ui/src/App.tsx", "React displays user input and may cause XSS.")},
		{name: "shell_script_no_untrusted_input", keep: false, finding: evalFinding("Shell command injection", "scripts/build.sh", "Shell command injection is claimed but no attacker input source is shown.")},
		{name: "generic_prompt_inclusion", keep: false, finding: evalFinding("Prompt injection", "internal/api/chat.go", "User prompt inclusion may cause generic prompt injection.")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertEvalFixtureExists(t, tc.keep, tc.name)
			result := FilterFindings([]*store.Finding{tc.finding}, FindingFilterOptions{})
			if tc.keep && (len(result.Kept) != 1 || len(result.Dropped) != 0) {
				t.Fatalf("FilterFindings() kept=%d dropped=%d, want kept: %#v", len(result.Kept), len(result.Dropped), result.Dropped)
			}
			if !tc.keep && (len(result.Kept) != 0 || len(result.Dropped) != 1) {
				t.Fatalf("FilterFindings() kept=%d dropped=%d, want dropped", len(result.Kept), len(result.Dropped))
			}
		})
	}
}

func evalFinding(title, filePath, summary string) *store.Finding {
	return &store.Finding{
		Title:      title,
		Category:   title,
		Summary:    summary,
		Severity:   "high",
		Confidence: "high",
		FilePath:   filePath,
		Evidence:   []store.FindingEvidenceRef{{Kind: "file", Path: filePath, StartLine: 1, EndLine: 1}},
	}
}

func assertEvalFixtureExists(t *testing.T, keep bool, name string) {
	t.Helper()
	kind := "false_positive"
	if keep {
		kind = "true_positive"
	}
	if _, err := os.Stat(filepath.Join("testdata", "scanner", kind, name, "README.md")); err != nil {
		t.Fatalf("missing scanner eval fixture %s/%s: %v", kind, name, err)
	}
}
