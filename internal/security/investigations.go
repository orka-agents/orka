package security

import (
	"encoding/json"
	"strings"
)

// Threat-model investigations are stored inside the threat model markdown so
// operators can read and edit them with the rest of the model. The marker and
// fenced JSON block make the section machine-readable.
const (
	threatModelInvestigationsHeading = "## Cross-component investigations"
	threatModelInvestigationsMarker  = "<!-- orka:security-investigations -->"

	MaxThreatModelInvestigations   = 8
	maxInvestigationTitleBytes     = 200
	maxInvestigationQuestionBytes  = 2000
	maxInvestigationPaths          = 12
	maxInvestigationsSectionsBytes = 64 << 10
)

// RenderThreatModelInvestigations replaces any investigations section in the
// threat model with one for items. Invalid items are dropped.
func RenderThreatModelInvestigations(content string, items []SecurityInvestigation) string {
	content = stripThreatModelInvestigations(content)
	items = SanitizeInvestigations(items)
	if len(items) == 0 {
		return content
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil || len(data) > maxInvestigationsSectionsBytes {
		return content
	}
	var out strings.Builder
	out.WriteString(strings.TrimRight(content, "\n"))
	out.WriteString("\n\n")
	out.WriteString(threatModelInvestigationsHeading)
	out.WriteString("\n\nEach question below becomes its own review slice that traces the path across components.\n\n")
	out.WriteString(threatModelInvestigationsMarker)
	out.WriteString("\n```json\n")
	out.Write(data)
	out.WriteString("\n```\n")
	return out.String()
}

// ThreatModelInvestigations returns the investigations recorded in a threat
// model, or none when the section is missing or malformed.
func ThreatModelInvestigations(content string) []SecurityInvestigation {
	block, _, _, ok := threatModelInvestigationsBlock(content)
	if !ok {
		return nil
	}
	var items []SecurityInvestigation
	if err := json.Unmarshal([]byte(block), &items); err != nil {
		return nil
	}
	return SanitizeInvestigations(items)
}

// SanitizeInvestigations bounds investigations and drops unsafe paths.
func SanitizeInvestigations(items []SecurityInvestigation) []SecurityInvestigation {
	out := make([]SecurityInvestigation, 0, min(len(items), MaxThreatModelInvestigations))
	seen := map[string]struct{}{}
	for _, item := range items {
		item.Title = truncateUTF8(singleLine(item.Title), maxInvestigationTitleBytes)
		item.Question = truncateUTF8(strings.TrimSpace(stripUnsafeTextRunes(item.Question)), maxInvestigationQuestionBytes)
		if item.Question == "" {
			continue
		}
		if item.Title == "" {
			item.Title = truncateUTF8(singleLine(item.Question), 80)
		}
		key := strings.ToLower(item.Question)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		paths := make([]string, 0, min(len(item.Paths), maxInvestigationPaths))
		for _, p := range item.Paths {
			p = strings.TrimSuffix(strings.TrimSpace(p), "/")
			if p == "" || !SafeRepoPath(p) {
				continue
			}
			paths = append(paths, p)
			if len(paths) == maxInvestigationPaths {
				break
			}
		}
		item.Paths = paths
		out = append(out, item)
		if len(out) == MaxThreatModelInvestigations {
			break
		}
	}
	return out
}

func singleLine(value string) string {
	return strings.Join(strings.Fields(stripUnsafeTextRunes(value)), " ")
}

func stripThreatModelInvestigations(content string) string {
	_, start, end, ok := threatModelInvestigationsBlock(content)
	if !ok {
		return content
	}
	return strings.TrimRight(content[:start], "\n") + "\n" + strings.TrimLeft(content[end:], "\n")
}

// threatModelInvestigationsBlock locates the last investigations section and
// returns its JSON and the byte range of the whole section.
func threatModelInvestigationsBlock(content string) (block string, start, end int, ok bool) {
	marker := strings.LastIndex(content, threatModelInvestigationsMarker)
	if marker < 0 {
		return "", 0, 0, false
	}
	start = marker
	if heading := strings.LastIndex(content[:marker], threatModelInvestigationsHeading); heading >= 0 &&
		!strings.Contains(content[heading+len(threatModelInvestigationsHeading):marker], "\n#") {
		start = heading
	}
	rest := content[marker+len(threatModelInvestigationsMarker):]
	open := strings.Index(rest, "```json")
	if open < 0 || strings.TrimSpace(rest[:open]) != "" {
		return "", 0, 0, false
	}
	body := rest[open+len("```json"):]
	closeIndex := strings.Index(body, "\n```")
	if closeIndex < 0 {
		return "", 0, 0, false
	}
	block = body[:closeIndex]
	end = marker + len(threatModelInvestigationsMarker) + open + len("```json") + closeIndex + len("\n```")
	return block, start, end, true
}
