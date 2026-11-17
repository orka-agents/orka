package controller

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/store"
)

const repositoryMonitorCILogLeakTimestamp = "2026-11-16T10:00:00.1234567Z "

// repositoryMonitorCILogLeakLog renders text as a timestamped job log that
// fails on a final ##[error] line.
func repositoryMonitorCILogLeakLog(text string) string {
	var log strings.Builder
	for line := range strings.SplitSeq(text, "\n") {
		log.WriteString(repositoryMonitorCILogLeakTimestamp + line + "\n")
	}
	log.WriteString(repositoryMonitorCILogLeakTimestamp + "##[error]Process completed with exit code 1.\n")
	return log.String()
}

func repositoryMonitorCILogLeakExcerpt(t *testing.T, log string) string {
	t.Helper()
	scanner, truncated, err := readRepositoryMonitorCILog(strings.NewReader(log), repositoryMonitorCILogReadLimit)
	if err != nil || truncated {
		t.Fatalf("readRepositoryMonitorCILog() truncated=%t err=%v", truncated, err)
	}
	excerpt, _ := scanner.excerpt(truncated)
	return repositoryMonitorCILogExcerptText(excerpt)
}

func TestRepositoryMonitorCILogExcerptWithholdsCICredentials(t *testing.T) {
	for _, tc := range repositoryMonitorEvidenceLeakCases() {
		t.Run(tc.name, func(t *testing.T) {
			text := repositoryMonitorCILogLeakExcerpt(t, repositoryMonitorCILogLeakLog(tc.input))
			if !strings.Contains(text, "##[error]Process completed") {
				t.Fatalf("excerpt = %q, want the error line", text)
			}
			for _, secret := range tc.secrets {
				if strings.Contains(text, secret) {
					t.Fatalf("log excerpt leaked %q: %q", secret, text)
				}
			}
		})
	}
}

// TestRepositoryMonitorCILogRepairPromptWithholdsCICredentials checks the
// assembled repair prompt when the credential is in a job log.
func TestRepositoryMonitorCILogRepairPromptWithholdsCICredentials(t *testing.T) {
	for _, tc := range repositoryMonitorEvidenceLeakCases() {
		t.Run(tc.name, func(t *testing.T) {
			blobs := newRepositoryMonitorCILogBlobServer(t)
			blobs.logs["/logs/951"] = repositoryMonitorCILogLeakLog(tc.input)
			github := &repositoryMonitorRepairLabelsGitHub{
				runs:            map[string][]map[string]any{"head51": {repositoryMonitorEvidenceTestRun(951, "deploy", "completed", "failure", 0)}},
				jobs:            map[int64]map[string]any{951: repositoryMonitorCILogTestJob(951, "head51")},
				jobLogLocations: map[int64]string{951: blobs.url("/logs/951")},
			}
			reconciler, monitor := repositoryMonitorEvidenceTestReconciler(t, github, "ci-log-leaks")
			monitor.Spec.Repair.CIEvidence.Logs = true
			reconciler.ciLogBlobClient = blobs.client()
			pr := repositoryMonitorPullRequest{Number: 51, HeadSHA: "head51"}
			input := reconciler.buildRepositoryMonitorRepairCIInput(context.Background(), monitor, "orka-agents/orka", pr)
			if len(input.Evidence.Checks) == 0 || input.Evidence.Checks[0].Log == nil || !strings.Contains(input.Evidence.Checks[0].Log.Excerpt, "##[error]Process completed") {
				t.Fatalf("no log excerpt: %+v", input.Evidence.Checks)
			}
			prompt, err := reconciler.buildRepositoryMonitorRepairPromptWithCI(context.Background(), monitor, repositoryMonitorCommandIntentFixCI, "orka-agents/orka", pr, &store.MonitorItem{}, &input)
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range tc.secrets {
				if strings.Contains(prompt, secret) {
					t.Fatalf("repair prompt leaked %q from the job log:\n%s", secret, prompt)
				}
			}
		})
	}
}

