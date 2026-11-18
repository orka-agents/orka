package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/remediation/contract"
)

const remediationSyntheticReport = `{"title":"Synthetic quantity check","problem":"Synthetic negative quantity","repository":"https://github.com/example/synthetic","ref":"v1.0.0"}`

const remediationSyntheticPatch = "diff --git a/quantity.txt b/quantity.txt\n" +
	"--- a/quantity.txt\n+++ b/quantity.txt\n@@ -1 +1 @@\n-synthetic-old-value\n+synthetic-new-value\n"

func remediationTestDirectory(t *testing.T) string {
	t.Helper()
	directory := ".remediation-test-" + uuid.NewString()
	require.NoError(t, os.Mkdir(directory, 0700))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(directory)) })
	absolute, err := filepath.Abs(directory)
	require.NoError(t, err)
	t.Setenv("HOME", absolute)
	return directory
}

func remediationTestFile(t *testing.T, directory, name, content string) string {
	t.Helper()
	filename := filepath.Join(directory, name)
	require.NoError(t, os.WriteFile(filename, []byte(content), 0600))
	return filename
}

func remediationCLI(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	command := newRootCmd()
	var output, diagnostic bytes.Buffer
	command.SetOut(&output)
	command.SetErr(&diagnostic)
	command.SetArgs(append([]string{"remediate"}, args...))
	err := command.Execute()
	return output.String(), diagnostic.String(), err
}

func remediationReply(w http.ResponseWriter, code int, id, phase, stage, outcome string, ready bool) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(contract.Status{
		ID: id, Namespace: "testing", Phase: phase, Stage: stage,
		Outcome: outcome, Reason: "synthetic-safe-reason", PatchReady: ready,
	})
}

func TestRemediationCommandSurface(t *testing.T) {
	command := newRemediationCmd()
	names := make([]string, 0, len(command.Commands()))
	for _, child := range command.Commands() {
		names = append(names, child.Name())
		for _, forbidden := range []string{"agent", "model", "provider", "source-dir", "state-dir", "input-pod", "input-root", "approve-plan", "tool-image", "publish", "retry"} {
			require.Nil(t, child.Flags().Lookup(forbidden))
		}
	}
	require.ElementsMatch(t, []string{"start", "status", "cancel", "download"}, names)
	for _, args := range [][]string{
		{"start"}, {"start", "unexpected"}, {"status"}, {"status", "run", "extra"},
		{"cancel"}, {"download", "run"}, {"status", " run"}, {"status", "../run"},
		{"ingest"}, {"plan"}, {"generate"}, {"validate"}, {"verify"},
	} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			output, _, err := remediationCLI(t, args...)
			require.Error(t, err)
			require.Empty(t, output)
		})
	}
}

