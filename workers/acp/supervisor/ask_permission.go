package supervisor

import (
	"encoding/json"
	"slices"
	"strings"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const acpToolKindEdit = "edit"

type askPermissionCall struct {
	Name      string                     `json:"name"`
	Kind      string                     `json:"kind"`
	RawInput  map[string]json.RawMessage `json:"rawInput"`
	Locations []json.RawMessage          `json:"locations"`
	Meta      json.RawMessage            `json:"_meta"`
}

func askPermissionToolName(provider string, configuration harnessv2.MCPPolicyConfiguration, toolCall json.RawMessage) string {
	if configuration.NativePermissions != harnessv2.NativePermissionsAsk {
		return ""
	}
	var call askPermissionCall
	if json.Unmarshal(toolCall, &call) != nil || call.Name != "" || len(call.Meta) != 0 {
		return ""
	}
	switch provider {
	case providerKindCopilot:
		return copilotAskPermissionToolName(configuration.ToolPolicy, call)
	case providerKindCodex:
		return codexAskPermissionToolName(configuration.ToolPolicy, call)
	case providerKindOpencode:
		return openCodeAskPermissionToolName(configuration.ToolPolicy, call)
	default:
		return ""
	}
}

func copilotAskPermissionToolName(toolPolicy harnessv2.MCPToolPolicy, call askPermissionCall) string {
	if call.Kind != acpToolKindEdit || !rawInputHasOnly(call.RawInput, "fileName", "diff") ||
		rawInputString(call.RawInput, "fileName") == "" || !rawInputIsString(call.RawInput, "diff") {
		return ""
	}
	policy, err := nativePolicyFor(providerKindCopilot, toolPolicy)
	switch {
	case err != nil:
		return ""
	case policy.grants(providerToolEdit):
		return providerToolEdit
	case policy.grants(providerToolWrite):
		return providerToolWrite
	default:
		return ""
	}
}

func codexAskPermissionToolName(toolPolicy harnessv2.MCPToolPolicy, call askPermissionCall) string {
	policy, err := nativePolicyFor(providerKindCodex, toolPolicy)
	if err != nil || !codexLaunchesAsk(policy) {
		return ""
	}
	switch call.Kind {
	case acpToolKindExecute:
		_, hasCWD := call.RawInput["cwd"]
		if rawInputHasOnly(call.RawInput, "command", "cwd") && strings.TrimSpace(rawInputString(call.RawInput, "command")) != "" &&
			(!hasCWD || rawInputIsString(call.RawInput, "cwd")) {
			return providerToolBash
		}
	case acpToolKindEdit:
		if len(call.RawInput) == 0 && len(call.Locations) > 0 {
			return providerToolEdit
		}
	}
	return ""
}

func openCodeAskPermissionToolName(toolPolicy harnessv2.MCPToolPolicy, call askPermissionCall) string {
	switch call.Kind {
	case acpToolKindExecute:
		if strings.TrimSpace(rawInputString(call.RawInput, "command")) != "" &&
			openCodeToolPolicyAllows(toolPolicy, openCodeToolBash) {
			return openCodeToolBash
		}
	case acpToolKindEdit:
		if rawInputString(call.RawInput, "filepath") == "" || !rawInputIsString(call.RawInput, "diff") ||
			!openCodeMutationPolicyAllows(toolPolicy) {
			return ""
		}
		for _, name := range []string{openCodeToolEdit, openCodeToolWrite, openCodeToolApplyPatch} {
			if openCodeToolPolicyAllows(toolPolicy, name) {
				return name
			}
		}
	}
	return ""
}

func codexLaunchesAsk(policy providerNativePolicy) bool {
	return policy.unrestricted
}

func rawInputHasOnly(input map[string]json.RawMessage, keys ...string) bool {
	for key := range input {
		if !slices.Contains(keys, key) {
			return false
		}
	}
	return true
}

func rawInputIsString(input map[string]json.RawMessage, key string) bool {
	var value *string
	raw, ok := input[key]
	return ok && json.Unmarshal(raw, &value) == nil && value != nil
}

func rawInputString(input map[string]json.RawMessage, key string) string {
	var value *string
	if raw, ok := input[key]; ok && json.Unmarshal(raw, &value) == nil && value != nil {
		return *value
	}
	return ""
}
