package security

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/orka-agents/orka/internal/store"
)

type DistilledRule struct {
	RuleID         string `json:"ruleId"`
	Category       string `json:"category"`
	Severity       string `json:"severity"`
	SinkSymbol     string `json:"sinkSymbol,omitempty"`
	SemgrepYAML    string `json:"semgrepYaml,omitempty"`
	ScanPolicyHint string `json:"scanPolicyHint"`
}

var safeSemgrepSymbolPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*(\.[A-Za-z_][A-Za-z0-9_]*){0,3}$`)

func BuildVariantInvestigation(finding *store.Finding) (SecurityInvestigation, bool) {
	if finding == nil {
		return SecurityInvestigation{}, false
	}
	title := singleLine(finding.Title)
	if title == "" {
		return SecurityInvestigation{}, false
	}
	category := singleLine(finding.Category)
	rootCause := singleLine(finding.RootCause)
	var parts []string
	parts = append(parts, fmt.Sprintf("Hunt for sibling variants of confirmed finding %q (%s).", title, finding.ID))
	if category != "" {
		parts = append(parts, fmt.Sprintf("Vulnerability class: %s.", category))
	}
	if rootCause != "" {
		parts = append(parts, fmt.Sprintf("Root cause pattern: %s.", rootCause))
	}
	if finding.FilePath != "" {
		parts = append(parts, fmt.Sprintf("Original instance: %s:%d. Search all sibling handlers, controllers, and call sites across the repository for the same missing control or unsafe sink.", finding.FilePath, finding.Line))
	} else {
		parts = append(parts, "Search all sibling handlers, controllers, and call sites across the repository for the same missing control or unsafe sink.")
	}

	paths := make([]string, 0, len(finding.Evidence)+1)
	seen := map[string]struct{}{}
	addPath := func(p string) {
		p = strings.TrimSuffix(strings.TrimSpace(p), "/")
		if p == "" || !SafeRepoPath(p) {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	addPath(finding.FilePath)
	for _, ref := range finding.Evidence {
		if ref.Kind == fileLocationKind {
			addPath(ref.Path)
		}
	}

	sanitized := SanitizeInvestigations([]SecurityInvestigation{{
		Title:    "Variant hunt: " + title,
		Question: strings.Join(parts, " "),
		Paths:    paths,
	}})
	if len(sanitized) == 0 {
		return SecurityInvestigation{}, false
	}
	return sanitized[0], true
}

const defaultDistilledRuleCategory = "security"

func DistillFindingRule(finding *store.Finding) (DistilledRule, error) {
	if finding == nil {
		return DistilledRule{}, fmt.Errorf("finding is required")
	}
	title := sanitizeDroppedSampleValue(finding.Title)
	if title == "" {
		return DistilledRule{}, fmt.Errorf("finding title is required")
	}
	category := sanitizeDroppedSampleValue(finding.Category)
	if category == "" {
		category = defaultDistilledRuleCategory
	}
	ruleID := "orka-" + sanitizeName(category) + "-" + sanitizeName(finding.ID)
	lang := semgrepLanguageForPath(finding.FilePath)
	sev := semgrepSeverity(finding.Severity)
	message := sanitizeDroppedSampleValue(finding.RootCause)
	if message == "" {
		message = sanitizeDroppedSampleValue(finding.Summary)
	}
	if message == "" {
		message = title
	}
	if LooksLikeSecret(message) || LooksLikeSecret(title) {
		return DistilledRule{}, fmt.Errorf("finding contains credential-shaped text and cannot be distilled")
	}

	sinkSymbol := distilledSinkSymbol(finding)
	var semgrepYAML string
	if sinkSymbol != "" {
		var yamlOut strings.Builder
		yamlOut.WriteString("rules:\n")
		fmt.Fprintf(&yamlOut, "  - id: %s\n", ruleID)
		fmt.Fprintf(&yamlOut, "    message: %q\n", message)
		fmt.Fprintf(&yamlOut, "    severity: %s\n", sev)
		fmt.Fprintf(&yamlOut, "    languages: [%s]\n", lang)
		yamlOut.WriteString("    metadata:\n")
		fmt.Fprintf(&yamlOut, "      category: %q\n", category)
		fmt.Fprintf(&yamlOut, "      sourceFindingId: %q\n", finding.ID)
		yamlOut.WriteString("    pattern: |\n")
		fmt.Fprintf(&yamlOut, "      %s(...)\n", sinkSymbol)
		semgrepYAML = yamlOut.String()
	}

	hint := fmt.Sprintf("Check for variants of %q (category: %s): %s", title, category, message)
	return DistilledRule{
		RuleID:         ruleID,
		Category:       category,
		Severity:       finding.Severity,
		SinkSymbol:     sinkSymbol,
		SemgrepYAML:    semgrepYAML,
		ScanPolicyHint: truncateUTF8(hint, 400),
	}, nil
}

func distilledSinkSymbol(finding *store.Finding) string {
	if finding == nil {
		return ""
	}
	for _, ref := range finding.Evidence {
		if strings.EqualFold(strings.TrimSpace(ref.Role), "sink") {
			if sym := strings.TrimSpace(ref.Symbol); safeSemgrepSymbolPattern.MatchString(sym) {
				return sym
			}
		}
	}
	for _, ref := range finding.Evidence {
		if sym := strings.TrimSpace(ref.Symbol); safeSemgrepSymbolPattern.MatchString(sym) {
			return sym
		}
	}
	return ""
}

func semgrepLanguageForPath(filePath string) string {
	lower := strings.ToLower(strings.TrimSpace(filePath))
	switch {
	case strings.HasSuffix(lower, ".go"):
		return "go"
	case strings.HasSuffix(lower, ".py"):
		return "python"
	case strings.HasSuffix(lower, ".ts"), strings.HasSuffix(lower, ".tsx"):
		return "typescript"
	case strings.HasSuffix(lower, ".js"), strings.HasSuffix(lower, ".jsx"):
		return "javascript"
	case strings.HasSuffix(lower, ".rs"):
		return "rust"
	case strings.HasSuffix(lower, ".java"):
		return "java"
	default:
		return "generic"
	}
}

func semgrepSeverity(severity string) string {
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case findingSeverityCritical, findingLevelHigh:
		return "ERROR"
	case findingLevelMedium:
		return "WARNING"
	default:
		return "INFO"
	}
}
