package acp

// BuiltInRuntimeAdapterDigests returns the reviewed adapter artifact digests
// advertised by the built-in ACP runtime for the given provider kind
// ("codex", "claude", "copilot", or "opencode"). It is the single source of
// truth shared by controller runtime planning and the runtime supervisor.
// Unknown providers return nil. Every call returns a fresh map.
func BuiltInRuntimeAdapterDigests(provider string) map[string]string {
	schema := "sha256:" + ACPSchemaSHA256
	switch provider {
	case "codex":
		return map[string]string{
			"codex-acp":             "sha256:" + CodexACPTarSHA256,
			"codex-acp-orka-patch":  "sha256:" + CodexACPOrkaPatchSHA256,
			"codex-acp-orka-dist":   "sha256:" + CodexACPOrkaDistSHA256,
			"codex-cli-linux-amd64": "sha256:" + CodexCLILinuxX64SHA256,
			"codex-cli-linux-arm64": "sha256:" + CodexCLILinuxARM64SHA256,
			"acp-schema":            schema,
		}
	case "claude":
		return map[string]string{
			"claude-agent-acp":        "sha256:" + ClaudeACPTarSHA256,
			"claude-code-linux-amd64": "sha256:" + ClaudeSDKLinuxX64SHA256,
			"claude-code-linux-arm64": "sha256:" + ClaudeSDKLinuxARM64SHA256,
			"acp-schema":              schema,
		}
	case "copilot":
		return map[string]string{
			"copilot-cli-linux-amd64": "sha256:" + CopilotCLILinuxX64SHA256,
			"copilot-cli-linux-arm64": "sha256:" + CopilotCLILinuxARM64SHA256,
			"acp-schema":              schema,
		}
	case "opencode":
		return map[string]string{
			"opencode-cli-linux-amd64":     "sha256:" + OpenCodeLinuxX64BinarySHA256,
			"opencode-cli-linux-arm64":     "sha256:" + OpenCodeLinuxARM64BinarySHA256,
			"opencode-ripgrep-linux-amd64": "sha256:" + OpenCodeRipgrepLinuxX64BinarySHA256,
			"opencode-ripgrep-linux-arm64": "sha256:" + OpenCodeRipgrepLinuxARM64BinarySHA256,
			"acp-schema":                   schema,
		}
	default:
		return nil
	}
}
