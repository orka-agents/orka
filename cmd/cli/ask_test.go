/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// askServer fakes the Orka API for one orka ask call: the Agent lookup, the
// Task create, its status and its result.
type askServer struct {
	*httptest.Server
	mu          sync.Mutex
	agentStatus int
	phases      []string
	execution   map[string]any
	created     map[string]any
	resultReads int
}

func newAskServer(t *testing.T, agentStatus int, phases ...string) *askServer {
	t.Helper()
	s := &askServer{agentStatus: agentStatus, phases: phases}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/agents/"):
			w.WriteHeader(s.agentStatus)
			_, _ = w.Write([]byte(`{"metadata":{"name":"orka-guide"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/tasks":
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &s.created); err != nil {
				t.Errorf("task request is not JSON: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": s.created["name"]}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/result"):
			s.resultReads++
			_ = json.NewEncoder(w).Encode(map[string]any{"result": "**Answer:** Yes.\n"})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/tasks/"):
			phase := s.phases[0]
			if len(s.phases) > 1 {
				s.phases = s.phases[1:]
			}
			status := map[string]any{"phase": phase}
			if s.execution != nil {
				status["execution"] = s.execution
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": "ask"}, "status": status})
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func runAsk(t *testing.T, srv *askServer, stdin string, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	previous := askPollInterval
	askPollInterval = 10 * time.Millisecond
	t.Cleanup(func() { askPollInterval = previous })

	root := newRootCmd()
	var stdout, stderr bytes.Buffer
	root.SetOut(&stdout)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader(stdin))
	root.SetArgs(append(append([]string{"ask"}, args...), "--server", srv.URL, "-n", "orka-system"))
	err := root.Execute()
	return stdout.String(), stderr.String(), err
}

func TestAskCreatesGuideTaskAndPrintsAnswer(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Pending", "Running", "Succeeded")
	stdout, stderr, err := runAsk(t, srv, "", "does", "Orka", "support", "toolboxes?")
	if err != nil {
		t.Fatalf("ask: %v (stderr %q)", err, stderr)
	}
	if stdout != "**Answer:** Yes.\n" {
		t.Fatalf("stdout = %q, want the answer as written", stdout)
	}
	created := srv.created
	agentRef, _ := created["agentRef"].(map[string]any)
	if created["type"] != "agent" || agentRef["name"] != "orka-guide" || created["namespace"] != "orka-system" ||
		created["prompt"] != "does Orka support toolboxes?" || !strings.HasPrefix(created["name"].(string), "ask-") {
		t.Fatalf("task request = %#v", created)
	}
	for _, field := range []string{"workspace", "agentRuntime", "ai", "image"} {
		if _, ok := created[field]; ok {
			t.Fatalf("a question must not carry %s: %#v", field, created)
		}
	}
	if !strings.Contains(stderr, "Asking orka-guide (task ask-") {
		t.Fatalf("stderr = %q", stderr)
	}
}

func TestAskReadsQuestionFromStdin(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Succeeded")
	if _, _, err := runAsk(t, srv, "  which runtimes?\n", "-"); err != nil {
		t.Fatal(err)
	}
	if srv.created["prompt"] != "which runtimes?" {
		t.Fatalf("prompt = %q", srv.created["prompt"])
	}
}

func TestAskStructuredOutputAndAgentOverride(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Succeeded")
	stdout, _, err := runAsk(t, srv, "", "q", "--agent", "docs-bot", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var answer askAnswer
	if err := json.Unmarshal([]byte(stdout), &answer); err != nil {
		t.Fatalf("output is not JSON: %v\n%s", err, stdout)
	}
	if answer.Agent != "docs-bot" || answer.Phase != "Succeeded" || answer.Answer != "**Answer:** Yes.\n" ||
		!strings.HasPrefix(answer.Task, "ask-") {
		t.Fatalf("answer = %#v", answer)
	}
}

func TestAskNoWaitPrintsTaskName(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Pending")
	stdout, _, err := runAsk(t, srv, "", "q", "--no-wait")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(stdout) != srv.created["name"] || srv.resultReads != 0 {
		t.Fatalf("stdout = %q, want only the task name", stdout)
	}
}

func TestAskExplainsMissingGuide(t *testing.T) {
	srv := newAskServer(t, http.StatusNotFound, "Succeeded")
	_, _, err := runAsk(t, srv, "", "q")
	if err == nil || !strings.Contains(err.Error(), "guide.enabled=true") || !strings.Contains(err.Error(), `"orka-system"`) {
		t.Fatalf("err = %v, want a pointer to guide.enabled", err)
	}
	if srv.created != nil {
		t.Fatal("no Task may be created for a missing Agent")
	}
}

func TestAskReportsFailedTask(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Failed")
	srv.execution = map[string]any{"reason": "ToolboxUnavailable", "message": "toolboxes are disabled"}
	_, _, err := runAsk(t, srv, "", "q")
	if err == nil || !strings.Contains(err.Error(), "phase Failed: ToolboxUnavailable toolboxes are disabled") ||
		!strings.Contains(err.Error(), "orka task logs ask-") {
		t.Fatalf("err = %v", err)
	}
	if srv.resultReads != 0 {
		t.Fatal("a failed Task has no answer to read")
	}
}

func TestAskTimeoutLeavesTaskRunning(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Running")
	_, _, err := runAsk(t, srv, "", "q", "--timeout", "50ms")
	if err == nil || !strings.Contains(err.Error(), "still running as task ask-") ||
		!strings.Contains(err.Error(), "orka task result ask-") {
		t.Fatalf("err = %v", err)
	}
}

func TestAskRequiresQuestion(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Succeeded")
	if _, _, err := runAsk(t, srv, "   ", "-"); err == nil || !strings.Contains(err.Error(), "a question is required") {
		t.Fatalf("err = %v", err)
	}
	if srv.created != nil {
		t.Fatal("no Task may be created without a question")
	}
}
