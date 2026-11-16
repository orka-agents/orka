/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/memoryconsolidation"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type consolidationFixture struct {
	app   *fiber.App
	store *storageclient.Client
}

func newConsolidationFixture(t *testing.T, consolidation *corev1alpha1.MemoryConsolidationSpec) consolidationFixture {
	t.Helper()
	return newConsolidationFixtureWith(t, consolidation, nil)
}

// movedCursorStore refuses its first begins as if another run moved the
// cursor while the run read its inputs.
type movedCursorStore struct {
	store.MemoryConsolidationStore
	refusals int
	begins   int
}

func (s *movedCursorStore) BeginMemoryConsolidationRun(ctx context.Context, run *store.MemoryConsolidationRun) error {
	s.begins++
	if s.begins <= s.refusals {
		return store.PreconditionFailedErrorf("another run completed past this window's start")
	}
	return s.MemoryConsolidationStore.BeginMemoryConsolidationRun(ctx, run)
}

func newConsolidationFixtureWith(t *testing.T, consolidation *corev1alpha1.MemoryConsolidationSpec,
	wrap func(store.MemoryConsolidationStore) store.MemoryConsolidationStore) consolidationFixture {
	t.Helper()
	// A run's window ends a commit grace before now; begin as if that long
	// passed after the test wrote its Sessions.
	previous := memoryConsolidationNow
	memoryConsolidationNow = func() time.Time { return time.Now().Add(memoryconsolidation.WindowCommitGrace + 5*time.Second) }
	t.Cleanup(func() { memoryConsolidationNow = previous })
	scheme := internalCallerAuthScheme(t)
	task := internalCallerAuthTask()
	task.Spec = corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI,
		AgentRef: &corev1alpha1.AgentReference{Name: "curator"},
		AI:       &corev1alpha1.AISpec{Prompt: "Consolidate", MemoryConsolidation: consolidation}}
	job := internalCallerAuthJob(task, "job-a", "job-uid")
	pod := internalCallerAuthPod(task, "pod-a", "pod-uid", job)
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, job, pod).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	backing := testStorageClient(sqlite.NewStore(db, ":memory:"))
	var runs store.MemoryConsolidationStore = backing
	if wrap != nil {
		runs = wrap(backing)
	}
	h := NewInternalHandlers(backing, backing, backing, backing, backing, InternalHandlersConfig{Client: k8sClient, APIReader: k8sClient,
		MemoryStore: backing, MemoryProposalStore: backing, MemoryConsolidationStore: runs})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, internalCallerAuthWorkerUser("pod-a", "pod-uid"))
		return c.Next()
	})
	app.Post("/internal/v1/memory-consolidation/:namespace/begin", h.BeginMemoryConsolidation)
	app.Post("/internal/v1/memory-consolidation/:namespace/complete", h.CompleteMemoryConsolidation)
	return consolidationFixture{app: app, store: backing}
}

func (f consolidationFixture) post(t *testing.T, path string, body any, out any) int {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	req := httptest.NewRequest(http.MethodPost, "/internal/v1/memory-consolidation/default/"+path, bytes.NewReader(encoded))
	req.Header.Set("Content-Type", "application/json")
	resp, err := f.app.Test(req)
	require.NoError(t, err)
	if out != nil && resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(out))
	}
	return resp.StatusCode
}

func (f consolidationFixture) seedSessions(t *testing.T, count int) {
	t.Helper()
	for i := range count {
		name := fmt.Sprintf("session-%d", i)
		require.NoError(t, f.store.CreateSession(t.Context(), &store.SessionRecord{Namespace: "default", Name: name, SessionType: store.SessionTypeChat}))
		require.NoError(t, f.store.AppendMessages(t.Context(), "default", name, []store.SessionMessage{
			{ID: name + "-user", Role: "user", Content: "How do we release?"},
			{ID: name + "-assistant", Role: "assistant", Content: "Run make test, then tag the release."},
		}))
	}
}