func TestRemediationRequiredInputsAndModes(t *testing.T) {
	directory := remediationTestDirectory(t)
	patch := remediationTestFile(t, directory, "candidate.patch", remediationSyntheticPatch)
	password := "SYNTHETIC_PASSWORD_" + rand.Text()
	credentials := url.UserPassword("SYNTHETIC_USER_"+rand.Text(), password)
	for _, tc := range []struct {
		name, report, mode, patch string
	}{
		{"missing-title", `{"problem":"synthetic","repository":"https://github.com/example/synthetic","ref":"v1"}`, "generate", ""},
		{"missing-problem", `{"title":"synthetic","repository":"https://github.com/example/synthetic","ref":"v1"}`, "generate", ""},
		{"missing-repository", `{"title":"synthetic","problem":"synthetic","ref":"v1"}`, "generate", ""},
		{"missing-ref", `{"title":"synthetic","problem":"synthetic","repository":"https://github.com/example/synthetic"}`, "generate", ""},
		{"version-not-ref", `{"title":"synthetic","problem":"synthetic","repository":"https://github.com/example/synthetic","version":"v1"}`, "generate", ""},
		{"credential-url", strings.Replace(remediationSyntheticReport, "https://github.com", (&url.URL{
			Scheme: "https",
			Host:   "github.com",
			User:   credentials,
		}).String(), 1), "generate", ""},
		{"local-source", strings.Replace(remediationSyntheticReport, "https://github.com/example/synthetic", "./source", 1), "generate", ""},
		{"duplicate-ref", strings.TrimSuffix(remediationSyntheticReport, "}") + `,"ref":"other"}`, "generate", ""},
		{"case-collision", strings.TrimSuffix(remediationSyntheticReport, "}") + `,"REF":"other"}`, "generate", ""},
		{"trailing-json", remediationSyntheticReport + `{}`, "generate", ""},
		{"not-object", `[]`, "generate", ""},
		{"invalid-mode", remediationSyntheticReport, "retry", ""},
		{"unsupported-verify-mode", remediationSyntheticReport, "verify", ""},
		{"generate-rejects-patch", remediationSyntheticReport, "generate", patch},
		{"validate-rejects-patch", remediationSyntheticReport, "validate", patch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := remediationTestFile(t, directory, tc.name+".json", tc.report)
			if tc.patch != "" {
				_, _, err := remediationCLI(t, "start", "--input", input, "--mode", tc.mode, "--patch", tc.patch)
				require.ErrorContains(t, err, "invalid remediation flags")
				return
			}
			_, err := readRemediationInput(input, contract.Mode(tc.mode), "request-one")
			require.Error(t, err)
			require.NotContains(t, err.Error(), credentials.Username())
			require.NotContains(t, err.Error(), password)
		})
	}
	description := remediationTestFile(t, directory, "description.json", strings.Replace(remediationSyntheticReport, `"problem":`, `"description":`, 1))
	request, err := readRemediationInput(description, contract.Validate, "request-one")
	require.NoError(t, err)
	require.Equal(t, "https://github.com/example/synthetic", request.Repository)
	require.Equal(t, "v1.0.0", request.Ref)
	for _, id := range []string{" request-one", "Request-One", strings.Repeat("r", 64), "x..y"} {
		_, err := readRemediationInput(description, contract.Validate, id)
		require.Error(t, err)
	}
}

func TestRemediationStartSingleSubmission(t *testing.T) {
	directory := remediationTestDirectory(t)
	input := remediationTestFile(t, directory, "report.json", remediationSyntheticReport)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token\n")
	for _, mode := range []contract.Mode{contract.Generate, contract.Validate} {
		t.Run(string(mode), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Equal(t, http.MethodPost, r.Method)
				require.Equal(t, "/api/v1/remediations", r.URL.Path)
				require.Equal(t, "testing", r.URL.Query().Get("namespace"))
				require.Equal(t, "Bearer synthetic-private-token", r.Header.Get("Authorization"))
				require.Empty(t, r.Header.Get("Txn-Token"))
				require.Equal(t, "no-store", r.Header.Get("Cache-Control"))
				require.Greater(t, r.ContentLength, int64(0))
				var request contract.Request
				require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
				require.Equal(t, "request-one", request.RequestID)
				require.Equal(t, mode, request.Mode)
				require.JSONEq(t, remediationSyntheticReport, string(request.Report))
				require.Equal(t, "https://github.com/example/synthetic", request.Repository)
				require.Equal(t, "v1.0.0", request.Ref)
				remediationReply(w, http.StatusAccepted, request.RequestID, contract.PhasePending, contract.StageReproduce, "", false)
			}))
			defer server.Close()
			args := []string{"start", "--input", input, "--mode", string(mode), "--request-id", "request-one",
				"--server", server.URL, "--namespace", "testing", "--token-file", token}
			output, diagnostic, err := remediationCLI(t, args...)
			require.NoError(t, err)
			require.Equal(t, int32(1), calls.Load())
			require.Equal(t, "{\"requestID\":\"request-one\"}\n{\"requestID\":\"request-one\",\"phase\":\"Pending\",\"stage\":\"reproduce\",\"reason\":\"synthetic-safe-reason\"}\n", output)
			require.Empty(t, diagnostic)
			require.NotContains(t, output, "Synthetic")
			require.NotContains(t, output, "synthetic-private-token")
			require.NotContains(t, output, "github.com")
		})
	}
}

type remediationLockedOutput struct {
	mu sync.Mutex
	bytes.Buffer
}

func (b *remediationLockedOutput) Write(content []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.Write(content)
}

func (b *remediationLockedOutput) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.Buffer.String()
}

