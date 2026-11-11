package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEvalProposeAndListProposals(t *testing.T) {
	var posted map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/evals/runs/training/proposals":
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &posted)
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"name":"eval-proposal-training-1","namespace":"team-a","evalRun":"training"}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/evals/runs/training/proposals":
			_, _ = w.Write([]byte(`{"items":[{"name":"eval-proposal-training-1","state":"Created","hypothesis":"It misses unit bugs.",` +
				`"candidateAgent":"reviewer-c-1","candidateRun":"training-p-1-candidate","createdAt":"2026-11-10T10:00:00Z"},` +
				`{"name":"eval-proposal-training-2","state":"Refused","reason":"the proposer's answer was refused: no JSON object",` +
				`"createdAt":"2026-11-10T10:05:00Z"},{"name":"eval-proposal-training-3","phase":"Running","createdAt":"2026-11-10T10:06:00Z"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	out, err := runEvalCommand(t, "eval", "propose", "training", "--held-out", "heldout", "--target", "verdict:defect",
		"--server", srv.URL, "--token", "tok")
	if err != nil || !strings.Contains(out, "Started proposal eval-proposal-training-1") {
		t.Fatalf("propose = %v\n%s", err, out)
	}
	target, _ := posted["target"].(map[string]any)
	if posted["heldOut"] != "heldout" || target["grader"] != "verdict" || target["tag"] != "defect" {
		t.Fatalf("posted %v", posted)
	}
	out, err = runEvalCommand(t, "eval", "proposals", "training", "--server", srv.URL, "--token", "tok")
	if err != nil {
		t.Fatalf("proposals = %v\n%s", err, out)
	}
	for _, want := range []string{"reviewer-c-1", "training-p-1-candidate", "It misses unit bugs.", "Refused", "refused: the proposer's answer",
		"Running"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output lacks %q:\n%s", want, out)
		}
	}
}
