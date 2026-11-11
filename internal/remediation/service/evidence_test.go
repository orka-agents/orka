package service

import (
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/workers/common"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type unscopedBackend struct{ contract.ExecutionBackend }

type scopedBackend struct {
	contract.ExecutionBackend
	scope contract.EvidenceScope
}

func (b scopedBackend) EvidenceScope() contract.EvidenceScope { return b.scope }

func TestServiceEvidenceRequirementAdmission(t *testing.T) {
	for _, mode := range []contract.Mode{contract.Generate, contract.Validate} {
		for _, tc := range []struct {
			name     string
			required contract.EvidenceScope
			err      error
		}{
			{"default", "", nil},
			{"source harness", contract.SourceHarness, nil},
			{"strict runtime", contract.ApplicationRuntime, contract.ErrEvidenceScopeUnsupported},
			{"unknown", "future-scope", contract.ErrInvalid},
			{"malformed", " source-harness", contract.ErrInvalid},
		} {
			t.Run(string(mode)+"/"+tc.name, func(t *testing.T) {
				f := newServiceFixture(t)
				creates := 0
				f.createHook = func(client.Object) error { creates++; return nil }
				request := f.request(mode)
				request.RequiredEvidenceScope = tc.required
				status, err := f.service.Submit(t.Context(), "private", "subject", request)
				require.ErrorIs(t, err, tc.err)
				require.Zero(t, f.backend.startCalls.Load())
				var tasks corev1alpha1.TaskList
				require.NoError(t, f.client.List(t.Context(), &tasks))
				require.Empty(t, tasks.Items)
				if tc.err != nil {
					require.Nil(t, status)
					require.Zero(t, creates, "rejection must precede all run/model/executor resources")
					_, err := f.service.load(t.Context(), request.RequestID)
					require.ErrorIs(t, err, contract.ErrNotFound)
					return
				}
				require.Equal(t, contract.SourceHarness, status.EvidenceScope)
				require.Equal(t, 1, creates)
				repeated, err := f.service.Submit(t.Context(), "private", "subject", request)
				require.NoError(t, err)
				require.Equal(t, status, repeated)
				require.Equal(t, 1, creates, "identical admission must reuse its state")
				request.RequiredEvidenceScope = contract.ApplicationRuntime
				_, err = f.service.Submit(t.Context(), "private", "subject", request)
				require.ErrorIs(t, err, contract.ErrConflict)
				require.Equal(t, 1, creates, "a different requirement cannot replace an accepted run")
			})
		}
	}
}

func TestServiceRequiresTrustedBackendScopeForNewAdmission(t *testing.T) {
	f := newServiceFixture(t)
	config := f.service.config
	config.Backend = unscopedBackend{ExecutionBackend: f.backend}
	unscoped, err := New(config)
	require.NoError(t, err, "cleanup remains available to unscoped backends")
	_, err = unscoped.Submit(t.Context(), "private", "subject", f.request(contract.Validate))
	require.ErrorIs(t, err, contract.ErrEvidenceScopeUnsupported)
	config.Backend = scopedBackend{ExecutionBackend: f.backend, scope: "future-scope"}
	_, err = New(config)
	require.ErrorIs(t, err, contract.ErrInvalid)
}

func TestServiceScopesGeneratedAndValidateEvidence(t *testing.T) {
	for _, mode := range []contract.Mode{contract.Generate, contract.Validate} {
		for _, required := range []contract.EvidenceScope{"", contract.SourceHarness} {
			t.Run(string(mode)+"/"+string(required), func(t *testing.T) {
				f := newServiceFixture(t)
				proposal := proposedChecks(t)
				proposal.Scope = []string{"application-runtime"}
				f.responses["checks"] = marshalTest(t, proposal)
				request := f.request(mode)
				request.RequiredEvidenceScope = required
				request.Report = json.RawMessage(`{"title":"Synthetic scope claim","problem":"Check synthetic behavior.","evidenceScope":"application-runtime","requiredEvidenceScope":"application-runtime"}`)
				f.start()
				accepted, err := f.service.Submit(t.Context(), "private", "subject", request)
				require.NoError(t, err)
				require.Equal(t, contract.SourceHarness, accepted.EvidenceScope)
				status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool {
					return contract.TerminalPhase(status.Phase)
				})
				require.Equal(t, contract.PhaseSucceeded, status.Phase)
				require.Equal(t, contract.SourceHarness, status.EvidenceScope)
				wantOutcome, wantExecutions := contract.OutcomeReproduced, 1
				if mode == contract.Generate {
					wantOutcome, wantExecutions = contract.OutcomeVerified, 2
				}
				require.Equal(t, wantOutcome, status.Outcome)
				require.Equal(t, mode == contract.Generate, status.PatchReady)
				f.backend.mu.Lock()
				executions, records := slices.Clone(f.backend.requests), maps.Clone(f.backend.records)
				f.backend.mu.Unlock()
				require.Len(t, executions, wantExecutions)
				for _, execution := range executions {
					require.Equal(t, contract.SourceHarness, execution.EvidenceScope)
					record := records[execution.ID]
					require.Equal(t, "source-harness", record.Manifest.Environment.Dependencies[contract.EvidenceScopeKey])
					require.NoError(t, checkRecord(execution, record))
				}
				data, err := f.results.GetResult(t.Context(), status.Namespace, status.TaskName)
				require.NoError(t, err)
				result := common.ParseStructuredResult(string(data))
				require.Equal(t, wantOutcome, result.Verdict)
				require.Contains(t, result.Summary, "Evidence scope: source-harness.")
				require.Contains(t, result.Summary, "application-runtime behavior is not qualified")
				require.NotContains(t, result.Summary, "Evidence scope: application-runtime")
			})
		}
	}
}

