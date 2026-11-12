package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAgentPromoteRollbackAndPromotions(t *testing.T) {
	var posted []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/reviewer/rollout":
			body, _ := io.ReadAll(r.Body)
			var request map[string]any
			_ = json.Unmarshal(body, &request)
			posted = append(posted, request)
			w.WriteHeader(http.StatusCreated)
			switch request["action"] {
			case "canary":
				_, _ = w.Write([]byte(`{"id":"c1","agent":"reviewer","action":"canary","percent":10,"candidateAgent":{"name":"reviewer-c-1"}}`))
				return
			case "abort":
				_, _ = w.Write([]byte(`{"id":"a1","agent":"reviewer","action":"abort"}`))
				return
			}
			if request["action"] == "promote" {
				_, _ = w.Write([]byte(`{"id":"p1","agent":"reviewer","action":"promote","state":"complete",` +
					`"candidateAgent":{"name":"reviewer-c-1","uid":"u","generation":1},"appliedGeneration":4}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"r1","agent":"reviewer","action":"rollback","state":"complete","promotionID":"p1","appliedGeneration":5}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/reviewer/promotions":
			_, _ = w.Write([]byte(`{"rollout":{"candidate":{"name":"reviewer-c-1"},"percent":10,"decisionID":"d1"},"items":[{"id":"p1","action":"promote","state":"complete","candidateAgent":{"name":"reviewer-c-1"},` +
				`"actor":{"username":"alice"},"appliedGeneration":4,"createdAt":"2026-11-10T10:00:00Z"},` +
				`{"id":"r1","action":"rollback","state":"complete","promotionID":"p1","actor":{"username":"bob"},` +
				`"reason":"regressed","appliedGeneration":5,"createdAt":"2026-11-10T11:00:00Z"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	if _, err := runEvalCommand(t, "agent", "promote", "reviewer", "--server", srv.URL, "--token", "tok"); err == nil ||
		!strings.Contains(err.Error(), "--decision is required") {
		t.Fatalf("promote without a decision = %v", err)
	}
	out, err := runEvalCommand(t, "agent", "promote", "reviewer", "--decision", "d1", "--server", srv.URL, "--token", "tok")
	if err != nil || !strings.Contains(out, "Promoted reviewer-c-1 onto agent/reviewer") || !strings.Contains(out, "orka agent rollback reviewer") {
		t.Fatalf("promote = %v\n%s", err, out)
	}
	out, err = runEvalCommand(t, "agent", "rollback", "reviewer", "--reason", "regressed", "--server", srv.URL, "--token", "tok")
	if err != nil || !strings.Contains(out, "Rolled back agent/reviewer to its values before promotion p1") {
		t.Fatalf("rollback = %v\n%s", err, out)
	}
	if len(posted) != 2 || posted[0]["decisionID"] != "d1" || posted[0]["action"] != "promote" ||
		posted[1]["action"] != "rollback" || posted[1]["reason"] != "regressed" {
		t.Fatalf("posted %v", posted)
	}

	out, err = runEvalCommand(t, "agent", "promotions", "reviewer", "--server", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Canary: 10% of new Sessions and Tasks run on reviewer-c-1 (decision d1)", "RECORD", "p1", "promote", "reviewer-c-1", "alice", "r1", "rollback", "bob", "r1: regressed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("promotions lacks %q:\n%s", want, out)
		}
	}
}

func TestAgentCanaryAndAbort(t *testing.T) {
	var posted []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		body, _ := io.ReadAll(r.Body)
		var request map[string]any
		_ = json.Unmarshal(body, &request)
		posted = append(posted, request)
		w.WriteHeader(http.StatusCreated)
		if request["action"] == "canary" {
			_, _ = w.Write([]byte(`{"id":"c1","agent":"reviewer","action":"canary","percent":10,"candidateAgent":{"name":"reviewer-c-1"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"a1","agent":"reviewer","action":"abort"}`))
	}))
	defer srv.Close()
	if _, err := runEvalCommand(t, "agent", "canary", "reviewer", "--decision", "d1", "--server", srv.URL, "--token", "tok"); err == nil ||
		!strings.Contains(err.Error(), "--percent must be 1 to 100") {
		t.Fatalf("canary without a percent = %v", err)
	}
	out, err := runEvalCommand(t, "agent", "canary", "reviewer", "--decision", "d1", "--percent", "10", "--server", srv.URL, "--token", "tok")
	if err != nil || !strings.Contains(out, "Routing 10% of new Sessions and Tasks for agent/reviewer to reviewer-c-1") {
		t.Fatalf("canary = %v\n%s", err, out)
	}
	out, err = runEvalCommand(t, "agent", "abort", "reviewer", "--reason", "noisy", "--server", srv.URL, "--token", "tok")
	if err != nil || !strings.Contains(out, "Ended the canary on agent/reviewer") {
		t.Fatalf("abort = %v\n%s", err, out)
	}
	if len(posted) != 2 || posted[0]["percent"] != float64(10) || posted[0]["decisionID"] != "d1" || posted[1]["reason"] != "noisy" {
		t.Fatalf("posted %v", posted)
	}
}

// Structured output keeps the API's prompt digests and sizes.
func TestAgentPromotionsStructuredOutputKeepsPromptViews(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"p1","action":"promote","previous":{"systemPromptDigest":"sha256:aa","systemPromptBytes":12}}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[{"id":"p1","action":"promote","target":{"systemPromptDigest":"sha256:bb","systemPromptBytes":7}}]}`))
	}))
	defer srv.Close()
	out, err := runEvalCommand(t, "agent", "promote", "reviewer", "--decision", "d1", "-o", "json", "--server", srv.URL, "--token", "tok")
	if err != nil || !strings.Contains(out, "sha256:aa") || !strings.Contains(out, `"systemPromptBytes": 12`) {
		t.Fatalf("promote -o json = %v\n%s", err, out)
	}
	out, err = runEvalCommand(t, "agent", "promotions", "reviewer", "-o", "json", "--server", srv.URL, "--token", "tok")
	if err != nil || !strings.Contains(out, "sha256:bb") {
		t.Fatalf("promotions -o json = %v\n%s", err, out)
	}
}

// Rollout requests carry the selected namespace, like every CLI request.
func TestAgentRolloutUsesTheSelectedNamespace(t *testing.T) {
	var namespaces []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		namespaces = append(namespaces, r.URL.Query().Get("namespace"))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"r1","agent":"reviewer","action":"rollback"}`))
			return
		}
		_, _ = w.Write([]byte(`{"items":[]}`))
	}))
	defer srv.Close()
	for _, args := range [][]string{
		{"agent", "rollback", "reviewer"}, {"agent", "promotions", "reviewer"},
	} {
		if _, err := runEvalCommand(t, append(args, "-n", "team-x", "--server", srv.URL, "--token", "tok")...); err != nil {
			t.Fatal(err)
		}
	}
	if len(namespaces) != 2 || namespaces[0] != "team-x" || namespaces[1] != "team-x" {
		t.Fatalf("namespaces = %v", namespaces)
	}
}
