package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	kubelabels "k8s.io/apimachinery/pkg/labels"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/intake"
	"github.com/orka-agents/orka/internal/remediation/source"
	"github.com/orka-agents/orka/workers/common"
)

func TestExecutionPreflightRejectionsAreSettled(t *testing.T) {
	for _, invalid := range []string{"check-id", "requirement", "declared-changes", "control-character"} {
		t.Run(invalid, func(t *testing.T) {
			f := newServiceFixture(t)
			checks := proposedChecks(t)
			switch invalid {
			case "check-id":
				checks.Checks[0].ID = "invalid/id"
			case "requirement":
				checks.Requirements = []pv.EnvironmentRequirement{{Kind: "process", Name: "invalid/name"}}
			case "control-character":
				checks.Scope[0] = "scope\x01"
			case "declared-changes":
				f.responses["generate"] = marshalTest(t, remediation.PatchProposal{
					Summary: "one candidate", Edits: []remediation.SourceEdit{{Path: "README", Old: "old", New: "new"}},
					DeclaredChanges: []pv.DeclaredChange{{Kind: "invalid", Description: "invalid metadata"}},
				})
			}
			f.responses["checks"] = marshalTest(t, checks)
			f.backend.validateRequest = func(request contract.ExecutionRequest) error {
				if invalid != "declared-changes" || request.Action == pv.VerifyPatch {
					return contract.ErrInvalid
				}
				return nil
			}
			f.start()
			request := f.request(contract.Generate)
			_, err := f.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
			require.Equal(t, contract.PhaseFailed, status.Phase)
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				require.NoError(c, EnsureNoActiveRuns(t.Context(), f.client, "private"))
			}, 5*time.Second, time.Millisecond)
			wantStarts := int32(0)
			if invalid == "declared-changes" {
				wantStarts = 1
				require.True(t, status.PatchReady)
			}
			require.Equal(t, wantStarts, f.backend.startCalls.Load())
		})
	}
}

func TestDefinitiveNoEffectsStartFailureIsSettled(t *testing.T) {
	f := newServiceFixture(t)
	f.backend.rejectStart = fixtureNoEffectsError{contract.ErrInvalid}
	f.start()
	request := f.request(contract.Generate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseFailed, status.Phase)
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, EnsureNoActiveRuns(t.Context(), f.client, "private"))
	}, 5*time.Second, time.Millisecond)
	require.EqualValues(t, 1, f.backend.startCalls.Load())
}

func TestDeletingExactTaskCanReachOrdinaryCleanup(t *testing.T) {
	f := newServiceFixture(t)
	stop := f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	stop()
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	op := r.state.Agents["select"]
	task := new(corev1alpha1.Task)
	require.NoError(t, f.client.Get(t.Context(), client.ObjectKey{Namespace: "private", Name: op.Name}, task))
	task.Finalizers = []string{labels.TaskFinalizer}
	require.NoError(t, f.client.Update(t.Context(), task))
	require.NoError(t, f.client.Delete(t.Context(), task))
	require.NoError(t, f.client.Get(t.Context(), client.ObjectKeyFromObject(task), task))
	_, handled, err := f.service.ReconcileRemediationTask(t.Context(), task)
	require.NoError(t, err)
	require.False(t, handled)
	settled, err := f.service.cancelAgent(t.Context(), r, op)
	require.NoError(t, err)
	require.False(t, settled, "normal deletion finalizer has not completed")
}

func TestSteadyPollingExcludesSettledStateServerSide(t *testing.T) {
	f := newServiceFixture(t)
	const activeKey = "remediation.orka.ai/active"
	var unfiltered atomic.Int32
	base := f.client.(client.WithWatch)
	f.service.config.Reader = interceptor.NewClient(base, interceptor.Funcs{
		List: func(ctx context.Context, delegate client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			resolved := new(client.ListOptions)
			for _, option := range options {
				option.ApplyToList(resolved)
			}
			if _, states := list.(*corev1.ConfigMapList); states &&
				(resolved.LabelSelector == nil || resolved.LabelSelector.Matches(kubelabels.Set{stateLabel: "v1", activeKey: "false"})) {
				unfiltered.Add(1)
			}
			return delegate.List(ctx, list, options...)
		},
	})
	f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Zero(t, unfiltered.Load(), "steady polling or readiness fetched settled state bodies")
}