func TestRemediationPrintsIDBeforeUncertainSubmission(t *testing.T) {
	directory := remediationTestDirectory(t)
	input := remediationTestFile(t, directory, "report.json", remediationSyntheticReport)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	var output remediationLockedOutput
	var calls atomic.Int32
	received := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var request contract.Request
		require.NoError(t, json.NewDecoder(r.Body).Decode(&request))
		receivedID := request.RequestID
		received <- receivedID
		require.True(t, validRemediationID(receivedID))
		require.Contains(t, output.String(), `"requestID":"`+receivedID+`"`)
		connection, _, err := w.(http.Hijacker).Hijack()
		require.NoError(t, err)
		require.NoError(t, connection.Close())
	}))
	defer server.Close()
	command := newRootCmd()
	command.SetOut(&output)
	command.SetErr(&output)
	command.SetArgs([]string{"remediate", "start", "--input", input, "--server", server.URL, "--namespace", "testing", "--token-file", token})
	err := command.Execute()
	require.ErrorContains(t, err, "same ID")
	require.NotContains(t, err.Error(), "synthetic-private-token")
	require.Equal(t, int32(1), calls.Load())
	receivedID := <-received
	require.NotEmpty(t, receivedID)
	require.Equal(t, "{\"requestID\":\""+receivedID+"\"}\n", output.String())
}

func TestRemediationDuplicateRequestIdentityIsNotChanged(t *testing.T) {
	directory := remediationTestDirectory(t)
	input := remediationTestFile(t, directory, "report.json", remediationSyntheticReport)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	var bodies []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		mu.Lock()
		defer mu.Unlock()
		bodies = append(bodies, string(body))
		if len(bodies) == 1 {
			remediationReply(w, http.StatusAccepted, "same-id", contract.PhasePending, contract.StageReproduce, "", false)
		} else {
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, "synthetic-private-conflict")
		}
	}))
	defer server.Close()
	args := []string{"start", "--input", input, "--request-id", "same-id", "--server", server.URL, "--namespace", "testing", "--token-file", token}
	_, _, err := remediationCLI(t, args...)
	require.NoError(t, err)
	output, _, err := remediationCLI(t, args...)
	require.ErrorContains(t, err, "conflicts with retained state")
	require.NotContains(t, err.Error(), "synthetic-private-conflict")
	require.Contains(t, output, `"requestID":"same-id"`)
	mu.Lock()
	defer mu.Unlock()
	require.Len(t, bodies, 2)
	require.Equal(t, bodies[0], bodies[1])
}

func TestRemediationRefusesRedirectsAndRawServerErrors(t *testing.T) {
	directory := remediationTestDirectory(t)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	var destinationCalls atomic.Int32
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		destinationCalls.Add(1)
	}))
	defer destination.Close()
	for _, code := range []int{http.StatusMovedPermanently, http.StatusTemporaryRedirect, http.StatusUnauthorized, http.StatusForbidden, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Location", destination.URL+"/synthetic-private-location")
				w.WriteHeader(code)
				_, _ = io.WriteString(w, "synthetic-private-response")
			}))
			defer server.Close()
			output, diagnostic, err := remediationCLI(t, "status", "run-one", "--server", server.URL, "--namespace", "testing", "--token-file", token)
			require.Error(t, err)
			require.NotContains(t, output+diagnostic+err.Error(), "synthetic-private")
			require.Equal(t, int32(1), calls.Load())
			require.Zero(t, destinationCalls.Load())
		})
	}
}

func TestRemediationResponseBoundsAndIdentity(t *testing.T) {
	directory := remediationTestDirectory(t)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	for _, tc := range []struct {
		name, body string
		chunked    bool
	}{
		{"oversized-declared", strings.Repeat("x", remediationMaxResponseBytes+1), false},
		{"oversized-streamed", strings.Repeat("x", remediationMaxResponseBytes+1), true},
		{"not-json", "synthetic-private-response", false},
		{"different-id", `{"id":"other","namespace":"testing","phase":"Pending","stage":"reproduce"}`, false},
		{"different-namespace", `{"id":"run-one","namespace":"other","phase":"Pending","stage":"reproduce"}`, false},
		{"unknown-phase", `{"id":"run-one","namespace":"testing","phase":"synthetic-private-phase","stage":"reproduce"}`, false},
		{"unknown-stage", `{"id":"run-one","namespace":"testing","phase":"Pending","stage":"synthetic-private-stage"}`, false},
		{"duplicate-id", `{"id":"other","id":"run-one","namespace":"testing","phase":"Pending","stage":"reproduce"}`, false},
		{"null", `null`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if tc.chunked {
					w.(http.Flusher).Flush()
				} else {
					w.Header().Set("Content-Length", fmt.Sprint(len(tc.body)))
				}
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			output, diagnostic, err := remediationCLI(t, "status", "run-one", "--server", server.URL, "--namespace", "testing", "--token-file", token)
			require.Error(t, err)
			require.Empty(t, output)
			require.NotContains(t, diagnostic+err.Error(), "synthetic-private")
		})
	}
}