func TestMemoryConsolidationRunProposesValidatedOperations(t *testing.T) {
	f := newConsolidationFixture(t, &corev1alpha1.MemoryConsolidationSpec{Mode: corev1alpha1.MemoryConsolidationPropose})
	f.seedSessions(t, 5)
	for _, id := range []string{"mem-a", "mem-b"} {
		require.NoError(t, f.store.CreateMemory(t.Context(), &store.Memory{ID: id, Namespace: "default", Content: "Release with make test"}))
	}
	require.NoError(t, f.store.CreateSession(t.Context(), &store.SessionRecord{Namespace: "default", Name: "gateway-chat", SessionType: store.SessionTypeGateway}))

	var begin memoryconsolidation.BeginResponse
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, &begin))
	require.Equal(t, memoryconsolidation.StatusRunning, begin.Status)
	require.NotNil(t, begin.Input)
	require.Len(t, begin.Input.Sessions, 5, "gateway Sessions are never read")
	require.Len(t, begin.Input.Memories, 2)

	var retried memoryconsolidation.BeginResponse
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, &retried))
	require.Equal(t, memoryconsolidation.StatusRunning, retried.Status, "a retried worker resumes the same run")

	submission := memoryconsolidation.Submission{Summary: "Two memories say the same thing.", Operations: []memoryconsolidation.Operation{
		{Type: "merge", Title: "Release checklist", Content: "Run make test, then tag the release.", Reason: "Duplicates",
			Targets: []store.MemoryProposalTarget{{MemoryID: "mem-a", Generation: 1}, {MemoryID: "mem-b", Generation: 1}},
			Sources: []store.MemoryProposalSource{{SessionName: "session-0", MessageID: "session-0-assistant"}}},
		{Type: "retire", Targets: []store.MemoryProposalTarget{{MemoryID: "mem-unknown", Generation: 1}}},
	}}
	var report memoryconsolidation.Report
	require.Equal(t, http.StatusOK, f.post(t, "complete", submission, &report))
	require.Equal(t, memoryconsolidation.StatusCompleted, report.Status)
	require.True(t, report.Structural, "the worker records only the structure as the Task result")
	require.Empty(t, report.Summary)
	require.Empty(t, report.Operations[0].Content)
	require.Len(t, report.Operations, 1)
	require.Len(t, report.Rejected, 1)
	require.Len(t, report.ProposalIDs, 1)
	require.Equal(t, 5, report.ReadSessions)

	proposal, err := f.store.GetMemoryProposal(t.Context(), "default", report.ProposalIDs[0])
	require.NoError(t, err)
	require.Equal(t, store.MemoryProposalTypeMerge, proposal.Type)
	require.Equal(t, store.MemoryProposalStatusPending, proposal.Status, "a run only proposes; a person reviews")
	require.Equal(t, "task-a", proposal.TaskName)
	require.Equal(t, "curator", proposal.AgentName)
	memory, err := f.store.GetMemory(t.Context(), "default", "mem-a")
	require.NoError(t, err)
	require.False(t, memory.Deleted, "no memory changes before review and apply")

	var again memoryconsolidation.Report
	require.Equal(t, http.StatusOK, f.post(t, "complete", memoryconsolidation.Submission{}, &again))
	require.Equal(t, report.ProposalIDs, again.ProposalIDs, "a finished run returns its report unchanged")
	var finished memoryconsolidation.BeginResponse
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, &finished))
	require.Equal(t, memoryconsolidation.StatusCompleted, finished.Status)
	require.Nil(t, finished.Input, "a finished run returns its report, not its inputs")
	require.NotNil(t, finished.Report)
}

