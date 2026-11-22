/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package anthropic

import (
	"strconv"
	"strings"
)

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
	if len(parts) < 3 || len(parts[2]) > 2 {
		return true
	}
	minor, err := strconv.Atoi(parts[2])
	return err != nil || minor < 7
}