func TestRemediationWaitStopsWithoutResubmitting(t *testing.T) {
	directory := remediationTestDirectory(t)
	input := remediationTestFile(t, directory, "report.json", remediationSyntheticReport)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	var submissions, polls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/remediations":
			submissions.Add(1)
			remediationReply(w, http.StatusAccepted, "request-one", contract.PhaseRunning, contract.StageVerify, "", true)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/remediations/request-one":
			polls.Add(1)
			remediationReply(w, http.StatusOK, "request-one", contract.PhaseFailed, contract.StageComplete, contract.OutcomeUnverified, true)
		default:
			t.Errorf("unexpected route %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	output, diagnostic, err := remediationCLI(t, "start", "--input", input, "--request-id", "request-one",
		"--wait", "--server", server.URL, "--namespace", "testing", "--token-file", token)
	require.ErrorContains(t, err, "not verified")
	require.Contains(t, diagnostic, `"phase":"Failed","stage":"complete"`)
	require.NotContains(t, output+diagnostic, "Synthetic")
	require.Equal(t, int32(1), submissions.Load())
	require.Equal(t, int32(1), polls.Load())
}

func TestRemediationPollingRecognizesAllTerminalPhases(t *testing.T) {
	command := &cobra.Command{}
	command.SetOut(io.Discard)
	for _, phase := range []string{contract.PhaseSucceeded, contract.PhaseFailed, contract.PhaseCancelled, contract.PhaseTimedOut, contract.PhaseNeedsInput} {
		status := &contract.Status{ID: "request-one", Phase: phase, Stage: contract.StageComplete}
		got, err := waitRemediation(t.Context(), command, nil, status, time.Millisecond)
		require.NoError(t, err)
		require.Same(t, status, got)
	}
	require.NoError(t, remediationCompletion(&contract.Status{Phase: contract.PhaseSucceeded, Outcome: contract.OutcomeReproduced}, contract.Validate))
	require.NoError(t, remediationCompletion(&contract.Status{Phase: contract.PhaseSucceeded, Outcome: contract.OutcomeNotReproduced}, contract.Validate))
	require.NoError(t, remediationCompletion(&contract.Status{Phase: contract.PhaseSucceeded, Outcome: contract.OutcomeVerified}, contract.Generate))
	require.Error(t, remediationCompletion(&contract.Status{Phase: contract.PhaseSucceeded, Outcome: contract.OutcomeUnverified}, contract.Generate))
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err := waitRemediation(ctx, command, nil, &contract.Status{ID: "request-one", Phase: contract.PhasePending}, time.Hour)
	require.ErrorContains(t, err, "remote run was not cancelled")
}

func TestRemediationInputBoundsSurviveJSONEncoding(t *testing.T) {
	directory := remediationTestDirectory(t)
	empty := `{"title":"Synthetic","problem":"","repository":"https://github.com/example/synthetic","ref":"v1"}`
	report := strings.Replace(empty, `"problem":""`, `"problem":"`+strings.Repeat("<", remediationMaxReportBytes-len(empty))+`"`, 1)
	require.Len(t, report, remediationMaxReportBytes)
	input := remediationTestFile(t, directory, "report.json", report)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		content, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		require.LessOrEqual(t, len(content), remediationMaxRequestBytes)
		var request contract.Request
		require.NoError(t, json.Unmarshal(content, &request))
		require.Len(t, request.Report, remediationMaxReportBytes)
		remediationReply(w, http.StatusAccepted, "request-one", contract.PhasePending, contract.StageReproduce, "", false)
	}))
	defer server.Close()
	_, _, err := remediationCLI(t, "start", "--input", input, "--mode", "generate",
		"--request-id", "request-one", "--server", server.URL, "--namespace", "testing", "--token-file", token)
	require.NoError(t, err)
}

