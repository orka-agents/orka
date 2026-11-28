package security

import (
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

func TestBuildSARIFLogAndGate(t *testing.T) {
	findings := []store.Finding{
		{
			ID:               "fnd_1",
			Fingerprint:      "fp_1",
			Title:            "Path traversal in archive extract",
			Category:         "path-traversal",
			Summary:          "Archive entry escapes target dir",
			Severity:         "high",
			Confidence:       "high",
			State:            "open",
			ValidationStatus: "validated",
			Evidence: []store.FindingEvidenceRef{
				{Kind: "file", Path: "internal/archive/extract.go", StartLine: 40, EndLine: 55},
			},
		},
		{
			ID:               "fnd_2",
			Fingerprint:      "fp_2",
			Title:            "Dismissed noise",
			Category:         "logging",
			Summary:          "False positive",
			Severity:         "critical",
			Confidence:       "low",
			State:            "false_positive",
			ValidationStatus: "failed",
			FilePath:         "internal/api/log.go",
			Line:             12,
		},
	}

	sarif := BuildSARIFLog(findings)
	if sarif.Version != SARIFVersion || len(sarif.Runs) != 1 || len(sarif.Runs[0].Results) != 2 {
		t.Fatalf("unexpected SARIF log: %#v", sarif)
	}
	if sarif.Runs[0].Results[0].Level != "error" || sarif.Runs[0].Results[0].RuleID != "orka/path-traversal" {
		t.Fatalf("unexpected first SARIF result: %#v", sarif.Runs[0].Results[0])
	}
	if len(sarif.Runs[0].Results[0].Locations) != 1 || sarif.Runs[0].Results[0].Locations[0].PhysicalLocation.Region.StartLine != 40 {
		t.Fatalf("unexpected SARIF locations: %#v", sarif.Runs[0].Results[0].Locations)
	}

	gateHigh := EvaluateFindingsGate(findings, "high")
	if gateHigh.Passed || gateHigh.MatchedCount != 1 || gateHigh.TotalOpen != 1 {
		t.Fatalf("EvaluateFindingsGate(high) = %#v, want fail with 1 matched open finding", gateHigh)
	}
	gateCritical := EvaluateFindingsGate(findings, "critical")
	if !gateCritical.Passed || gateCritical.MatchedCount != 0 {
		t.Fatalf("EvaluateFindingsGate(critical) = %#v, want pass (dismissed critical excluded)", gateCritical)
	}
}

func TestBuildDismissedFindingsFeedback(t *testing.T) {
	findings := []store.Finding{
		{Title: "Open issue", Category: "auth", State: "open", FilePath: "internal/api/auth.go"},
		{Title: "Known safe pattern", Category: "xss", State: "false_positive", FilePath: "ui/src/view.tsx"},
		{Title: "Accepted operational risk", Category: "dos", State: "wont_fix", FilePath: "internal/api/rate.go"},
	}
	feedback := BuildDismissedFindingsFeedback(findings)
	if !strings.Contains(feedback, "Known safe pattern") || !strings.Contains(feedback, "Accepted operational risk") {
		t.Fatalf("feedback missing dismissed items:\n%s", feedback)
	}
	if strings.Contains(feedback, "Open issue") {
		t.Fatalf("feedback must not include open findings:\n%s", feedback)
	}
}

func TestScannerEvalPrecisionRecallScoring(t *testing.T) {
	corpus := []struct {
		name    string
		finding *store.Finding
		wantTP  bool
	}{
		{name: "tp_path_traversal", wantTP: true, finding: evalFinding("Archive path traversal", "internal/archive/extract.go", "Attacker-controlled archive path crosses filesystem trust boundary to privileged write sink.")},
		{name: "tp_auth_bypass", wantTP: true, finding: evalFinding("API auth bypass", "internal/api/auth.go", "Attacker-controlled request bypasses server-side authorization and exposes tenant data.")},
		{name: "tp_secret_leak", wantTP: true, finding: evalFinding("TxToken secret logging", "internal/api/token.go", "Raw TxToken credential is logged and persisted.")},
		{name: "fp_docs_only", wantTP: false, finding: evalFinding("Docs-only rate limit", "docs/security.md", "Documentation says rate limiting is missing.")},
		{name: "fp_generic_dos", wantTP: false, finding: evalFinding("Generic DoS", "internal/api/server.go", "Generic denial of service due to memory exhaustion.")},
		{name: "fp_client_auth", wantTP: false, finding: evalFinding("Client auth missing", "ui/src/App.tsx", "Client-side authorization check is missing.")},
	}

	var tp, fp, fn int
	for _, tc := range corpus {
		res := FilterFindings([]*store.Finding{tc.finding})
		kept := len(res.Kept) == 1
		switch {
		case tc.wantTP && kept:
			tp++
		case tc.wantTP && !kept:
			fn++
		case !tc.wantTP && kept:
			fp++
		}
	}
	precision := float64(tp) / float64(tp+fp)
	recall := float64(tp) / float64(tp+fn)
	if precision < 1.0 || recall < 1.0 {
		t.Fatalf("scanner eval corpus precision=%.2f recall=%.2f (tp=%d fp=%d fn=%d), want 1.00/1.00", precision, recall, tp, fp, fn)
	}
}