func TestMemoryConsolidationDryRunCreatesNoProposals(t *testing.T) {
	f := newConsolidationFixture(t, &corev1alpha1.MemoryConsolidationSpec{})
	f.seedSessions(t, 5)
	var begin memoryconsolidation.BeginResponse
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, &begin))
	var report memoryconsolidation.Report
	require.Equal(t, http.StatusOK, f.post(t, "complete", memoryconsolidation.Submission{Operations: []memoryconsolidation.Operation{
		{Type: "add", Content: "Tag releases after make test.", Sources: []store.MemoryProposalSource{{SessionName: "session-1"}}},
	}}, &report))
	require.Equal(t, corev1alpha1.MemoryConsolidationDryRun, report.Mode)
	require.Len(t, report.Operations, 1, "a dry run reports what it would propose")
	require.Empty(t, report.ProposalIDs)
	proposals, err := f.store.ListMemoryProposals(t.Context(), store.MemoryProposalFilter{Namespace: "default"})
	require.NoError(t, err)
	require.Empty(t, proposals)
}

func TestMemoryConsolidationSkipsWithoutEnoughSessions(t *testing.T) {
	f := newConsolidationFixture(t, &corev1alpha1.MemoryConsolidationSpec{})
	f.seedSessions(t, 2)
	var begin memoryconsolidation.BeginResponse
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, &begin))
	require.Equal(t, memoryconsolidation.StatusSkipped, begin.Status)
	require.Nil(t, begin.Input)
	require.NotNil(t, begin.Report)
	require.Contains(t, begin.Report.SkippedReason, "2 Sessions changed")
	run, err := f.store.GetMemoryConsolidationRun(t.Context(), "default", "task-uid")
	require.NoError(t, err)
	cursor, err := f.store.GetMemoryConsolidationCursor(t.Context(), "default", "DryRun")
	require.NoError(t, err)
	require.NotNil(t, cursor)
	require.True(t, run.WindowStart.Equal(*cursor), "a skipped run keeps its start for the next run")
}

func TestMemoryConsolidationRoutesRefuseOtherTasks(t *testing.T) {
	f := newConsolidationFixture(t, nil)
	require.Equal(t, http.StatusForbidden, f.post(t, "begin", struct{}{}, nil))
	require.Equal(t, http.StatusForbidden, f.post(t, "complete", memoryconsolidation.Submission{}, nil))
}

func TestMemoryConsolidationRefusesASecondRunningRun(t *testing.T) {
	f := newConsolidationFixture(t, &corev1alpha1.MemoryConsolidationSpec{Mode: corev1alpha1.MemoryConsolidationPropose})
	f.seedSessions(t, 5)
	now := time.Now().UTC()
	require.NoError(t, f.store.BeginMemoryConsolidationRun(t.Context(), &store.MemoryConsolidationRun{Namespace: "default",
		TaskUID: "other-run", TaskName: "other", Mode: "DryRun", Status: store.MemoryConsolidationRunning,
		WindowStart: now.Add(-time.Hour), WindowEnd: now, CreatedAt: now}))
	require.Equal(t, http.StatusConflict, f.post(t, "begin", struct{}{}, nil),
		"a run never proposes over memories another run is still reading")
}