func TestRemediationStatusCancelAndSavedLogin(t *testing.T) {
	directory := remediationTestDirectory(t)
	var calls []string
	var mu sync.Mutex
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		require.Equal(t, "testing", r.URL.Query().Get("namespace"))
		require.Equal(t, "Bearer synthetic-saved-token", r.Header.Get("Authorization"))
		code := http.StatusOK
		if r.Method == http.MethodPost {
			code = http.StatusAccepted
			content, err := io.ReadAll(r.Body)
			require.NoError(t, err)
			require.Empty(t, content)
		}
		remediationReply(w, code, "request-one", contract.PhaseCancelled, contract.StageComplete, contract.OutcomeCancelled, false)
	}))
	defer server.Close()
	require.NoError(t, os.Mkdir(filepath.Join(directory, ".orka"), 0700))
	remediationTestFile(t, filepath.Join(directory, ".orka"), "config.yaml",
		fmt.Sprintf("server: %s\nnamespace: testing\ntoken: synthetic-saved-token\n", server.URL))
	for _, action := range []string{"status", "cancel"} {
		output, _, err := remediationCLI(t, action, "request-one")
		require.NoError(t, err)
		require.Equal(t, "{\"requestID\":\"request-one\",\"phase\":\"Cancelled\",\"stage\":\"complete\",\"outcome\":\"Cancelled\",\"reason\":\"synthetic-safe-reason\"}\n", output)
	}
	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []string{"GET /api/v1/remediations/request-one", "POST /api/v1/remediations/request-one/cancel"}, calls)
}

func TestRemediationExplicitConnectionDoesNotReadSavedCredentials(t *testing.T) {
	directory := remediationTestDirectory(t)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	require.NoError(t, os.Mkdir(filepath.Join(directory, ".orka"), 0700))
	remediationTestFile(t, filepath.Join(directory, ".orka"), "config.yaml", "[synthetic-invalid-private-config")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		require.Equal(t, "Bearer synthetic-private-token", r.Header.Get("Authorization"))
		remediationReply(w, http.StatusOK, "request-one", contract.PhasePending, contract.StageReproduce, "", false)
	}))
	defer server.Close()
	_, _, err := remediationCLI(t, "status", "request-one", "--server", server.URL, "--namespace", "testing", "--token-file", token)
	require.NoError(t, err)
	require.Equal(t, int32(1), calls.Load())
	_, _, err = remediationCLI(t, "status", "request-one", "--server", server.URL, "--namespace", "testing")
	require.ErrorContains(t, err, "saved login configuration is invalid")
	require.NotContains(t, err.Error(), "synthetic-invalid-private-config")
	require.Equal(t, int32(1), calls.Load())
}

func TestRemediationDownloadLabelsAndPermissions(t *testing.T) {
	directory := remediationTestDirectory(t)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	for _, tc := range []struct {
		name, phase, outcome, label string
	}{
		{"unverified", contract.PhaseFailed, contract.OutcomeUnverified, "Candidate patch saved (not verified)."},
		{"running", contract.PhaseRunning, "", "Candidate patch saved (not verified)."},
		{"premature", contract.PhaseRunning, contract.OutcomeVerified, "Candidate patch saved (not verified)."},
		{"verified", contract.PhaseSucceeded, contract.OutcomeVerified, "Patch saved (verified checks; evidence scope unrecorded)."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				require.Equal(t, http.MethodGet, r.Method)
				switch r.URL.Path {
				case "/api/v1/remediations/request-one":
					remediationReply(w, http.StatusOK, "request-one", tc.phase, contract.StageVerify, tc.outcome, true)
				case "/api/v1/remediations/request-one/patch":
					w.Header().Set("Content-Type", "application/octet-stream")
					_, _ = io.WriteString(w, remediationSyntheticPatch)
				default:
					t.Errorf("unexpected download path %q", r.URL.Path)
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			outputPath := filepath.Join(directory, tc.name+".patch")
			output, diagnostic, err := remediationCLI(t, "download", "request-one", "--output", outputPath,
				"--server", server.URL, "--namespace", "testing", "--token-file", token)
			require.NoError(t, err)
			require.Equal(t, tc.label+"\n", diagnostic)
			require.NotContains(t, output+diagnostic, "synthetic-new-value")
			content, err := os.ReadFile(outputPath)
			require.NoError(t, err)
			require.Equal(t, remediationSyntheticPatch, string(content))
			info, err := os.Stat(outputPath)
			require.NoError(t, err)
			require.Equal(t, os.FileMode(0600), info.Mode().Perm())
			require.Equal(t, int32(2), calls.Load())
			_, _, err = remediationCLI(t, "download", "request-one", "--output", outputPath,
				"--server", server.URL, "--namespace", "testing", "--token-file", token)
			require.ErrorContains(t, err, "refusing overwrite")
			require.Equal(t, int32(2), calls.Load())
		})
	}
}

