package security

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sozercan/orka/internal/store"
)

const filterTestAuthzCategory = "authz"

func TestFilterFindingsDropsDocsOnlyFindings(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding("README rate limit", "docs/security.md", "Documentation says rate limiting is missing.")}, FindingFilterOptions{})
	assertFilterDropped(t, got, "docs-only")
}

func TestFilterFindingsKeepsNegatedDocsOnlyProductionFinding(t *testing.T) {
	finding := filterFinding(
		"Production authorization bypass is not docs-only",
		"internal/api/auth.go",
		"This is not only documentation; a runtime handler skips the tenant authorization check.",
	)
	finding.Category = filterTestAuthzCategory
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsCodeUnderDocsDirectory(t *testing.T) {
	finding := filterFinding(
		"Docs preview server auth bypass",
		"cmd/docs/server.go",
		"The docs preview service skips authorization for a runtime handler.",
	)
	finding.Category = filterTestAuthzCategory
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsDropsNonCodeDocsAssetsUnderDocsDirectory(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"OpenAPI rate limit note",
		"docs/openapi.yaml",
		"Documentation says rate limiting is missing.",
	)}, FindingFilterOptions{})
	assertFilterDropped(t, got, "docs-only")
}

func TestFilterFindingsDropsSourceSnippetUnderRootDocsDirectory(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Docs example auth bypass",
		"docs/examples/auth.go",
		"Documentation example code skips authorization.",
	)}, FindingFilterOptions{})
	assertFilterDropped(t, got, "docs-only")
}

func TestFilterFindingsKeepsRuntimeTextArtifacts(t *testing.T) {
	finding := filterFinding(
		"Dependency confusion risk",
		"requirements.txt",
		"Production dependency constraints can install a malicious package from an untrusted source.",
	)
	finding.Category = "dependency-confusion"
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsDropsTestOnlyFindings(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding("Test helper command injection", "internal/api/auth_test.go", "Test-only helper has command injection.")}, FindingFilterOptions{})
	assertFilterDropped(t, got, "test-only")
}

func TestFilterFindingsKeepsNegatedTestOnlyProductionFinding(t *testing.T) {
	finding := filterFinding(
		"Production authorization bypass is not a test-only issue",
		"internal/api/auth.go",
		"This is not merely test-only; a runtime handler skips the tenant authorization check.",
	)
	finding.Category = filterTestAuthzCategory
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsDropsGenericRateLimit(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding("Missing rate limit", "internal/api/status.go", "Endpoint should add generic rate limiting.")}, FindingFilterOptions{})
	assertFilterDropped(t, got, "rate-limit")
}

func TestFilterFindingsKeepsConcreteTenantBoundaryRateLimit(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding("Tenant quota bypass", "internal/api/auth.go", "Missing rate limit permits cross-tenant cost exhaustion across a security boundary.")}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsDropsGenericPromptInjection(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding("Prompt injection", "internal/api/chat.go", "User prompt inclusion may cause generic prompt injection.")}, FindingFilterOptions{})
	assertFilterDropped(t, got, "prompt-injection")
}

func TestFilterFindingsKeepsPrivilegedToolPromptInjection(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding("Privileged tool prompt injection", "internal/api/chat.go", "Untrusted prompt injection can influence privileged tool use and artifact contents.")}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsDropsReactXSSWithoutUnsafeSink(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding("React XSS", "ui/src/App.tsx", "React renders a value, causing XSS.")}, FindingFilterOptions{})
	assertFilterDropped(t, got, "react xss")
}

func TestFilterFindingsKeepsDangerouslySetInnerHTML(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding("React XSS", "ui/src/App.tsx", "Attacker-controlled HTML reaches dangerouslySetInnerHTML.")}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsDroppedDiagnosticsAreSanitized(t *testing.T) {
	tokenPrefix := "g" + "hp_"
	finding := filterFinding("Best practice hardening "+tokenPrefix+strings.Repeat("x", 32), "internal/api/security.go", "Generic best practice hardening.")
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterDropped(t, got, "best-practice")
	data, err := json.Marshal(got.Dropped[0].Sample)
	if err != nil {
		t.Fatalf("marshal sample: %v", err)
	}
	if strings.Contains(string(data), tokenPrefix+"xxxx") {
		t.Fatalf("sample was not redacted: %s", string(data))
	}
	if got.Dropped[0].Layer != "filter" {
		t.Fatalf("Layer = %q, want filter", got.Dropped[0].Layer)
	}
}

