//nolint:goconst // Runner and tool names are literal protocol vocabulary, shared with the legacy policy tables.
package acp

import (
	"fmt"
	"slices"
	"strings"
)

// BuiltInRuntimePolicyToolNames includes names the explicit policy projections
// understand. It is not the full-mode catalog or the legacy default grant.
func BuiltInRuntimePolicyToolNames(provider string) []string {
	names := BuiltInRuntimeNativeToolNames(provider)
	if provider == "opencode" {
		names = append(names, "webfetch", "websearch", "todowrite")
	}
	return names
}

func isPolicyNativeTool(provider, name string) bool {
	return slices.ContainsFunc(BuiltInRuntimePolicyToolNames(provider), func(native string) bool {
		return strings.EqualFold(name, native)
	})
}

// NormalizeExplicitNativeToolNames gives named native grants and denials the
// same spelling before hashing and exact matching. Brokered identifiers remain
// case-sensitive, and nil remains distinct from an explicit empty allowlist.
func NormalizeExplicitNativeToolNames(provider string, names []string) []string {
	if names == nil {
		return nil
	}
	if provider == "opencode" {
		return normalizeOpenCodeToolNames(names)
	}
	result := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		for _, native := range BuiltInRuntimePolicyToolNames(provider) {
			if strings.EqualFold(name, native) {
				name = native
				break
			}
		}
		result = append(result, name)
	}
	return sortedUniqueToolNames(result)
}

// ValidateExplicitNativeToolNames rejects native aliases in an already frozen
// policy. A supervisor must not reinterpret an exact, signed denylist.
func ValidateExplicitNativeToolNames(provider string, names []string) error {
	for _, name := range names {
		for _, native := range BuiltInRuntimePolicyToolNames(provider) {
			if provider == "opencode" {
				native = strings.ToLower(native)
			}
			if strings.EqualFold(name, native) && name != native {
				return fmt.Errorf("native tool %q must use canonical name %q in the frozen policy", name, native)
			}
		}
	}
	return nil
}

// ValidateNativeToolPolicy rejects explicit policy combinations whose stated
// restrictions the runner cannot enforce. The omitted mode retains v2 behavior.
func ValidateNativeToolPolicy(mode, provider string, readIntent bool, allowed, disallowed []string, allowBash bool) error {
	if mode == "" {
		return nil
	}
	if len(BuiltInRuntimeNativeToolNames(provider)) == 0 {
		return fmt.Errorf("native tool policy is unsupported for runner %q", provider)
	}
	switch mode {
	case "full":
		if readIntent {
			return fmt.Errorf("full built-in tools are incompatible with workspace.intent: read; select restricted tools or an explicitly writable workspace")
		}
		if !allowBash {
			return fmt.Errorf("full built-in tools are incompatible with allowBash: false; select restricted tools")
		}
		for _, names := range [][]string{allowed, disallowed} {
			for _, name := range names {
				if isPolicyNativeTool(provider, builtInRuntimeToolBaseName(name)) || (provider == "claude" && claudeNativeToolIdentifier(builtInRuntimeToolBaseName(name))) {
					return fmt.Errorf("full built-in tools cannot honor native tool restriction %q; omit native tool lists or select restricted tools", name)
				}
			}
		}
		for _, name := range disallowed {
			if !slices.Contains(allowed, name) {
				return fmt.Errorf("full built-in tools can only deny explicitly granted Orka tools; cannot honor disallowed tool %q", name)
			}
		}
		return nil
	case "restricted":
		if provider == "codex" {
			return fmt.Errorf("codex restricted tools are unsupported: the pinned ACP runner cannot exactly remove native tools; the legacy read-only preset is not a complete tool restriction")
		}
		if allowed == nil {
			return fmt.Errorf("restricted tools require an explicit defaultAllowedTools or Task allowedTools list; [] denies all tools")
		}
	default:
		return fmt.Errorf("unsupported native tool policy %q", mode)
	}
	allows := func(name string) bool {
		if strings.EqualFold(name, "bash") && !allowBash {
			return false
		}
		return slices.ContainsFunc(allowed, func(item string) bool { return strings.EqualFold(item, name) }) &&
			!slices.ContainsFunc(disallowed, func(item string) bool { return strings.EqualFold(item, name) })
	}
	if readIntent {
		for _, name := range []string{"Bash", "Write", "Edit", "apply_patch"} {
			if allows(name) {
				return fmt.Errorf("restricted tool %q can modify a read-only workspace; remove it from allowedTools", name)
			}
		}
		if provider == "opencode" && allows("grep") {
			return fmt.Errorf("opencode read-only policy cannot apply protected-file exclusions to Grep; use Read and Glob")
		}
	}
	// A shell or interpreter can read, write, and make HTTP requests without
	// calling the named native tools. No runner-specific allowlist prevents it.
	if allows("Bash") {
		for _, name := range []string{"Read", "Write", "Edit", "Glob", "Grep", "WebFetch", "WebSearch"} {
			if !allows(name) {
				return fmt.Errorf("restricted Bash can bypass the denial of %s; disable Bash or select full tools with appropriate workspace and network boundaries", name)
			}
		}
	}
	return nil
}

// FullNativeToolPermissionAllowed applies only to structured runner-native
// identities. MCP calls always require their own exact descriptor and grant.
// Unknown permission requests remain denied, even in full mode: runner catalog
// defaults need no permission round-trip, while elevations are never implicit.
func FullNativeToolPermissionAllowed(provider, name string) bool {
	if strings.TrimSpace(name) != name || name == "" {
		return false
	}
	if provider == "claude" {
		// Claude Code 2.1.217 / SDK 0.3.217 native tools beyond the shared
		// policy names. New names need review when the pinned runner changes.
		switch name {
		case "NotebookEdit", "TodoWrite", "TaskCreate", "TaskGet", "TaskUpdate", "TaskList", "ReportFindings":
			return true
		}
	}
	return isPolicyNativeTool(provider, name)
}

// claudeNativeToolIdentifier conservatively recognizes native-shaped restrictions.
// It must not authorize permission requests, which require an approved tool name.
func claudeNativeToolIdentifier(name string) bool {
	if len(name) == 0 || len(name) > 253 || name[0] < 'A' || name[0] > 'Z' {
		return false
	}
	for _, char := range name[1:] {
		if (char < 'A' || char > 'Z') && (char < 'a' || char > 'z') && (char < '0' || char > '9') {
			return false
		}
	}
	return true
}
