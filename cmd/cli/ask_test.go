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
	mu           sync.Mutex
	agentStatus  int
	phases       []string
	execution    map[string]any
	message      string
	missing      int // Task reads that answer 404 before the Task is visible
	createStatus int // non-zero fails the Task create with this status
	answer       string
	stallResult  chan struct{} // when set, result reads hang until the client gives up or this closes
	created      map[string]any
	resultReads  int
}

func newAskServer(t *testing.T, agentStatus int, phases ...string) *askServer {
	t.Helper()
	s := &askServer{agentStatus: agentStatus, phases: phases, answer: "**Answer:** Yes.\n"}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		stall := s.stallResult
		s.mu.Unlock()
		if stall != nil && r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/result") {
			select {
			case <-r.Context().Done():
			case <-stall:
			}
			return
		}
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
			if s.createStatus != 0 {
				w.WriteHeader(s.createStatus)
				_, _ = w.Write([]byte(`{"error":"upstream timed out"}`))
				return
			}
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{"metadata": map[string]any{"name": s.created["name"]}})
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/result"):
			s.resultReads++
			_ = json.NewEncoder(w).Encode(map[string]any{"result": s.answer})
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/tasks/") && s.missing > 0:
			s.missing--
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":"task not found"}`))
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/tasks/"):
			phase := s.phases[0]
			if len(s.phases) > 1 {
				s.phases = s.phases[1:]
			}
			status := map[string]any{"phase": phase}
			if s.execution != nil {
				status["execution"] = s.execution
			}
			if s.message != "" {
				status["message"] = s.message
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
	previousInterval, previousGrace := askPollInterval, askNotFoundGrace
	askPollInterval, askNotFoundGrace = 10*time.Millisecond, time.Second
	t.Cleanup(func() { askPollInterval, askNotFoundGrace = previousInterval, previousGrace })

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
	if err == nil || !strings.Contains(err.Error(), "phase Failed: ToolboxUnavailable: toolboxes are disabled") ||
		!strings.Contains(err.Error(), "orka task logs ask-") {
		t.Fatalf("err = %v", err)
	}
	if srv.resultReads != 0 {
		t.Fatal("a failed Task has no answer to read")
	}
}

func TestAskReportsFailureMessageOutsideExecution(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Failed")
	srv.message = `Agent "orka-guide" not found`
	_, _, err := runAsk(t, srv, "", "q")
	if err == nil || !strings.Contains(err.Error(), `phase Failed: Agent "orka-guide" not found`) {
		t.Fatalf("err = %v", err)
	}
}

func TestAskWaitsForNewTaskToBecomeVisible(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Running", "Succeeded")
	srv.missing = 2
	stdout, _, err := runAsk(t, srv, "", "q")
	if err != nil {
		t.Fatalf("ask: %v", err)
	}
	if stdout != "**Answer:** Yes.\n" {
		t.Fatalf("stdout = %q", stdout)
	}
}

func TestAskStopsWhenTaskNeverAppears(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Succeeded")
	srv.missing = 1 << 20
	_, _, err := runAsk(t, srv, "", "q")
	if err == nil || !strings.Contains(err.Error(), "HTTP 404") || strings.Contains(err.Error(), "still running") {
		t.Fatalf("err = %v, want the not-found error once the grace period ends", err)
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

func TestAskRejectsSucceededTaskWithoutAnswer(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Succeeded")
	srv.answer = " \n"
	stdout, _, err := runAsk(t, srv, "", "q", "-o", "json")
	if err == nil || !strings.Contains(err.Error(), "succeeded without an answer") ||
		!strings.Contains(err.Error(), "orka task logs ask-") {
		t.Fatalf("err = %v, want an error for the missing answer", err)
	}
	if stdout != "" {
		t.Fatalf("stdout = %q, want nothing printed without an answer", stdout)
	}
}

func TestAskCreateFailureNamesTheTask(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Succeeded")
	srv.createStatus = http.StatusGatewayTimeout
	_, _, err := runAsk(t, srv, "", "q")
	name, _ := srv.created["name"].(string)
	if err == nil || !strings.HasPrefix(name, "ask-") || !strings.Contains(err.Error(), "create the question task "+name+":") {
		t.Fatalf("err = %v, want it to name the requested task %q", err, name)
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

func TestAskBoundsTheAnswerReadByTimeout(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Succeeded")
	release := make(chan struct{})
	srv.mu.Lock()
	srv.stallResult = release
	srv.mu.Unlock()
	t.Cleanup(func() { close(release) }) // runs before the server closes

	done := make(chan error, 1)
	go func() {
		_, _, err := runAsk(t, srv, "", "q", "--timeout", "300ms")
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "finished, but reading its answer outlasted --timeout") ||
			!strings.Contains(err.Error(), "orka task result ask-") {
			t.Fatalf("err = %v, want the timeout error with the command that reads the answer", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("orka ask kept waiting for the answer after --timeout")
	}
}

func TestAskRejectsQuestionAndTokenBothOnStdin(t *testing.T) {
	srv := newAskServer(t, http.StatusOK, "Succeeded")
	_, _, err := runAsk(t, srv, "txn-token-value", "-", "--txn-token-file", "-")
	if err == nil || !strings.Contains(err.Error(), "can't both come from stdin") {
		t.Fatalf("err = %v, want the shared-stdin error", err)
	}
	if srv.created != nil {
		t.Fatalf("no Task may be created when stdin holds the token: %#v", srv.created)
	}
}
