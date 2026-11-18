/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func findingFixture(id, severity, validation, title string) map[string]any {
	return map[string]any{
		"id": id, "severity": severity, "validationStatus": validation, "title": title,
		"filePath": "routes/" + id + ".js", "line": 7, "state": "open", "summary": "s",
	}
}

func TestSecurityFindingListTableColumnsOrderAndTruncation(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	items := []any{
		findingFixture("fnd_low", "low", "unvalidated", "Verbose logging"),
		findingFixture("fnd_crit_b", "critical", "unvalidated", "Unauthenticated command injection via exec('identify ' + url) "+strings.Repeat("and more ", 20)),
		findingFixture("fnd_high", "high", "validated", "Hard-coded express-session secret enables cookie forgery"),
		findingFixture("fnd_crit_a", "critical", "validated", "Zip-slip via AdmZip.extractAllTo on POST /import"),
		findingFixture("fnd_pending", "medium", "pending", "SQL built with string concatenation"),
	}
	response := map[string]any{"items": items, "metadata": map[string]any{"continue": ""}}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		json.NewEncoder(w).Encode(response) //nolint:errcheck
	}))
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "security", "finding", "list", "nodejs-goof")
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if lines[0] != "SEVERITY  VALIDATED  ID          TITLE                                                                 FILE" && !strings.HasPrefix(lines[0], "SEVERITY  VALIDATED  ID") {
		t.Fatalf("header = %q", lines[0])
	}
	for _, want := range []string{"SEVERITY", "VALIDATED", "ID", "TITLE", "FILE"} {
		if !strings.Contains(lines[0], want) {
			t.Fatalf("header missing %q: %q", want, lines[0])
		}
	}
	order := []string{"fnd_crit_a", "fnd_crit_b", "fnd_high", "fnd_pending", "fnd_low"}
	for i, id := range order {
		if !strings.Contains(lines[i+1], id) {
			t.Fatalf("row %d should be %s:\n%s", i+1, id, out)
		}
	}
	if !strings.Contains(lines[1], "critical  yes") || !strings.Contains(lines[2], "critical  no ") || !strings.Contains(lines[4], "pending") {
		t.Fatalf("VALIDATED column wrong:\n%s", out)
	}
	if !strings.Contains(lines[1], "routes/fnd_crit_a.js:7") {
		t.Fatalf("FILE column wrong:\n%s", out)
	}
	for _, line := range lines {
		if len([]rune(line)) > 100 {
			t.Fatalf("row wider than the terminal (%d): %q", len([]rune(line)), line)
		}
	}
	if !strings.Contains(lines[2], "…") {
		t.Fatalf("long title was not truncated:\n%s", out)
	}
	jsonOut, err := runCLI(t, srv.URL, "security", "finding", "list", "nodejs-goof", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	expected, _ := json.MarshalIndent(response, "", "  ")
	if jsonOut != string(expected)+"\n" {
		t.Fatalf("-o json changed:\n%s", jsonOut)
	}
}

func TestSecurityScanListAndDroppedFindingsTables(t *testing.T) {
	started := time.Now().Add(-30 * time.Minute).UTC()
	completed := started.Add(12 * time.Minute)
	srv := jsonServer(t, map[string]any{
		"/api/v1/security/repositories/nodejs-goof/scans": map[string]any{"items": []any{map[string]any{
			"id": "scan-1", "phase": "succeeded", "mode": "manual", "sliceCount": 14, "reviewedSliceCount": 14,
			"skippedSliceCount": 1, "acceptedFindings": 3, "droppedFindings": 5,
			"startedAt": started.Format(time.RFC3339), "completedAt": completed.Format(time.RFC3339),
		}}},
		"/api/v1/security/repositories/nodejs-goof/dropped-findings": map[string]any{"items": []any{map[string]any{
			"id": "drop-1", "layer": "filter", "reason": "evidence path outside review context", "sliceID": "slc-3",
			"createdAt": started.Format(time.RFC3339),
		}}},
	})
	defer srv.Close()
	out, err := runCLI(t, srv.URL, "security", "scan", "list", "nodejs-goof")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"ID", "PHASE", "SLICES", "FINDINGS", "DROPPED", "scan-1", "succeeded", "14/14 (1 skipped)", "12m"} {
		if !strings.Contains(out, want) {
			t.Errorf("scan list missing %q:\n%s", want, out)
		}
	}
	out, err = runCLI(t, srv.URL, "security", "dropped-findings", "list", "nodejs-goof")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"LAYER", "REASON", "drop-1", "filter", "evidence path outside review context", "slc-3"} {
		if !strings.Contains(out, want) {
			t.Errorf("dropped list missing %q:\n%s", want, out)
		}
	}
}

