package supervisor

import (
	"fmt"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// hyperlightOnlyCommandPolicy scopes provider lockdown to the validated frozen
// policy that grants runtime-local sandbox_exec and forbids native Bash. Merely
// enabling Hyperlight in the runtime, or denying Bash in an ordinary read-only
// session, must not change provider configuration.
func hyperlightOnlyCommandPolicy(request harnessv2.CreateRuntimeSessionRequest) bool {
	policy := request.MCPConfiguration.ToolPolicy
	return !policy.AllowBash && sandboxExecGranted(policy)
}

// sandboxExecGranted reports whether the frozen policy grants runtime-local
// sandbox_exec. Its result comes only when the call ends, so such a session's
// provider must wait acp.SandboxExecClientTimeout for an MCP tool call.
func sandboxExecGranted(policy harnessv2.MCPToolPolicy) bool {
	for _, descriptor := range policy.Tools {
		if descriptor.Name == acp.SandboxExecToolName && descriptor.Source == harnessv2.MCPToolSourceRuntimeLocal {
			return true
		}
	}
	return false
}

// claudeHyperlightCommandOptions is an overlay for _meta.claudeCode.options,
// not for _meta.claudeCode. In the pinned ACP
// adapter (verified on 0.61.0, re-read on 0.89.1) these fields survive its
// userProvidedOptions spread, and provider routing merges into, rather than
// replaces, settings; the adapter still supplies its own canUseTool and SDK
// hook callbacks afterward. The SDK loads settings in the highest-priority
// user-controlled flag layer. Prewarmed claim() overlays refuse hook keys, but
// the adapter starts every session with query().
// disableAllHooks is a Settings field: it suppresses filesystem command hooks
// and statusLine, while trusted SDK hook callbacks remain active.
//
// Keep settingSources unchanged: clearing it would also stop loading CLAUDE.md.
// strictMcpConfig limits servers to the adapter-supplied mcpServers and ignores
// project, user, plugin, and on-disk agent MCP definitions.
func claudeHyperlightCommandOptions(request harnessv2.CreateRuntimeSessionRequest) map[string]any {
	if !hyperlightOnlyCommandPolicy(request) {
		return nil
	}
	return map[string]any{
		"strictMcpConfig": true,
		"settings": map[string]any{
			"disableAllHooks": true,
		},
	}
}

// validateCopilotHyperlightCommandPolicy fails closed only for strict sessions.
// Copilot (probed on 1.0.77) exposes disableAllHooks in its filesystem configuration, but
// has no verified immutable hook or strict-MCP control in this integration.
// --disable-builtin-mcps does not exclude configured MCP servers: an offline
// ACP probe started a planted COPILOT_HOME/mcp-config.json command even with
// native Bash excluded and session/new.mcpServers empty. Do not substitute
// guessed CLI flags or rely on a clean, session-writable configuration file.
func validateCopilotHyperlightCommandPolicy(request harnessv2.CreateRuntimeSessionRequest) error {
	if !hyperlightOnlyCommandPolicy(request) {
		return nil
	}
	return fmt.Errorf("copilot ACP runtime %s cannot enforce Hyperlight-only commands: immutable hook and unmanaged MCP controls are not verified", acp.CopilotCLIVersion)
}
