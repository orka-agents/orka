package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorkspaceCanaryExecutesWriteResumeAndRestore(t *testing.T) {
	for _, tool := range []string{"exec_command", "shell_command"} {
		t.Run(tool, func(t *testing.T) {
			source, restored := t.TempDir(), t.TempDir()
			var saved []byte
			for _, marker := range []string{workspaceWriteMarker, workspaceResumeMarker, workspaceRestoreMarker} {
				resetWorkspaceCanary(t, marker)
				input := make([]any, 1, 3)
				input[0] = map[string]any{"role": "user", "content": "Reply exactly: " + marker}
				stream := tool == "exec_command"
				first := requestWorkspaceCanary(t, input, tool, stream)
				call := workspaceCanaryResponseItem(t, first)
				if call["type"] != functionCallType || call["name"] != tool {
					t.Fatalf("first response must request the advertised shell tool: %v", call)
				}
				assertWorkspaceCanaryVerified(t, marker, false)
				dir := source
				if marker == workspaceRestoreMarker {
					// The fixture's read command must inspect its current workspace,
					// even when another copy contains newer data.
					dir = restored
					if err := os.WriteFile(filepath.Join(dir, workspaceCanaryFile), saved, 0o600); err != nil {
						t.Fatal(err)
					}
				}
				output := executeWorkspaceCanary(t, call, dir)
				result := map[string]any{"type": functionCallOutputType, "call_id": call["call_id"], "output": output}
				if stream {
					result["output"] = []any{map[string]any{"type": "input_text", "text": output}}
				}
				final := requestWorkspaceCanary(t, append(input, call, result), tool, stream)
				item := workspaceCanaryResponseItem(t, final)
				if item["type"] != "message" || strings.Join(appendTextContent(nil, item["content"]), "") != marker {
					t.Fatalf("successful shell output did not produce the final marker: %v", item)
				}
				assertWorkspaceCanaryVerified(t, marker, true)
				contents, err := os.ReadFile(filepath.Join(dir, workspaceCanaryFile))
				if err != nil {
					t.Fatal(err)
				}
				switch marker {
				case workspaceWriteMarker:
					saved = contents
				case workspaceResumeMarker:
					if string(contents) != workspaceCanaryChange+"\n" {
						t.Fatalf("continuation did not change the source file: %q", contents)
					}
				case workspaceRestoreMarker:
					if !bytes.Equal(contents, saved) {
						t.Fatal("restore command changed the restored file")
					}
				}
			}
		})
	}
}

func TestWorkspaceCanaryRejectsMissingChangedOrFailedReads(t *testing.T) {
	for _, scenario := range []string{"missing file", "changed file", "failed command"} {
		t.Run(scenario, func(t *testing.T) {
			resetWorkspaceCanary(t, workspaceRestoreMarker)
			input := make([]any, 1, 3)
			input[0] = map[string]any{"role": "user", "content": "Reply exactly: " + workspaceRestoreMarker}
			call := workspaceCanaryResponseItem(t, requestWorkspaceCanary(t, input, "exec_command", true))
			dir := t.TempDir()
			if scenario == "changed file" {
				path := filepath.Join(dir, workspaceCanaryFile)
				if err := os.WriteFile(path, []byte(workspaceCanaryChange+"\n"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			output := executeWorkspaceCanary(t, call, dir)
			if scenario == "failed command" {
				output = "Process exited with code 1\nOutput:\n" + workspaceCanaryData + "\n"
			}
			result := map[string]any{"type": functionCallOutputType, "call_id": call["call_id"], "output": output}
			response := requestWorkspaceCanary(t, append(input, call, result), "exec_command", true)
			if response.Code != http.StatusUnprocessableEntity {
				t.Fatalf("invalid read status = %d, want 422", response.Code)
			}
			assertWorkspaceCanaryVerified(t, workspaceRestoreMarker, false)
		})
	}
}

func TestWorkspaceCanaryRejectsUnissuedResultsAndIgnoresHistory(t *testing.T) {
	for _, scenario := range []string{"unissued", "different call", "previous turn", "user echo", "assistant echo"} {
		t.Run(scenario, func(t *testing.T) {
			resetWorkspaceCanary(t, workspaceRestoreMarker)
			user := map[string]any{"role": "user", "content": "Reply exactly: " + workspaceRestoreMarker}
			input := make([]any, 1, 2)
			input[0] = user
			result := map[string]any{
				"type": functionCallOutputType, "call_id": "not-issued",
				"output": "Process exited with code 0\nOutput:\n" + workspaceCanaryData + "\n",
			}
			if scenario != "unissued" {
				call := workspaceCanaryResponseItem(t, requestWorkspaceCanary(t, input, "exec_command", false))
				result["call_id"] = call["call_id"]
			}
			wantStatus := http.StatusOK
			switch scenario {
			case "unissued":
				input = append(input, result)
				wantStatus = http.StatusUnprocessableEntity
			case "different call":
				result["call_id"] = "a-different-call"
				input = append(input, result)
				wantStatus = http.StatusUnprocessableEntity
			case "previous turn":
				input = append(input[:0], result, user)
			case "user echo":
				user["content"] = result["output"].(string) + "Reply exactly: " + workspaceRestoreMarker
			case "assistant echo":
				input = append(input, map[string]any{"role": "assistant", "content": result["output"]})
			}
			response := requestWorkspaceCanary(t, input, "exec_command", false)
			if response.Code != wantStatus {
				t.Fatalf("status = %d, want %d", response.Code, wantStatus)
			}
			if wantStatus == http.StatusOK && workspaceCanaryResponseItem(t, response)["type"] != functionCallType {
				t.Fatal("replayed or echoed output bypassed the fresh file read")
			}
			assertWorkspaceCanaryVerified(t, workspaceRestoreMarker, false)
		})
	}
}

func TestWorkspaceCanaryUsesAdvertisedToolNamespace(t *testing.T) {
	resetWorkspaceCanary(t, workspaceRestoreMarker)
	body := fmt.Sprintf(`{"model":"gpt-5.5","input":"Reply exactly: %s",`+
		`"tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec_command"}]}]}`,
		workspaceRestoreMarker)
	response := httptest.NewRecorder()
	handleResponses(response, httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body)))
	if workspaceCanaryResponseItem(t, response)["namespace"] != "functions" {
		t.Fatal("tool call omitted its advertised namespace")
	}

	missing := requestWorkspaceCanary(t, []any{map[string]any{
		"role": "user", "content": "Reply exactly: " + workspaceRestoreMarker,
	}}, "not-a-shell-tool", false)
	if missing.Code != http.StatusBadRequest {
		t.Fatalf("missing shell tool status = %d, want 400", missing.Code)
	}
}

func requestWorkspaceCanary(t *testing.T, input []any, tool string, stream bool) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "gpt-5.5", "stream": stream, "input": input,
		"tools": []any{map[string]any{"type": "function", "name": tool}},
	})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	handleResponses(response, httptest.NewRequest(http.MethodPost, "/responses", bytes.NewReader(body)))
	return response
}