func filterFinding(title, filePath, summary string) *store.Finding {
	return &store.Finding{
		Title:      title,
		Category:   "test",
		Summary:    summary,
		Severity:   "medium",
		Confidence: "medium",
		FilePath:   filePath,
		Evidence: []store.FindingEvidenceRef{{
			Kind:      "file",
			Path:      filePath,
			StartLine: 1,
			EndLine:   1,
		}},
	}
}

func assertFilterDropped(t *testing.T, got FindingFilterResult, reasonContains string) {
	t.Helper()
	if len(got.Kept) != 0 || len(got.Dropped) != 1 {
		t.Fatalf("FilterFindings() kept=%d dropped=%d, want 0/1", len(got.Kept), len(got.Dropped))
	}
	if !strings.Contains(got.Dropped[0].Reason, reasonContains) {
		t.Fatalf("drop reason = %q, want contains %q", got.Dropped[0].Reason, reasonContains)
	}
}

func assertFilterKept(t *testing.T, got FindingFilterResult) {
	t.Helper()
	if len(got.Kept) != 1 || len(got.Dropped) != 0 {
		t.Fatalf("FilterFindings() kept=%d dropped=%d, want 1/0: %#v", len(got.Kept), len(got.Dropped), got.Dropped)
	}
}

func TestFilterFindingsDroppedDiagnosticsRedactEmbeddedKeyValueSecrets(t *testing.T) {
	prefix := "g" + "hp_"
	finding := filterFinding("generic hardening "+"to"+"ken="+prefix+strings.Repeat("x", 32), "internal/api/security.go", "Generic best practice hardening.")
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterDropped(t, got, "best-practice")
	data, err := json.Marshal(got.Dropped[0].Sample)
	if err != nil {
		t.Fatalf("marshal sample: %v", err)
	}
	if strings.Contains(string(data), prefix+"xxxx") {
		t.Fatalf("embedded token sample was not redacted: %s", string(data))
	}
}

