package acp

import (
	"slices"
	"sort"
	"strings"
)

const openCodeToolRead = "read"

var openCodeDefaultAllowedTools = [...]string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}

var builtInRuntimeNativeTools = map[string][]string{
	"codex":    {"Read", "Write", "Edit", "Bash", "Glob", "Grep", "WebSearch", "WebFetch"},
	"claude":   {"Read", "Write", "Edit", "Bash", "Glob", "Grep", "WebSearch", "WebFetch"},
	"copilot":  {"Read", "Write", "Edit", "Bash", "Glob", "Grep", "WebSearch", "WebFetch"},
	"opencode": {"Read", "Write", "Edit", "apply_patch", "Bash", "Glob", "Grep"},
}

// BuiltInRuntimeNativeToolNames returns the provider-native tool names owned by
// one built-in ACP runtime. Callers receive a copy so policy tables remain
// immutable.
func BuiltInRuntimeNativeToolNames(provider string) []string {
	return append([]string(nil), builtInRuntimeNativeTools[strings.ToLower(strings.TrimSpace(provider))]...)
}

// IsBuiltInRuntimeNativeTool reports whether name is provider-native for the
// selected built-in runtime. Brokered and custom tool names return false.
func IsBuiltInRuntimeNativeTool(provider, name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for _, native := range builtInRuntimeNativeTools[strings.ToLower(strings.TrimSpace(provider))] {
		if strings.EqualFold(native, name) {
			return true
		}
	}
	return false
}

// NormalizeBuiltInRuntimeToolPolicy expands an implicit provider-native tool
// surface only when deny-only metadata or the Bash gate narrows it. Explicit
// allowlists, including an explicit empty deny-all list, remain authoritative.
func NormalizeBuiltInRuntimeToolPolicy(
	provider string,
	allowed, disallowed []string,
	allowBash bool,
) ([]string, []string, bool) {
	if allowed != nil || (len(disallowed) == 0 && allowBash) {
		return allowed, disallowed, allowBash
	}
	native := BuiltInRuntimeNativeToolNames(provider)
	if len(native) == 0 {
		return allowed, disallowed, allowBash
	}
	denied := make(map[string]struct{}, len(disallowed)+1)
	for _, name := range disallowed {
		if name = strings.ToLower(strings.TrimSpace(name)); name != "" {
			denied[name] = struct{}{}
		}
	}
	if !allowBash {
		denied["bash"] = struct{}{}
	}
	normalizedAllowed := make([]string, 0, len(native))
	for _, name := range native {
		if _, blocked := denied[strings.ToLower(name)]; blocked {
			continue
		}
		normalizedAllowed = append(normalizedAllowed, name)
	}
	sort.Strings(normalizedAllowed)
	return normalizedAllowed, disallowed, allowBash
}

// BuiltInRuntimeEffectiveAllowedTools returns the tools that a normalized
// non-OpenCode policy can execute after deny metadata and the Bash gate apply.
// A nil allowlist remains nil so callers can preserve the unrestricted sentinel.
func BuiltInRuntimeEffectiveAllowedTools(allowed, disallowed []string, allowBash bool) []string {
	if allowed == nil {
		return nil
	}
	result := make([]string, 0, len(allowed))
	for _, name := range allowed {
		name = strings.TrimSpace(name)
		if name == "" || (!allowBash && strings.EqualFold(builtInRuntimeToolBaseName(name), "bash")) {
			continue
		}
		blocked := false
		for _, denied := range disallowed {
			denied = strings.TrimSpace(denied)
			if denied == name || (strings.EqualFold(denied, "bash") && strings.EqualFold(builtInRuntimeToolBaseName(name), "bash")) {
				blocked = true
				break
			}
		}
		if !blocked {
			result = append(result, name)
		}
	}
	return sortedUniqueToolNames(result)
}

