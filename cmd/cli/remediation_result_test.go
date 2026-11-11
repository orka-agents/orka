package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/workers/common"
)

func remediationResultStatus(t *testing.T, w http.ResponseWriter, code int, phase, outcome, reason string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"id": "request-one", "namespace": "testing", "phase": phase, "stage": contract.StageComplete,
		"outcome": outcome, "reason": reason, "patchReady": true,
		"taskName": "remediation-parent", "taskUID": "parent-uid",
		"report": "synthetic-private-report", "credential": "synthetic-private-auth",
	}))
}

func remediationResultTask(t *testing.T, w http.ResponseWriter, uid string) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	require.NoError(t, json.NewEncoder(w).Encode(map[string]any{
		"metadata": map[string]any{"name": "remediation-parent", "namespace": "testing", "uid": uid},
		"spec":     map[string]any{"prompt": "synthetic-private-report", "credentials": "synthetic-private-auth"},
		"status":   map[string]any{"resultRef": map[string]any{"available": true}},
	}))
}

func remediationAggregateFixture(t *testing.T, outcome string) string {
	t.Helper()
	feedback, diff := "original: bug reproduced; candidate: parser regression failed", remediationSyntheticPatch
	switch outcome {
	case contract.OutcomeVerified:
		feedback = "original: bug reproduced; candidate: required checks passed"
	case contract.OutcomeReproduced:
		feedback, diff = "original: bug reproduced; no candidate was requested", ""
	case contract.OutcomeUnableToValidate:
		feedback, diff = "original: execution unavailable; no candidate was generated", ""
	}
	result, err := common.FormatStructuredResult(&common.StructuredResult{
		Version: 1, Summary: "Remediation role tasks recorded their final outcome and feedback.",
		Verdict: outcome, Feedback: feedback,
		Diff: diff, BaseSHA: strings.Repeat("a", 40),
	})
	require.NoError(t, err)
	return string(result)
}

func TestRemediationStatusShowsSafeOutcomeAndTaskReference(t *testing.T) {
	directory := remediationTestDirectory(t)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/remediations/request-one", r.URL.Path)
		remediationResultStatus(t, w, http.StatusOK, contract.PhaseFailed, contract.OutcomeUnverified, "candidate-regression")
	}))
	defer server.Close()
	output, diagnostic, err := remediationCLI(t, "status", "request-one", "--server", server.URL, "--namespace", "testing", "--token-file", token)
	require.NoError(t, err)
	require.Empty(t, diagnostic)
	var status map[string]any
	require.NoError(t, json.Unmarshal([]byte(output), &status))
	require.Equal(t, contract.OutcomeUnverified, status["outcome"])
	require.Equal(t, "candidate-regression", status["reason"])
	require.Equal(t, "remediation-parent", status["taskName"])
	require.Equal(t, "parent-uid", status["taskUID"])
	require.NotContains(t, output, "synthetic-private")
}