func TestMemoryConsolidationValidatesAgainstWhatTheRunRead(t *testing.T) {
	f := newConsolidationFixture(t, &corev1alpha1.MemoryConsolidationSpec{Mode: corev1alpha1.MemoryConsolidationPropose})
	f.seedSessions(t, 5)
	for _, id := range []string{"mem-a", "mem-b", "mem-c"} {
		require.NoError(t, f.store.CreateMemory(t.Context(), &store.Memory{ID: id, Namespace: "default", Content: "note " + id}))
	}
	var begin memoryconsolidation.BeginResponse
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, &begin))

	// While the model works, a cited Session moves on and a target is edited.
	require.NoError(t, f.store.AppendMessages(t.Context(), "default", "session-0", []store.SessionMessage{
		{ID: "session-0-later", Role: "user", Content: "A later turn"}}))
	edited, err := f.store.GetMemory(t.Context(), "default", "mem-c")
	require.NoError(t, err)
	edited.Content = "edited by a person"
	require.NoError(t, f.store.UpdateMemory(t.Context(), edited))
	var resumed memoryconsolidation.BeginResponse
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, &resumed))
	require.Equal(t, begin.Input, resumed.Input, "a resumed worker reads the same input")

	var report memoryconsolidation.Report
	require.Equal(t, http.StatusOK, f.post(t, "complete", memoryconsolidation.Submission{Operations: []memoryconsolidation.Operation{
		{Type: "merge", Content: "merged", Targets: []store.MemoryProposalTarget{{MemoryID: "mem-a", Generation: 1}, {MemoryID: "mem-b", Generation: 1}},
			Sources: []store.MemoryProposalSource{{SessionName: "session-0", MessageID: "session-0-user"}}},
		{Type: "retire", Targets: []store.MemoryProposalTarget{{MemoryID: "mem-c", Generation: 1}}},
		{Type: "add", Content: "x", Sources: []store.MemoryProposalSource{{SessionName: "session-0", MessageID: "session-0-later"}}},
	}}, &report))
	require.Len(t, report.Operations, 1, "evidence the run read stays valid after its Session changes")
	require.Len(t, report.Rejected, 2)
	require.Contains(t, report.Rejected[0].Reason, "changed since the run read it")
	require.Equal(t, "a source message is not one the run read in its Session", report.Rejected[1].Reason)
}

func TestMemoryConsolidationAbandonedRunCannotResumeOrComplete(t *testing.T) {
	f := newConsolidationFixture(t, &corev1alpha1.MemoryConsolidationSpec{Mode: corev1alpha1.MemoryConsolidationPropose})
	f.seedSessions(t, 5)
	var begin memoryconsolidation.BeginResponse
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, &begin))
	run, err := f.store.GetMemoryConsolidationRun(t.Context(), "default", "task-uid")
	require.NoError(t, err)
	successor := run.CreatedAt.Add(store.MemoryConsolidationRunLease + time.Minute)
	require.NoError(t, f.store.BeginMemoryConsolidationRun(t.Context(), &store.MemoryConsolidationRun{Namespace: "default",
		TaskUID: "successor", TaskName: "successor", Mode: "Propose", Status: store.MemoryConsolidationRunning,
		WindowStart: run.WindowStart, WindowEnd: successor, CreatedAt: successor}))
	require.Equal(t, http.StatusConflict, f.post(t, "begin", struct{}{}, nil), "a run taken over cannot resume")
	require.Equal(t, http.StatusConflict, f.post(t, "complete", memoryconsolidation.Submission{}, nil), "nor complete")
}

func TestMemoryConsolidationRefusesARevokedWorker(t *testing.T) {
	f := newConsolidationFixture(t, &corev1alpha1.MemoryConsolidationSpec{Mode: corev1alpha1.MemoryConsolidationPropose})
	f.seedSessions(t, 5)
	var begun memoryconsolidation.BeginResponse
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, &begun))
	require.NotNil(t, begun.Input)

	// Kubernetes still shows the Job running; only the store knows it was
	// revoked.
	require.NoError(t, f.store.RevokeTaskJob(t.Context(), store.TaskJobIdentity{Namespace: "default", TaskUID: "task-uid", JobUID: "job-uid"}))
	require.Equal(t, http.StatusForbidden, f.post(t, "begin", struct{}{}, nil), "a revoked worker reads no transcripts")
	submission := memoryconsolidation.Submission{Operations: []memoryconsolidation.Operation{{Type: memoryconsolidation.OperationAdd,
		Content: "Release by running make test, then tagging.",
		Sources: []store.MemoryProposalSource{{SessionName: "session-0", MessageID: "session-0-assistant"}}}}}
	require.Equal(t, http.StatusForbidden, f.post(t, "complete", submission, nil), "a revoked worker completes nothing")

	proposals, err := f.store.ListMemoryProposals(t.Context(), store.MemoryProposalFilter{Namespace: "default"})
	require.NoError(t, err)
	require.Empty(t, proposals)
	run, err := f.store.GetMemoryConsolidationRun(t.Context(), "default", "task-uid")
	require.NoError(t, err)
	require.Equal(t, store.MemoryConsolidationRunning, run.Status)
}