func TestPrelabelSettledStateIsMigratedOutOfPolling(t *testing.T) {
	f := newServiceFixture(t)
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	r.state.Status.Phase, r.state.Status.Stage = contract.PhaseSucceeded, contract.StageComplete
	r.state.Status.Outcome, r.state.Status.Reason = contract.OutcomeReproduced, "report-reproduced"
	r.state.Settled = true
	require.NoError(t, f.service.save(t.Context(), r))
	delete(r.object.Labels, "remediation.orka.ai/active")
	require.NoError(t, f.client.Update(t.Context(), r.object))
	f.start()
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		current := new(corev1.ConfigMap)
		require.NoError(c, f.client.Get(t.Context(), client.ObjectKeyFromObject(r.object), current))
		require.Equal(c, "false", current.Labels["remediation.orka.ai/active"])
	}, time.Second, time.Millisecond)
}

func TestServiceRejectsSuppliedPatchModeBeforeWork(t *testing.T) {
	f := newServiceFixture(t)
	request := f.request(contract.Mode("verify"))
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.ErrorIs(t, err, contract.ErrInvalid)
	require.Zero(t, f.backend.startCalls.Load())
	tasks := &corev1alpha1.TaskList{}
	require.NoError(t, f.client.List(t.Context(), tasks))
	require.Empty(t, tasks.Items)
}

func TestOversizedCandidateDoesNotExecuteComparison(t *testing.T) {
	f := newServiceFixture(t)
	patch := "diff --git a/README b/README\n" + strings.Repeat("x", 128<<10)
	f.responses["generate"] = marshalTest(t, remediation.PatchProposal{
		Summary: "one large candidate", Patch: patch,
		DeclaredChanges: []pv.DeclaredChange{{Kind: "source", Description: "candidate"}},
	})
	f.start()
	request := f.request(contract.Generate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseFailed, status.Phase)
	require.True(t, status.PatchReady)
	require.EqualValues(t, 1, f.backend.startCalls.Load(), "comparison ran even though the encoded validator prompt could never fit")
}

func TestLargeSourceAndChecksArePreflightedBeforeRunner(t *testing.T) {
	f := newServiceFixture(t)
	reader := f.service.config.Source.(fixtureSource)
	reader.content = strings.Repeat("x", (64<<10)-1) + "\n"
	f.service.config.Source = reader
	checks := proposedChecks(t)
	checks.Files[0].Content += strings.Repeat("#", 40<<10) + "\n"
	f.responses["checks"] = marshalTest(t, checks)
	f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.PhaseFailed, status.Phase)
	require.Equal(t, "source-too-large", status.Reason)
	require.Zero(t, f.backend.startCalls.Load())
}

func TestCompactModelEvidenceDoesNotAlterAuthoritativeRecord(t *testing.T) {
	f := newServiceFixture(t)
	f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	var record pv.Record
	require.NoError(t, f.service.readJSON(t.Context(), r, r.state.Baseline, &record))
	before := marshalTest(t, record)
	projected := modelExecution(&record)
	require.Equal(t, before, marshalTest(t, record))
	require.Equal(t, record.Manifest.Files[0].Content, []byte(projected.Checks.Files[0].Content))
	require.Equal(t, record.Seal.Digest, projected.SealDigest)
	require.Len(t, projected.Evidence, len(record.Evidence))
	for i, evidence := range record.Evidence {
		require.Equal(t, evidence.Observation.StdoutDigest, projected.Evidence[i].StdoutDigest)
		require.Equal(t, evidence.Observation.HTTPCompleted, projected.Evidence[i].HTTPCompleted)
	}
	var readable struct {
		Evidence []struct {
			ObservedStdout string `json:"observedStdout"`
		} `json:"evidence"`
	}
	require.NoError(t, json.Unmarshal([]byte(marshalTest(t, projected)), &readable))
	for i, evidence := range record.Evidence {
		require.NotEmpty(t, readable.Evidence[i].ObservedStdout)
		require.Equal(t, evidence.Observation.StdoutBytes, len(readable.Evidence[i].ObservedStdout))
		require.Equal(t, evidence.Observation.StdoutDigest, pv.Digest([]byte(readable.Evidence[i].ObservedStdout)))
	}
	require.NotContains(t, marshalTest(t, projected), `"manifest":`)
}

