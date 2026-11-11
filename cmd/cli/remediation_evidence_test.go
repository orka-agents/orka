package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/cli/client"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

func TestRemediationEvidenceRequirementFlag(t *testing.T) {
	directory := remediationTestDirectory(t)
	input := remediationTestFile(t, directory, "report.json", remediationSyntheticReport)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	for _, mode := range []string{"generate", "validate"} {
		for _, scope := range []string{"source-harness", "application-runtime"} {
			t.Run(mode+"/"+scope, func(t *testing.T) {
				var calls atomic.Int32
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var fields map[string]json.RawMessage
					require.NoError(t, json.NewDecoder(r.Body).Decode(&fields))
					require.JSONEq(t, `"`+scope+`"`, string(fields["requiredEvidenceScope"]))
					require.JSONEq(t, `"`+mode+`"`, string(fields["mode"]))
					require.JSONEq(t, remediationSyntheticReport, string(fields["report"]))
					w.Header().Set("Content-Type", "application/json")
					if scope == "application-runtime" {
						w.WriteHeader(http.StatusUnprocessableEntity)
						_, _ = w.Write([]byte(`{"error":"unsupported requirement"}`))
						return
					}
					w.WriteHeader(http.StatusAccepted)
					_, _ = w.Write([]byte(`{"id":"request-one","namespace":"testing","phase":"Pending","stage":"reproduce","reason":"accepted","evidenceScope":"source-harness"}`))
				}))
				defer server.Close()
				output, diagnostic, err := remediationCLI(t, "start", "--input", input, "--request-id", "request-one",
					"--mode", mode, "--required-evidence-scope", scope,
					"--server", server.URL, "--namespace", "testing", "--token-file", token)
				require.EqualValues(t, 1, calls.Load(), "unsupported evidence must not trigger resubmission")
				require.Contains(t, output, `"requestID":"request-one"`)
				require.Empty(t, diagnostic)
				if scope == "application-runtime" {
					require.ErrorContains(t, err, "does not support the required evidence scope")
					require.ErrorContains(t, err, "no run was admitted")
				} else {
					require.NoError(t, err)
					require.Contains(t, output, `"evidenceScope":"source-harness"`)
				}
			})
		}
	}
	for _, value := range []string{"", "runtime", "Source-Harness", "source-harness "} {
		_, _, err := remediationCLI(t, "start", "--input", input, "--required-evidence-scope", value)
		require.ErrorContains(t, err, "--required-evidence-scope must be")
	}
}

func TestRemediationEvidenceScopeStatusAndPatchLabels(t *testing.T) {
	for _, tc := range []struct {
		scope contract.EvidenceScope
		label string
	}{
		{contract.SourceHarness, "Patch saved (verified source-harness checks only; not application-runtime evidence)."},
		{contract.ApplicationRuntime, "Patch saved (verified application-runtime checks only)."},
		{"", "Patch saved (verified checks; evidence scope unrecorded)."},
	} {
		t.Run(string(tc.scope), func(t *testing.T) {
			status := &contract.Status{ID: "request-one", Phase: contract.PhaseSucceeded, Stage: contract.StageComplete,
				Outcome: contract.OutcomeVerified, Reason: "verified", PatchReady: true, EvidenceScope: tc.scope}
			require.Equal(t, tc.label, remediationPatchLabel(status))
			var output bytes.Buffer
			require.NoError(t, printRemediationStatusTo(&output, status))
			var fields map[string]json.RawMessage
			require.NoError(t, json.Unmarshal(output.Bytes(), &fields))
			if tc.scope == "" {
				require.NotContains(t, fields, "evidenceScope", "legacy output must not invent a scope")
			} else {
				require.JSONEq(t, `"`+string(tc.scope)+`"`, string(fields["evidenceScope"]))
			}
			status.Phase = contract.PhaseRunning
			require.Equal(t, "Candidate patch saved (not verified).", remediationPatchLabel(status))
		})
	}
	var output bytes.Buffer
	require.Error(t, printRemediationStatusTo(&output, &contract.Status{ID: "request-one", EvidenceScope: "future-scope"}))
	require.Empty(t, output.String())
}

func TestRemediationClientRejectsEvidenceDowngrade(t *testing.T) {
	for _, tc := range []struct {
		required contract.EvidenceScope
		response string
	}{
		{contract.ApplicationRuntime, `,"evidenceScope":"source-harness"`},
		{contract.ApplicationRuntime, ""},
		{contract.SourceHarness, ""},
		{contract.SourceHarness, `,"evidenceScope":"future-scope"`},
		{contract.SourceHarness, `,"evidenceScope":null`},
	} {
		t.Run(string(tc.required)+"/"+tc.response, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusAccepted)
				_, _ = w.Write([]byte(`{"id":"request-one","namespace":"testing","phase":"Pending","stage":"reproduce"` + tc.response + `}`))
			}))
			defer server.Close()
			base, err := url.Parse(server.URL)
			require.NoError(t, err)
			api := &remediationClient{baseURL: base, namespace: "testing", http: server.Client()}
			_, err = api.statusRequest(t.Context(), http.MethodPost, "", "request-one",
				&contract.Request{RequestID: "request-one", RequiredEvidenceScope: tc.required})
			require.Error(t, err)
		})
	}
}

func TestRemediationWaitingFencesEvidenceScope(t *testing.T) {
	for _, scope := range []contract.EvidenceScope{"", contract.ApplicationRuntime} {
		t.Run(string(scope), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				require.NoError(t, json.NewEncoder(w).Encode(contract.Status{
					ID: "request-one", Namespace: "testing", Phase: contract.PhaseSucceeded, Stage: contract.StageComplete,
					Outcome: contract.OutcomeVerified, PatchReady: true, EvidenceScope: scope,
				}))
			}))
			defer server.Close()
			base, err := url.Parse(server.URL)
			require.NoError(t, err)
			api := &remediationClient{baseURL: base, namespace: "testing", http: server.Client()}
			_, err = waitRemediation(t.Context(), &cobra.Command{}, api, &contract.Status{
				ID: "request-one", Phase: contract.PhasePending, EvidenceScope: contract.SourceHarness,
			}, time.Millisecond)
			require.ErrorContains(t, err, "evidence scope changed")
		})
	}
}

func TestRemediationTaskResultFencesEvidenceScope(t *testing.T) {
	status := &contract.Status{
		ID: "request-one", Namespace: "testing", Phase: contract.PhaseSucceeded, Stage: contract.StageComplete,
		Outcome: contract.OutcomeVerified, PatchReady: true, TaskName: "remediation-parent", TaskUID: "parent-uid",
		EvidenceScope: contract.SourceHarness,
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/result"):
			w.Header().Set("Content-Type", "application/json")
			require.NoError(t, json.NewEncoder(w).Encode(client.TaskResultResponse{
				Result: remediationAggregateFixture(t, contract.OutcomeVerified),
			}))
		case r.URL.Path == "/api/v1/tasks/remediation-parent":
			remediationResultTask(t, w, "parent-uid")
		case r.URL.Path == "/api/v1/remediations/request-one":
			w.Header().Set("Content-Type", "application/json")
			changed := *status
			changed.EvidenceScope = contract.ApplicationRuntime
			require.NoError(t, json.NewEncoder(w).Encode(changed))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	base, err := url.Parse(server.URL)
	require.NoError(t, err)
	api := &remediationClient{baseURL: base, namespace: "testing", http: server.Client()}
	var output bytes.Buffer
	command := &cobra.Command{}
	command.SetOut(&output)
	require.Error(t, printRemediationTaskResult(t.Context(), command, api, status))
	require.Empty(t, output.String())
}
