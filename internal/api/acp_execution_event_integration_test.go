package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	v2eventjournal "github.com/orka-agents/orka/internal/harness/v2/eventjournal"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	"github.com/orka-agents/orka/internal/tasktrace"
)

const (
	acpPublicConsumerAgentName  = "agent"
	acpPublicConsumerPromptID   = "prompt-1"
	acpPublicConsumerProvider   = "openai"
	acpPublicConsumerRuntimeID  = "runtime-1"
	acpPublicConsumerToolCallID = "call-1"
	acpPublicConsumerToolKind   = "file_read"
)

//nolint:gocyclo // The public consumer contract intentionally exercises events, trace, plan, and fork together.
func TestACPJournalEventsFeedPublicTaskConsumers(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "acp-public-consumers.db")
	db, err := sqlite.NewDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	persistence := sqlite.NewStore(db, dbPath)

	const taskName = "acp-public-consumers"
	source := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNamespace, Name: taskName},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "inspect the repository",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	journal := v2eventjournal.Journal{
		EventStore: persistence,
		MapContext: v2eventjournal.MapContext{
			Namespace: defaultNamespace, TaskName: taskName, StreamID: taskName,
			AgentName: acpPublicConsumerAgentName, Provider: acpPublicConsumerProvider, Model: "gpt-test",
		},
	}
	state, err := journal.Open(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	updates := []harnessv2.UpdateEvent{
		{
			Kind: harnessv2.UpdateToolCall,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: acpPublicConsumerToolCallID, Title: "Read README", Kind: acpPublicConsumerToolKind,
				Status: harnessv2.ToolCallStatusPending,
			},
		},
		{
			Kind: harnessv2.UpdatePlan,
			Plan: &harnessv2.PlanUpdate{Entries: []harnessv2.PlanEntry{
				{Content: "inspect", Status: harnessv2.PlanEntryCompleted},
				{Content: "report", Status: harnessv2.PlanEntryInProgress},
			}},
		},
		{
			Kind: harnessv2.UpdateUsage,
			Usage: &harnessv2.UsageUpdate{
				InputTokens: 120, OutputTokens: 30, CachedInputTokens: 40,
			},
		},
		{
			Kind: harnessv2.UpdateToolCallUpdate,
			ToolCall: &harnessv2.ToolCallUpdate{
				ToolCallID: acpPublicConsumerToolCallID, Title: "Read README", Kind: acpPublicConsumerToolKind,
				Status: harnessv2.ToolCallStatusCompleted,
			},
		},
	}
	for index, update := range updates {
		event := acpPublicConsumerUpdateEvent(uint64(index+1), now.Add(time.Duration(index)*time.Second), update)
		if update.Plan == nil {
			if appended, isNew, err := state.AppendUpdateIfNew(ctx, event); err != nil || !isNew || appended == nil {
				t.Fatalf("append ACP update %d = %#v new=%t err=%v", index+1, appended, isNew, err)
			}
			continue
		}
		projection := state.ProjectPlanUpdate(*update.Plan)
		plan := &store.PlanState{
			Namespace: defaultNamespace, TaskName: taskName,
			Summary: projection.Summary, ProgressPct: projection.ProgressPct,
			GoalComplete: projection.GoalComplete, PlanDocument: projection.Document,
		}
		if appended, isNew, err := state.AppendPlanUpdateIfNew(ctx, event, plan); err != nil || !isNew || appended == nil {
			t.Fatalf("append ACP plan = %#v new=%t err=%v", appended, isNew, err)
		}
	}

	h, app := setupTaskEventHandlers(t, persistence, source)
	h.planStore = persistence
	app.Get("/api/v1/tasks/:id/events", h.ListTaskEvents)
	app.Get("/api/v1/tasks/:id/trace", h.GetTaskTrace)
	app.Get("/api/v1/tasks/:id/plan", h.GetTaskPlan)
	app.Post("/api/v1/tasks/:id/fork", h.ForkTask)

	listed := getTaskEvents(t, app, "/api/v1/tasks/"+taskName+"/events?namespace=default")
	if listed.LatestSeq != 4 || len(listed.Events) != 4 {
		t.Fatalf("ACP public events = %#v", listed)
	}
	var usage *ExecutionEventResponse
	for index := range listed.Events {
		if listed.Events[index].Type == events.ExecutionEventTypeModelUsageUpdated {
			usage = &listed.Events[index]
			break
		}
	}
	if usage == nil || usage.InputTokens != 120 || usage.OutputTokens != 30 || usage.CachedInputTokens != 40 {
		t.Fatalf("ACP public usage event = %#v", usage)
	}

	traceResp, err := app.Test(httptest.NewRequest(
		http.MethodGet,
		"/api/v1/tasks/"+taskName+"/trace?namespace=default",
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	if traceResp.StatusCode != http.StatusOK {
		t.Fatalf("trace status = %d", traceResp.StatusCode)
	}
	var trace tasktrace.TaskTrace
	if err := json.NewDecoder(traceResp.Body).Decode(&trace); err != nil {
		t.Fatal(err)
	}
	if trace.LatestSeq != 4 || len(trace.ToolCalls) != 1 || trace.ToolCalls[0].Status != tasktrace.StatusCompleted {
		t.Fatalf("ACP public trace = %#v", trace)
	}

	planResp, err := app.Test(httptest.NewRequest(
		http.MethodGet,
		"/api/v1/tasks/"+taskName+"/plan?namespace=default",
		nil,
	))
	if err != nil {
		t.Fatal(err)
	}
	if planResp.StatusCode != http.StatusOK {
		t.Fatalf("plan status = %d", planResp.StatusCode)
	}
	var plan store.PlanState
	if err := json.NewDecoder(planResp.Body).Decode(&plan); err != nil {
		t.Fatal(err)
	}
	if plan.ProgressPct != 50 || !strings.Contains(plan.PlanDocument, "report") {
		t.Fatalf("ACP public plan = %#v", plan)
	}

	forkReq := httptest.NewRequest(
		http.MethodPost,
		"/api/v1/tasks/"+taskName+"/fork?namespace=default",
		bytes.NewBufferString(`{"afterSeq":2,"newTaskName":"acp-public-fork","prompt":"continue"}`),
	)
	forkReq.Header.Set("Content-Type", "application/json")
	forkResp, err := app.Test(forkReq)
	if err != nil {
		t.Fatal(err)
	}
	if forkResp.StatusCode != http.StatusCreated {
		t.Fatalf("fork status = %d", forkResp.StatusCode)
	}
	var forked ForkTaskResponse
	if err := json.NewDecoder(forkResp.Body).Decode(&forked); err != nil {
		t.Fatal(err)
	}
	if forked.AfterSeq != 2 || len(forked.ForkContext.Events) != 2 ||
		forked.ForkContext.Events[1].Type != events.ExecutionEventTypePlanUpdated {
		t.Fatalf("ACP fork checkpoint = %#v", forked)
	}
}

func acpPublicConsumerUpdateEvent(sequence uint64, at time.Time, update harnessv2.UpdateEvent) harnessv2.Event {
	return harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion,
		Type:     harnessv2.EventUpdate,
		Identity: harnessv2.EventIdentity{
			RuntimeInstanceID: acpPublicConsumerRuntimeID, SupervisorBootID: "boot-1",
			RuntimeSessionUID: "session-uid-1", RuntimeSessionGeneration: 1,
			TaskUID: "task-uid-1", TaskAttempt: 1, PromptID: acpPublicConsumerPromptID,
			Sequence: sequence, RequestDigest: harnessv2.RequestDigest("sha256:" + strings.Repeat("a", 64)),
			Timestamp: at,
		},
		Update: &update,
	}
}