func TestExecutionScopeRequiresExactBoundObservations(t *testing.T) {
	proposal := proposedChecks(t)
	files, err := remediation.FreezeProposalFiles(proposal)
	require.NoError(t, err)
	request := contract.ExecutionRequest{
		ID: "scope-check", Action: pv.ValidateReport, Problem: "Synthetic behavior",
		Source: contract.Source{Repository: "https://github.com/example/project",
			Commit: strings.Repeat("a", 40), Tree: strings.Repeat("b", 40)},
		Checks: contract.Checks{Scope: proposal.Scope, Checks: proposal.Checks, Files: files},
	}
	for _, tc := range []struct {
		name               string
		expected, observed contract.EvidenceScope
		valid              bool
	}{
		{"source harness", contract.SourceHarness, contract.SourceHarness, true},
		{"legacy remains unscoped", "", "", true},
		{"missing scope", contract.SourceHarness, "", false},
		{"runtime claim", contract.SourceHarness, contract.ApplicationRuntime, false},
		{"unknown scope", contract.SourceHarness, "future-scope", false},
		{"legacy cannot be relabelled", "", contract.SourceHarness, false},
		{"unknown request", "future-scope", "future-scope", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			expected := request
			expected.EvidenceScope = tc.expected
			observed := request
			observed.EvidenceScope = tc.observed
			record := executionRecord(t, observed, "", "")
			require.NoError(t, pv.ValidateRunBinding(record.Manifest, record.Binding))
			err := checkRecord(expected, record)
			if tc.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err, "even an internally consistent seal cannot change the authorized scope")
			}
		})
	}
	request.EvidenceScope = contract.SourceHarness
	record := executionRecord(t, request, "", "")
	record.Evidence = nil
	require.Error(t, checkRecord(request, record), "a matching scope string without observations is not proof")
}

func TestServiceLegacyScopePreservesIdentityAndDoesNotPromoteEvidence(t *testing.T) {
	f := newServiceFixture(t)
	request := f.request(contract.Generate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	inputData, digest := r.object.Data["input"], r.state.InputDigest
	require.NotContains(t, inputData, "requiredEvidenceScope")
	r.state.Authority.EvidenceScope, r.state.Status.EvidenceScope = "", ""
	require.NoError(t, f.service.save(t.Context(), r))
	require.NoError(t, f.service.CheckReady(t.Context()))
	again, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	require.Empty(t, again.EvidenceScope)
	f.start()
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool {
		return contract.TerminalPhase(status.Phase)
	})
	require.Equal(t, contract.OutcomeVerified, status.Outcome)
	require.Empty(t, status.EvidenceScope)
	after, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.Equal(t, inputData, after.object.Data["input"])
	require.Equal(t, digest, after.state.InputDigest)
	require.Empty(t, after.state.Authority.EvidenceScope)
	f.backend.mu.Lock()
	executions, records := slices.Clone(f.backend.requests), maps.Clone(f.backend.records)
	f.backend.mu.Unlock()
	for _, execution := range executions {
		require.Empty(t, execution.EvidenceScope)
		require.NotContains(t, records[execution.ID].Manifest.Environment.Dependencies, contract.EvidenceScopeKey)
	}
	data, err := f.results.GetResult(t.Context(), status.Namespace, status.TaskName)
	require.NoError(t, err)
	require.Contains(t, common.ParseStructuredResult(string(data)).Summary, "Evidence scope: unrecorded (legacy)")
}

func TestRetainedEvidenceScopeCannotChangeAuthority(t *testing.T) {
	f := newServiceFixture(t)
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	for _, field := range []string{"authority", "status"} {
		for _, scope := range []any{"application-runtime", "future-scope", "", nil, true} {
			var data map[string]any
			require.NoError(t, json.Unmarshal([]byte(r.object.Data["state"]), &data))
			data[field].(map[string]any)["evidenceScope"] = scope
			object := r.object.DeepCopy()
			object.Data["state"] = marshalTest(t, data)
			_, err := decodeRun(object, request.RequestID, "private")
			require.ErrorIs(t, err, contract.ErrConflict)
		}
	}
	r.state.Authority.EvidenceScope, r.state.Status.EvidenceScope = "", ""
	require.NoError(t, f.service.save(t.Context(), r))
	f.service.config.Backend = scopedBackend{ExecutionBackend: f.backend, scope: contract.ApplicationRuntime}
	require.ErrorIs(t, f.service.CheckReady(t.Context()), contract.ErrConflict)
	var states corev1.ConfigMapList
	require.NoError(t, f.client.List(t.Context(), &states))
	require.Len(t, states.Items, 1, "readiness must not create replacement state or Tasks")
	var tasks corev1alpha1.TaskList
	require.NoError(t, f.client.List(t.Context(), &tasks))
	require.Empty(t, tasks.Items)
}
