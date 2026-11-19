/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package anthropic

import (
	"strconv"
	"strings"
)

// legacyMaxTokens is the output limit Orka sends when a request sets none and
// the model is not one claudeModel recognizes as accepting more.
const legacyMaxTokens = 4096

// recentMaxTokens is the output limit Orka sends when a request sets none to
// Claude 4.5 and later models. Each of them accepts at least 64,000 output
// tokens, including through GitHub Copilot, which caps some of them there.
// Thinking counts toward the limit, and on models that always think a smaller
// one cuts replies short. The API charges only for the tokens a reply uses,
// and on these models a reply that reaches the end of the context window
// stops there instead of failing the request.
const recentMaxTokens = 64000

// claudeModel is a Claude model name split into its family and version.
// Version numbers are zero when the name has none.
type claudeModel struct {
	family string
	major  int
	minor  int
}

// parseClaudeModel reads the Claude model name wherever it appears in model,
// so it also recognizes Bedrock IDs (anthropic.claude-opus-5-5), Vertex
// snapshots (claude-opus-4-5@20251101) and dotted proxy names
// (claude-opus-4.7). It reports false for other names, including a
// non-Claude model behind an Anthropic-compatible endpoint and Claude 3
// names, which put the version before the family (claude-3-7-sonnet).
func parseClaudeModel(model string) (claudeModel, bool) {
	_, name, found := strings.Cut(strings.ToLower(model), "claude-")
	if !found {
		return claudeModel{}, false
	}
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '.' || r == '@'
	})
	if len(parts) == 0 {
		return claudeModel{}, false
	}
	m := claudeModel{family: parts[0]}
	switch m.family {
	case "fable", "mythos":
		// Every Fable and Mythos model is newer than Claude 4, including
		// claude-mythos-preview, which has no version.
		return m, true
	case "opus", "sonnet", "haiku":
	default:
		return claudeModel{}, false
	}
	if len(parts) < 2 {
		return claudeModel{}, false
	}
	major, err := strconv.Atoi(parts[1])
	if err != nil {
		return claudeModel{}, false
	}
	m.major = major
	// claude-opus-4-20250514 has a date where later names put a minor version.
	if len(parts) >= 3 && len(parts[2]) <= 2 {
		if minor, err := strconv.Atoi(parts[2]); err == nil {
			m.minor = minor
		}
	}
	return m, true
}

// atLeast reports whether m is Fable, Mythos, or an Opus, Sonnet or Haiku
// model of version major.minor or later.
func (m claudeModel) atLeast(major, minor int) bool {
	if m.family == "fable" || m.family == "mythos" {
		return true
	}
	return m.major > major || (m.major == major && m.minor >= minor)
}

// acceptsSamplingParams reports whether model accepts temperature. Claude
// Opus 4.7 and every later Claude model return a 400 for a request that sets
// it, so Orka omits the setting for them rather than fail the request. Any
// name parseClaudeModel does not recognize keeps its temperature.
func acceptsSamplingParams(model string) bool {
	m, ok := parseClaudeModel(model)
	return !ok || !m.atLeast(4, 7)
}

// defaultMaxTokens is the output limit for a request to model that sets none.
func defaultMaxTokens(model string) int64 {
	if m, ok := parseClaudeModel(model); ok && m.atLeast(4, 5) {
		return recentMaxTokens
	}
	return legacyMaxTokens
}