// Multi-line credential values whose key line the rolling window prunes
// stay withheld: a quoted value, a diff-prefixed block scalar, and a quoted
// value whose removed and added diff sides close separately.
func TestRepositoryMonitorCILogWithholdsMultilineValuesPastTheWindow(t *testing.T) {
	for _, tc := range []struct {
		name   string
		header string
		line   func(i int, fragment string) string
		footer string
		lines  int
	}{
		{name: "quoted value", header: "config:\n  password: \"violet", line: func(_ int, f string) string { return "    " + f + " meadow" },
			footer: "    amber forest\"\n  user: deploy-bot", lines: 60},
		{name: "diff-prefixed block scalar", header: "+ password: |", line: func(_ int, f string) string { return "+   " + f + " meadow" },
			footer: "  user: deploy-bot", lines: 80},
		{name: "quoted value replaced in a diff", header: " password: \"violet", line: func(i int, f string) string {
			if i == 29 {
				return "-  " + f + " meadow\""
			}
			if i < 29 {
				return "   " + f + " meadow"
			}
			return "+  " + f + " meadow"
		}, footer: "+  amber forest\"\n user: deploy-bot", lines: 60},
	} {
		t.Run(tc.name, func(t *testing.T) {
			text := tc.header + "\n"
			fragments := make([]string, 0, tc.lines)
			for i := range tc.lines {
				fragment := fmt.Sprintf("Zq%02dvT8mK3pR6wN", i)
				fragments = append(fragments, fragment)
				text += tc.line(i, fragment) + "\n"
			}
			got := repositoryMonitorCILogLeakExcerpt(t, repositoryMonitorCILogLeakLog(text+tc.footer))
			if !strings.Contains(got, "##[error]Process completed") || !strings.Contains(got, "user: deploy-bot") {
				t.Fatalf("excerpt = %q, want the error and the sibling key", got)
			}
			for _, fragment := range append(fragments, "amber forest") {
				if strings.Contains(got, fragment) {
					t.Fatalf("excerpt = %q, leaks value fragment %q", got, fragment)
				}
			}
		})
	}
}

// The text and logs opt-ins are independent: log excerpts never bring check
// text along, and check text never brings a log.
func TestRepositoryMonitorCIEvidenceTextAndLogsAreIndependent(t *testing.T) {
	for _, tc := range []struct{ text, logs bool }{{false, false}, {false, true}, {true, false}, {true, true}} {
		t.Run(fmt.Sprintf("text=%t logs=%t", tc.text, tc.logs), func(t *testing.T) {
			blobs := newRepositoryMonitorCILogBlobServer(t)
			blobs.logs["/logs/961"] = repositoryMonitorCILogLeakLog("--- FAIL: TestFromLog (0.01s)")
			run := repositoryMonitorEvidenceTestRun(961, "unit", "completed", "failure", 1)
			run["output"].(map[string]any)["summary"] = "--- FAIL: TestFromSummary (0.01s)"
			github := &repositoryMonitorRepairLabelsGitHub{
				runs:            map[string][]map[string]any{"head51": {run}},
				annotations:     map[int64][]map[string]any{961: {{"path": "a.go", "start_line": 3, "annotation_level": "failure", "message": "annotation text"}}},
				jobs:            map[int64]map[string]any{961: repositoryMonitorCILogTestJob(961, "head51")},
				jobLogLocations: map[int64]string{961: blobs.url("/logs/961")},
			}
			reconciler, monitor := repositoryMonitorEvidenceTestReconciler(t, github, "ci-text-logs")
			monitor.Spec.Repair.CIEvidence.Text, monitor.Spec.Repair.CIEvidence.Logs = tc.text, tc.logs
			reconciler.ciLogBlobClient = blobs.client()
			pr := repositoryMonitorPullRequest{Number: 51, HeadSHA: "head51"}
			input := reconciler.buildRepositoryMonitorRepairCIInput(context.Background(), monitor, "orka-agents/orka", pr)
			prompt, err := reconciler.buildRepositoryMonitorRepairPromptWithCI(context.Background(), monitor, repositoryMonitorCommandIntentFixCI, "orka-agents/orka", pr, &store.MonitorItem{}, &input)
			if err != nil {
				t.Fatal(err)
			}
			for marker, want := range map[string]bool{"TestFromSummary": tc.text, "annotation text": tc.text, "TestFromLog": tc.logs} {
				if strings.Contains(prompt, marker) != want {
					t.Fatalf("prompt has %q = %t, want %t:\n%s", marker, !want, want, prompt)
				}
			}
			if input.Evidence.TextOmitted == tc.text {
				t.Fatalf("textOmitted = %t with text=%t", input.Evidence.TextOmitted, tc.text)
			}
		})
	}
}