func TestRemediationDownloadRefusesUnsafeOutputsAndBadPatches(t *testing.T) {
	directory := remediationTestDirectory(t)
	existing := remediationTestFile(t, directory, "existing.patch", "preserve this")
	link := filepath.Join(directory, "link.patch")
	require.NoError(t, os.Symlink("existing.patch", link))
	dangling := filepath.Join(directory, "dangling.patch")
	require.NoError(t, os.Symlink("missing.patch", dangling))
	parentLink := filepath.Join(directory, "directory-link")
	require.NoError(t, os.Symlink(".", parentLink))
	for _, output := range []string{existing, link, dangling, filepath.Join(parentLink, "new.patch"), filepath.Join(directory, "missing", "new.patch"), "-"} {
		_, _, err := remediationCLI(t, "download", "request-one", "--output", output)
		require.Error(t, err)
	}
	content, err := os.ReadFile(existing)
	require.NoError(t, err)
	require.Equal(t, "preserve this", string(content))
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	for _, tc := range []struct {
		name  string
		ready bool
		patch string
	}{
		{"not-ready", false, "should not be read"},
		{"empty", true, ""},
		{"too-large", true, strings.Repeat("x", remediationMaxPatchBytes+1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var patchRequests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.HasSuffix(r.URL.Path, "/patch") {
					patchRequests.Add(1)
					w.Header().Set("Content-Type", "application/octet-stream")
					w.(http.Flusher).Flush()
					_, _ = io.WriteString(w, tc.patch)
					return
				}
				remediationReply(w, http.StatusOK, "request-one", contract.PhaseFailed, contract.StageComplete, contract.OutcomeUnverified, tc.ready)
			}))
			defer server.Close()
			outputPath := filepath.Join(directory, tc.name+".patch")
			output, _, err := remediationCLI(t, "download", "request-one", "--output", outputPath,
				"--server", server.URL, "--namespace", "testing", "--token-file", token)
			require.Error(t, err)
			require.Empty(t, output)
			_, err = os.Lstat(outputPath)
			require.ErrorIs(t, err, os.ErrNotExist)
			if !tc.ready {
				require.Zero(t, patchRequests.Load())
			}
		})
	}
}