func TestChecksPromptStatesActualWorkerContract(t *testing.T) {
	text := checksPrompt(intake.Report{}, source.Packet{})
	for _, required := range []string{
		"/src", "/work/<roleUID>", "socket.socket(fileno=3)", "socket.fromfd", "net.FileListener", "SIGTERM", "exit 0", "2 seconds",
		`"name":"python3","description":"..."`, `^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`,
		"no spaces or slashes", "description (at most 4096 bytes)",
		"Only the selected files in source.files are guaranteed to be staged at /src/<path>",
	} {
		require.Contains(t, text, required)
	}
	require.NotContains(t, text, "Working directory is the staged source root")
}

func TestSelectionPromptCompactsLargeInventoryAndPreservesScopedPaths(t *testing.T) {
	entries := make([]source.Entry, 0, 700)
	entries = append(entries, source.Entry{Path: "vendor", Mode: "040000", BlobSHA: strings.Repeat("a", 40)})
	for i := range 680 {
		entries = append(entries, source.Entry{
			Path:    "pkg/component/file_" + strings.Repeat("0", 3) + string(rune('a'+i%26)) + "/" + string(rune('a'+(i/26)%26)) + ".go",
			Mode:    "100644",
			Size:    1024,
			BlobSHA: strings.Repeat("b", 40),
		})
	}
	promptText := selectionPrompt(intake.Report{Title: "synthetic"}, source.Target{}, entries)
	require.Less(t, len(promptText), maxPromptBytes)
	require.NotContains(t, promptText, `"vendor"`)
	require.NotContains(t, promptText, strings.Repeat("b", 40))

	packet := source.Packet{Files: []source.PacketFile{{Path: entries[1].Path, Content: "package main\n"}}}
	paths := scopedPreparationPaths(entries, packet, "")
	require.Equal(t, []string{entries[1].Path}, paths)
}

