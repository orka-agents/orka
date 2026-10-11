//go:build e2e
// +build e2e

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package e2e

import (
	"encoding/json"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestLiveRuntimeAgentManifestsPinHarnessV2Contract(t *testing.T) {
	t.Parallel()

	for _, runtimeType := range []string{"codex", "claude", "copilot", "opencode"} {
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

	claudeAgent := runtimeAgentManifest("claude-agent", "claude", "claude-test", 5, new(false))
	claudeAgentRuntime := manifestNestedMap(t, claudeAgent, "spec", "runtime")
	if got, present := claudeAgentRuntime["defaultAllowBash"]; !present || got != false {
		t.Fatalf("Claude Agent defaultAllowBash = %#v, present = %t; want false and present", got, present)
	}

	claudeTask := runtimeAgentTaskManifest(
		"claude-task",
		"claude-agent",
		"reply exactly",
		3,
		new(false),
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

func TestLiveCodexReadPrompt(t *testing.T) {
	t.Run("cloud prompt is unchanged", func(t *testing.T) {
		t.Setenv("E2E_LOCAL_MODEL", "")
		want := "Use a tool to read README in the repository root. " +
			"Wait for the read to succeed, then reply with the exact file contents and nothing else."
		if got := liveCodexReadPrompt(); got != want {
			t.Fatalf("cloud prompt = %q, want %q", got, want)
		}
	})
	t.Run("local prompt requires the real read, not a canned answer", func(t *testing.T) {
		t.Setenv("E2E_LOCAL_MODEL", "qwen-3.5-2b")
		prompt := liveCodexReadPrompt()
		for _, required := range []string{"README, NOT README.md", "Use exec_command", "Wait for the command to succeed", "only its stdout text"} {
			if !strings.Contains(prompt, required) {
				t.Fatalf("local prompt omits %q", required)
			}
		}
		if strings.Contains(prompt, liveRuntimeRepoSentinel) {
			t.Fatal("local prompt must not contain the expected file contents")
		}
		var arguments map[string]any
		start, end := strings.Index(prompt, "{"), strings.Index(prompt, "}")
		if start < 0 || end < start {
			t.Fatal("local prompt lacks tool arguments")
		}
		if err := json.Unmarshal([]byte(prompt[start:end+1]), &arguments); err != nil {
			t.Fatal(err)
		}
		if len(arguments) != 3 || arguments["cmd"] != "cat README" ||
			arguments["yield_time_ms"] != float64(1000) || arguments["max_output_tokens"] != float64(2048) {
			t.Fatalf("local read arguments = %#v", arguments)
		}
	})
}
