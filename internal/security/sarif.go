package security

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/orka-agents/orka/internal/store"
)

const (
	SARIFSchemaURI    = "https://json.schemastore.org/sarif-2.1.0.json"
	SARIFVersion      = "2.1.0"
	SARIFToolName     = "Orka Security Scanner"
	sarifPropSeverity = "severity"
	sarifPropConf     = "confidence"
	sarifLevelNote    = "note"
)

type SARIFLog struct {
	Schema  string     `json:"$schema"`
	Version string     `json:"version"`
	Runs    []SARIFRun `json:"runs"`
}

type SARIFRun struct {
	Tool    SARIFTool     `json:"tool"`
	Results []SARIFResult `json:"results"`
}

type SARIFTool struct {
	Driver SARIFToolDriver `json:"driver"`
}

type SARIFToolDriver struct {
	Name    string      `json:"name"`
	Version string      `json:"version"`
	Rules   []SARIFRule `json:"rules,omitempty"`
}

type SARIFRule struct {
	ID               string           `json:"id"`
	Name             string           `json:"name,omitempty"`
	ShortDescription SARIFMessageText `json:"shortDescription"`
}

type SARIFMessageText struct {
	Text string `json:"text"`
}

type SARIFSuppression struct {
	Kind          string `json:"kind"`
	Status        string `json:"status,omitempty"`
	Justification string `json:"justification,omitempty"`
}

type SARIFResult struct {
	RuleID              string             `json:"ruleId"`
	Level               string             `json:"level"`
	Message             SARIFMessageText   `json:"message"`
	Locations           []SARIFLocation    `json:"locations,omitempty"`
	Suppressions        []SARIFSuppression `json:"suppressions,omitempty"`
	PartialFingerprints map[string]string  `json:"partialFingerprints,omitempty"`
	Properties          map[string]any     `json:"properties,omitempty"`
}

type SARIFLocation struct {
	PhysicalLocation SARIFPhysicalLocation `json:"physicalLocation"`
}

type SARIFPhysicalLocation struct {
	ArtifactLocation SARIFArtifactLocation `json:"artifactLocation"`
	Region           *SARIFRegion          `json:"region,omitempty"`
}

type SARIFArtifactLocation struct {
	URI string `json:"uri"`
}

type SARIFRegion struct {
	StartLine int `json:"startLine"`
	EndLine   int `json:"endLine,omitempty"`
}

func BuildSARIFLog(findings []store.Finding) SARIFLog {
	ruleSeen := make(map[string]struct{}, len(findings))
	rules := make([]SARIFRule, 0)
	results := make([]SARIFResult, 0, len(findings))

	for i := range findings {
		finding := findings[i]
		state := strings.ToLower(strings.TrimSpace(finding.State))
		if state == "fixed" || state == "resolved" {
			continue
		}
		ruleID := sarifRuleID(finding.Category)
		if _, ok := ruleSeen[ruleID]; !ok {
			ruleSeen[ruleID] = struct{}{}
			rules = append(rules, SARIFRule{
				ID:               ruleID,
				ShortDescription: SARIFMessageText{Text: strings.TrimSpace(finding.Category)},
			})
		}

		msg := strings.TrimSpace(finding.Title)
		if summary := strings.TrimSpace(finding.Summary); summary != "" {
			msg = fmt.Sprintf("%s: %s", msg, summary)
		}

		result := SARIFResult{
			RuleID:  ruleID,
			Level:   sarifLevel(finding.Severity),
			Message: SARIFMessageText{Text: msg},
			Properties: map[string]any{
				"findingId":        finding.ID,
				sarifPropSeverity:  finding.Severity,
				sarifPropConf:      finding.Confidence,
				"state":            finding.State,
				"validationStatus": finding.ValidationStatus,
			},
		}
		if state == "false_positive" || state == "wont_fix" || state == "dismissed" {
			result.Suppressions = []SARIFSuppression{{
				Kind:          "external",
				Status:        "accepted",
				Justification: state,
			}}
		}
		if fp := strings.TrimSpace(finding.Fingerprint); fp != "" {
			result.PartialFingerprints = map[string]string{"orkaFingerprint": fp}
		}
		if locs := sarifLocations(finding); len(locs) > 0 {
			result.Locations = locs
		}
		results = append(results, result)
	}

	return SARIFLog{
		Schema:  SARIFSchemaURI,
		Version: SARIFVersion,
		Runs: []SARIFRun{
			{
				Tool: SARIFTool{
					Driver: SARIFToolDriver{
						Name:    SARIFToolName,
						Version: ScannerPolicyVersion,
						Rules:   rules,
					},
				},
				Results: results,
			},
		},
	}
}