func scanProgressFixture(phase string, review [4]int, failedNames []string) map[string]any {
	stages := []any{
		map[string]any{"stage": "threat-model", "label": "threat model", "tasks": 1, "succeeded": 1},
		map[string]any{"stage": "mapper", "label": "map the code", "tasks": 1, "succeeded": 1},
		map[string]any{"stage": "review", "label": "review slices", "tasks": review[0] + review[1] + review[2] + review[3],
			"pending": review[0], "running": review[1], "succeeded": review[2], "failed": review[3], "failedTasks": failedNames},
		map[string]any{"stage": "validation", "label": "validate findings"},
		map[string]any{"stage": "patch", "label": "patch findings"},
	}
	return map[string]any{
		"scan": map[string]any{
			"id": "scan-1", "mode": "manual", "phase": phase, "sliceCount": 14, "reviewedSliceCount": review[2],
			"startedAt": time.Now().Add(-6 * time.Minute).UTC().Format(time.RFC3339),
		},
		"stages":   stages,
		"complete": phase != "running" && phase != "pending",
	}
}

func TestRenderScanProgressShowsStagesAndFailedTasks(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	out := renderScanProgress(scanProgressFixture("running", [4]int{0, 6, 8, 1}, []string{"goof-review-slice-9"}))
	for _, want := range []string{"Scan:", "scan-1 manual", "Phase:", "running, started 6m ago", "Slices:", "8 of 14 reviewed",
		"STAGE", "TASKS", "PENDING", "RUNNING", "SUCCEEDED", "FAILED",
		"threat model", "map the code", "review slices", "validate findings", "patch findings",
		"Failed tasks:", "  goof-review-slice-9"} {
		if !strings.Contains(out, want) {
			t.Errorf("progress missing %q:\n%s", want, out)
		}
	}
	lines := strings.SplitSeq(out, "\n")
	for line := range lines {
		if strings.HasPrefix(line, "review slices") && !strings.Contains(line, "15") {
			t.Fatalf("review row wrong: %q", line)
		}
	}
	noFail := renderScanProgress(scanProgressFixture("running", [4]int{2, 4, 8, 0}, nil))
	if strings.Contains(noFail, "Failed tasks") {
		t.Fatalf("failed section printed with no failures:\n%s", noFail)
	}
}

func TestSecurityScanStatusWatchExitsWithScanOutcome(t *testing.T) {
	t.Setenv("COLUMNS", "100")
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/security/repositories/nodejs-goof/scans":
			json.NewEncoder(w).Encode(map[string]any{"items": []any{map[string]any{"id": "scan-1", "phase": "running"}}}) //nolint:errcheck
		case "/api/v1/security/repositories/nodejs-goof/scans/scan-1/progress":
			calls++
			phase := "running"
			if calls >= 2 {
				phase = "failed"
			}
			json.NewEncoder(w).Encode(scanProgressFixture(phase, [4]int{0, 0, 14, 1}, []string{"goof-review-slice-2"})) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	out, err := runCLI(t, srv.URL, "security", "scan", "status", "nodejs-goof", "--watch", "--interval", "10ms")
	if err == nil || !strings.Contains(err.Error(), "finished with phase failed") {
		t.Fatalf("watch of a failed scan should exit non-zero, got err=%v\n%s", err, out)
	}
	if !strings.Contains(out, "goof-review-slice-2") || strings.Count(out, "STAGE") != 2 {
		t.Fatalf("watch output:\n%s", out)
	}

	out, err = runCLI(t, srv.URL, "security", "scan", "status", "nodejs-goof")
	if err != nil {
		t.Fatalf("non-watch status of a failed scan is informational: %v", err)
	}
	if !strings.Contains(out, "Phase:") || !strings.Contains(out, "failed") {
		t.Fatalf("status output:\n%s", out)
	}
	jsonOut, err := runCLI(t, srv.URL, "security", "scan", "status", "nodejs-goof", "--scan", "scan-1", "-o", "json")
	if err != nil {
		t.Fatal(err)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(jsonOut), &parsed); err != nil || len(anySliceToMaps(parsed["stages"])) != 5 {
		t.Fatalf("-o json = %s (%v)", jsonOut, err)
	}
}

func TestSecurityScanStatusFailsWithoutRuns(t *testing.T) {
	srv := jsonServer(t, map[string]any{"/api/v1/security/repositories/empty/scans": map[string]any{"items": []any{}}})
	defer srv.Close()
	_, err := runCLI(t, srv.URL, "security", "scan", "status", "empty")
	if err == nil || !strings.Contains(err.Error(), "no scan runs found") {
		t.Fatalf("error = %v", err)
	}
}
