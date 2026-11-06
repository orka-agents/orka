/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

const evalTestManifest = `apiVersion: core.orka.ai/v1alpha1
kind: EvalRun
metadata:
  generateName: pr-review-
spec:
  subject:
    type: repositoryMonitorReview
    repositoryMonitorReview: {monitorRef: {name: monitor}}
  cases:
    configMapRef: {name: cases, key: cases.jsonl}
  trials: 2
  graders: [verdict, contract]
`

// evalTestServer serves one EvalRun that reaches finalPhase after a poll.
func evalTestServer(t *testing.T, finalPhase string, created *map[string]any) *httptest.Server {
	t.Helper()
	polls := 0
	status := func(phase string, completed int) map[string]any {
		s := map[string]any{
			"phase":  phase,
			"counts": map[string]any{"planned": 2, "completed": completed, "passed": 1, "failed": completed - 1},
		}
		if phase == finalPhase {
			s["conditions"] = []any{map[string]any{"type": "Completed", "status": "True", "reason": finalPhase,
				"message": "2 trials graded, 0 infra errors excluded", "lastTransitionTime": "2026-11-06T00:00:00Z"}}
			s["results"] = []any{map[string]any{"grader": "verdict", "trials": 2, "passedTrials": 1, "cases": 1,
				"casesAnyPassed": 1, "casesAllPassed": 0}}
		}
		return s
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		run := map[string]any{"metadata": map[string]any{"name": "pr-review-x7", "namespace": "team-a"}}
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/evals/runs":
			if err := json.NewDecoder(r.Body).Decode(created); err != nil {
				t.Errorf("decode: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/evals/runs/pr-review-x7":
			polls++
			if polls == 1 {
				run["status"] = status("Running", 1)
			} else {
				run["status"] = status(finalPhase, 2)
			}
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/evals/runs/pr-review-x7/trials":
			if r.URL.Query().Get("format") != "jsonl" {
				t.Errorf("trials format = %q", r.URL.Query().Get("format"))
			}
			w.Header().Set("Content-Type", "application/jsonl")
			_, _ = w.Write([]byte(`{"caseID":"clean-1","trial":1,"taskName":"eval-a","outcome":"pass","verdict":"passed"}` + "\n" +
				`{"caseID":"clean-1","trial":2,"taskName":"eval-b","outcome":"fail","verdict":"needs_changes",` +
				`"grades":[{"grader":"verdict","status":"fail"},{"grader":"contract","status":"pass"}]}` + "\n"))
			return
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/tasks/eval-b/events":
			_, _ = w.Write([]byte(`{"items":[{"type":"ModelMessage"}]}`))
			return
		default:
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(run)
	}))
}