func TestRemediationDownloadRejectsMIMEAndInvalidUTF8(t *testing.T) {
	directory := remediationTestDirectory(t)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	for _, tc := range []struct {
		name         string
		contentTypes []string
		body         string
	}{
		{"html", []string{"text/html; charset=utf-8"}, "<html>synthetic-private-response</html>"},
		{"json", []string{"application/json"}, `{"error":"synthetic-private-response"}`},
		{"mislabeled-html", []string{"application/octet-stream"}, "<html>synthetic-private-response</html>"},
		{"mislabeled-json", []string{"application/octet-stream"}, `{"error":"synthetic-private-response"}`},
		{"mislabeled-html-newline", []string{"application/octet-stream"}, "<html>synthetic-private-response</html>\n"},
		{"mislabeled-json-newline", []string{"application/octet-stream"}, "{\"error\":\"synthetic-private-response\"}\n"},
		{"headers-without-hunks", []string{"application/octet-stream"}, "diff --git a/file b/file\n--- a/file\n+++ b/file\n"},
		{"truncated-hunk", []string{"application/octet-stream"}, "--- a/file\n+++ b/file\n@@ -1,2 +1,2 @@\n-old\n+new\n"},
		{"patch-with-json-tail", []string{"application/octet-stream"}, remediationSyntheticPatch + "{\"error\":\"synthetic-private-response\"}\n"},
		{"text", []string{"text/plain"}, remediationSyntheticPatch},
		{"missing-type", nil, remediationSyntheticPatch},
		{"malformed-type", []string{"application/octet-stream; charset"}, remediationSyntheticPatch},
		{"ambiguous-type", []string{"application/octet-stream", "text/html"}, remediationSyntheticPatch},
		{"invalid-utf8", []string{"application/octet-stream"}, remediationSyntheticPatch + "\xff"},
		{"empty", []string{"application/octet-stream"}, ""},
		{"whitespace", []string{"application/octet-stream"}, " \n\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path == "/api/v1/remediations/request-one" {
					remediationReply(w, http.StatusOK, "request-one", contract.PhaseSucceeded,
						contract.StageComplete, contract.OutcomeVerified, true)
					return
				}
				require.Equal(t, "/api/v1/remediations/request-one/patch", r.URL.Path)
				w.Header()["Content-Type"] = tc.contentTypes
				_, _ = io.WriteString(w, tc.body)
			}))
			defer server.Close()
			outputPath := filepath.Join(directory, tc.name+".patch")
			output, diagnostic, err := remediationCLI(t, "download", "request-one", "--output", outputPath,
				"--server", server.URL, "--namespace", "testing", "--token-file", token)
			require.Error(t, err, "unexpected output %q, diagnostic %q", output, diagnostic)
			require.Empty(t, output)
			require.Empty(t, diagnostic)
			require.NotContains(t, err.Error(), "synthetic-private")
			require.Equal(t, int32(2), calls.Load())
			_, err = os.Lstat(outputPath)
			require.ErrorIs(t, err, os.ErrNotExist)
		})
	}
}

func TestRemediationStatusRequiresJSONMIME(t *testing.T) {
	directory := remediationTestDirectory(t)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	for _, tc := range []struct {
		name         string
		contentTypes []string
	}{
		{"html", []string{"text/html"}},
		{"octet-stream", []string{"application/octet-stream"}},
		{"missing-type", nil},
		{"malformed-type", []string{"application/json; charset"}},
		{"ambiguous-type", []string{"application/json", "text/html"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header()["Content-Type"] = tc.contentTypes
				_, _ = io.WriteString(w, `{"id":"request-one","namespace":"testing","phase":"Pending","stage":"reproduce"}`)
			}))
			defer server.Close()
			output, diagnostic, err := remediationCLI(t, "status", "request-one",
				"--server", server.URL, "--namespace", "testing", "--token-file", token)
			require.Error(t, err)
			require.Empty(t, output)
			require.Empty(t, diagnostic)
		})
	}
}

func TestRemediationDownloadAcceptsMIMEParameters(t *testing.T) {
	directory := remediationTestDirectory(t)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/patch") {
			require.Equal(t, "application/octet-stream", r.Header.Get("Accept"))
			w.Header().Set("Content-Type", `Application/Octet-Stream; name="candidate.patch"`)
			_, _ = io.WriteString(w, remediationSyntheticPatch)
			return
		}
		require.Equal(t, "application/json", r.Header.Get("Accept"))
		w.Header().Set("Content-Type", "Application/JSON; charset=utf-8")
		_ = json.NewEncoder(w).Encode(contract.Status{
			ID: "request-one", Namespace: "testing", Phase: contract.PhaseSucceeded,
			Stage: contract.StageComplete, Outcome: contract.OutcomeVerified, PatchReady: true,
		})
	}))
	defer server.Close()
	outputPath := filepath.Join(directory, "candidate.patch")
	_, diagnostic, err := remediationCLI(t, "download", "request-one", "--output", outputPath,
		"--server", server.URL, "--namespace", "testing", "--token-file", token)
	require.NoError(t, err)
	require.Equal(t, "Patch saved (verified checks; evidence scope unrecorded).\n", diagnostic)
	patch, err := os.ReadFile(outputPath)
	require.NoError(t, err)
	require.Equal(t, remediationSyntheticPatch, string(patch))
}