func workspaceCanaryResponseItem(t *testing.T, response *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	if response.Code != http.StatusOK {
		t.Fatalf("response status = %d: %s", response.Code, response.Body)
	}
	body := response.Body.Bytes()
	if response.Header().Get("Content-Type") == "text/event-stream" {
		_, completed, found := strings.Cut(string(body), "event: response.completed\ndata: ")
		if !found {
			t.Fatal("stream omitted response.completed")
		}
		var event struct {
			Response json.RawMessage `json:"response"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(completed)), &event); err != nil {
			t.Fatal(err)
		}
		body = event.Response
	}
	var completed struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal(body, &completed); err != nil || len(completed.Output) != 1 {
		t.Fatalf("response must contain one output item: %s, error %v", body, err)
	}
	return completed.Output[0]
}

func executeWorkspaceCanary(t *testing.T, call map[string]any, dir string) string {
	t.Helper()
	var args map[string]any
	if err := json.Unmarshal([]byte(call["arguments"].(string)), &args); err != nil {
		t.Fatal(err)
	}
	command, _ := args["cmd"].(string)
	if command == "" {
		command, _ = args["command"].(string)
	}
	cmd := exec.Command("/bin/sh", "-c", command)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	code := 0
	if err != nil {
		if exit, ok := err.(*exec.ExitError); ok {
			code = exit.ExitCode()
		} else {
			t.Fatal(err)
		}
	}
	if call["name"] == "shell_command" {
		return fmt.Sprintf("Exit code: %d\nWall time: 0.1 seconds\nOutput:\n%s", code, output)
	}
	return fmt.Sprintf("Chunk ID: canary\nWall time: 0.1 seconds\nProcess exited with code %d\nOutput:\n%s", code, output)
}

func resetWorkspaceCanary(t *testing.T, marker string) {
	t.Helper()
	key := markerKey(marker)
	clear := func() {
		workspaceCanaryCalls.Delete(key)
		workspaceCanaryResults.Delete(key)
	}
	clear()
	t.Cleanup(clear)
}

func assertWorkspaceCanaryVerified(t *testing.T, marker string, want bool) {
	t.Helper()
	response := httptest.NewRecorder()
	handleMarkerObservations(response, httptest.NewRequest(http.MethodGet, "/fixture/marker-observations", nil))
	var observations map[string]struct {
		Verified bool `json:"workspaceCanaryVerified"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &observations); err != nil {
		t.Fatal(err)
	}
	if got := observations[markerKey(marker)].Verified; got != want {
		t.Fatalf("workspace canary verified = %v, want %v", got, want)
	}
}