func TestRemediationWaitUsesNormalTaskResultOutput(t *testing.T) {
	directory := remediationTestDirectory(t)
	input := remediationTestFile(t, directory, "report.json", remediationSyntheticReport)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	for _, test := range []struct {
		mode    contract.Mode
		phase   string
		outcome string
		reason  string
		failed  bool
	}{
		{contract.Generate, contract.PhaseFailed, contract.OutcomeUnverified, "candidate-regression", true},
		{contract.Generate, contract.PhaseSucceeded, contract.OutcomeVerified, "verified", false},
		{contract.Validate, contract.PhaseSucceeded, contract.OutcomeReproduced, "report-reproduced", false},
		{contract.Validate, contract.PhaseNeedsInput, contract.OutcomeUnableToValidate, "execution-unavailable", true},
	} {
		t.Run(test.outcome, func(t *testing.T) {
			result := remediationAggregateFixture(t, test.outcome)
			var mu sync.Mutex
			var calls []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				require.Equal(t, "Bearer synthetic-private-token", r.Header.Get("Authorization"))
				require.Empty(t, r.Header.Get("Txn-Token"))
				require.Equal(t, "testing", r.URL.Query().Get("namespace"))
				require.Equal(t, "no-store", r.Header.Get("Cache-Control"))
				mu.Lock()
				calls = append(calls, r.Method+" "+r.URL.Path)
				mu.Unlock()
				switch r.URL.Path {
				case "/api/v1/remediations":
					require.Equal(t, http.MethodPost, r.Method)
					remediationResultStatus(t, w, http.StatusAccepted, test.phase, test.outcome, test.reason)
				case "/api/v1/remediations/request-one":
					require.Equal(t, http.MethodGet, r.Method)
					remediationResultStatus(t, w, http.StatusOK, test.phase, test.outcome, test.reason)
				case "/api/v1/tasks/remediation-parent":
					remediationResultTask(t, w, "parent-uid")
				case "/api/v1/tasks/remediation-parent/result":
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(client.TaskResultResponse{Result: result}))
				default:
					t.Errorf("unexpected output route %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			output, diagnostic, err := remediationCLI(t, "start", "--input", input, "--mode", string(test.mode),
				"--request-id", "request-one", "--wait", "--server", server.URL, "--namespace", "testing", "--token-file", token)
			if test.failed {
				require.ErrorContains(t, err, test.outcome)
				require.ErrorContains(t, err, test.reason)
			} else {
				require.NoError(t, err)
			}
			normalCommand := newTaskResultCmd()
			var expected bytes.Buffer
			normalCommand.SetOut(&expected)
			require.NoError(t, printTaskResult(normalCommand, &client.TaskResultResponse{Result: result}))
			require.Equal(t, expected.String(), output)
			require.Contains(t, diagnostic, `"requestID":"request-one"`)
			require.Contains(t, diagnostic, `"outcome":"`+test.outcome+`"`)
			require.Contains(t, diagnostic, `"reason":"`+test.reason+`"`)
			require.NotContains(t, output+diagnostic, "synthetic-private")
			mu.Lock()
			require.Equal(t, []string{
				"POST /api/v1/remediations",
				"GET /api/v1/tasks/remediation-parent",
				"GET /api/v1/tasks/remediation-parent/result",
				"GET /api/v1/tasks/remediation-parent",
				"GET /api/v1/remediations/request-one",
			}, calls)
			mu.Unlock()
		})
	}
}

func TestRemediationTaskOutputNeverPrintsDeniedOrReplacedResults(t *testing.T) {
	directory := remediationTestDirectory(t)
	input := remediationTestFile(t, directory, "report.json", remediationSyntheticReport)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	for _, failure := range []string{
		"denied", "wrong-initial-uid", "replaced-uid", "deleted-parent", "conflicting-final-fence",
		"changed-fence-uid", "changed-fence-name", "changed-fence-outcome", "nonterminal-fence",
	} {
		t.Run(failure, func(t *testing.T) {
			var mu sync.Mutex
			taskReads, resultReads := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				mu.Lock()
				defer mu.Unlock()
				switch r.URL.Path {
				case "/api/v1/remediations":
					remediationResultStatus(t, w, http.StatusAccepted, contract.PhaseFailed, contract.OutcomeUnverified, "candidate-regression")
				case "/api/v1/tasks/remediation-parent":
					taskReads++
					if failure == "denied" {
						w.WriteHeader(http.StatusForbidden)
						_, _ = w.Write([]byte("synthetic-private-auth"))
						return
					}
					uid := "parent-uid"
					if failure == "wrong-initial-uid" || (failure == "replaced-uid" && taskReads > 1) {
						uid = "replacement-uid"
					}
					remediationResultTask(t, w, uid)
				case "/api/v1/tasks/remediation-parent/result":
					resultReads++
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(client.TaskResultResponse{Result: "synthetic-private-stale-result"}))
				case "/api/v1/remediations/request-one":
					if strings.HasPrefix(failure, "changed-fence-") || failure == "nonterminal-fence" {
						status := map[string]any{
							"id": "request-one", "namespace": "testing", "phase": contract.PhaseFailed,
							"stage": contract.StageComplete, "outcome": contract.OutcomeUnverified,
							"taskName": "remediation-parent", "taskUID": "parent-uid",
						}
						switch failure {
						case "changed-fence-uid":
							status["taskUID"] = "replacement-uid"
						case "changed-fence-name":
							status["taskName"] = "replacement-parent"
						case "changed-fence-outcome":
							status["outcome"] = contract.OutcomeUnableToVerify
						case "nonterminal-fence":
							status["phase"] = contract.PhaseRunning
						}
						w.Header().Set("Content-Type", "application/json")
						require.NoError(t, json.NewEncoder(w).Encode(status))
						return
					}
					code := http.StatusNotFound
					if failure == "conflicting-final-fence" {
						code = http.StatusConflict
					}
					w.WriteHeader(code)
					_, _ = w.Write([]byte("synthetic-private-authoritative-error"))
				default:
					t.Errorf("unexpected request %s", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			output, diagnostic, err := remediationCLI(t, "start", "--input", input, "--request-id", "request-one",
				"--wait", "--server", server.URL, "--namespace", "testing", "--token-file", token)
			require.Error(t, err)
			require.Empty(t, output)
			require.Contains(t, diagnostic, `"reason":"candidate-regression"`)
			require.NotContains(t, diagnostic+err.Error(), "synthetic-private")
			mu.Lock()
			if failure == "denied" || failure == "wrong-initial-uid" {
				require.Zero(t, resultReads)
			} else {
				require.Equal(t, 1, resultReads)
			}
			mu.Unlock()
		})
	}
}

func TestRemediationTaskResultReaderBoundsAndFormats(t *testing.T) {
	for _, test := range []struct {
		name, contentType, body string
		chunked                 bool
	}{
		{"html", "text/html", "<html>synthetic-private-result</html>", false},
		{"missing-result", "application/json", `{"report":"synthetic-private-report"}`, false},
		{"oversized-declared", "application/json", strings.Repeat("x", remediationMaxTaskWireBytes+1), false},
		{"oversized-streamed", "application/json", strings.Repeat("x", remediationMaxTaskWireBytes+1), true},
		{"oversized-decoded", "application/json", `{"result":"` + strings.Repeat("x", remediationMaxTaskResultBytes+1) + `"}`, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !strings.HasSuffix(r.URL.Path, "/result") {
					remediationResultTask(t, w, "parent-uid")
					return
				}
				w.Header().Set("Content-Type", test.contentType)
				if test.chunked {
					w.(http.Flusher).Flush()
				} else {
					w.Header().Set("Content-Length", strconv.Itoa(len(test.body)))
				}
				_, _ = w.Write([]byte(test.body))
			}))
			defer server.Close()
			base, err := url.Parse(server.URL)
			require.NoError(t, err)
			api := &remediationClient{baseURL: base, namespace: "testing", token: "synthetic-private-token", http: server.Client()}
			result, err := api.parentTaskResult(t.Context(), "remediation-parent", "parent-uid")
			require.Error(t, err)
			require.Nil(t, result)
			require.NotContains(t, err.Error(), "synthetic-private")
		})
	}
}