func TestFilterFindingsKeepsServerSideTypeScriptAuthFinding(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Authorization bypass",
		"server/routes/auth.ts",
		"Attacker-controlled request bypasses authorization and exposes tenant data.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsNextJSServerAPIAuthFinding(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Admin API authorization bypass",
		"pages/api/admin.ts",
		"Attacker-controlled request bypasses authorization and exposes tenant data.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsNextJSAppRouterAPIAuthFinding(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Admin API authorization bypass",
		"web/app/api/admin/route.ts",
		"Attacker-controlled request bypasses authorization and exposes tenant data.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsWebAPIAuthFinding(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Web API authorization bypass",
		"web/api/admin.ts",
		"Attacker-controlled request bypasses authorization and exposes tenant data.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsServerRoutesAPIAuthFinding(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Admin route authorization bypass",
		"src/routes/api/admin.ts",
		"Attacker-controlled request bypasses authorization and exposes tenant data.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsClientAuthFindingWithBackendEvidence(t *testing.T) {
	finding := filterFinding(
		"Authorization bypass spans frontend and backend",
		"ui/src/AuthGate.tsx",
		"A runtime backend route fails to enforce the tenant authorization decision.",
	)
	finding.Category = filterTestAuthzCategory
	finding.Evidence = append(finding.Evidence, store.FindingEvidenceRef{Kind: "file", Path: "internal/api/auth.go", StartLine: 10, EndLine: 20})
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsClientAuthFindingWithNextJSBackendEvidence(t *testing.T) {
	finding := filterFinding(
		"Authorization bypass spans frontend and backend",
		"ui/src/AuthGate.tsx",
		"A backend API route fails to enforce the tenant authorization decision.",
	)
	finding.Category = filterTestAuthzCategory
	finding.Evidence = append(finding.Evidence, store.FindingEvidenceRef{Kind: "file", Path: "pages/api/admin.ts", StartLine: 10, EndLine: 20})
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsClientAuthFindingWithSrcAPIBackendEvidence(t *testing.T) {
	finding := filterFinding(
		"Authorization bypass spans frontend and backend",
		"ui/src/AuthGate.tsx",
		"A backend API route fails to enforce the tenant authorization decision.",
	)
	finding.Category = filterTestAuthzCategory
	finding.Evidence = append(finding.Evidence, store.FindingEvidenceRef{Kind: "file", Path: "src/api/auth.ts", StartLine: 10, EndLine: 20})
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsDropsClientAuthFindingWithOnlyFrontendAPIWrapperEvidence(t *testing.T) {
	finding := filterFinding(
		"Client authorization bypass",
		"ui/src/AuthGate.tsx",
		"Client auth gate bypasses authorization without backend trust boundary evidence.",
	)
	finding.Category = filterTestAuthzCategory
	finding.Evidence = append(finding.Evidence, store.FindingEvidenceRef{Kind: "file", Path: "ui/src/api/auth.ts", StartLine: 10, EndLine: 20})
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterDropped(t, got, "client-side auth")
}

func TestFilterFindingsDropsWebClientAuthFindingWithOnlyFrontendAPIWrapperEvidence(t *testing.T) {
	finding := filterFinding(
		"Client authorization bypass",
		"web/src/AuthGate.tsx",
		"Client auth gate bypasses authorization without backend trust boundary evidence.",
	)
	finding.Category = filterTestAuthzCategory
	finding.Evidence = append(finding.Evidence, store.FindingEvidenceRef{Kind: "file", Path: "web/src/api/auth.ts", StartLine: 10, EndLine: 20})
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterDropped(t, got, "client-side auth")
}

func TestFilterFindingsKeepsDocsCredentialLeak(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Credential leak in README",
		"docs/security.md",
		"README contains an API key committed to documentation.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsProductionFindingWithTestOnlyRegressionText(t *testing.T) {
	finding := filterFinding("Command injection", "internal/api/run.go", "Attacker-controlled request reaches shell execution.")
	finding.SuggestedRegressionTest = "Add a test-only case for shell metacharacters."
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsDocsAPIKeyCredentialLeak(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"README contains API_KEY credential",
		"docs/security.md",
		"Documentation contains API_KEY material.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsClientSideCredentialLeak(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Authentication token leak",
		"ui/src/App.tsx",
		"Authentication token is stored in localStorage and exposed to script execution.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsAuthorBioXSSWithUnsafeSink(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Author bio XSS",
		"ui/src/components/Profile.tsx",
		"Attacker-controlled author bio reaches dangerouslySetInnerHTML.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsDropsTopLevelTestDirectoryFinding(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Test-only auth helper issue",
		"tests/e2e/auth.ts",
		"Test-only fixture has an auth helper issue.",
	)}, FindingFilterOptions{})
	assertFilterDropped(t, got, "test-only")
}

func TestFilterFindingsKeepsDocsAWSAccessKeyLeak(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"AWS access key ID "+"A"+"KIA"+strings.Repeat("A", 16),
		"docs/security.md",
		"Documentation contains an AWS access key.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsDroppedDiagnosticsRedactAWSAccessKey(t *testing.T) {
	key := "A" + "KIA" + strings.Repeat("A", 16)
	finding := filterFinding("Generic hardening "+key, "internal/api/security.go", "Generic best practice hardening.")
	got := FilterFindings([]*store.Finding{finding}, FindingFilterOptions{})
	assertFilterDropped(t, got, "best-practice")
	data, err := json.Marshal(got.Dropped[0].Sample)
	if err != nil {
		t.Fatalf("marshal sample: %v", err)
	}
	if strings.Contains(string(data), key) {
		t.Fatalf("AWS access key sample was not redacted: %s", string(data))
	}
}

func TestFilterFindingsKeepsLoginPasswordGuessingRateLimit(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Login endpoint missing rate limit",
		"internal/api/login.go",
		"Missing rate limit enables online password guessing and account takeover.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}

func TestFilterFindingsKeepsAuditLogInjection(t *testing.T) {
	got := FilterFindings([]*store.Finding{filterFinding(
		"Audit log injection",
		"internal/api/audit.go",
		"Attacker-controlled username can forge admin audit entries and break audit integrity.",
	)}, FindingFilterOptions{})
	assertFilterKept(t, got)
}
