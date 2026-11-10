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
	var posted []map[string]string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/agents/reviewer/rollout":
			body, _ := io.ReadAll(r.Body)
			var request map[string]string
			_ = json.Unmarshal(body, &request)
			posted = append(posted, request)
			w.WriteHeader(http.StatusCreated)
			if request["action"] == "promote" {
				_, _ = w.Write([]byte(`{"id":"p1","agent":"reviewer","action":"promote","state":"complete",` +
					`"candidateAgent":{"name":"reviewer-c-1","uid":"u","generation":1},"appliedGeneration":4}`))
				return
			}
			_, _ = w.Write([]byte(`{"id":"r1","agent":"reviewer","action":"rollback","state":"complete","promotionID":"p1","appliedGeneration":5}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/agents/reviewer/promotions":
			_, _ = w.Write([]byte(`{"items":[{"id":"p1","action":"promote","state":"complete","candidateAgent":{"name":"reviewer-c-1"},` +
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
	for _, want := range []string{"RECORD", "p1", "promote", "reviewer-c-1", "alice", "r1", "rollback", "bob", "r1: regressed"} {
		if !strings.Contains(out, want) {
			t.Fatalf("promotions lacks %q:\n%s", want, out)
		}
	}
}
