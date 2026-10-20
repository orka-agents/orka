package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/yaml"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/usage"
)

func usageCLIFixture() (usage.Report, usage.Work, usage.OtherWork) {
	start := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	asOf := start.Add(17*24*time.Hour + 10*time.Hour)
	totals := usage.Totals{InputTokens: 100, TotalTokens: 100, Measurements: 2, ReportedMeasurements: 1, MissingMeasurements: 1,
		Calls: 1, Attempts: 1, Completeness: "partial", ModelCost: "Price unavailable", CachedUsageReported: true}
	summary := usage.Summary{Totals: totals, WorkRequests: 1, PRsOpened: 1, PRsReady: 1, TokensPerPROpened: new(float64(100))}
	tasks := []usage.Task{
		{UsageTask: store.UsageTask{Namespace: "payments", TaskUID: "task-uid", TaskName: "implementation", Role: "implementation", Phase: "Succeeded", SessionName: "session-1"},
			Totals: usage.Totals{InputTokens: 100, TotalTokens: 100, Measurements: 1, ReportedMeasurements: 1, Completeness: "complete"},
			Measurements: []usage.Measurement{{ID: "call-1", AttemptID: "attempt-1", Scope: "call", Source: "provider", Provider: "provider-1", Model: "model-1",
				InputTokens: new(int64(100)), OutputTokens: new(int64(0)), CachedInputTokens: new(int64(0)), Completeness: "complete", Status: "completed", ObservedAt: asOf}}},
		{UsageTask: store.UsageTask{Namespace: "payments", TaskUID: "review-uid", TaskName: "review", Role: "review", Phase: "Failed"}, Shared: true,
			Totals:       usage.Totals{Measurements: 1, MissingMeasurements: 1, Completeness: "unavailable"},
			Measurements: []usage.Measurement{{ID: "attempt-2", Scope: "attempt", Source: "agent", Completeness: "unavailable", Status: "failed", Gap: "No consumed-token counts reported", ObservedAt: asOf}}},
	}
	work := usage.Work{UsageWorkRequest: store.UsageWorkRequest{ID: "work-123", Namespace: "payments", Repository: "org/repo", Kind: "issue", Number: 123, StartedAt: start},
		Summary: summary, Tasks: tasks, PullRequests: []usage.PullRequest{{UsagePullRequest: store.UsagePullRequest{Repository: "org/repo", Number: 42,
			State: "OPEN", Ready: true, HeadSHA: "revision-42", URL: "https://github.com/org/repo/pull/42", ReadinessReason: "Current head is ready"}, Origin: "created"}}}
	other := usage.OtherWork{Category: "unassociated", Explanation: "Usage without a verified work reference", Totals: totals, TaskCount: 2,
		Tasks: tasks[:1], Page: &usage.Page{Limit: 1, Offset: 0, Total: 2}}
	workRow := work
	workRow.Tasks, workRow.PullRequests = nil, nil
	combined := summary
	combined.WorkRequests = 2
	combined.InputTokens, combined.TotalTokens = 250, 250
	combined.TokensPerPROpened = new(float64(250))
	report := usage.Report{Selection: store.UsageFilter{Namespaces: []string{"payments"}, From: start, Until: start.AddDate(0, 1, 0), AsOf: asOf},
		Summary: combined, Teams: []usage.Team{{Namespace: "payments", Summary: combined}}, Works: []usage.Work{workRow},
		OtherWork: []usage.OtherWork{other}, Page: &usage.Page{Limit: 1, Offset: 0, Total: 2}, RetainedSince: &start}
	return report, work, other
}

func usageCLIPayload(view string) any {
	report, work, other := usageCLIFixture()
	switch view {
	case "work":
		return map[string]any{"selection": report.Selection, "work": work}
	case "other":
		return map[string]any{"selection": report.Selection, "otherWork": other}
	default:
		return report
	}
}

func runUsageCLI(t *testing.T, server string, args ...string) (string, error) {
	t.Helper()
	root := newRootCmd()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs(append([]string{"--server", server, "--namespace", "payments", "--token", "usage-test-auth", "--txn-token", "usage-test-context", "usage"}, args...))
	err := root.Execute()
	require.NotContains(t, output.String(), "usage-test-auth")
	require.NotContains(t, output.String(), "usage-test-context")
	return output.String(), err
}