func TestMemoryConsolidationBeginsAgainAfterTheCursorMoved(t *testing.T) {
	for name, tc := range map[string]struct {
		refusals int
		status   int
	}{
		"once":       {refusals: 1, status: http.StatusOK},
		"every time": {refusals: memoryConsolidationBeginAttempts, status: http.StatusConflict},
	} {
		t.Run(name, func(t *testing.T) {
			moved := &movedCursorStore{refusals: tc.refusals}
			f := newConsolidationFixtureWith(t, &corev1alpha1.MemoryConsolidationSpec{Mode: corev1alpha1.MemoryConsolidationPropose},
				func(runs store.MemoryConsolidationStore) store.MemoryConsolidationStore {
					moved.MemoryConsolidationStore = runs
					return moved
				})
			f.seedSessions(t, 5)
			require.Equal(t, tc.status, f.post(t, "begin", struct{}{}, nil))
			require.Equal(t, min(tc.refusals+1, memoryConsolidationBeginAttempts), moved.begins,
				"the run reads its inputs again from the moved cursor, a bounded number of times")
		})
	}
}

func TestMemoryConsolidationRefusesAResultWithoutOperations(t *testing.T) {
	f := newConsolidationFixture(t, &corev1alpha1.MemoryConsolidationSpec{Mode: corev1alpha1.MemoryConsolidationPropose})
	f.seedSessions(t, 5)
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, nil))
	require.Equal(t, http.StatusBadRequest, f.post(t, "complete", map[string]any{"summary": "no list"}, nil))
	require.Equal(t, http.StatusBadRequest, f.post(t, "complete", map[string]any{"operations": nil}, nil))
	run, err := f.store.GetMemoryConsolidationRun(t.Context(), "default", "task-uid")
	require.NoError(t, err)
	require.Equal(t, store.MemoryConsolidationRunning, run.Status, "a malformed result finishes nothing, so the cursor stays")

	var report memoryconsolidation.Report
	require.Equal(t, http.StatusOK, f.post(t, "complete", map[string]any{"operations": []any{}}, &report),
		"an empty list is a run that found nothing")
	require.Equal(t, store.MemoryConsolidationCompleted, report.Status)
}

func TestMemoryConsolidationRedactsTheProposalsItCreates(t *testing.T) {
	f := newConsolidationFixture(t, &corev1alpha1.MemoryConsolidationSpec{Mode: corev1alpha1.MemoryConsolidationPropose})
	f.seedSessions(t, 5)
	require.Equal(t, http.StatusOK, f.post(t, "begin", struct{}{}, nil))
	const secret = "Authorization: Bearer consolidation-secret-value"
	submission := memoryconsolidation.Submission{Operations: []memoryconsolidation.Operation{{Type: memoryconsolidation.OperationAdd,
		Title: "Release " + secret, Content: "Call the release API with " + secret, Reason: "Seen in " + secret,
		Sources: []store.MemoryProposalSource{{SessionName: "session-0", MessageID: "session-0-assistant"}}}}}
	var report memoryconsolidation.Report
	require.Equal(t, http.StatusOK, f.post(t, "complete", submission, &report))
	require.Len(t, report.ProposalIDs, 1)
	proposal, err := f.store.GetMemoryProposal(t.Context(), "default", report.ProposalIDs[0])
	require.NoError(t, err)
	for field, text := range map[string]string{"title": proposal.Title, "content": proposal.Content, "description": proposal.Description} {
		require.NotContains(t, text, "consolidation-secret-value", "the stored proposal's %s is redacted", field)
	}
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), "consolidation-secret-value", "and so is the report")
}