func TestRemediationDownloadRejectsSymlinkParentTraversal(t *testing.T) {
	directory := remediationTestDirectory(t)
	safe := filepath.Join(directory, "safe")
	outside := filepath.Join(directory, "outside")
	require.NoError(t, os.Mkdir(safe, 0700))
	require.NoError(t, os.MkdirAll(filepath.Join(outside, "child"), 0700))
	require.NoError(t, os.Symlink(filepath.Join("..", "outside", "child"), filepath.Join(safe, "link")))
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if strings.HasSuffix(r.URL.Path, "/patch") {
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = io.WriteString(w, remediationSyntheticPatch)
			return
		}
		remediationReply(w, http.StatusOK, "request-one", contract.PhaseSucceeded,
			contract.StageComplete, contract.OutcomeVerified, true)
	}))
	defer server.Close()
	separator := string(os.PathSeparator)
	rawPath := safe + separator + "link" + separator + ".." + separator + "candidate.patch"
	output, diagnostic, err := remediationCLI(t, "download", "request-one", "--output", rawPath,
		"--server", server.URL, "--namespace", "testing", "--token-file", token)
	require.Error(t, err, "unexpected output %q, diagnostic %q", output, diagnostic)
	require.Empty(t, output)
	require.Empty(t, diagnostic)
	require.Zero(t, calls.Load())
	for _, parent := range []string{safe, outside} {
		_, err := os.Lstat(filepath.Join(parent, "candidate.patch"))
		require.ErrorIs(t, err, os.ErrNotExist)
	}
}

func TestRemediationPatchWriteRejectsRawParentComponents(t *testing.T) {
	directory := remediationTestDirectory(t)
	safe := filepath.Join(directory, "safe")
	require.NoError(t, os.MkdirAll(filepath.Join(safe, "nested"), 0700))
	for _, suffix := range []string{"/../slash.patch", `/nested\..\backslash.patch`, `/nested/..\mixed.patch`} {
		t.Run(suffix, func(t *testing.T) {
			rawPath := safe + suffix
			require.Error(t, remediationOutputAvailable(rawPath))
			require.Error(t, writeRemediationPatch(rawPath, []byte(remediationSyntheticPatch)))
		})
	}
}

func TestRemediationPatchWritePreservesPlainRelativeFilename(t *testing.T) {
	directory := remediationTestDirectory(t)
	t.Chdir(directory)
	path := "candidate.patch"
	require.NoError(t, remediationOutputAvailable(path))
	require.NoError(t, writeRemediationPatch(path, []byte(remediationSyntheticPatch)))
	content, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, remediationSyntheticPatch, string(content))
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0600), info.Mode().Perm())
	require.ErrorContains(t, writeRemediationPatch(path, []byte("must not overwrite")), "refusing overwrite")
	content, err = os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, remediationSyntheticPatch, string(content))
}

func TestRemediationInputFilesAndCredentialFlagsAreBounded(t *testing.T) {
	directory := remediationTestDirectory(t)
	oversized := remediationTestFile(t, directory, "large.json", strings.Repeat("x", remediationMaxReportBytes+1))
	_, err := readRemediationInput(oversized, contract.Generate, "")
	require.Error(t, err)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	require.NoError(t, os.Chmod(token, 0644))
	_, err = readRemediationFile(token, 16<<10, true)
	require.Error(t, err)
	link := filepath.Join(directory, "token-link")
	require.NoError(t, os.Symlink("token", link))
	_, err = readRemediationFile(link, 16<<10, true)
	require.Error(t, err)
	for _, flag := range []string{"--token", "--txn-token", "--txn-token-file", "--kubeconfig"} {
		output, diagnostic, err := remediationCLI(t, "status", "request-one", flag, "synthetic-private-value")
		require.Error(t, err)
		require.NotContains(t, output+diagnostic+err.Error(), "synthetic-private-value")
	}
}

type remediationFailingWriter struct{}

func (remediationFailingWriter) Write([]byte) (int, error) {
	return 0, errors.New("synthetic-private-write-error")
}

func TestRemediationDoesNotSubmitWhenIDCannotBePrinted(t *testing.T) {
	directory := remediationTestDirectory(t)
	input := remediationTestFile(t, directory, "report.json", remediationSyntheticReport)
	token := remediationTestFile(t, directory, "token", "synthetic-private-token")
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusAccepted)
	}))
	defer server.Close()
	command := newRootCmd()
	command.SetOut(remediationFailingWriter{})
	command.SetArgs([]string{"remediate", "start", "--input", input, "--server", server.URL, "--namespace", "testing", "--token-file", token})
	err := command.Execute()
	require.ErrorContains(t, err, "could not print")
	require.NotContains(t, err.Error(), "synthetic-private")
	require.Zero(t, calls.Load())
}
