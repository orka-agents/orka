//go:build e2e

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/test/utils"
)

func TestExternalCodexReadTaskUsesClassBackedExecution(t *testing.T) {
	manifest := externalCodexReadTaskManifest("read-task", "codex-agent", "external-class")
	workspace := manifestNestedMap(t, manifest, "spec", "workspace")
	if workspace["intent"] != "read" || workspace["gitRepo"] != liveRuntimeRepoURL ||
		workspace["ref"] != liveRuntimeRepoRef {
		t.Fatalf("repository workspace is not the pinned read workspace: %#v", workspace)
	}
	executionWorkspace := manifestNestedMap(t, manifest, "spec", "execution", "workspace")
	class := manifestNestedMap(t, manifest, "spec", "execution", "workspace", "classRef")
	if class["name"] != "external-class" || executionWorkspace["reusePolicy"] != "none" ||
		executionWorkspace["onDetach"] != "Delete" {
		t.Fatalf("execution workspace is not class-backed and ephemeral: %#v", executionWorkspace)
	}
	if _, present := manifestNestedMap(t, manifest, "spec", "agentRuntime")["allowBash"]; present {
		t.Fatal("Codex must not receive the legacy Bash policy")
	}
}

func TestExternalCodexReadFixtureRequiresRealToolResult(t *testing.T) {
	projectDir, err := utils.GetProjectDir()
	if err != nil {
		t.Fatal(err)
	}
	// E2E command helpers change the working directory to the project root.
	t.Chdir(projectDir)
	data, err := os.ReadFile(filepath.Join(projectDir, mockLLMFixtureDir, "agent_runtime_matrix.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Fixtures []json.RawMessage `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	var matched []map[string]any
	for _, raw := range file.Fixtures {
		var fixture map[string]any
		if err := json.Unmarshal(raw, &fixture); err != nil {
			t.Fatal(err)
		}
		match := fixture["match"].(map[string]any)
		if match["userMessage"] == externalCodexReadMarker {
			matched = append(matched, fixture)
		}
	}
	if len(matched) != 2 {
		t.Fatalf("class-backed smoke has %d fixtures, want exactly two", len(matched))
	}
	terminal := matched[0]["match"].(map[string]any)
	if terminal["toolResultContains"] != liveRuntimeRepoSentinel {
		t.Fatalf("terminal fixture does not require the README contents: %#v", terminal)
	}
	initial := matched[1]["match"].(map[string]any)
	if initial["hasToolResult"] != false {
		t.Fatal("initial fixture must match only before a tool result exists")
	}
	response := matched[1]["response"].(map[string]any)
	tools := response["toolCalls"].([]any)
	if len(tools) != 1 {
		t.Fatalf("expected one README read tool, got %#v", tools)
	}
	tool := tools[0].(map[string]any)
	if tool["name"] != "exec_command" || tool["arguments"].(map[string]any)["cmd"] != "cat README" {
		t.Fatalf("fixture does not execute the real Codex README read: %#v", tool)
	}
}

func TestExternalCodexReadJournalRequiresSuccessfulReadFixture(t *testing.T) {
	read := `{"match":{"userMessage":"` + externalCodexReadMarker + `","toolResultContains":"Hello World!"}}`
	initial := `{"match":{"userMessage":"` + externalCodexReadMarker + `","hasToolResult":false}}`
	other := `{"match":{"userMessage":"[e2e:runtime-codex-read]","toolResultContains":"Hello World!"}}`
	for _, test := range []struct {
		name, fixture string
		status        int
		want          bool
	}{
		{"completed read", read, 200, true},
		{"failed model response", read, 500, false},
		{"initial tool call only", initial, 200, false},
		{"another smoke", other, 200, false},
		{"catch-all", `{"match":{}}`, 200, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			request := mockLLMRequest{}
			request.Response.Status = test.status
			request.Response.Fixture = json.RawMessage(test.fixture)
			if got := mockLLMServedExternalCodexRead([]mockLLMRequest{request}); got != test.want {
				t.Fatalf("served read = %t, want %t", got, test.want)
			}
		})
	}
}