func TestRemediationTaskOutputMissingReferenceKeepsFailureReason(t *testing.T) {
	directory := remediationTestDirectory(t)
	input := remediationTestFile(t, directory, "report.json", remediationSyntheticReport)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "/api/v1/remediations", r.URL.Path)
		remediationReply(w, http.StatusAccepted, "request-one", contract.PhaseFailed, contract.StageComplete, contract.OutcomeUnverified, true)
	}))
	defer server.Close()
	output, diagnostic, err := remediationCLI(t, "start", "--input", input, "--request-id", "request-one",
		"--wait", "--server", server.URL, "--namespace", "testing", "--token-file", token)
	require.ErrorContains(t, err, "Unverified")
	require.ErrorContains(t, err, "parent Task")
	require.Empty(t, output)
	require.Contains(t, diagnostic, `"outcome":"Unverified"`)
}

func TestRemediationWaitRejectsChangedParentReference(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		require.NoError(t, json.NewEncoder(w).Encode(contract.Status{
			ID: "request-one", Namespace: "testing", TaskName: "remediation-parent", TaskUID: "replacement-uid",
			Phase: contract.PhaseFailed, Stage: contract.StageComplete, Outcome: contract.OutcomeUnverified,
		}))
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	require.NoError(t, err)
	api := &remediationClient{baseURL: base, namespace: "testing", token: "synthetic-private-token", http: server.Client()}
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	command.SetErr(&output)
	_, err = waitRemediation(t.Context(), command, api, &contract.Status{
		ID: "request-one", Namespace: "testing", TaskName: "remediation-parent", TaskUID: "parent-uid",
		Phase: contract.PhaseRunning, Stage: contract.StageVerify,
	}, time.Millisecond)
	require.ErrorContains(t, err, "identity changed")
	require.Empty(t, output.String())
}

func TestRemediationResultDestinationFlagUnchanged(t *testing.T) {
	command := newRemediationDownloadCmd()
	destination := filepath.Join("private", "candidate.patch")
	require.NoError(t, command.Flags().Set("output", destination))
	value, err := command.Flags().GetString("output")
	require.NoError(t, err)
	require.Equal(t, destination, value)
}
