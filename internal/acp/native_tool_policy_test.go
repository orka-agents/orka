package acp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativeToolPolicyDoesNotBroadenLegacyDefaults(t *testing.T) {
	require.Equal(t, []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}, OpenCodeDefaultAllowedTools())
	require.NotContains(t, BuiltInRuntimeNativeToolNames("opencode"), "websearch")
	require.Contains(t, BuiltInRuntimePolicyToolNames("opencode"), "websearch")
	// An omitted mode retains the original provider-specific interpretation.
	require.NoError(t, ValidateNativeToolPolicy("", "opencode", true, []string{"Read", "Bash"}, nil, true))
	require.Error(t, ValidateNativeToolPolicy("restricted", "opencode", true, []string{"Read", "Bash"}, nil, true))
}

func TestFullNativeToolPermissionKeepsNativeAndBrokeredNamesSeparate(t *testing.T) {
	for _, name := range []string{"Read", "NotebookEdit", "TaskCreate", "TaskGet", "TaskUpdate", "TaskList", "TodoWrite", "ReportFindings"} {
		require.True(t, FullNativeToolPermissionAllowed("claude", name), name)
	}
	for _, provider := range []string{"claude", "codex", "copilot", "opencode"} {
		for _, name := range []string{"", " Bash ", "DeleteEverything", "delegate_task", "mcp__orka__delegate_task", "mcp__other__Read", "orca_Read",
			"Agent", "Task", "TaskStop", "CronCreate", "ScheduleWakeup", "RemoteTrigger", "AskUserQuestion", "RequestPermissions", "Skill", "external_directory"} {
			require.False(t, FullNativeToolPermissionAllowed(provider, name), provider+"/"+name)
		}
	}
}

func TestExplicitNativeToolPolicyFailsClosed(t *testing.T) {
	require.ErrorContains(t, ValidateNativeToolPolicy("full", "opencode", true, nil, nil, true), "workspace.intent: read")
	require.ErrorContains(t, ValidateNativeToolPolicy("full", "opencode", false, nil, nil, false), "allowBash: false")
	require.ErrorContains(t, ValidateNativeToolPolicy("full", "claude", false, nil, []string{"NotebookEdit"}, true), "native tool restriction")
	require.ErrorContains(t, ValidateNativeToolPolicy("full", "unknown", false, nil, nil, true), "unsupported")
	require.ErrorContains(t, ValidateNativeToolPolicy("arbitrary", "claude", false, nil, nil, true), "unsupported")
	require.ErrorContains(t, ValidateNativeToolPolicy("restricted", "codex", true, []string{"Read", "Glob", "Grep"}, nil, false), "unsupported")
	require.ErrorContains(t, ValidateNativeToolPolicy("restricted", "claude", false, nil, nil, true), "explicit")
	require.NoError(t, ValidateNativeToolPolicy("restricted", "opencode", true, []string{}, nil, false))
	require.NoError(t, ValidateNativeToolPolicy("full", "opencode", false, []string{"delegate_task", "cancel_task"}, []string{"cancel_task"}, true))
	// Full catalogs include names outside the shared list. A Task cannot
	// silently turn their native denials into ineffective broker denials.
	for _, provider := range []string{"claude", "codex", "copilot", "opencode"} {
		require.ErrorContains(t, ValidateNativeToolPolicy("full", provider, false, nil, []string{"unknown_native_tool"}, true), "cannot honor disallowed tool")
	}
}

func TestExplicitNativeToolNamesNormalizeWithoutChangingBrokerGrants(t *testing.T) {
	for _, provider := range []string{"claude", "copilot", "opencode"} {
		t.Run(provider, func(t *testing.T) {
			require.Nil(t, NormalizeExplicitNativeToolNames(provider, nil))
			require.Equal(t, []string{}, NormalizeExplicitNativeToolNames(provider, []string{}))
			native := "Read"
			if provider == "opencode" {
				native = "read"
			}
			names := NormalizeExplicitNativeToolNames(provider, []string{"READ", "read", "Read", "BrokerTool", "brokerTool"})
			require.Len(t, names, 3)
			require.Contains(t, names, native)
			require.Contains(t, names, "BrokerTool")
			require.Contains(t, names, "brokerTool")
			require.NoError(t, ValidateExplicitNativeToolNames(provider, names))
			require.ErrorContains(t, ValidateExplicitNativeToolNames(provider, []string{"READ"}), "canonical name")
		})
	}
}
