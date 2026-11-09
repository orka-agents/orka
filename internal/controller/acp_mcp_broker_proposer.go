package controller

import (
	"strings"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/labels"
)

// proposerBrokeredTools is the complete brokered tool allowlist for an
// improvement proposer. Every other brokered tool, including delegation,
// validation, web, memory recall, transcript search, messaging and connector
// tools, is refused by the broker for a proposer Task.
var proposerBrokeredTools = map[string]struct{}{
	"propose_memory": {},
}

// proposerDeniedNativeTools are provider-native tools a proposer must not
// list even though its runtime offers them: web access exists only through
// brokered tools, which a proposer is denied.
var proposerDeniedNativeTools = []string{providerNativeToolWebFetch, providerNativeToolWebSearch}

// builtInRuntimeProviders names every built-in runtime whose native tool
// names a proposer allowlist may contain.
var builtInRuntimeProviders = []string{
	runtimePoolProviderCodex, runtimePoolProviderClaude, runtimePoolProviderCopilot, runtimePoolProviderOpencode,
}

// taskImprovementRole returns the improvement role marked on a Task, or ""
// when the Task carries no role annotation. A present but blank value is
// returned as "invalid" so it is never mistaken for an unmarked Task.
func taskImprovementRole(annotations map[string]string) string {
	value, ok := annotations[labels.AnnotationImprovementRole]
	if !ok {
		return ""
	}
	if value = strings.TrimSpace(value); value == "" {
		return "invalid"
	}
	return value
}

// proposerBrokeredToolAllowed reports whether a Task with the given
// improvement role may call a brokered tool. Unmarked Tasks are unaffected;
// any marked role other than proposer may call no brokered tool.
func proposerBrokeredToolAllowed(role, toolName string) bool {
	if role == "" {
		return true
	}
	if role != labels.ImprovementRoleProposer {
		return false
	}
	_, ok := proposerBrokeredTools[toolName]
	return ok
}

// proposerAllowedToolName reports whether a name may appear in a proposer's
// effective tool allowlist: a provider-native workspace tool or an allowed
// brokered tool.
func proposerAllowedToolName(name string) bool {
	if _, ok := proposerBrokeredTools[name]; ok {
		return true
	}
	for _, denied := range proposerDeniedNativeTools {
		if strings.EqualFold(name, denied) {
			return false
		}
	}
	for _, provider := range builtInRuntimeProviders {
		if acp.IsBuiltInRuntimeNativeTool(provider, name) {
			return true
		}
	}
	return false
}
