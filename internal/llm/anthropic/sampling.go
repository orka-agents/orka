/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package anthropic

import (
	"strconv"
	"strings"
)

const legacyMaxTokens = 4096

const recentMaxTokens = 64000

type claudeModel struct {
	family string
	major  int
	minor  int
}

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
	if len(parts) >= 3 && len(parts[2]) <= 2 {
		if minor, err := strconv.Atoi(parts[2]); err == nil {
			m.minor = minor
		}
	}
	return m, true
}

func (m claudeModel) atLeast(major, minor int) bool {
	if m.family == "fable" || m.family == "mythos" {
		return true
	}
	return m.major > major || (m.major == major && m.minor >= minor)
}

func acceptsSamplingParams(model string) bool {
	m, ok := parseClaudeModel(model)
	return !ok || !m.atLeast(4, 7)
}

func defaultMaxTokens(model string) int64 {
	if m, ok := parseClaudeModel(model); ok && m.atLeast(4, 5) {
		return recentMaxTokens
	}
	return legacyMaxTokens
}
