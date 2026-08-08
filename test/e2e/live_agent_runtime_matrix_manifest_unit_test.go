//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestLiveRuntimeAgentManifestsPinHarnessV2Contract(t *testing.T) {
	t.Parallel()

	for _, runtimeType := range []string{"codex", "claude", "copilot", "opencode"} {
		runtimeType := runtimeType
		t.Run(runtimeType, func(t *testing.T) {
			t.Parallel()
			agent := runtimeAgentManifest(runtimeType+"-agent", runtimeType, "test-model", 5, nil)
			runtime := manifestNestedMap(t, agent, "spec", "runtime")
			if got := runtime["contractVersion"]; got != corev1alpha1.AgentRuntimeContractHarnessV2 {
				t.Fatalf("contractVersion = %#v, want %q", got, corev1alpha1.AgentRuntimeContractHarnessV2)
			}
		})
	}
}

func TestLiveRuntimeManifestsOmitCodexBashPolicy(t *testing.T) {
	t.Parallel()

	codexAgent := runtimeAgentManifest("codex-agent", "codex", "gpt-test", 5, nil)
	codexAgentRuntime := manifestNestedMap(t, codexAgent, "spec", "runtime")
	if _, present := codexAgentRuntime["defaultAllowBash"]; present {
		t.Fatal("Codex Agent manifest must omit spec.runtime.defaultAllowBash")
	}

	codexTask := runtimeAgentTaskManifest(
		"codex-task",
		"codex-agent",
		"read the repository",
		4,
		nil,
		nil,
		"",
		nil,
		nil,
	)
	codexTaskRuntime := manifestNestedMap(t, codexTask, "spec", "agentRuntime")
	if _, present := codexTaskRuntime["allowBash"]; present {
		t.Fatal("Codex Task manifest must omit spec.agentRuntime.allowBash")
	}

	claudeAgent := runtimeAgentManifest("claude-agent", "claude", "claude-test", 5, boolPtr(false))
	claudeAgentRuntime := manifestNestedMap(t, claudeAgent, "spec", "runtime")
	if got, present := claudeAgentRuntime["defaultAllowBash"]; !present || got != false {
		t.Fatalf("Claude Agent defaultAllowBash = %#v, present = %t; want false and present", got, present)
	}

	claudeTask := runtimeAgentTaskManifest(
		"claude-task",
		"claude-agent",
		"reply exactly",
		3,
		boolPtr(false),
		nil,
		"",
		nil,
		nil,
	)
	claudeTaskRuntime := manifestNestedMap(t, claudeTask, "spec", "agentRuntime")
	if got, present := claudeTaskRuntime["allowBash"]; !present || got != false {
		t.Fatalf("Claude Task allowBash = %#v, present = %t; want false and present", got, present)
	}
}

func manifestNestedMap(t *testing.T, manifest map[string]any, keys ...string) map[string]any {
	t.Helper()

	current := manifest
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("manifest field %q = %#v, want map[string]any", key, current[key])
		}
		current = next
	}
	return current
}
