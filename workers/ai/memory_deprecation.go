/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"os"
	"slices"

	"github.com/orka-agents/orka/internal/aitools"
)

// warnDeprecatedMemoryTool runs only at an enabled built-in call site. Log only
// the known tool name, never the model's arguments or memory/transcript content.
func warnDeprecatedMemoryTool(name string, deprecated []string) {
	if !slices.Contains(aitools.MemoryToolNames(), name) || !slices.Contains(deprecated, name) {
		return
	}
	_ = json.NewEncoder(os.Stderr).Encode(map[string]any{
		"level": "warning",
		"tool":  name,
		"message": "Implicit memory tool access is deprecated and will be disabled in the next release; " +
			"select the tool in Agent spec.tools or Task spec.ai.tools, or enable coordination without " +
			"orka.ai/disable-coordination-tool-injection. See issue #680.",
	})
}