func TestUsageCLIRequestsPreserveFiltersAndAuthentication(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name, path string
		args       []string
		paged      bool
	}{
		{"summary", "/api/v1/usage", []string{"summary"}, true},
		{"work", "/api/v1/usage/work/work%20with%2Freserved%3Fcharacters", []string{"work", "work with/reserved?characters"}, false},
		{"other", "/api/v1/usage/other/unassociated", []string{"other", "unassociated"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(chan url.Values, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet || r.URL.EscapedPath() != tc.path {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
				}
				if r.Header.Get("Authorization") != "Bearer usage-test-auth" || r.Header.Get("Txn-Token") != "usage-test-context" {
					t.Error("request did not preserve CLI authentication")
				}
				seen <- r.URL.Query()
				_ = json.NewEncoder(w).Encode(usageCLIPayload(tc.name))
			}))
			defer server.Close()
			args := append(tc.args, "--from", "2026-09-01", "--until", "2026-10-01", "--as-of", "2026-09-18T10:00:00Z",
				"--teams", "payments,platform", "--repository", "org/repo", "--model", "model-1", "--kind", "issue", "-o", "json")
			expected := url.Values{"namespace": {"payments"}, "from": {"2026-09-01"}, "until": {"2026-10-01"}, "asOf": {"2026-09-18T10:00:00Z"},
				"teams": {"payments,platform"}, "repository": {"org/repo"}, "model": {"model-1"}, "kind": {"issue"}}
			if tc.paged {
				args = append(args, "--limit", "100", "--offset", "25")
				expected["limit"], expected["offset"] = []string{"100"}, []string{"25"}
			}
			_, err := runUsageCLI(t, server.URL, args...)
			require.NoError(t, err)
			require.Equal(t, expected, <-seen)
		})
	}
}

func TestUsageCLITablesExposeCoverageDetailsAndPagination(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		view string
		args []string
		want []string
	}{
		{"summary", []string{"summary"}, []string{"TOTAL", "payments", "100", "partial", "work-123", "issue #123", "unassociated", "Price unavailable", "ESTIMATED TOKENS", "total 2", "--offset 1 --as-of 2026-09-18T10:00:00Z", "Inactive cohorts"}},
		{"work", []string{"work", "work-123"}, []string{"implementation", "Succeeded", "session-1", "attempt-1", "model-1", "provider", "revision-42", "created", "Current head is ready", "Task: payments/review", "SHARED", "true", "No consumed-token counts reported", "Unavailable"}},
		{"other", []string{"other", "unassociated"}, []string{"Usage without a verified work reference", "Tasks: 1 shown", "total 2", "--offset 1 --as-of 2026-09-18T10:00:00Z", "attempt-1", "CACHE WRITE", "Unavailable"}},
	} {
		t.Run(tc.view, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(usageCLIPayload(tc.view))
			}))
			defer server.Close()
			output, err := runUsageCLI(t, server.URL, tc.args...)
			require.NoError(t, err)
			for _, want := range tc.want {
				require.Contains(t, output, want)
			}
			if tc.view == "summary" {
				// The API total includes usage outside the displayed work page.
				require.Regexp(t, `(?m)^TOTAL +2 +250 +1 +1 +0 +250 +Unavailable +partial$`, output)
			} else {
				found := false
				for line := range strings.SplitSeq(output, "\n") {
					if strings.HasPrefix(line, "call-1 ") {
						found = true
						require.Equal(t, []string{"call-1", "attempt-1", "provider-1", "model-1", "provider", "call", "100", "0", "0", "Unavailable", "completed", "complete"}, strings.Fields(line))
					}
				}
				require.True(t, found, "missing measurement row")
			}
		})
	}
}

func TestUsageCLIOutputPreservesExactCountsNullsAndFullResponse(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			report, _, _ := usageCLIFixture()
			report.Summary.TotalTokens = store.MaxUsageTokenCount
			encoded, err := json.Marshal(report)
			require.NoError(t, err)
			var expected map[string]any
			require.NoError(t, json.Unmarshal(encoded, &expected))
			expected["futureField"] = map[string]any{"retained": true}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(expected)
			}))
			defer server.Close()
			output, err := runUsageCLI(t, server.URL, "summary", "-o", format)
			require.NoError(t, err)
			require.Contains(t, output, "9007199254740991")
			var got map[string]any
			if format == "yaml" {
				require.NoError(t, yaml.Unmarshal([]byte(output), &got))
			} else {
				require.NoError(t, json.Unmarshal([]byte(output), &got))
			}
			require.Equal(t, expected, got)
		})
	}
}