func runEvalCommand(t *testing.T, args ...string) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	previous := evalRunPollInterval
	evalRunPollInterval = time.Millisecond
	t.Cleanup(func() { evalRunPollInterval = previous })
	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func writeEvalTestManifest(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "evalrun.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestEvalRunCreatesWaitsAndWritesResults(t *testing.T) {
	var created map[string]any
	srv := evalTestServer(t, "Succeeded", &created)
	defer srv.Close()
	dir := filepath.Join(t.TempDir(), "out")
	out, err := runEvalCommand(t, "eval", "run", "-f", writeEvalTestManifest(t, evalTestManifest), "--server", srv.URL,
		"--token", "tok", "--namespace", "team-a", "--output-dir", dir)
	if err != nil {
		t.Fatalf("Execute() error: %v\n%s", err, out)
	}
	metadata := created["metadata"].(map[string]any)
	if metadata["generateName"] != "pr-review-" || metadata["namespace"] != "team-a" {
		t.Fatalf("create metadata = %v", metadata)
	}
	if _, ok := created["apiVersion"]; ok {
		t.Fatalf("create body carries apiVersion: %v", created)
	}
	for _, want := range []string{"evalrun/pr-review-x7 created in team-a", "Running: 1/2 trials done", "Succeeded", "verdict", "1/2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
	for _, file := range []string{"evalrun.json", "trials.jsonl", "summary.md", "events/eval-b.json"} {
		if _, err := os.Stat(filepath.Join(dir, file)); err != nil {
			t.Fatalf("missing %s: %v", file, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "events", "eval-a.json")); err == nil {
		t.Fatal("wrote events for a passing trial")
	}
	summary, err := os.ReadFile(filepath.Join(dir, "summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(summary), "| clean-1 | 2 | needs_changes | verdict | eval-b |") {
		t.Fatalf("summary:\n%s", summary)
	}
}

func TestEvalRunFailsWhenTheRunDoesNotSucceed(t *testing.T) {
	var created map[string]any
	srv := evalTestServer(t, "Invalid", &created)
	defer srv.Close()
	out, err := runEvalCommand(t, "eval", "run", "-f", writeEvalTestManifest(t, evalTestManifest), "--server", srv.URL,
		"--token", "tok", "--wait")
	if err == nil || !strings.Contains(err.Error(), "ended Invalid") {
		t.Fatalf("Execute() error = %v\n%s", err, out)
	}
}

func TestEvalRunCreatesWithoutWaiting(t *testing.T) {
	var created map[string]any
	srv := evalTestServer(t, "Succeeded", &created)
	defer srv.Close()
	out, err := runEvalCommand(t, "eval", "run", "-f", writeEvalTestManifest(t, evalTestManifest), "--server", srv.URL, "--token", "tok")
	if err != nil || !strings.Contains(out, "created") || strings.Contains(out, "Succeeded") {
		t.Fatalf("Execute() = %v\n%s", err, out)
	}
}

func TestEvalRunRejectsInvalidManifests(t *testing.T) {
	for name, manifest := range map[string]string{
		"other kind":     strings.Replace(evalTestManifest, "kind: EvalRun", "kind: Task", 1),
		"other version":  strings.Replace(evalTestManifest, "core.orka.ai/v1alpha1", "core.orka.ai/v1", 1),
		"no name":        strings.Replace(evalTestManifest, "generateName: pr-review-", "labels: {a: b}", 1),
		"unknown field":  evalTestManifest + "extra: true\n",
		"not a manifest": "{",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := runEvalCommand(t, "eval", "run", "-f", writeEvalTestManifest(t, manifest), "--server", "http://127.0.0.1:1",
				"--token", "tok"); err == nil {
				t.Fatal("Execute() succeeded")
			}
		})
	}
}

func TestEvalGetPrintsResults(t *testing.T) {
	var created map[string]any
	srv := evalTestServer(t, "Succeeded", &created)
	defer srv.Close()
	// The first GET reports Running; the second, Succeeded.
	if _, err := runEvalCommand(t, "eval", "get", "pr-review-x7", "--server", srv.URL, "--token", "tok"); err != nil {
		t.Fatal(err)
	}
	out, err := runEvalCommand(t, "eval", "get", "pr-review-x7", "--server", srv.URL, "--token", "tok")
	if err != nil || !strings.Contains(out, "EvalRun team-a/pr-review-x7: Succeeded") || !strings.Contains(out, "GRADER") {
		t.Fatalf("Execute() = %v\n%s", err, out)
	}
}

func TestEvalRunOutputRefusesTaskNamesThatEscape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/evals/runs":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"r","namespace":"team-a"}}`))
		case "/api/v1/evals/runs/r":
			_, _ = w.Write([]byte(`{"metadata":{"name":"r","namespace":"team-a"},"status":{"phase":"Succeeded"}}`))
		case "/api/v1/evals/runs/r/trials":
			_, _ = w.Write([]byte(`{"caseID":"c","trial":1,"taskName":"../../settings","outcome":"fail"}` + "\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	base := t.TempDir()
	dir := filepath.Join(base, "results", "out")
	_, err := runEvalCommand(t, "eval", "run", "-f", writeEvalTestManifest(t, evalTestManifest), "--server", srv.URL,
		"--token", "tok", "--output-dir", dir)
	if err == nil || !strings.Contains(err.Error(), "invalid Task") {
		t.Fatalf("Execute() error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(base, "settings.json")); err == nil {
		t.Fatal("a Task name wrote outside --output-dir")
	}
}

// The manifest's namespace differs from the CLI's: every request after the
// create targets the run's namespace, and a failed trial's events are read
// across pages.
func TestEvalRunFollowsTheRunNamespaceAndPagesEvents(t *testing.T) {
	var mu sync.Mutex
	var wrongNamespace []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && r.URL.Query().Get("namespace") != "team-b" {
			mu.Lock()
			wrongNamespace = append(wrongNamespace, r.URL.Path+"?"+r.URL.RawQuery)
			mu.Unlock()
		}
		switch r.URL.Path {
		case "/api/v1/evals/runs":
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"metadata":{"name":"r","namespace":"team-b"}}`))
		case "/api/v1/evals/runs/r":
			_, _ = w.Write([]byte(`{"metadata":{"name":"r","namespace":"team-b"},"status":{"phase":"Succeeded"}}`))
		case "/api/v1/evals/runs/r/trials":
			_, _ = w.Write([]byte(`{"caseID":"c","trial":1,"taskName":"eval-c","outcome":"fail"}` + "\n"))
		case "/api/v1/tasks/eval-c/events":
			switch r.URL.Query().Get("after") {
			case "0":
				_, _ = w.Write([]byte(`{"latestSeq":3,"events":[{"seq":1,"type":"ModelRequestStarted"},{"seq":2,"type":"ModelMessage"}]}`))
			case "2":
				_, _ = w.Write([]byte(`{"latestSeq":3,"events":[{"seq":3,"type":"TaskFailed"}]}`))
			default:
				t.Errorf("unexpected after=%q", r.URL.Query().Get("after"))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	manifest := strings.Replace(evalTestManifest, "generateName: pr-review-", "generateName: pr-review-\n  namespace: team-b", 1)
	dir := filepath.Join(t.TempDir(), "out")
	out, err := runEvalCommand(t, "eval", "run", "-f", writeEvalTestManifest(t, manifest), "--server", srv.URL,
		"--token", "tok", "--namespace", "team-a", "--output-dir", dir)
	if err != nil {
		t.Fatalf("Execute() error: %v\n%s", err, out)
	}
	if len(wrongNamespace) > 0 {
		t.Fatalf("requests outside the run's namespace: %v", wrongNamespace)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "events", "eval-c.json"))
	if err != nil {
		t.Fatal(err)
	}
	var events struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal(raw, &events); err != nil || len(events.Events) != 3 {
		t.Fatalf("events file = %s, %v", raw, err)
	}
}

func TestEvalRunRefusesAShortTTLWhenWaiting(t *testing.T) {
	manifest := evalTestManifest + "  ttlSecondsAfterFinished: 0\n"
	for _, args := range [][]string{{"--wait"}, {"--output-dir", t.TempDir()}} {
		full := append([]string{"eval", "run", "-f", writeEvalTestManifest(t, manifest), "--server", "http://127.0.0.1:1", "--token", "tok"}, args...)
		_, err := runEvalCommand(t, full...)
		if err == nil || !strings.Contains(err.Error(), "ttlSecondsAfterFinished 0") {
			t.Fatalf("eval run %v: %v", args, err)
		}
	}
	// Without waiting, a short TTL is the caller's choice: the create is
	// attempted (and fails here only because no server listens).
	_, err := runEvalCommand(t, "eval", "run", "-f", writeEvalTestManifest(t, manifest), "--server", "http://127.0.0.1:1", "--token", "tok")
	if err == nil || strings.Contains(err.Error(), "ttlSecondsAfterFinished") {
		t.Fatalf("eval run without --wait: %v", err)
	}
}

// Only a missing Task gets a placeholder events file; any other download
// failure fails the export.
func TestEvalRunOutputFailsOnEventErrorsOtherThanNotFound(t *testing.T) {
	for name, status := range map[string]int{"not found": http.StatusNotFound, "forbidden": http.StatusForbidden, "server error": http.StatusInternalServerError} {
		t.Run(name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch r.URL.Path {
				case "/api/v1/evals/runs":
					w.WriteHeader(http.StatusCreated)
					_, _ = w.Write([]byte(`{"metadata":{"name":"r","namespace":"team-a"}}`))
				case "/api/v1/evals/runs/r":
					_, _ = w.Write([]byte(`{"metadata":{"name":"r","namespace":"team-a"},"status":{"phase":"Succeeded"}}`))
				case "/api/v1/evals/runs/r/trials":
					_, _ = w.Write([]byte(`{"caseID":"c","trial":1,"taskName":"eval-c","outcome":"fail"}` + "\n"))
				default:
					w.WriteHeader(status)
				}
			}))
			defer srv.Close()
			dir := filepath.Join(t.TempDir(), "out")
			_, err := runEvalCommand(t, "eval", "run", "-f", writeEvalTestManifest(t, evalTestManifest), "--server", srv.URL,
				"--token", "tok", "--output-dir", dir)
			if status == http.StatusNotFound {
				raw, readErr := os.ReadFile(filepath.Join(dir, "events", "eval-c.json"))
				if err != nil || readErr != nil || !strings.Contains(string(raw), "no longer exists") {
					t.Fatalf("Execute() = %v, events file %q, %v", err, raw, readErr)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), "read events of failed trial Task eval-c") {
				t.Fatalf("Execute() error = %v", err)
			}
		})
	}
}
