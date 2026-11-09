package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const evalTestExperimentJSON = `{"experiment":{"namespace":"team-a","candidateRun":"cand","baselineRun":"base","hypothesis":"tighter prompt",` +
	`"reservedAt":"2026-11-09T10:00:00Z"},"outcome":{"result":"Passed","decidedAt":"2026-11-09T11:00:00Z",` +
	`"target":{"grader":"verdict","tag":"defect","baselinePassedTrials":2,"baselineTrials":9,"candidatePassedTrials":8,` +
	`"candidateTrials":9,"improvedPairs":6,"regressedPairs":0,"improvementPoints":"66.7","pValue":"0.0156"},` +
	`"binding":{"candidateAgent":{"name":"reviewer-v2","generation":3},"baselineAgent":{"name":"reviewer","generation":7}}},` +
	`"decisions":[{"id":"d1","candidateRun":"cand","decision":"approved","reason":"looks good","actor":{"username":"alice"},` +
	`"decidedAt":"2026-11-09T12:00:00Z"}],"effective":{"id":"d1","candidateRun":"cand","decision":"approved","reason":"looks good",` +
	`"actor":{"username":"alice"},"decidedAt":"2026-11-09T12:00:00Z"},"approval":{"current":false,"changed":["candidateAgent"]}}`

func TestEvalExperimentsListsOutcomesAndDecisions(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/evals/experiments" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"items":[` + evalTestExperimentJSON + `,{"experiment":{"candidateRun":"pending-cand","baselineRun":"base",` +
			`"reservedAt":"2026-11-09T10:00:00Z"}}],"continue":"tok2"}`))
	}))
	defer srv.Close()
	out, err := runEvalCommand(t, "eval", "experiments", "--limit", "2", "--continue", "tok1", "--server", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("Execute() = %v\n%s", err, out)
	}
	if !strings.Contains(gotQuery, "limit=2") || !strings.Contains(gotQuery, "continue=tok1") {
		t.Fatalf("query = %q", gotQuery)
	}
	for _, want := range []string{"cand", "Passed", "2/9 -> 8/9", "approved", "alice", "pending-cand", "Pending",
		"orka eval experiments --continue tok2"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestEvalDecisionsShowsAStaleApproval(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/evals/runs/cand/decisions" || r.Method != http.MethodGet {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(evalTestExperimentJSON))
	}))
	defer srv.Close()
	out, err := runEvalCommand(t, "eval", "decisions", "cand", "--server", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("Execute() = %v\n%s", err, out)
	}
	for _, want := range []string{"against baseline base", "Hypothesis: tighter prompt", "Outcome: Passed",
		"verdict on defect pass@1: 2/9 -> 8/9", "rollback target: reviewer (generation 7)", "approved by alice: looks good",
		"Approval: stale; changed since approval: candidateAgent"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestEvalApproveAndRejectPostADecision(t *testing.T) {
	for verb, decision := range map[string]string{"approve": "approved", "reject": "rejected"} {
		t.Run(verb, func(t *testing.T) {
			var got map[string]string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/v1/evals/runs/cand/decisions" || r.Method != http.MethodPost {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				body, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &got)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"d9","candidateRun":"cand","decision":"` + decision + `","reason":"why",` +
					`"actor":{"username":"alice"},"decidedAt":"2026-11-09T12:00:00Z"}`))
			}))
			defer srv.Close()
			out, err := runEvalCommand(t, "eval", verb, "cand", "--reason", "  why  ", "--server", srv.URL, "--token", "tok")
			if err != nil {
				t.Fatalf("Execute() = %v\n%s", err, out)
			}
			if got["decision"] != decision || got["reason"] != "why" {
				t.Fatalf("posted %v", got)
			}
			if !strings.Contains(out, "Recorded "+decision+" for evalrun/cand by alice (decision d9)") {
				t.Fatalf("output = %s", out)
			}
		})
	}
}

func TestEvalApproveNeedsAReason(t *testing.T) {
	_, err := runEvalCommand(t, "eval", "approve", "cand", "--server", "http://127.0.0.1:1", "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "--reason is required") {
		t.Fatalf("Execute() error = %v", err)
	}
}

// A refusal from the server, such as a caller outside the promoter groups,
// reaches the user as an error.
func TestEvalApproveReportsARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"eval decisions need membership in a promoter group"}`))
	}))
	defer srv.Close()
	_, err := runEvalCommand(t, "eval", "approve", "cand", "--reason", "ok", "--server", srv.URL, "--token", "tok")
	if err == nil || !strings.Contains(err.Error(), "promoter group") {
		t.Fatalf("Execute() error = %v", err)
	}
}