func TestUsageCLITablesPreserveZeroUnavailableAndExactLimit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, tc := range []struct {
		name, want string
		totals     usage.Totals
	}{
		{"zero", "0", usage.Totals{Measurements: 1, ReportedMeasurements: 1, Completeness: "complete"}},
		{"missing", "Unavailable", usage.Totals{Measurements: 1, MissingMeasurements: 1, Completeness: "unavailable"}},
		{"no calls", "No model calls", usage.Totals{Completeness: "unavailable"}},
		{"maximum", "9007199254740991", usage.Totals{Measurements: 1, ReportedMeasurements: 1, Completeness: "complete", TotalTokens: store.MaxUsageTokenCount}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			report, _, _ := usageCLIFixture()
			report.Summary = usage.Summary{Totals: tc.totals}
			report.Teams, report.Works, report.OtherWork = nil, nil, nil
			report.Page = &usage.Page{Limit: 25}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_ = json.NewEncoder(w).Encode(report)
			}))
			defer server.Close()
			output, err := runUsageCLI(t, server.URL, "summary")
			require.NoError(t, err)
			var totalLine string
			for line := range strings.SplitSeq(output, "\n") {
				if strings.HasPrefix(line, "TOTAL ") {
					totalLine = line
				}
			}
			// Check the token column and both unavailable ratios independently.
			require.Equal(t, strings.Fields("TOTAL 0 "+tc.want+" 0 0 0 Unavailable Unavailable "+tc.totals.Completeness), strings.Fields(totalLine))
			require.NotContains(t, output, "Next page:")
		})
	}
}

func TestUsageCLITablesSanitizeUntrustedTerminalText(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	report, work, _ := usageCLIFixture()
	work.Repository = "org/repo\x1b[31m"
	work.Tasks[1].Measurements[0].Gap = "gap\nforged row\x1b]0;title\a"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"selection": report.Selection, "work": work})
	}))
	defer server.Close()
	output, err := runUsageCLI(t, server.URL, "work", work.ID)
	require.NoError(t, err)
	require.NotContains(t, output, "\x1b")
	require.NotContains(t, output, "\a")
	require.NotContains(t, output, "\nforged row")
}

func TestUsageCLIRejectsInvalidArgumentsBeforeRequest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		t.Error("invalid arguments issued an API request")
	}))
	defer server.Close()
	for _, args := range [][]string{
		{"summary", "unexpected"}, {"work"}, {"other", "invalid"}, {"other", "unassociated", "extra"},
		{"summary", "--limit", "0"}, {"other", "review_only", "--offset", "-1"}, {"summary", "-o", "invalid"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			_, err := runUsageCLI(t, server.URL, args...)
			require.Error(t, err)
		})
	}
}

func TestUsageCLIReturnsAPIErrorsWithoutReport(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, status := range []int{http.StatusForbidden, http.StatusNotFound, http.StatusUnprocessableEntity} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "usage selection rejected", status)
			}))
			defer server.Close()
			for _, args := range [][]string{{"summary"}, {"work", "work-123"}, {"other", "unassociated"}} {
				output, err := runUsageCLI(t, server.URL, args...)
				require.ErrorContains(t, err, fmt.Sprintf("HTTP %d", status))
				require.ErrorContains(t, err, "usage selection rejected")
				require.Empty(t, output)
			}
		})
	}
}

func TestUsageBinary(t *testing.T) {
	// Exercise the actual command registration, config, formatting, and exit
	// status against a read-only API fixture, without a Kubernetes cluster.
	binary := filepath.Join(t.TempDir(), "orka")
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, ".")
	buildOutput, err := build.CombinedOutput()
	require.NoError(t, err, string(buildOutput))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.Header.Get("Authorization") != "Bearer usage-binary-fixture" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		switch r.URL.Path {
		case "/api/v1/usage":
			_ = json.NewEncoder(w).Encode(usageCLIPayload("summary"))
		case "/api/v1/usage/work/work-123":
			_ = json.NewEncoder(w).Encode(usageCLIPayload("work"))
		case "/api/v1/usage/other/unassociated":
			_ = json.NewEncoder(w).Encode(usageCLIPayload("other"))
		default:
			http.Error(w, "work request not found", http.StatusNotFound)
		}
	}))
	defer server.Close()
	cliHome := t.TempDir()
	require.NoError(t, os.Mkdir(filepath.Join(cliHome, ".orka"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(cliHome, ".orka", "config.yaml"),
		[]byte(fmt.Sprintf("server: %s\nnamespace: payments\ntoken: usage-binary-fixture\n", server.URL)), 0o600))
	t.Setenv("HOME", cliHome)
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"usage", "summary"}, "TOKENS/MERGED"},
		{[]string{"usage", "work", "work-123", "-o", "json"}, `"attemptID": "attempt-1"`},
		{[]string{"usage", "other", "unassociated", "-o", "yaml"}, "category: unassociated"},
	} {
		command := exec.CommandContext(t.Context(), binary, tc.args...)
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		require.Contains(t, string(output), tc.want)
		require.NotContains(t, string(output), "usage-binary-fixture")
	}
	failed := exec.CommandContext(t.Context(), binary, "usage", "work", "missing")
	output, err := failed.CombinedOutput()
	require.Error(t, err)
	require.Contains(t, string(output), "HTTP 404")
	require.NotContains(t, string(output), "usage-binary-fixture")
}