func sarifRuleID(category string) string {
	clean := sanitizeName(strings.TrimSpace(category))
	if clean == "" {
		return "orka-security-finding"
	}
	return "orka/" + clean
}

func sarifLevel(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case findingSeverityCritical, findingLevelHigh:
		return "error"
	case findingLevelMedium:
		return "warning"
	default:
		return sarifLevelNote
	}
}

func sarifRelativeURI(repoPath string) string {
	parts := strings.Split(strings.TrimSpace(repoPath), "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}

func sarifLocations(finding store.Finding) []SARIFLocation {
	locs := make([]SARIFLocation, 0, max(1, len(finding.Evidence)))
	for _, ref := range finding.Evidence {
		if ref.Kind != "file" || strings.TrimSpace(ref.Path) == "" {
			continue
		}
		loc := SARIFLocation{
			PhysicalLocation: SARIFPhysicalLocation{
				ArtifactLocation: SARIFArtifactLocation{URI: sarifRelativeURI(ref.Path)},
			},
		}
		if ref.StartLine > 0 {
			region := &SARIFRegion{StartLine: ref.StartLine}
			if ref.EndLine >= ref.StartLine {
				region.EndLine = ref.EndLine
			}
			loc.PhysicalLocation.Region = region
		}
		locs = append(locs, loc)
	}
	if len(locs) == 0 && strings.TrimSpace(finding.FilePath) != "" {
		loc := SARIFLocation{
			PhysicalLocation: SARIFPhysicalLocation{
				ArtifactLocation: SARIFArtifactLocation{URI: sarifRelativeURI(finding.FilePath)},
			},
		}
		if finding.Line > 0 {
			loc.PhysicalLocation.Region = &SARIFRegion{StartLine: finding.Line}
		}
		locs = append(locs, loc)
	}
	return locs
}

type GateResult struct {
	Passed       bool   `json:"passed"`
	FailOn       string `json:"failOn"`
	MatchedCount int    `json:"matchedCount"`
	TotalOpen    int    `json:"totalOpen"`
}

func EvaluateFindingsGate(findings []store.Finding, failOnSeverity string) GateResult {
	threshold := strings.ToLower(strings.TrimSpace(failOnSeverity))
	if threshold == "" {
		threshold = findingLevelHigh
	}
	res := GateResult{
		Passed: true,
		FailOn: threshold,
	}
	for i := range findings {
		f := findings[i]
		if !isOpenFindingState(f.State) {
			continue
		}
		res.TotalOpen++
		if SeverityMeetsMinimum(f.Severity, threshold) {
			res.MatchedCount++
		}
	}
	res.Passed = res.MatchedCount == 0
	return res
}

func isOpenFindingState(state string) bool {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "", "open", "patch_pending", "patch_ready", "pr_open":
		return true
	default:
		return false
	}
}

const (
	maxDismissedFeedbackItems     = 12
	maxDismissedFeedbackLineBytes = 200
)

func BuildDismissedFindingsFeedback(findings []store.Finding) string {
	var lines []string
	for i := range findings {
		f := findings[i]
		state := strings.ToLower(strings.TrimSpace(f.State))
		if state != "false_positive" && state != "wont_fix" && state != "dismissed" {
			continue
		}
		title := sanitizeDroppedSampleValue(f.Title)
		category := sanitizeDroppedSampleValue(f.Category)
		filePath := sanitizeDroppedSampleValue(f.FilePath)
		if title == "" {
			continue
		}
		entry := fmt.Sprintf("- [%s] %s", state, title)
		if category != "" {
			entry += fmt.Sprintf(" (category: %s)", category)
		}
		if filePath != "" {
			entry += fmt.Sprintf(" at %s", filePath)
		}
		entry = truncateUTF8(entry, maxDismissedFeedbackLineBytes)
		lines = append(lines, entry)
		if len(lines) >= maxDismissedFeedbackItems {
			break
		}
	}
	if len(lines) == 0 {
		return ""
	}
	return "Previously dismissed repository findings (do not re-report unless new changes introduce a concrete exploit path):\n" + strings.Join(lines, "\n")
}