// BuiltInRuntimeEffectiveAllowBash projects the effective Bash capability into
// the separate gate used by authorization subset checks.
func BuiltInRuntimeEffectiveAllowBash(allowed, disallowed []string, allowBash bool) bool {
	if !allowBash {
		return false
	}
	for _, denied := range disallowed {
		if strings.EqualFold(strings.TrimSpace(denied), "bash") {
			return false
		}
	}
	if allowed == nil {
		return true
	}
	return slices.ContainsFunc(allowed, func(name string) bool {
		return strings.EqualFold(builtInRuntimeToolBaseName(name), "bash")
	})
}

func builtInRuntimeToolBaseName(value string) string {
	value = strings.TrimSpace(value)
	if index := strings.IndexByte(value, '('); index >= 0 {
		value = strings.TrimSpace(value[:index])
	}
	return value
}

// OpenCodeDefaultAllowedTools returns the governed provider-native tool defaults
// used when an OpenCode Agent omits runtime.defaultAllowedTools.
func OpenCodeDefaultAllowedTools() []string {
	return append([]string(nil), openCodeDefaultAllowedTools[:]...)
}

// NormalizeOpenCodeToolPolicy canonicalizes OpenCode-native tool names and
// applies the fail-closed read-intent restrictions used by the runtime.
func NormalizeOpenCodeToolPolicy(
	readIntent bool,
	allowed, disallowed []string,
	allowBash bool,
) ([]string, []string, bool) {
	allowed = normalizeOpenCodeToolNames(allowed)
	disallowed = normalizeOpenCodeToolNames(disallowed)
	if !readIntent {
		return allowed, disallowed, allowBash
	}
	// OpenCode's Grep permission cannot carry the path-specific secret-file
	// exclusions applied to Read, so read-intent sessions disable it entirely.
	blocked := map[string]struct{}{
		"apply_patch": {}, "bash": {}, "edit": {}, "grep": {}, "write": {},
	}
	filtered := allowed[:0]
	for _, name := range allowed {
		if _, denied := blocked[name]; !denied {
			filtered = append(filtered, name)
		}
	}
	disallowed = append(disallowed, "apply_patch", "bash", "edit", "grep", "write")
	return sortedUniqueToolNames(filtered), sortedUniqueToolNames(disallowed), false
}

// OpenCodeEffectiveAllowedTools returns the tools that the normalized policy
// can actually execute after disallowed names and the Bash gate are applied.
func OpenCodeEffectiveAllowedTools(allowed, disallowed []string, allowBash bool) []string {
	denied := make(map[string]struct{}, len(disallowed))
	for _, name := range disallowed {
		denied[name] = struct{}{}
	}
	result := make([]string, 0, len(allowed))
	for _, name := range allowed {
		if _, blocked := denied[name]; blocked || (name == "bash" && !allowBash) {
			continue
		}
		result = append(result, name)
	}
	return sortedUniqueToolNames(result)
}

// NormalizeOpenCodeAuthorizationTools canonicalizes user-facing OpenCode tool
// grants so token and transaction allowlists can be compared with runtime
// policy names without exposing internal alias details to issuers.
func NormalizeOpenCodeAuthorizationTools(values []string) []string {
	allowed, _, _ := NormalizeOpenCodeToolPolicy(false, values, nil, true)
	return allowed
}

// normalizeOpenCodeToolNames canonicalizes OpenCode-native permissions while
// preserving brokered/custom tool identifiers. OpenCode selects apply_patch for
// GPT-family models and edit/write for others, so those aliases are one governed
// mutation capability.
func normalizeOpenCodeToolNames(values []string) []string {
	mutation := false
	result := make([]string, 0, len(values)+2)
	for _, value := range values {
		trimmed := strings.TrimSpace(value)
		switch normalized := strings.ToLower(trimmed); normalized {
		case "apply_patch", "edit", "write":
			mutation = true
		case "bash", "glob", "grep", openCodeToolRead:
			result = append(result, normalized)
		default:
			result = append(result, trimmed)
		}
	}
	if mutation {
		result = append(result, "apply_patch", "edit", "write")
	}
	return sortedUniqueToolNames(result)
}

func sortedUniqueToolNames(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}