func TestAggregateUsesNormalStructuredTaskResult(t *testing.T) {
	f := newServiceFixture(t)
	f.backend.candidate = "regression"
	f.start()
	request := f.request(contract.Generate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	data, err := f.results.GetResult(t.Context(), "private", r.state.Parent.Name)
	require.NoError(t, err, "terminal status was published without the ordinary parent Task result")
	result := common.ParseStructuredResult(string(data))
	require.Equal(t, contract.OutcomeUnverified, result.Verdict)
	require.NotEmpty(t, result.Summary)
	require.Contains(t, result.Feedback, "normal")
	require.Contains(t, result.Feedback, "regression")
	require.Contains(t, result.Diff, "+hello new")
	task := new(corev1alpha1.Task)
	require.NoError(t, f.client.Get(t.Context(), client.ObjectKey{Namespace: "private", Name: r.state.Parent.Name}, task))
	require.NotNil(t, task.Status.ResultRef)
	require.True(t, task.Status.ResultRef.Available)
	require.Equal(t, contract.OutcomeUnverified, status.Outcome)
	require.Equal(t, r.state.Parent.Name, status.TaskName)
	require.Equal(t, r.state.Parent.UID, status.TaskUID)
}

func TestAggregatePublicationPrecedesTerminalStatus(t *testing.T) {
	f := newServiceFixture(t)
	var blocked atomic.Bool
	var attempts atomic.Int32
	blocked.Store(true)
	f.results.beforeSave = func(_, name string, _ []byte) error {
		if strings.HasSuffix(name, "-parent") && blocked.Load() {
			attempts.Add(1)
			return contract.ErrUnavailable
		}
		return nil
	}
	f.start()
	request := f.request(contract.Generate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	require.Eventually(t, func() bool { return attempts.Load() > 0 }, 5*time.Second, time.Millisecond)
	status, err := f.service.Get(t.Context(), "private", request.RequestID)
	require.NoError(t, err)
	require.False(t, contract.TerminalPhase(status.Phase))
	_, err = f.results.GetResult(t.Context(), "private", status.TaskName)
	require.Error(t, err)
	blocked.Store(false)
	status = f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.OutcomeVerified, status.Outcome)
	body, err := f.results.GetResult(t.Context(), "private", status.TaskName)
	require.NoError(t, err)
	require.Equal(t, contract.OutcomeVerified, common.ParseStructuredResult(string(body)).Verdict)
}

func TestAggregateLostWriteAcknowledgementKeepsOneResult(t *testing.T) {
	f := newServiceFixture(t)
	var parentWrites atomic.Int32
	f.results.afterSave = func(_, name string, _ []byte) error {
		if strings.HasSuffix(name, "-parent") {
			parentWrites.Add(1)
			return contract.ErrUnavailable
		}
		return nil
	}
	f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.Equal(t, contract.OutcomeReproduced, status.Outcome)
	require.EqualValues(t, 1, parentWrites.Load(), "acknowledgement recovery rewrote the aggregate result")
}

func TestNoEffectsClaimAfterReceiptCannotDiscardExecution(t *testing.T) {
	f := newServiceFixture(t)
	f.backend.afterReceiptError = fixtureNoEffectsError{contract.ErrInvalid}
	f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, EnsureNoActiveRuns(t.Context(), f.client, "private"))
	}, 5*time.Second, time.Millisecond)
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.NotNil(t, r.state.Executions["original"].Receipt)
	require.False(t, r.state.Executions["original"].Rejected)
	f.backend.mu.Lock()
	require.Positive(t, f.backend.cancels)
	f.backend.mu.Unlock()
}

func TestNoEffectsClaimOnResumeCannotDiscardExecution(t *testing.T) {
	f := newServiceFixture(t)
	f.backend.block.Store(true)
	stop := f.start()
	request := f.request(contract.Validate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	select {
	case <-f.backend.started:
	case <-time.After(5 * time.Second):
		t.Fatal("execution receipt was not persisted")
	}
	stop()
	f.backend.block.Store(false)
	f.backend.resumeError = fixtureNoEffectsError{contract.ErrInvalid}
	f.service, err = New(f.service.config)
	require.NoError(t, err)
	f.start()
	f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
	require.EventuallyWithT(t, func(c *assert.CollectT) {
		require.NoError(c, EnsureNoActiveRuns(t.Context(), f.client, "private"))
	}, 5*time.Second, time.Millisecond)
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.False(t, r.state.Executions["original"].Rejected)
	require.NotNil(t, r.state.Executions["original"].Receipt)
	f.backend.mu.Lock()
	require.Equal(t, 1, f.backend.resumes)
	require.Positive(t, f.backend.cancels)
	f.backend.mu.Unlock()
	require.EqualValues(t, 1, f.backend.startCalls.Load())
}

func TestGetChecksExactAggregateParentAndResult(t *testing.T) {
	for _, replacement := range []string{"task", "result"} {
		t.Run(replacement, func(t *testing.T) {
			f := newServiceFixture(t)
			stop := f.start()
			request := f.request(contract.Validate)
			_, err := f.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
			stop()
			if replacement == "result" {
				require.NoError(t, f.results.SaveResult(t.Context(), "private", status.TaskName, []byte("different result")))
			} else {
				task := new(corev1alpha1.Task)
				require.NoError(t, f.client.Get(t.Context(), client.ObjectKey{Namespace: "private", Name: status.TaskName}, task))
				task.Finalizers = nil
				require.NoError(t, f.client.Update(t.Context(), task))
				require.NoError(t, f.client.Delete(t.Context(), task))
				task.UID, task.ResourceVersion = "", ""
				require.NoError(t, f.client.Create(t.Context(), task))
			}
			_, err = f.service.Get(t.Context(), "private", request.RequestID)
			require.ErrorIs(t, err, contract.ErrConflict)
		})
	}
}
