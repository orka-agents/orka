/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package anthropic

import (
	"strconv"
	"strings"
)

// acceptsSamplingParams reports whether model accepts temperature. Claude
// Opus 4.7 and every later Claude model return a 400 for a request that sets
// it, so Orka omits the setting for them rather than fail the request.
//
// The check reads the Claude model name wherever it appears, so it also
// recognizes Bedrock IDs (anthropic.claude-opus-5-5), Vertex snapshots
// (claude-opus-4-5@20251101) and dotted proxy names (claude-opus-4.7). Any
// other name, including a non-Claude model behind an Anthropic-compatible
// endpoint, keeps its temperature.
func acceptsSamplingParams(model string) bool {
	_, name, found := strings.Cut(strings.ToLower(model), "claude-")
	if !found {
		return true
	}
	parts := strings.FieldsFunc(name, func(r rune) bool {
		return r == '-' || r == '.' || r == '@'
	})
	if len(parts) == 0 {
		return true
	}
	switch parts[0] {
	case "fable", "mythos":
		return false
	case "opus", "sonnet", "haiku":
	default:
		// Claude 3 names put the version first (claude-3-7-sonnet).
		return true
	}
	if len(parts) < 2 {
		return true
	}
	major, err := strconv.Atoi(parts[1])
	if err != nil {
		return true
	}
	if major != 4 {
		return major < 4
	}
	// claude-opus-4-20250514 has a date where later names put a minor version.
	if len(parts) < 3 || len(parts[2]) > 2 {
		return true
	}
	minor, err := strconv.Atoi(parts[2])
	return err != nil || minor < 7
}
