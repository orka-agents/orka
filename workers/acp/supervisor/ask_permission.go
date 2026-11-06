package supervisor

import (
	"encoding/json"
	"slices"
	"strings"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const (
	acpToolKindExecute = "execute"
	acpToolKindEdit    = "edit"
)

// askPermissionCall is the structured part of an ACP permission tool call.
type askPermissionCall struct {
	Name      string                     `json:"name"`
	Kind      string                     `json:"kind"`
	RawInput  map[string]json.RawMessage `json:"rawInput"`
	Locations []json.RawMessage          `json:"locations"`
	Meta      json.RawMessage            `json:"_meta"`
}

// askPermissionToolName names the Orka native tool behind a permission
// request that carries no structured tool name, in a session launched to ask
// (harnessv2.NativePermissionsAsk). It reads only structured ACP fields, the
// tool call kind and the shape of its raw input, never a title. A kind can
// identify a tool only because the ask launch of that provider leaves exactly
// one policy-granted tool group able to produce it; see each provider's
// session projection. Every other request stays unnamed, so the frozen policy
// refuses it. Pre-approved sessions never reach this classification.
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

// copilotAskPermissionToolName names Copilot file writes. Its shell requests
// already carry a recognized command envelope (see mapPermission). The CLI
// asks for its create and edit tools under one permission, and the launch
// keeps edit only when the policy grants Edit and create only when it grants
// Write: a write request comes from Edit when that is granted, and otherwise
// from Write.
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

// codexAskPermissionToolName names Codex command and file change requests.
func codexAskPermissionToolName(toolPolicy harnessv2.MCPToolPolicy, call askPermissionCall) string {
	policy, err := nativePolicyFor(providerKindCodex, toolPolicy)
	if err != nil || !codexLaunchesAsk(policy) {
		return ""
	}
	switch call.Kind {
	case acpToolKindExecute:
		// A command request carries the command and its directory; network
		// and additional-permission requests add other keys. Brokered MCP
		// calls carry no raw input and are named by their tool call.
		_, hasCWD := call.RawInput["cwd"]
		if rawInputHasOnly(call.RawInput, "command", "cwd") && strings.TrimSpace(rawInputString(call.RawInput, "command")) != "" &&
			(!hasCWD || rawInputIsString(call.RawInput, "cwd")) {
			return providerToolBash
		}
	case acpToolKindEdit:
		// A file change request names only the files it touches.
		if len(call.RawInput) == 0 && len(call.Locations) > 0 {
			return providerToolEdit
		}
	}
	return ""
}

// openCodeAskPermissionToolName names OpenCode requests. OpenCode reports
// only bash as execute, and only its write, edit and apply_patch tools, which
// share the edit permission, as edit.
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
		// The launch opens the shared permission only when the group is
		// granted, so any granted member names the request's authority.
		for _, name := range []string{openCodeToolEdit, openCodeToolWrite, openCodeToolApplyPatch} {
			if openCodeToolPolicyAllows(toolPolicy, name) {
				return name
			}
		}
	}
	return ""
}

// codexLaunchesAsk reports whether an ask session launches Codex in the
// asking agent mode. Codex sessions run either an unrestricted policy or the
// exact read-only one, whose policy grants no acting tool; that one launches
// exactly as in the pre-approved mode.
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
