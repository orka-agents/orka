package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/harness/harnesstest"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
	"github.com/orka-agents/orka/workers/harness/cliwrapper"
)

const testFalseValue = "false"

func TestHarnessWrapperTaskRunsThroughTurnRunner(t *testing.T) {
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess, RuntimeName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	updated := runHarnessWrapperTaskToCompletion(t, r, task)
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
	if updated.Status.JobName != "" {
		t.Fatalf("JobName = %q, want no worker job", updated.Status.JobName)
	}
	if updated.Status.ResultRef == nil || !updated.Status.ResultRef.Available {
		t.Fatalf("ResultRef = %#v, want available result reference", updated.Status.ResultRef)
	}
	result, err := r.ResultStore.GetResult(context.Background(), task.Namespace, task.Name)
	if err != nil {
		t.Fatalf("GetResult: %v", err)
	}
	if string(result) != "ok" {
		t.Fatalf("result = %q, want ok", string(result))
	}
	eventsList, err := r.ExecutionEventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{Namespace: task.Namespace, StreamID: task.Name})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if !hasExecutionEventType(eventsList, events.ExecutionEventTypeAgentRuntimeCompleted) {
		t.Fatalf("events = %#v, want harness mapped runtime completed", eventsList)
	}
}

func TestHarnessWrapperStartClearsStaleResult(t *testing.T) {
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess, RuntimeName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	if err := r.ResultStore.SaveResult(context.Background(), task.Namespace, task.Name, []byte("stale")); err != nil {
		t.Fatal(err)
	}
	_ = runHarnessWrapperTaskToRunning(t, r, task)
	if _, err := r.ResultStore.GetResult(context.Background(), task.Namespace, task.Name); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("GetResult() after new turn start = %v, want ErrNotFound", err)
	}
}

func TestHarnessWrapperControllerSendsBearerToken(t *testing.T) {
	t.Setenv(harnessWrapperAuthValueEnv, "x")
	cfg := cliwrapper.DefaultConfig()
	cfg.AuthValue = "x"
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess, RuntimeName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	updated := runHarnessWrapperTaskToCompletion(t, r, task)
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", updated.Status.Phase)
	}
}

func TestPatchHarnessWrapperStartedPreservesPlannedTurnAnnotationsFromLocalTask(t *testing.T) {
	task, _ := harnessWrapperTaskAndAgent()
	expected := map[string]string{
		harnessWrapperTurnIDAnnotation:  "turn-1",
		harnessWrapperRuntimeAnnotation: "runtime-1",
		harnessWrapperCorrelationIDAnno: "correlation-1",
		harnessWrapperLastFrameSeqAnno:  "0",
		harnessWrapperPlannedAtAnno:     time.Now().UTC().Format(time.RFC3339Nano),
		harnessWrapperMetadataAnno:      ` {"runtime":"claude","wrapper":"cli"} `,
	}
	local := task.DeepCopy()
	local.Annotations = map[string]string{}
	maps.Copy(local.Annotations, expected)
	local.Annotations[harnessWrapperOutputFetchRetriesAnno] = "1"
	r := newUnitReconciler(newTestScheme(), task)

	if err := r.patchHarnessWrapperStarted(context.Background(), local, false); err != nil {
		t.Fatalf("patchHarnessWrapperStarted: %v", err)
	}

	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	for _, key := range []string{
		harnessWrapperTurnIDAnnotation,
		harnessWrapperRuntimeAnnotation,
		harnessWrapperCorrelationIDAnno,
		harnessWrapperLastFrameSeqAnno,
		harnessWrapperPlannedAtAnno,
		harnessWrapperMetadataAnno,
	} {
		if updated.Annotations[key] != expected[key] {
			t.Fatalf("annotation %s = %q, want %q", key, updated.Annotations[key], expected[key])
		}
	}
	if updated.Annotations[harnessWrapperStartedAnno] != scheduledRunLabelValue {
		t.Fatalf("started annotation = %q, want %q", updated.Annotations[harnessWrapperStartedAnno], scheduledRunLabelValue)
	}
	if _, ok := updated.Annotations[harnessWrapperOutputFetchRetriesAnno]; ok {
		t.Fatalf("output retry annotation should not be restored during start: %#v", updated.Annotations)
	}
}

func TestHarnessWrapperTaskRunsAgainstRuntimeRefAgentRuntime(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "fibey-agentkit"})
	defer server.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, server.URL())
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	updated := runHarnessWrapperTaskToCompletion(t, r, task)
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
	if got := updated.Annotations[harnessWrapperRuntimeRefAnno]; got != "fibey-agentkit" {
		t.Fatalf("runtimeRef annotation = %q, want fibey-agentkit", got)
	}
	if got := updated.Annotations[harnessWrapperContractAnno]; got != "orka.harness.v1" {
		t.Fatalf("contract annotation = %q, want orka.harness.v1", got)
	}
}

func TestHarnessWrapperBrokeredReadToolExecutesAndContinuesRuntime(t *testing.T) {
	var toolCalls atomic.Int32
	var idempotencyHeader atomic.Value
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		toolCalls.Add(1)
		idempotencyHeader.Store(r.Header.Get("Idempotency-Key"))
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode tool request: %v", err)
		}
		if body["incident"] != "quincy-north" {
			t.Fatalf("tool body = %#v, want incident", body)
		}
		_, _ = w.Write([]byte(`{"success":true,"data":{"status":"investigating"}}`))
	}))
	defer toolServer.Close()

	var started harness.StartTurnRequest
	var continued harness.ContinueTurnRequest
	continueCh := make(chan struct{})
	runtimeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case harness.CapabilitiesPath:
			harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
				Version:                 harness.ProtocolVersion,
				ProtocolVersion:         harness.ProtocolVersion,
				Transport:               harness.HTTPTransport,
				RuntimeName:             "fibey-agentkit",
				ProviderKind:            harness.ProviderKindRemote,
				ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved, harness.ToolExecutionModeBrokered},
				BrokeredToolClasses:     []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
				SupportsCancel:          true,
				SupportsRuntimeSessions: true,
				SupportsContinuation:    true,
			})
		case harness.TurnsPath:
			if err := json.NewDecoder(r.Body).Decode(&started); err != nil {
				t.Fatalf("decode start turn: %v", err)
			}
			eventsPath, _ := harness.EventStreamPath(started.TurnID)
			harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
				Version:          harness.ProtocolVersion,
				Accepted:         true,
				RuntimeSessionID: started.RuntimeSessionID,
				TurnID:           started.TurnID,
				CorrelationID:    started.CorrelationID,
				EventStreamPath:  eventsPath,
			})
		default:
			turnID, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
			if err != nil || turnID != started.TurnID {
				harness.WriteError(w, http.StatusNotFound, "not found")
				return
			}
			switch resource {
			case harness.TurnResourceEvents:
				w.Header().Set("Content-Type", "text/event-stream")
				_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
					Version:          harness.ProtocolVersion,
					Type:             harness.FrameTurnStarted,
					RuntimeSessionID: started.RuntimeSessionID,
					TurnID:           started.TurnID,
					CorrelationID:    started.CorrelationID,
					Seq:              1,
				})
				_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
					Version:          harness.ProtocolVersion,
					Type:             harness.FrameToolCallRequested,
					RuntimeSessionID: started.RuntimeSessionID,
					TurnID:           started.TurnID,
					CorrelationID:    started.CorrelationID,
					Seq:              2,
					ToolName:         "read_incident",
					ToolCallID:       "call-read-1",
					Content:          json.RawMessage(`{"incident":"quincy-north"}`),
				})
				<-continueCh
				_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
					Version:          harness.ProtocolVersion,
					Type:             harness.FrameToolResultReceived,
					RuntimeSessionID: started.RuntimeSessionID,
					TurnID:           started.TurnID,
					CorrelationID:    started.CorrelationID,
					Seq:              3,
					ToolName:         "read_incident",
					ToolCallID:       "call-read-1",
					Content:          continued.ToolResults[0].Output,
				})
				_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
					Version:          harness.ProtocolVersion,
					Type:             harness.FrameTurnCompleted,
					RuntimeSessionID: started.RuntimeSessionID,
					TurnID:           started.TurnID,
					CorrelationID:    started.CorrelationID,
					Seq:              4,
					Completed:        &harness.TurnCompleted{Result: "done", FinalEventSeq: 4},
				})
				_ = harness.WriteSSEDone(w)
			case harness.TurnResourceContinue:
				if err := json.NewDecoder(r.Body).Decode(&continued); err != nil {
					t.Fatalf("decode continue turn: %v", err)
				}
				close(continueCh)
				harness.WriteJSON(w, http.StatusAccepted, harness.ContinueTurnResponse{
					Version:          harness.ProtocolVersion,
					Accepted:         true,
					RuntimeSessionID: continued.RuntimeSessionID,
					TurnID:           continued.TurnID,
					CorrelationID:    continued.CorrelationID,
				})
			default:
				harness.WriteError(w, http.StatusNotFound, "not found")
			}
		}
	}))
	defer runtimeServer.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, runtimeServer.URL)
	runtime.Status.ObservedCapabilities.ProviderKind = string(harness.ProviderKindRemote)
	runtime.Status.ObservedCapabilities.ToolExecutionModes = []corev1alpha1.AgentRuntimeToolExecutionMode{
		corev1alpha1.AgentRuntimeToolExecutionModeObserved,
		corev1alpha1.AgentRuntimeToolExecutionModeBrokered,
	}
	runtime.Status.ObservedCapabilities.BrokeredToolClasses = []corev1alpha1.AgentRuntimeBrokeredToolClass{
		corev1alpha1.AgentRuntimeBrokeredToolClassRead,
	}
	runtime.Status.ObservedCapabilities.SupportsContinuation = true
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "read_incident", Namespace: task.Namespace},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Read incident status",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP:              &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token, tool)

	updated := runHarnessWrapperTaskToCompletion(t, r, task)
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
	if started.ToolExecutionMode != harness.ToolExecutionModeBrokered {
		t.Fatalf("ToolExecutionMode = %q, want brokered", started.ToolExecutionMode)
	}
	if toolCalls.Load() != 1 {
		t.Fatalf("tool calls = %d, want 1", toolCalls.Load())
	}
	if got, _ := idempotencyHeader.Load().(string); got == "" {
		t.Fatalf("Idempotency-Key header was not set")
	}
	if len(continued.ToolResults) != 1 || continued.ToolResults[0].ToolCallID != "call-read-1" {
		t.Fatalf("continue tool results = %#v", continued.ToolResults)
	}
	listed, err := r.ExecutionEventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: task.Namespace,
		StreamID:  task.Name,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if !hasExecutionEventType(listed, events.ExecutionEventTypeToolCallCompleted) {
		t.Fatalf("events = %#v, want brokered ToolCallCompleted", listed)
	}
}

func TestHarnessWrapperBrokeredRejectsToolCallIDReuseAcrossTools(t *testing.T) {
	var readExecutions atomic.Int32
	readServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		readExecutions.Add(1)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer readServer.Close()
	var otherExecutions atomic.Int32
	otherServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherExecutions.Add(1)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer otherServer.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident", "other_read"}}
	readTool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "read_incident", Namespace: task.Namespace}, Spec: corev1alpha1.ToolSpec{Description: "Read incident", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead, HTTP: &corev1alpha1.HTTPExecution{URL: readServer.URL}}}
	otherTool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "other_read", Namespace: task.Namespace}, Spec: corev1alpha1.ToolSpec{Description: "Other read", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead, HTTP: &corev1alpha1.HTTPExecution{URL: otherServer.URL}}}
	r := newUnitReconciler(newTestScheme(), task, agent, readTool, otherTool)
	frame := harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: harness.FrameToolCallRequested, RuntimeSessionID: "runtime-session", TurnID: "turn-1", CorrelationID: "corr-1", Seq: 1, ToolName: "read_incident", ToolCallID: "call-1", Content: json.RawMessage(`{"incident":"inc-1"}`)}
	if result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame); err != nil || result.Error != nil {
		t.Fatalf("first handle = %#v, %v", result, err)
	}
	frame.ToolName = "other_read"
	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil {
		t.Fatalf("reused id handle error = %v", err)
	}
	if result.Error == nil || result.Error.Code != "tool_call_id_reused" {
		t.Fatalf("result = %#v, want tool_call_id_reused", result)
	}
	if readExecutions.Load() != 1 || otherExecutions.Load() != 0 {
		t.Fatalf("executions read=%d other=%d, want only initial tool", readExecutions.Load(), otherExecutions.Load())
	}
}

func TestHarnessWrapperBrokeredReplayNormalizesToolIdentity(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executions.Add(1)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer toolServer.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident"}}
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Name: "read_incident", Namespace: task.Namespace}, Spec: corev1alpha1.ToolSpec{Description: "Read incident", BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead, HTTP: &corev1alpha1.HTTPExecution{URL: toolServer.URL}}}
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	frame := harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: harness.FrameToolCallRequested, RuntimeSessionID: "runtime-session", TurnID: "turn-1", CorrelationID: "corr-1", Seq: 1, ToolName: " read_incident ", ToolCallID: " call-1 ", Content: json.RawMessage(`{"incident":"inc-1"}`)}
	if result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame); err != nil || result.Error != nil {
		t.Fatalf("first handle = %#v, %v", result, err)
	}
	if result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame); err != nil || result.Error != nil {
		t.Fatalf("replay handle = %#v, %v", result, err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions = %d, want replay from normalized ledger", executions.Load())
	}
}

func TestHarnessWrapperBrokeredReplayRejectsChangedArguments(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executions.Add(1)
		_, _ = w.Write([]byte(`{"success":true,"data":{"status":"investigating"}}`))
	}))
	defer toolServer.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident"}}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "read_incident", Namespace: task.Namespace},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Read incident",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP:              &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	frame := harness.HarnessEventFrame{Version: harness.ProtocolVersion, Type: harness.FrameToolCallRequested, RuntimeSessionID: "runtime-session", TurnID: "turn-1", CorrelationID: "corr-1", Seq: 1, ToolName: "read_incident", ToolCallID: "call-1", Content: json.RawMessage(`{"incident":"inc-1"}`)}
	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil || result.Error != nil {
		t.Fatalf("first handle = %#v, %v", result, err)
	}
	changed := frame
	changed.Content = json.RawMessage(`{"incident":"inc-2"}`)
	result, err = r.handleHarnessBrokeredToolCall(context.Background(), task, agent, changed)
	if err != nil {
		t.Fatalf("changed handle error = %v", err)
	}
	if result.Error == nil || result.Error.Code != "tool_call_arguments_changed" {
		t.Fatalf("result = %#v, want changed arguments error", result)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions = %d, want cached failure without second call", executions.Load())
	}
}

func TestHarnessWrapperBrokeredReadRejectsInvalidArgsBeforeExecution(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executions.Add(1)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer toolServer.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident"}}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "read_incident", Namespace: task.Namespace},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Read incident",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			Parameters:        &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","required":["incident"],"properties":{"incident":{"type":"string"}}}`)},
			HTTP:              &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	frame := harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameToolCallRequested,
		RuntimeSessionID: "runtime-session",
		TurnID:           "turn-1",
		CorrelationID:    "corr-1",
		Seq:              1,
		ToolName:         "read_incident",
		ToolCallID:       "call-1",
		Content:          json.RawMessage(`{"other":"value"}`),
	}
	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil {
		t.Fatalf("handleHarnessBrokeredToolCall() error = %v", err)
	}
	if result.Error == nil || result.Error.Code != "invalid_tool_arguments" {
		t.Fatalf("result = %#v, want invalid_tool_arguments", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("executions = %d, want none", executions.Load())
	}
}

func TestHarnessWrapperBrokeredWriteRejectsInvalidArgsBeforeApproval(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"dispatch_work_order"}}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch_work_order", Namespace: task.Namespace, ResourceVersion: "1"},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Dispatch technician",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			Parameters:        &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","required":["incident"],"properties":{"incident":{"type":"string"}}}`)},
			HTTP:              &corev1alpha1.HTTPExecution{URL: "http://tool.invalid"},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	frame := brokeredWriteFrame()
	frame.Content = json.RawMessage(`{"action":"dispatch technician"}`)
	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil {
		t.Fatalf("handleHarnessBrokeredToolCall() error = %v", err)
	}
	if result.Error == nil || result.Error.Code != "invalid_tool_arguments" {
		t.Fatalf("result = %#v, want invalid_tool_arguments", result)
	}
	listed, err := r.ExecutionEventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{Namespace: task.Namespace, StreamID: task.Name, EventTypes: []string{events.ExecutionEventTypeApprovalRequested}})
	if err != nil {
		t.Fatalf("list events: %v", err)
	}
	if len(listed) != 0 {
		t.Fatalf("approval events = %d, want none", len(listed))
	}
}

func TestHarnessWrapperBrokeredToolCallRejectsDisallowedAndUnclassifiedTools(t *testing.T) {
	tests := []struct {
		name         string
		allowedTools []string
		toolClass    corev1alpha1.AgentRuntimeBrokeredToolClass
		wantCode     string
	}{
		{
			name:         "disallowed",
			allowedTools: []string{"other_tool"},
			toolClass:    corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			wantCode:     "tool_not_allowed",
		},
		{
			name:         "unclassified",
			allowedTools: []string{"read_incident"},
			wantCode:     "tool_class_not_allowed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task, agent := harnessWrapperTaskAndAgent()
			agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: tt.allowedTools}
			tool := &corev1alpha1.Tool{
				ObjectMeta: metav1.ObjectMeta{Name: "read_incident", Namespace: task.Namespace},
				Spec: corev1alpha1.ToolSpec{
					Description:       "Read incident status",
					BrokeredToolClass: tt.toolClass,
					HTTP:              &corev1alpha1.HTTPExecution{URL: "https://tools.example.test/read"},
				},
			}
			r := newUnitReconciler(newTestScheme(), task, agent, tool)
			result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, harness.HarnessEventFrame{
				Version:          harness.ProtocolVersion,
				Type:             harness.FrameToolCallRequested,
				RuntimeSessionID: "runtime-session",
				TurnID:           "turn",
				CorrelationID:    "corr",
				Seq:              1,
				ToolName:         "read_incident",
				ToolCallID:       "call-read-1",
				Content:          json.RawMessage(`{"incident":"quincy-north"}`),
			})
			if err != nil {
				t.Fatalf("handleHarnessBrokeredToolCall() error = %v", err)
			}
			if result.Error == nil || result.Error.Code != tt.wantCode {
				t.Fatalf("result.Error = %#v, want %s", result.Error, tt.wantCode)
			}
			listed, err := r.ExecutionEventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
				Namespace: task.Namespace,
				StreamID:  task.Name,
			})
			if err != nil {
				t.Fatalf("ListExecutionEvents: %v", err)
			}
			if !hasExecutionEventType(listed, events.ExecutionEventTypeToolCallFailed) {
				t.Fatalf("events = %#v, want ToolCallFailed", listed)
			}
		})
	}
}

func TestHarnessWrapperBrokeredToolRejectsClassChangedAfterPlanning(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident"}}
	task.Annotations = map[string]string{
		harnessWrapperMetadataAnno: `{"toolExecutionMode":"brokered","brokeredToolClassMap":"{\"read_incident\":\"read\"}"}`,
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "read_incident", Namespace: task.Namespace},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Read incident status",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			HTTP:              &corev1alpha1.HTTPExecution{URL: "https://tools.example.test/read"},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameToolCallRequested,
		RuntimeSessionID: "runtime-session",
		TurnID:           "turn",
		CorrelationID:    "corr",
		Seq:              1,
		ToolName:         "read_incident",
		ToolCallID:       "call-read-1",
		Content:          json.RawMessage(`{"incident":"quincy-north"}`),
	})
	if err != nil {
		t.Fatalf("handleHarnessBrokeredToolCall() error = %v", err)
	}
	if result.Error == nil || result.Error.Code != "tool_class_changed" {
		t.Fatalf("result.Error = %#v, want tool_class_changed", result.Error)
	}
}

func TestHarnessWrapperBrokeredToolRejectsCorruptPlannedClassMap(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident"}}
	task.Annotations = map[string]string{
		harnessWrapperMetadataAnno: `{"toolExecutionMode":"brokered","brokeredToolClassMap":"not-json"}`,
	}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "read_incident", Namespace: task.Namespace},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Read incident status",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP:              &corev1alpha1.HTTPExecution{URL: "https://tools.example.test/read"},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameToolCallRequested,
		RuntimeSessionID: "runtime-session",
		TurnID:           "turn",
		CorrelationID:    "corr",
		Seq:              1,
		ToolName:         "read_incident",
		ToolCallID:       "call-read-1",
		Content:          json.RawMessage(`{"incident":"quincy-north"}`),
	})
	if err != nil {
		t.Fatalf("handleHarnessBrokeredToolCall() error = %v", err)
	}
	if result.Error == nil || result.Error.Code != "invalid_brokered_plan" {
		t.Fatalf("result.Error = %#v, want invalid_brokered_plan", result.Error)
	}
}

func TestHarnessWrapperBrokeredWriteToolRequestsApprovalThenExecutesAfterApproval(t *testing.T) {
	var executions atomic.Int32
	var idempotencyHeader atomic.Value
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executions.Add(1)
		idempotencyHeader.Store(r.Header.Get("Idempotency-Key"))
		_, _ = w.Write([]byte(`{"success":true,"data":{"dispatched":true}}`))
	}))
	defer toolServer.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"dispatch_work_order"}}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch_work_order", Namespace: task.Namespace, ResourceVersion: "1"},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Dispatch technician",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			HTTP:              &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	frame := brokeredWriteFrame()

	_, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if !errors.Is(err, errHarnessBrokeredApprovalPending) {
		t.Fatalf("first handleHarnessBrokeredToolCall() error = %v, want pending", err)
	}
	approvalID := brokeredApprovalIDForTest(t, r, task)
	if executions.Load() != 0 {
		t.Fatalf("executions before approval = %d, want 0", executions.Load())
	}
	appendApprovalDecisionForTest(t, r, task, approvalID, events.ExecutionEventTypeApprovalApproved)

	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil {
		t.Fatalf("approved handleHarnessBrokeredToolCall() error = %v", err)
	}
	if result.Error != nil || len(result.Output) == 0 {
		t.Fatalf("result = %#v, want successful output", result)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions after approval = %d, want 1", executions.Load())
	}
	if got, _ := idempotencyHeader.Load().(string); got != approvalID {
		t.Fatalf("Idempotency-Key = %q, want approvalID %q", got, approvalID)
	}
	result, err = r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil {
		t.Fatalf("replay handleHarnessBrokeredToolCall() error = %v", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions after replay = %d, want 1", executions.Load())
	}
}

func TestHarnessWrapperBrokeredWriteToolFailsClosedForUnresolvedExecutionLedger(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executions.Add(1)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer toolServer.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"dispatch_work_order"}}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch_work_order", Namespace: task.Namespace, ResourceVersion: "1"},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Dispatch technician",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			HTTP:              &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	frame := brokeredWriteFrame()
	_, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if !errors.Is(err, errHarnessBrokeredApprovalPending) {
		t.Fatalf("first handleHarnessBrokeredToolCall() error = %v, want pending", err)
	}
	approvalID := brokeredApprovalIDForTest(t, r, task)
	appendApprovalDecisionForTest(t, r, task, approvalID, events.ExecutionEventTypeApprovalApproved)
	idempotencyKey := harness.ToolRequestIdempotencyKey(frame.RuntimeSessionID, frame.TurnID, frame.ToolCallID)
	content, _ := json.Marshal(map[string]any{
		"brokered":         true,
		"idempotencyKey":   idempotencyKey,
		"targetArgsDigest": "sha256:different",
		"executionState":   "started",
	})
	if _, err := r.ExecutionEventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  task.Namespace,
		StreamID:   task.Name,
		TaskName:   task.Name,
		Type:       events.ExecutionEventTypeToolCallStarted,
		ToolName:   frame.ToolName,
		ToolCallID: frame.ToolCallID,
		Content:    content,
	}); err != nil {
		t.Fatalf("append unresolved ledger: %v", err)
	}

	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil {
		t.Fatalf("handleHarnessBrokeredToolCall() error = %v", err)
	}
	if result.Error == nil || result.Error.Code != "tool_execution_outcome_unknown" {
		t.Fatalf("result = %#v, want outcome unknown error", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("executions = %d, want fail-closed without duplicate side effect", executions.Load())
	}
}

func TestHarnessWrapperBrokeredWriteToolDeclineContinuesWithoutExecution(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		executions.Add(1)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer toolServer.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"dispatch_work_order"}}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "dispatch_work_order", Namespace: task.Namespace, ResourceVersion: "1"},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Dispatch technician",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			HTTP:              &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	frame := brokeredWriteFrame()

	_, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if !errors.Is(err, errHarnessBrokeredApprovalPending) {
		t.Fatalf("first handleHarnessBrokeredToolCall() error = %v, want pending", err)
	}
	approvalID := brokeredApprovalIDForTest(t, r, task)
	appendApprovalDecisionForTest(t, r, task, approvalID, events.ExecutionEventTypeApprovalDeclined)

	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil {
		t.Fatalf("declined handleHarnessBrokeredToolCall() error = %v", err)
	}
	if result.Error == nil || result.Error.Code != "approval_declined" {
		t.Fatalf("result = %#v, want approval_declined", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("executions after decline = %d, want 0", executions.Load())
	}
	result, err = r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil {
		t.Fatalf("decline replay error = %v", err)
	}
	if result.Approved || result.Error == nil || result.Error.Code != "approval_declined" {
		t.Fatalf("decline replay result = %#v, want Approved=false approval_declined", result)
	}
}

func TestHarnessWrapperBrokeredCoordinationDelegateTaskCreatesChild(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	agent.Spec.Coordination = &corev1alpha1.CoordinationConfig{
		Enabled:  true,
		MaxDepth: 3,
		AllowedAgents: []corev1alpha1.AllowedAgent{{
			Name: "worker-agent",
		}},
	}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"delegate_task"}}
	targetAgent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "worker-agent", Namespace: task.Namespace},
		Spec: corev1alpha1.AgentSpec{
			ProviderRef: &corev1alpha1.ProviderReference{Name: "test-provider"},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, targetAgent)
	frame := harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameToolCallRequested,
		RuntimeSessionID: "runtime-session",
		TurnID:           "turn",
		CorrelationID:    "corr",
		Seq:              1,
		ToolName:         "delegate_task",
		ToolCallID:       "call-delegate-1",
		Content:          json.RawMessage(`{"agent":"worker-agent","prompt":"investigate"}`),
	}

	result, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, frame)
	if err != nil {
		t.Fatalf("handleHarnessBrokeredToolCall() error = %v", err)
	}
	if result.Error != nil || len(result.Output) == 0 {
		t.Fatalf("result = %#v, want successful delegate output", result)
	}
	var tasks corev1alpha1.TaskList
	if err := r.List(context.Background(), &tasks); err != nil {
		t.Fatalf("List Tasks: %v", err)
	}
	childCount := 0
	for _, item := range tasks.Items {
		if item.Name != task.Name && labels.ParentTaskName(item.Labels, item.Annotations) == task.Name {
			childCount++
			if item.Spec.AgentRef == nil || item.Spec.AgentRef.Name != "worker-agent" {
				t.Fatalf("child AgentRef = %#v", item.Spec.AgentRef)
			}
		}
	}
	if childCount != 1 {
		t.Fatalf("child task count = %d, want 1; tasks=%#v", childCount, tasks.Items)
	}
}

func TestHarnessWrapperBrokeredCoordinationMessagingToolsUseMessageStore(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	agent.Spec.Coordination = &corev1alpha1.CoordinationConfig{Enabled: true, MaxDepth: 3}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"send_message", "check_messages"}}
	r := newUnitReconciler(newTestScheme(), task, agent)

	sendResult, err := r.handleHarnessBrokeredToolCall(context.Background(), task, agent, harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameToolCallRequested,
		RuntimeSessionID: "runtime-session",
		TurnID:           "turn",
		CorrelationID:    "corr",
		Seq:              1,
		ToolName:         "send_message",
		ToolCallID:       "call-send-1",
		Content:          json.RawMessage(`{"to_task":"*","content":"hello sibling"}`),
	})
	if err != nil {
		t.Fatalf("send handleHarnessBrokeredToolCall() error = %v", err)
	}
	if sendResult.Error != nil {
		t.Fatalf("send result error = %#v", sendResult.Error)
	}

	workerTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "worker-b",
			Namespace: task.Namespace,
			Labels:    map[string]string{labels.LabelParentTask: labels.SelectorValue(task.Name)},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName: task.Name,
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAI},
	}
	checkAgent := agent.DeepCopy()
	checkTask := task.DeepCopy()
	checkTask.Name = "worker-b"
	checkTask.Labels = maps.Clone(workerTask.Labels)
	checkTask.Annotations = maps.Clone(workerTask.Annotations)
	checkTask.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"check_messages"}}
	if err := r.Create(context.Background(), workerTask); err != nil {
		t.Fatalf("create worker task: %v", err)
	}
	checkResult, err := r.handleHarnessBrokeredToolCall(context.Background(), checkTask, checkAgent, harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameToolCallRequested,
		RuntimeSessionID: "runtime-session",
		TurnID:           "turn",
		CorrelationID:    "corr",
		Seq:              2,
		ToolName:         "check_messages",
		ToolCallID:       "call-check-1",
		Content:          json.RawMessage(`{"mark_read":true}`),
	})
	if err != nil {
		t.Fatalf("check handleHarnessBrokeredToolCall() error = %v", err)
	}
	if checkResult.Error != nil {
		t.Fatalf("check result = %#v", checkResult)
	}
	if !strings.Contains(string(checkResult.Output), "hello sibling") {
		t.Fatalf("check output = %s, want message content", string(checkResult.Output))
	}
}

func TestHarnessWrapperBrokeredRunningTaskFailsClosedWhenAgentMissing(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "fibey-agentkit", AuthToken: "x"})
	defer server.Close()
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident"}}
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	task.Status.Attempts = 1
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[harnessWrapperStartedAnno] = scheduledRunLabelValue
	task.Annotations[harnessWrapperTurnIDAnnotation] = string(harnessWrapperTurnID(task, 1))
	task.Annotations[harnessWrapperRuntimeAnnotation] = string(harnessWrapperRuntimeSessionID(task, "fibey-agentkit"))
	task.Annotations[harnessWrapperCorrelationIDAnno] = string(task.UID)
	task.Annotations[harnessWrapperLastFrameSeqAnno] = "0"
	task.Annotations[harnessWrapperMetadataAnno] = `{"runtime":"fibey-agentkit","runtimeRef":"fibey-agentkit","toolExecutionMode":"brokered"}`
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, server.URL())
	task.Status.HarnessRuntime = &corev1alpha1.HarnessRuntimeStatus{
		RuntimeRefName:         runtime.Name,
		RuntimeName:            runtime.Name,
		ContractVersion:        harness.ProtocolVersion,
		Endpoint:               runtime.Spec.Deployment.Endpoint,
		RuntimeGeneration:      runtime.Generation,
		AuthRefName:            token.Name,
		AuthRefField:           "token",
		AuthRefResourceVersion: token.ResourceVersion,
	}
	r := newUnitReconciler(newTestScheme(), task, runtime, token)
	if _, err := r.handleRunning(context.Background(), task); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
}

func brokeredWriteFrame() harness.HarnessEventFrame {
	return harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameToolCallRequested,
		RuntimeSessionID: "runtime-session",
		TurnID:           "turn",
		CorrelationID:    "corr",
		Seq:              1,
		ToolName:         "dispatch_work_order",
		ToolCallID:       "call-write-1",
		Content:          json.RawMessage(`{"incident":"quincy-north","action":"dispatch technician"}`),
	}
}

func TestMarkHarnessBrokeredApprovalWaitingSetsTaskCondition(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	r := newUnitReconciler(newTestScheme(), task, agent)
	if err := r.markHarnessBrokeredApprovalWaiting(context.Background(), task, "approval-canonical", "dispatch_work_order"); err != nil {
		t.Fatalf("markHarnessBrokeredApprovalWaiting: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	cond := meta.FindStatusCondition(updated.Status.Conditions, ConditionTypeWaitingForApproval)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != "BrokeredToolApprovalPending" {
		t.Fatalf("WaitingForApproval condition = %#v", cond)
	}
	if !strings.Contains(updated.Status.Message, "approval-canonical") {
		t.Fatalf("status message = %q", updated.Status.Message)
	}
	if err := r.clearHarnessBrokeredApprovalWaiting(context.Background(), task, "dispatch_work_order"); err != nil {
		t.Fatalf("clearHarnessBrokeredApprovalWaiting: %v", err)
	}
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get cleared task: %v", err)
	}
	cond = meta.FindStatusCondition(updated.Status.Conditions, ConditionTypeWaitingForApproval)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != "BrokeredToolContinued" {
		t.Fatalf("cleared WaitingForApproval condition = %#v", cond)
	}
}

func brokeredApprovalIDForTest(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task) string {
	t.Helper()
	listed, err := r.ExecutionEventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace:  task.Namespace,
		StreamID:   task.Name,
		EventTypes: []string{events.ExecutionEventTypeApprovalRequested},
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(listed) != 1 || listed[0].ToolCallID == "" {
		t.Fatalf("approval events = %#v, want one approval with ID", listed)
	}
	return listed[0].ToolCallID
}

func appendApprovalDecisionForTest(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task, approvalID, eventType string) {
	t.Helper()
	content, err := json.Marshal(map[string]string{
		"approvalID": approvalID,
		"taskUID":    string(task.UID),
		"actor":      "unit-test",
	})
	if err != nil {
		t.Fatalf("marshal approval decision: %v", err)
	}
	if _, err := r.ExecutionEventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
		TaskName:   task.Name,
		Type:       eventType,
		Severity:   events.ExecutionEventSeverityInfo,
		ToolCallID: approvalID,
		Summary:    "approval decision",
		Content:    content,
	}); err != nil {
		t.Fatalf("append approval decision: %v", err)
	}
}

func TestHarnessWrapperBrokeredContinueFailureRetriesToolRequest(t *testing.T) {
	var toolCalls atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		toolCalls.Add(1)
		_, _ = w.Write([]byte(`{"success":true}`))
	}))
	defer toolServer.Close()

	var started harness.StartTurnRequest
	var continueAttempts atomic.Int32
	continueCh := make(chan struct{})
	runtimeServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case harness.CapabilitiesPath:
			harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
				Version:                 harness.ProtocolVersion,
				ProtocolVersion:         harness.ProtocolVersion,
				Transport:               harness.HTTPTransport,
				RuntimeName:             "fibey-agentkit",
				ProviderKind:            harness.ProviderKindRemote,
				ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved, harness.ToolExecutionModeBrokered},
				BrokeredToolClasses:     []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
				SupportsCancel:          true,
				SupportsRuntimeSessions: true,
				SupportsContinuation:    true,
			})
		case harness.TurnsPath:
			_ = json.NewDecoder(r.Body).Decode(&started)
			eventsPath, _ := harness.EventStreamPath(started.TurnID)
			harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
				Version:          harness.ProtocolVersion,
				Accepted:         true,
				RuntimeSessionID: started.RuntimeSessionID,
				TurnID:           started.TurnID,
				CorrelationID:    started.CorrelationID,
				EventStreamPath:  eventsPath,
			})
		default:
			_, resource, err := harness.ParseTurnResourcePath(r.URL.EscapedPath())
			if err != nil {
				harness.WriteError(w, http.StatusNotFound, "not found")
				return
			}
			switch resource {
			case harness.TurnResourceEvents:
				w.Header().Set("Content-Type", "text/event-stream")
				_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
					Version:          harness.ProtocolVersion,
					Type:             harness.FrameTurnStarted,
					RuntimeSessionID: started.RuntimeSessionID,
					TurnID:           started.TurnID,
					CorrelationID:    started.CorrelationID,
					Seq:              1,
				})
				_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
					Version:          harness.ProtocolVersion,
					Type:             harness.FrameToolCallRequested,
					RuntimeSessionID: started.RuntimeSessionID,
					TurnID:           started.TurnID,
					CorrelationID:    started.CorrelationID,
					Seq:              2,
					ToolName:         "read_incident",
					ToolCallID:       "call-read-1",
					Content:          json.RawMessage(`{"incident":"quincy-north"}`),
				})
				select {
				case <-continueCh:
					_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
						Version:          harness.ProtocolVersion,
						Type:             harness.FrameTurnCompleted,
						RuntimeSessionID: started.RuntimeSessionID,
						TurnID:           started.TurnID,
						CorrelationID:    started.CorrelationID,
						Seq:              3,
						Completed:        &harness.TurnCompleted{Result: "done", FinalEventSeq: 3},
					})
					_ = harness.WriteSSEDone(w)
				case <-r.Context().Done():
				}
			case harness.TurnResourceContinue:
				if continueAttempts.Add(1) == 1 {
					harness.WriteError(w, http.StatusServiceUnavailable, "transient")
					return
				}
				close(continueCh)
				harness.WriteJSON(w, http.StatusAccepted, harness.ContinueTurnResponse{
					Version:          harness.ProtocolVersion,
					Accepted:         true,
					RuntimeSessionID: started.RuntimeSessionID,
					TurnID:           started.TurnID,
					CorrelationID:    started.CorrelationID,
				})
			}
		}
	}))
	defer runtimeServer.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, runtimeServer.URL)
	runtime.Status.ObservedCapabilities.ToolExecutionModes = []corev1alpha1.AgentRuntimeToolExecutionMode{
		corev1alpha1.AgentRuntimeToolExecutionModeObserved,
		corev1alpha1.AgentRuntimeToolExecutionModeBrokered,
	}
	runtime.Status.ObservedCapabilities.BrokeredToolClasses = []corev1alpha1.AgentRuntimeBrokeredToolClass{
		corev1alpha1.AgentRuntimeBrokeredToolClassRead,
	}
	runtime.Status.ObservedCapabilities.SupportsContinuation = true
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "read_incident", Namespace: task.Namespace},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Read incident status",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			HTTP:              &corev1alpha1.HTTPExecution{URL: toolServer.URL},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token, tool)
	running := runHarnessWrapperTaskToRunning(t, r, task)
	if _, err := r.handleRunning(context.Background(), &running); err != nil {
		t.Fatalf("first handleRunning: %v", err)
	}
	var afterFirst corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &afterFirst); err != nil {
		t.Fatalf("get after first running: %v", err)
	}
	if got := harnessWrapperLastFrameSeq(&afterFirst); got != 0 {
		t.Fatalf("last frame seq after failed continue = %d, want 0 for retry", got)
	}
	if _, err := r.handleRunning(context.Background(), &afterFirst); err != nil {
		t.Fatalf("second handleRunning: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get completed: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
	if continueAttempts.Load() != 2 {
		t.Fatalf("continue attempts = %d, want 2", continueAttempts.Load())
	}
	if toolCalls.Load() != 1 {
		t.Fatalf("tool calls = %d, want 1 despite continuation retry", toolCalls.Load())
	}
}

func TestHarnessWrapperRuntimeRefUsesObservedRuntimeName(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "agentkit-fibey-runtime"})
	defer server.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, server.URL())
	runtime.Status.ObservedCapabilities.RuntimeName = "agentkit-fibey-runtime"
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	updated := runHarnessWrapperTaskToCompletion(t, r, task)
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
	if updated.Status.HarnessRuntime == nil || updated.Status.HarnessRuntime.RuntimeName != "agentkit-fibey-runtime" {
		t.Fatalf("HarnessRuntime = %#v, want observed runtime name", updated.Status.HarnessRuntime)
	}
}

func TestHarnessWrapperRuntimeRefFreezesEndpointForRunningTurn(t *testing.T) {
	server := harnesstest.NewFakeHarnessServer(harnesstest.FakeHarnessConfig{RuntimeName: "fibey-agentkit"})
	defer server.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, server.URL())
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)
	running := runHarnessWrapperTaskToRunning(t, r, task)
	var changed corev1alpha1.AgentRuntime
	if err := r.Get(context.Background(), types.NamespacedName{Name: "fibey-agentkit", Namespace: task.Namespace}, &changed); err != nil {
		t.Fatalf("get runtime: %v", err)
	}
	changed.Spec.Deployment.Endpoint = "http://127.0.0.1:1"
	changed.Generation = 2
	changed.Status.Ready = false
	changed.Status.ObservedGeneration = 1
	if err := r.Update(context.Background(), &changed); err != nil {
		t.Fatalf("update runtime spec: %v", err)
	}
	if err := r.Status().Update(context.Background(), &changed); err != nil {
		t.Fatalf("update runtime status: %v", err)
	}

	if _, err := r.handleRunning(context.Background(), &running); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get completed task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded using frozen endpoint (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
}

func TestHarnessWrapperRuntimeRefNotReadyWaitsBeforeStartTurn(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "should not be called", http.StatusInternalServerError)
	}))
	defer server.Close()

	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, server.URL)
	runtime.Status.Ready = false
	runtime.Status.Message = "probe failed"
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %s, want dependency wait", result.RequeueAfter)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("phase = %s, want Pending", updated.Status.Phase)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("server requests = %d, want 0 before StartTurn", got)
	}
}

func TestHarnessWrapperRuntimeRefStaleGenerationWaitsBeforeStartTurn(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, "http://127.0.0.1:1")
	runtime.Generation = 2
	runtime.Status.ObservedGeneration = 1
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %s, want dependency wait", result.RequeueAfter)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("phase = %s, want Pending", updated.Status.Phase)
	}
}

func TestHarnessWrapperBuiltInRuntimeIgnoresRuntimeRefAnnotation(t *testing.T) {
	t.Setenv(harnessWrapperEndpointEnv, "http://wrapper.example.invalid")
	task, agent := harnessWrapperTaskAndAgent()
	task.Annotations = map[string]string{harnessWrapperRuntimeRefAnno: "fibey-agentkit"}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, "http://custom.example.invalid")
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	target, err := r.resolveHarnessRuntimeTarget(context.Background(), task, agent)
	if err != nil {
		t.Fatalf("resolveHarnessRuntimeTarget: %v", err)
	}
	if target.RuntimeRefName != "" {
		t.Fatalf("RuntimeRefName = %q, want built-in wrapper target", target.RuntimeRefName)
	}
	if target.Endpoint != "http://wrapper.example.invalid" {
		t.Fatalf("Endpoint = %q, want shared wrapper endpoint", target.Endpoint)
	}
	if target.RuntimeName != string(corev1alpha1.AgentRuntimeCodex) {
		t.Fatalf("RuntimeName = %q, want codex", target.RuntimeName)
	}
}

func TestHarnessWrapperFrozenRuntimeRefWaitsForAuthRevalidation(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, "http://custom.example.invalid")
	runtime.Status.ObservedAuthRefResourceVersion = "1"
	token.ResourceVersion = "2"
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(harnessWrapperTurnID(task, 1)),
		harnessWrapperRuntimeAnnotation: string(harnessWrapperRuntimeSessionID(task, "fibey-agentkit")),
		harnessWrapperCorrelationIDAnno: string(task.UID),
	}
	task.Status.HarnessRuntime = &corev1alpha1.HarnessRuntimeStatus{
		RuntimeRefName:         "fibey-agentkit",
		RuntimeName:            "fibey-agentkit",
		ContractVersion:        "orka.harness.v1",
		Endpoint:               "http://custom.example.invalid",
		RuntimeGeneration:      1,
		AuthRefName:            "fibey-agentkit-token",
		AuthRefField:           "token",
		AuthRefResourceVersion: "1",
	}
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	_, err := r.resolveHarnessRuntimeTarget(context.Background(), task, agent)
	if !isAgentRuntimeDependencyNotReady(err) {
		t.Fatalf("resolveHarnessRuntimeTarget() error = %v, want dependency-not-ready for frozen target after auth Secret version changed", err)
	}
}

func TestHarnessWrapperRuntimeRefWaitsForAuthRevalidation(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "fibey-agentkit"}}
	runtime, token := harnessWrapperReadyAgentRuntime(task.Namespace, "http://custom.example.invalid")
	runtime.Status.ObservedAuthRefResourceVersion = "1"
	token.ResourceVersion = "2"
	r := newUnitReconciler(newTestScheme(), task, agent, runtime, token)

	_, err := r.resolveHarnessRuntimeTarget(context.Background(), task, agent)
	if !isAgentRuntimeDependencyNotReady(err) {
		t.Fatalf("resolveHarnessRuntimeTarget() error = %v, want dependency-not-ready after auth Secret version changed", err)
	}
}

func TestHarnessWrapperRuntimeRefMissingAgentRuntimeFailsClearly(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "missing-runtime"}}
	r := newUnitReconciler(newTestScheme(), task, agent)

	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if !strings.Contains(updated.Status.Message, `AgentRuntime "missing-runtime" not found`) {
		t.Fatalf("message = %q, want missing AgentRuntime context", updated.Status.Message)
	}
}

func TestHarnessWrapperPlannedTurnMatchesFrozenRuntimeRefAfterAgentChange(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "changed-runtime"}}
	task.Status.HarnessRuntime = &corev1alpha1.HarnessRuntimeStatus{RuntimeRefName: "fibey-agentkit"}
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(harnessWrapperTurnID(task, 1)),
		harnessWrapperRuntimeAnnotation: string(harnessWrapperRuntimeSessionID(task, "fibey-agentkit")),
		harnessWrapperCorrelationIDAnno: string(task.UID),
	}
	if !harnessWrapperPlannedTurnMatchesTask(task, agent, 1) {
		t.Fatal("planned turn should match frozen runtimeRef even after Agent runtimeRef changes")
	}
}

func TestHarnessWrapperCapabilitiesRequireObservedRuntimeRefCapabilities(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != harness.CapabilitiesPath {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
			Version:                 harness.ProtocolVersion,
			ProtocolVersion:         harness.ProtocolVersion,
			Transport:               harness.HTTPTransport,
			RuntimeName:             "fibey-agentkit",
			ProviderKind:            harness.ProviderKindKubernetesService,
			ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
			SupportsCancel:          false,
			SupportsRuntimeSessions: true,
		})
	}))
	defer server.Close()
	client, err := harness.NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	err = (&TaskReconciler{}).validateHarnessWrapperCapabilities(context.Background(), client, harness.StartTurnRequest{
		Metadata: map[string]string{"runtime": "fibey-agentkit", "runtimeRef": "fibey-agentkit"},
	})
	if err == nil || !strings.Contains(err.Error(), "supportsCancel") {
		t.Fatalf("validateHarnessWrapperCapabilities() error = %v, want supportsCancel requirement", err)
	}
}

func TestHarnessWrapperStartTurnUsesComputedAttemptForTurnID(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Status.Attempts = 1
	r := newUnitReconciler(newTestScheme(), task, agent)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 2)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	if !strings.HasPrefix(string(request.TurnID), "harness-task-") || !strings.HasSuffix(string(request.TurnID), "-2") {
		t.Fatalf("TurnID = %q, want namespaced/UID-scoped attempt 2 turn ID", request.TurnID)
	}
}

func TestHarnessWrapperPlannedBuiltInRetryUsesFrozenRuntimeMetadata(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	turnID := harnessWrapperTurnID(task, 1)
	runtimeID := harnessWrapperRuntimeSessionID(task, string(corev1alpha1.AgentRuntimeCodex))
	var startCalls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harness.CapabilitiesPath:
			harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
				Version:                 harness.ProtocolVersion,
				ProtocolVersion:         harness.ProtocolVersion,
				Transport:               harness.HTTPTransport,
				RuntimeName:             "codex",
				ProviderKind:            harness.ProviderKindKubernetesService,
				ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
				SupportsCancel:          true,
				SupportsRuntimeSessions: true,
			})
		case r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath:
			startCalls++
			var request harness.StartTurnRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if got := request.Metadata["runtime"]; got != "codex" {
				harness.WriteError(w, http.StatusBadRequest, fmt.Sprintf("runtime metadata = %q, want codex", got))
				return
			}
			if request.RuntimeSessionID != runtimeID {
				harness.WriteError(w, http.StatusBadRequest, fmt.Sprintf("runtimeSessionID = %q", request.RuntimeSessionID))
				return
			}
			if got := request.Metadata["systemPrompt"]; got != "private instructions" {
				harness.WriteError(w, http.StatusBadRequest, fmt.Sprintf("systemPrompt metadata = %q, want private instructions", got))
				return
			}
			streamPath, _ := harness.EventStreamPath(request.TurnID)
			harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
				Version:          harness.ProtocolVersion,
				Accepted:         true,
				RuntimeSessionID: request.RuntimeSessionID,
				TurnID:           request.TurnID,
				CorrelationID:    request.CorrelationID,
				EventStreamPath:  streamPath,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(turnID),
		harnessWrapperRuntimeAnnotation: string(runtimeID),
		harnessWrapperCorrelationIDAnno: string(task.UID),
		harnessWrapperLastFrameSeqAnno:  "0",
		harnessWrapperStartedAnno:       testFalseValue,
		harnessWrapperMetadataAnno:      `{"runtime":"codex","wrapper":"cli","contractVersion":"orka.harness.v1"}`,
	}
	agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeClaude
	agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "private instructions"}
	task.Annotations[harnessWrapperMetadataAnno] = `{"runtime":"codex","wrapper":"cli","contractVersion":"orka.harness.v1","maxTurns":"50"}`
	r := newUnitReconciler(newTestScheme(), task, agent)

	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %s, want positive delay", result.RequeueAfter)
	}
	if startCalls != 1 {
		t.Fatalf("StartTurn calls = %d, want 1", startCalls)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase = %s, want Running (message=%s)", updated.Status.Phase, updated.Status.Message)
	}
}

func TestHarnessWrapperPendingFirstOnlyPlansTurn(t *testing.T) {
	startCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harness.CapabilitiesPath:
			harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
				Version:            harness.ProtocolVersion,
				ProtocolVersion:    harness.ProtocolVersion,
				Transport:          harness.HTTPTransport,
				RuntimeName:        "codex",
				ProviderKind:       harness.ProviderKindKubernetesService,
				ToolExecutionModes: []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
			})
		case r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath:
			startCalls++
			var request harness.StartTurnRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			harness.WriteJSON(w, http.StatusAccepted, harness.StartTurnResponse{
				Version:          harness.ProtocolVersion,
				Accepted:         true,
				RuntimeSessionID: request.RuntimeSessionID,
				TurnID:           request.TurnID,
				CorrelationID:    request.CorrelationID,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)

	result, err := r.handlePending(context.Background(), task)
	if err != nil {
		t.Fatalf("first handlePending: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("first handlePending requeue = %s, want positive delay", result.RequeueAfter)
	}
	if startCalls != 0 {
		t.Fatalf("StartTurn calls after planning = %d, want 0", startCalls)
	}
	var planned corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &planned); err != nil {
		t.Fatalf("get planned task: %v", err)
	}
	if planned.Status.Phase != corev1alpha1.TaskPhasePending {
		t.Fatalf("phase after planning = %s, want Pending", planned.Status.Phase)
	}
	if !taskHasPlannedHarnessWrapperTurn(&planned) {
		t.Fatalf("planned harness annotations missing: %#v", planned.Annotations)
	}
	if taskHasHarnessWrapperTurn(&planned) {
		t.Fatalf("harness turn marked started during planning: %#v", planned.Annotations)
	}
	if got := planned.Annotations[harnessWrapperStartedAnno]; got != testFalseValue {
		t.Fatalf("%s = %q, want false", harnessWrapperStartedAnno, got)
	}

	if _, err := r.handlePending(context.Background(), &planned); err != nil {
		t.Fatalf("second handlePending: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("StartTurn calls after start = %d, want 1", startCalls)
	}
	var running corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &running); err != nil {
		t.Fatalf("get running task: %v", err)
	}
	if running.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase after start = %s, want Running", running.Status.Phase)
	}
	if !taskHasHarnessWrapperTurn(&running) {
		t.Fatalf("started harness annotations missing: %#v", running.Annotations)
	}
}

func TestHarnessWrapperStartTurnAttemptIsPersistedBeforeSubmission(t *testing.T) {
	startCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harness.CapabilitiesPath:
			harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
				Version:                 harness.ProtocolVersion,
				ProtocolVersion:         harness.ProtocolVersion,
				Transport:               harness.HTTPTransport,
				RuntimeName:             "codex",
				ProviderKind:            harness.ProviderKindKubernetesService,
				ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
				SupportsCancel:          true,
				SupportsRuntimeSessions: true,
			})
		case r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath:
			startCalls++
			harness.WriteError(w, http.StatusInternalServerError, "submission outcome unknown")
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("planning handlePending: %v", err)
	}
	var planned corev1alpha1.Task
	key := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
	if err := r.Get(context.Background(), key, &planned); err != nil {
		t.Fatalf("get planned task: %v", err)
	}

	result, err := r.handlePending(context.Background(), &planned)
	if err != nil {
		t.Fatalf("submission handlePending: %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("submission requeue = %s, want positive delay", result.RequeueAfter)
	}
	if startCalls != 1 {
		t.Fatalf("StartTurn calls after uncertain submission = %d, want 1", startCalls)
	}
	var attempted corev1alpha1.Task
	if err := r.Get(context.Background(), key, &attempted); err != nil {
		t.Fatalf("get attempted task: %v", err)
	}
	if !taskHasHarnessWrapperSubmissionAttempt(&attempted) || taskHasHarnessWrapperTurn(&attempted) {
		t.Fatalf("submission annotations = %#v, want attempted but not started", attempted.Annotations)
	}

	if _, err := r.handlePending(context.Background(), &attempted); err != nil {
		t.Fatalf("recovery handlePending: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("StartTurn calls after recovery = %d, want no duplicate", startCalls)
	}
	var running corev1alpha1.Task
	if err := r.Get(context.Background(), key, &running); err != nil {
		t.Fatalf("get running task: %v", err)
	}
	if running.Status.Phase != corev1alpha1.TaskPhaseRunning || !taskHasHarnessWrapperTurn(&running) {
		t.Fatalf("recovered task phase=%s annotations=%#v, want Running started turn", running.Status.Phase, running.Annotations)
	}
	if !taskHasHarnessWrapperSubmissionAttempt(&running) {
		t.Fatal("unknown submission marker was not preserved through Running recovery")
	}
	if _, err := r.handleRunning(context.Background(), &running); err != nil {
		t.Fatalf("handleRunning after lost submission: %v", err)
	}
	if startCalls != 1 {
		t.Fatalf("StartTurn calls after missing-turn failure = %d, want no duplicate", startCalls)
	}
	var failed corev1alpha1.Task
	if err := r.Get(context.Background(), key, &failed); err != nil {
		t.Fatalf("get failed task: %v", err)
	}
	if failed.Status.Phase != corev1alpha1.TaskPhaseFailed ||
		!strings.Contains(failed.Status.Message, "submission outcome became unknown") {
		t.Fatalf("failed task phase=%s message=%q", failed.Status.Phase, failed.Status.Message)
	}
}

func TestHarnessWrapperAuthRetryClearsSubmissionAttemptAtomically(t *testing.T) {
	task, _ := harnessWrapperTaskAndAgent()
	task.Annotations = map[string]string{
		harnessWrapperSubmissionAttemptedAnno: scheduledRunLabelValue,
	}
	r := newUnitReconciler(newTestScheme(), task)
	wait, err := r.waitForHarnessWrapperAuthRetry(context.Background(), task, true)
	if err != nil {
		t.Fatalf("waitForHarnessWrapperAuthRetry: %v", err)
	}
	if !wait {
		t.Fatal("auth retry did not request a wait")
	}
	var updated corev1alpha1.Task
	key := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
	if err := r.Get(context.Background(), key, &updated); err != nil {
		t.Fatalf("get updated task: %v", err)
	}
	if got := harnessWrapperAuthRetries(&updated); got != 1 {
		t.Fatalf("auth retries = %d, want 1", got)
	}
	if taskHasHarnessWrapperSubmissionAttempt(&updated) {
		t.Fatal("submission-attempt marker remained after definite auth rejection")
	}
}

func TestHarnessRuntimeRunningTaskFinishesAfterStart(t *testing.T) {
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess, RuntimeName: "codex"})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	running := runHarnessWrapperTaskToRunning(t, r, task)
	if _, err := r.handleRunning(context.Background(), &running); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get completed task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", updated.Status.Phase)
	}
}

func TestHarnessWrapperStartSkipsStartTurnWhenJournalHasPersistedFrames(t *testing.T) {
	startCalls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == harness.CapabilitiesPath:
			harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
				Version:                 harness.ProtocolVersion,
				ProtocolVersion:         harness.ProtocolVersion,
				Transport:               harness.HTTPTransport,
				RuntimeName:             string(corev1alpha1.AgentRuntimeCodex),
				ProviderKind:            harness.ProviderKindKubernetesService,
				ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeObserved},
				SupportsCancel:          true,
				SupportsRuntimeSessions: true,
			})
		case r.Method == http.MethodPost && r.URL.Path == harness.TurnsPath:
			startCalls++
			harness.WriteError(w, http.StatusInternalServerError, "StartTurn should not be called")
		default:
			harness.WriteError(w, http.StatusNotFound, "not found")
		}
	}))
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	task, agent := harnessWrapperTaskAndAgent()
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(harnessWrapperTurnID(task, 1)),
		harnessWrapperRuntimeAnnotation: string(harnessWrapperRuntimeSessionID(task, string(agent.Spec.Runtime.Type))),
		harnessWrapperCorrelationIDAnno: string(task.UID),
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	if err := appendMappedHarnessFrame(context.Background(), r.ExecutionEventStore, task, harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameTurnStarted,
		RuntimeSessionID: harness.RuntimeSessionID(task.Annotations[harnessWrapperRuntimeAnnotation]),
		TurnID:           harness.HarnessTurnID(task.Annotations[harnessWrapperTurnIDAnnotation]),
		CorrelationID:    task.Annotations[harnessWrapperCorrelationIDAnno],
		Seq:              1,
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append persisted frame: %v", err)
	}

	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	if startCalls != 0 {
		t.Fatalf("StartTurn calls = %d, want 0", startCalls)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase = %s, want Running", updated.Status.Phase)
	}
	if !taskHasHarnessWrapperTurn(&updated) {
		t.Fatalf("started harness annotations missing: %#v", updated.Annotations)
	}
}

func TestFinishHarnessWrapperTaskUsesJournalDedupeOnReplay(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	turnID := harnessWrapperTurnID(task, 1)
	runtimeID := harnessWrapperRuntimeSessionID(task, string(agent.Spec.Runtime.Type))
	correlationID := string(task.UID)
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	task.Status.Attempts = 1
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(turnID),
		harnessWrapperRuntimeAnnotation: string(runtimeID),
		harnessWrapperCorrelationIDAnno: correlationID,
		harnessWrapperStartedAnno:       scheduledRunLabelValue,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eventsPath, _ := harness.EventStreamPath(turnID)
		if r.Method != http.MethodGet || r.URL.Path != eventsPath {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
			Version:          harness.ProtocolVersion,
			Type:             harness.FrameTurnStarted,
			RuntimeSessionID: runtimeID,
			TurnID:           turnID,
			CorrelationID:    correlationID,
			Seq:              1,
			CreatedAt:        time.Now().UTC(),
		})
		_ = harness.WriteSSEFrame(w, harness.HarnessEventFrame{
			Version:          harness.ProtocolVersion,
			Type:             harness.FrameTurnCompleted,
			RuntimeSessionID: runtimeID,
			TurnID:           turnID,
			CorrelationID:    correlationID,
			Seq:              2,
			CreatedAt:        time.Now().UTC(),
			Completed:        &harness.TurnCompleted{Result: "ok"},
		})
		_ = harness.WriteSSEDone(w)
	}))
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)

	r := newUnitReconciler(newTestScheme(), task, agent)
	if err := appendMappedHarnessFrame(context.Background(), r.ExecutionEventStore, task, harness.HarnessEventFrame{
		Version:          harness.ProtocolVersion,
		Type:             harness.FrameTurnStarted,
		RuntimeSessionID: runtimeID,
		TurnID:           turnID,
		CorrelationID:    correlationID,
		Seq:              1,
		CreatedAt:        time.Now().UTC(),
	}); err != nil {
		t.Fatalf("append preexisting frame: %v", err)
	}

	if _, err := r.handleRunning(context.Background(), task); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get completed task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
		t.Fatalf("phase = %s, want Succeeded", updated.Status.Phase)
	}

	listed, err := r.ExecutionEventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace:  task.Namespace,
		StreamType: store.ExecutionEventStreamTypeTask,
		StreamID:   task.Name,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	counts := map[string]int{}
	for _, event := range listed {
		identity, ok := harness.MappedFrameIdentityFromEvent(event)
		if ok {
			counts[identity.Key()]++
		}
	}
	seq1Key := strings.Join([]string{string(runtimeID), string(turnID), correlationID, "1"}, "\x00")
	seq2Key := strings.Join([]string{string(runtimeID), string(turnID), correlationID, "2"}, "\x00")
	if counts[seq1Key] != 1 {
		t.Fatalf("seq1 mapped frame count = %d, want 1 (counts=%#v)", counts[seq1Key], counts)
	}
	if counts[seq2Key] != 1 {
		t.Fatalf("seq2 mapped frame count = %d, want 1 (counts=%#v)", counts[seq2Key], counts)
	}
}

func appendMappedHarnessFrame(ctx context.Context, eventStore store.ExecutionEventStore, task *corev1alpha1.Task, frame harness.HarnessEventFrame) error {
	mapped, err := harness.MapFrameToExecutionEvent(frame, harness.EventMapContext{
		Namespace: task.Namespace,
		TaskName:  task.Name,
	})
	if err != nil {
		return err
	}
	_, err = eventStore.AppendExecutionEvent(ctx, mapped)
	return err
}

func TestHarnessRuntimeMissingEndpointFailsAgentTask(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	secret := attachHarnessWrapperRuntimeSecret(task, agent)
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("handlePending: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get task: %v", err)
	}
	if updated.Status.Phase != corev1alpha1.TaskPhaseFailed {
		t.Fatalf("phase = %s, want Failed", updated.Status.Phase)
	}
	if updated.Status.JobName != "" {
		t.Fatalf("JobName = %q, want no job", updated.Status.JobName)
	}
}

func runHarnessWrapperTaskToCompletion(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task) corev1alpha1.Task {
	t.Helper()
	running := runHarnessWrapperTaskToRunning(t, r, task)
	if _, err := r.handleRunning(context.Background(), &running); err != nil {
		t.Fatalf("handleRunning: %v", err)
	}
	var updated corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &updated); err != nil {
		t.Fatalf("get completed task: %v", err)
	}
	return updated
}

func runHarnessWrapperTaskToRunning(t *testing.T, r *TaskReconciler, task *corev1alpha1.Task) corev1alpha1.Task {
	t.Helper()
	if _, err := r.handlePending(context.Background(), task); err != nil {
		t.Fatalf("first handlePending: %v", err)
	}
	var current corev1alpha1.Task
	if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &current); err != nil {
		t.Fatalf("get task after first pending: %v", err)
	}
	if current.Status.Phase == corev1alpha1.TaskPhasePending && taskHasPlannedHarnessWrapperTurn(&current) && !taskHasHarnessWrapperTurn(&current) {
		if _, err := r.handlePending(context.Background(), &current); err != nil {
			t.Fatalf("second handlePending: %v", err)
		}
		if err := r.Get(context.Background(), types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, &current); err != nil {
			t.Fatalf("get task after second pending: %v", err)
		}
	}
	if current.Status.Phase != corev1alpha1.TaskPhaseRunning {
		t.Fatalf("phase after pending = %s, want Running", current.Status.Phase)
	}
	return current
}

func harnessWrapperTaskAndAgent() (*corev1alpha1.Task, *corev1alpha1.Agent) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-task", Namespace: "default", UID: types.UID("uid-harness-task")},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &corev1alpha1.AgentReference{Name: "harness-agent"},
			Prompt:   "hello harness",
		},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhasePending},
	}
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-agent", Namespace: "default"},
		Spec:       corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}},
	}
	return task, agent
}

func attachHarnessWrapperRuntimeSecret(task *corev1alpha1.Task, agent *corev1alpha1.Agent) *corev1.Secret {
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "harness-runtime-secret"}
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "harness-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.OpenAIAPIKey: []byte("test-runtime-key"),
		},
	}
}

func harnessWrapperReadyAgentRuntime(namespace, endpoint string) (*corev1alpha1.AgentRuntime, *corev1.Secret) {
	const name = "fibey-agentkit"
	runtime := &corev1alpha1.AgentRuntime{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace, Generation: 1},
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV1,
			Deployment: corev1alpha1.AgentRuntimeDeploymentSpec{
				Mode:     corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint,
				Endpoint: endpoint,
			},
			ClientAuth: corev1alpha1.AgentRuntimeClientAuth{BearerAuthRef: corev1alpha1.AgentRuntimeBearerAuthReference{
				Name: name + "-token",
				Key:  "token",
			}},
		},
		Status: corev1alpha1.AgentRuntimeStatus{
			Ready:                          true,
			ObservedGeneration:             1,
			ObservedAuthRefResourceVersion: "1",
			ObservedCapabilities: &corev1alpha1.AgentRuntimeObservedCapabilities{
				ProtocolVersion:         "orka.harness.v1",
				RuntimeName:             name,
				ToolExecutionModes:      []corev1alpha1.AgentRuntimeToolExecutionMode{corev1alpha1.AgentRuntimeToolExecutionModeObserved},
				SupportsCancel:          true,
				MaxConcurrentTurns:      1,
				SupportsRuntimeSessions: true,
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-token", Namespace: namespace, ResourceVersion: "1", Labels: map[string]string{agentRuntimeAuthUseLabel: scheduledRunLabelValue, agentRuntimeAuthRefNameLabel: name}, Annotations: map[string]string{agentRuntimeAuthEndpointAnnotation: endpoint}},
		Data:       map[string][]byte{"token": []byte("x")},
	}
	return runtime, secret
}

func hasExecutionEventType(eventsList []store.ExecutionEvent, typ string) bool {
	for _, event := range eventsList {
		if event.Type == typ {
			return true
		}
	}
	return false
}

func TestHarnessWrapperCapabilitiesUseInputToolsForRequiredBrokeredClasses(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != harness.CapabilitiesPath {
			harness.WriteError(w, http.StatusNotFound, "not found")
			return
		}
		harness.WriteJSON(w, http.StatusOK, harness.CapabilitiesResponse{
			Version:                 harness.ProtocolVersion,
			ProtocolVersion:         harness.ProtocolVersion,
			Transport:               harness.HTTPTransport,
			RuntimeName:             "brokered-runtime",
			ProviderKind:            harness.ProviderKindRemote,
			ToolExecutionModes:      []harness.ToolExecutionMode{harness.ToolExecutionModeBrokered},
			BrokeredToolClasses:     []harness.BrokeredToolClass{harness.BrokeredToolClassRead},
			SupportsRuntimeSessions: true,
			SupportsContinuation:    true,
		})
	}))
	defer server.Close()
	client, err := harness.NewClient(server.URL)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	request := harness.StartTurnRequest{
		Metadata:          map[string]string{"runtime": "brokered-runtime", "runtimeRef": "runtime", "brokeredToolClasses": "read"},
		ToolExecutionMode: harness.ToolExecutionModeBrokered,
		Input: harness.TurnInput{Tools: []harness.ToolDefinition{{
			Name:          "dispatch_work_order",
			BrokeredClass: harness.BrokeredToolClassWrite,
		}}},
	}
	err = (&TaskReconciler{}).validateHarnessWrapperCapabilities(context.Background(), client, request)
	if err == nil || !strings.Contains(err.Error(), "brokeredToolClass") || !strings.Contains(err.Error(), "write") {
		t.Fatalf("validateHarnessWrapperCapabilities() = %v, want write class rejection from input.tools", err)
	}
}

func TestHarnessWrapperBuiltInToolFiltersStayMetadataOnly(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{
		AllowedTools:    []string{"Read", "Grep"},
		DisallowedTools: []string{"Bash", "Write"},
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	if request.ToolExecutionMode != harness.ToolExecutionModeObserved {
		t.Fatalf("ToolExecutionMode = %q, want observed", request.ToolExecutionMode)
	}
	if len(request.Input.Tools) != 0 {
		t.Fatalf("Input.Tools = %#v, want no brokered tools for built-in CLI runtime", request.Input.Tools)
	}
	if request.Metadata["allowedTools"] != "Read,Grep" {
		t.Fatalf("metadata allowedTools = %q, want Read,Grep", request.Metadata["allowedTools"])
	}
	if request.Metadata["disallowedTools"] != "Bash,Write" {
		t.Fatalf("metadata disallowedTools = %q, want Bash,Write", request.Metadata["disallowedTools"])
	}
}

func TestHarnessWrapperTurnRequestCarriesSafeBrokeredToolSchemas(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.Runtime = &corev1alpha1.AgentCLIRuntime{RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "runtime"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{"read_incident", "delegate_task"}}
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: "read_incident", Namespace: task.Namespace},
		Spec: corev1alpha1.ToolSpec{
			Description:       "Read incident status",
			BrokeredToolClass: corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			Parameters:        &apiextensionsv1.JSON{Raw: []byte(`{"type":"object","properties":{"incident":{"type":"string"}}}`)},
			HTTP:              &corev1alpha1.HTTPExecution{URL: "https://tools.example.invalid/read", AuthSecretRef: &corev1alpha1.SecretKeySelector{Name: "tool-secret", Key: "token"}},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, tool)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	if request.ToolExecutionMode != harness.ToolExecutionModeBrokered {
		t.Fatalf("ToolExecutionMode = %q, want brokered", request.ToolExecutionMode)
	}
	if len(request.Input.Tools) != 2 {
		t.Fatalf("Input.Tools = %#v, want read tool and coordination tool", request.Input.Tools)
	}
	byName := map[string]harness.ToolDefinition{}
	for _, definition := range request.Input.Tools {
		byName[definition.Name] = definition
		encoded, _ := json.Marshal(definition)
		if strings.Contains(string(encoded), "tools.example") || strings.Contains(string(encoded), "tool-secret") {
			t.Fatalf("tool definition leaked execution details: %s", encoded)
		}
	}
	read := byName["read_incident"]
	if read.Description != "Read incident status" || read.BrokeredClass != harness.BrokeredToolClassRead || !json.Valid(read.Parameters) {
		t.Fatalf("read tool definition = %#v", read)
	}
	coord := byName["delegate_task"]
	if coord.BrokeredClass != harness.BrokeredToolClassCoordination || coord.Description == "" {
		t.Fatalf("coordination tool definition = %#v", coord)
	}
}

func TestHarnessWrapperCompletedResultBytesPreservesStructuredPayload(t *testing.T) {
	encoded := harnessWrapperCompletedResultBytes(&harness.TurnCompleted{
		Result: "done",
		Data:   map[string]any{"incident": "quincy-north", "score": float64(0.9)},
		Artifacts: []harness.ArtifactRef{{
			Filename:    "evidence.json",
			ContentType: "application/json",
			Size:        42,
		}},
	})
	parsed := common.ParseStructuredResult(string(encoded))
	if parsed.Summary != "done" || parsed.Data["incident"] != "quincy-north" {
		t.Fatalf("parsed structured result = %#v", parsed)
	}
	if len(parsed.Artifacts) != 1 || parsed.Artifacts[0].Filename != "evidence.json" {
		t.Fatalf("artifacts = %#v", parsed.Artifacts)
	}
}

func TestHarnessWrapperTurnRequestCarriesAgentRuntimeSecretEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "agent-runtime-secret"}
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "task-runtime-secret"}
	agentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-runtime-secret", Namespace: agent.Namespace},
		Data: map[string][]byte{
			workerenv.OpenAIAPIKey: []byte("runtime-openai-key"),
			workerenv.GitHubToken:  []byte("runtime-github-token"),
		},
	}
	taskSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "task-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.AnthropicAPIKey: []byte("runtime-anthropic-key"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, agentSecret, taskSecret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.OpenAIAPIKey] != "runtime-openai-key" {
		t.Fatalf("%s = %q, want runtime credential", workerenv.OpenAIAPIKey, env[workerenv.OpenAIAPIKey])
	}
	if env[workerenv.GitHubToken] != "runtime-github-token" {
		t.Fatalf("%s = %q, want runtime credential", workerenv.GitHubToken, env[workerenv.GitHubToken])
	}
	if env[workerenv.AnthropicAPIKey] != "runtime-anthropic-key" {
		t.Fatalf("%s = %q, want task runtime credential", workerenv.AnthropicAPIKey, env[workerenv.AnthropicAPIKey])
	}
}

func TestPlannedHarnessWrapperStartTurnRequestRebuildsFullInput(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.Env = []corev1.EnvVar{{Name: workerenv.CodexCLIPath, Value: "/bin/codex-test"}}
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "agent-runtime-secret"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.OpenAIAPIKey: []byte("runtime-openai-key"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	planned, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(planned.TurnID),
		harnessWrapperRuntimeAnnotation: string(planned.RuntimeSessionID),
		harnessWrapperCorrelationIDAnno: planned.CorrelationID,
		harnessWrapperMetadataAnno:      `{"runtime":"codex","wrapper":"cli"}`,
		harnessWrapperLastFrameSeqAnno:  "0",
		harnessWrapperStartedAnno:       scheduledRunLabelValue,
	}

	replayed, err := r.plannedHarnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now())
	if err != nil {
		t.Fatalf("plannedHarnessWrapperStartTurnRequest: %v", err)
	}
	if replayed.TurnID != planned.TurnID || replayed.RuntimeSessionID != planned.RuntimeSessionID || replayed.CorrelationID != planned.CorrelationID {
		t.Fatalf("replayed identity = (%q,%q,%q), want planned (%q,%q,%q)", replayed.TurnID, replayed.RuntimeSessionID, replayed.CorrelationID, planned.TurnID, planned.RuntimeSessionID, planned.CorrelationID)
	}
	env := map[string]string{}
	for _, item := range replayed.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.CodexCLIPath] != "/bin/codex-test" {
		t.Fatalf("%s = %q, want task env", workerenv.CodexCLIPath, env[workerenv.CodexCLIPath])
	}
	if env[workerenv.OpenAIAPIKey] != "runtime-openai-key" {
		t.Fatalf("%s = %q, want runtime secret env", workerenv.OpenAIAPIKey, env[workerenv.OpenAIAPIKey])
	}
}

func TestHarnessWrapperTurnRequestUsesTaskNamespaceForCrossNamespaceAgentSecret(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Namespace = "shared-agents"
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "runtime-secret"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime-secret", Namespace: task.Namespace},
		Data:       map[string][]byte{workerenv.OpenAIAPIKey: []byte("task-local-key")},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.OpenAIAPIKey] != "task-local-key" {
		t.Fatalf("%s = %q, want task-local secret", workerenv.OpenAIAPIKey, env[workerenv.OpenAIAPIKey])
	}
}

func TestHarnessWrapperTurnRequestAllowsRuntimeSecretProviderBaseURL(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "runtime-secret"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.OpenAIAPIKey:  []byte("runtime-key"),
			workerenv.OpenAIBaseURL: []byte("https://proxy.example.invalid/v1"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.OpenAIBaseURL] != "https://proxy.example.invalid/v1" {
		t.Fatalf("%s = %q, want proxy base URL", workerenv.OpenAIBaseURL, env[workerenv.OpenAIBaseURL])
	}
}

func TestHarnessWrapperSecretEnvSkipsFileStyleKeys(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "runtime-secret"}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.OpenAIAPIKey: []byte("runtime-key"),
			".npmrc":               []byte("registry=https://example.invalid"),
			"config.json":          []byte("{}"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, secret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.OpenAIAPIKey] != "runtime-key" {
		t.Fatalf("%s = %q, want runtime key", workerenv.OpenAIAPIKey, env[workerenv.OpenAIAPIKey])
	}
	if _, ok := env[".npmrc"]; ok {
		t.Fatal("file-style key .npmrc was projected as env")
	}
	if _, ok := env["config.json"]; ok {
		t.Fatal("file-style key config.json was projected as env")
	}
}

func TestHarnessWrapperTurnRequestRejectsWrapperPrivateSecretEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "task-runtime-secret"}
	taskSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "task-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			"ORKA_HARNESS_WRAPPER_CHILD_UID": []byte("0"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, taskSecret)
	_, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err == nil || !strings.Contains(err.Error(), "reserved for wrapper configuration") {
		t.Fatalf("harnessWrapperStartTurnRequest() error = %v, want wrapper-private Secret env rejection", err)
	}
}

func TestHarnessWrapperTurnRequestRejectsControllerUploadSecretEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "task-runtime-secret"}
	taskSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "task-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.ControllerURL: []byte("https://attacker.example.invalid"),
			"HTTPS_PROXY":           []byte("https://proxy.example.invalid"),
			"ORKA_ARTIFACTS_DIR":    []byte("/tmp/evil-artifacts"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, taskSecret)
	_, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err == nil || !strings.Contains(err.Error(), "reserved for controller-managed runtime configuration") {
		t.Fatalf("harnessWrapperStartTurnRequest() error = %v, want controller upload env rejection", err)
	}
}

func TestHarnessWrapperTurnRequestPrependsSkillsToSystemPrompt(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "Base instructions"}
	agent.Spec.Skills = []corev1alpha1.SkillReference{{Name: "agent-skill"}}
	task.Spec.AI = &corev1alpha1.AISpec{Skills: []corev1alpha1.SkillReference{{
		ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "task-skills", Key: "review"},
	}}}
	skill := &corev1alpha1.Skill{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-skill", Namespace: task.Namespace},
		Spec: corev1alpha1.SkillSpec{Content: corev1alpha1.SkillContent{
			Inline: "Use the agent skill.",
		}},
	}
	skillCM := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "task-skills", Namespace: task.Namespace},
		Data:       map[string]string{"review": "Use the task skill."},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, skill, skillCM)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	want := "Use the agent skill.\n\nUse the task skill.\n\nBase instructions"
	if request.Metadata["systemPrompt"] != want {
		t.Fatalf("systemPrompt = %q, want %q", request.Metadata["systemPrompt"], want)
	}
}

func TestHarnessWrapperTurnRequestFiltersReadOnlyRuntimeSecretEnv(t *testing.T) {
	const readOnlyWorkspaceGitCredential = "test"
	task, agent := harnessWrapperTaskAndAgent()
	task.Annotations = map[string]string{labels.AnnotationAgentReadOnly: scheduledRunLabelValue}
	task.Spec.Env = []corev1.EnvVar{
		{Name: workerenv.AgentReadOnly, Value: testFalseValue},
		{Name: workerenv.ResultStdout, Value: testFalseValue},
		{Name: workerenv.AllowBash, Value: scheduledRunLabelValue},
		{Name: workerenv.AllowedTools, Value: "Bash,Write"},
	}
	agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeClaude
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "agent-runtime-secret"}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{Workspace: &corev1alpha1.WorkspaceConfig{
		GitRepo:      "https://github.com/orka-agents/orka",
		GitSecretRef: &corev1.LocalObjectReference{Name: "git-credentials"},
	}}
	agentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-runtime-secret", Namespace: agent.Namespace},
		Data: map[string][]byte{
			workerenv.AnthropicAPIKey: []byte("runtime-anthropic-key"),
			workerenv.GitHubToken:     []byte("runtime-github-token"),
		},
	}
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "task-runtime-secret"}
	taskSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "task-runtime-secret", Namespace: task.Namespace},
		Data:       map[string][]byte{workerenv.OpenAIAPIKey: []byte("task-openai-key")},
	}
	gitSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-credentials", Namespace: task.Namespace},
		Data:       map[string][]byte{"token": []byte(readOnlyWorkspaceGitCredential)},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, agentSecret, taskSecret, gitSecret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.AnthropicAPIKey] != "runtime-anthropic-key" {
		t.Fatalf("%s = %q, want runtime credential", workerenv.AnthropicAPIKey, env[workerenv.AnthropicAPIKey])
	}
	if env[workerenv.GitHubToken] == "runtime-github-token" {
		t.Fatalf("read-only Claude runtime should not receive unrelated %s", workerenv.GitHubToken)
	}
	if env[workerenv.GitToken] != readOnlyWorkspaceGitCredential {
		t.Fatalf("%s = %q, want workspace git credential for read-only prep", workerenv.GitToken, env[workerenv.GitToken])
	}
	if env[workerenv.OpenAIAPIKey] == "task-openai-key" {
		t.Fatalf("task secret credentials should not be sent to read-only harness turns")
	}
	if env[workerenv.AllowBash] != "" || env[workerenv.AllowedTools] == "Bash,Write" {
		t.Fatalf("read-only task env should not override runtime permissions: %#v", env)
	}
	if env[workerenv.AgentReadOnly] != scheduledRunLabelValue || env[workerenv.ResultStdout] != scheduledRunLabelValue {
		t.Fatalf("read-only control env not forced: %#v", env)
	}
	if env[workerenv.GitToken] != readOnlyWorkspaceGitCredential {
		t.Fatalf("workspace git credentials not preserved for read-only prep: %#v", env)
	}
}

func TestHarnessWrapperTurnRequestRejectsCrossNamespaceTaskSecret(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.SecretRef = &corev1alpha1.SecretReference{Name: "task-runtime-secret", Namespace: "other"}
	r := newUnitReconciler(newTestScheme(), task, agent)
	_, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err == nil || !strings.Contains(err.Error(), "does not match task namespace") {
		t.Fatalf("harnessWrapperStartTurnRequest() error = %v, want namespace rejection", err)
	}
}

func TestHarnessWrapperPlannedTurnMustMatchTaskIdentity(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(harnessWrapperTurnID(task, 1)),
		harnessWrapperRuntimeAnnotation: string(harnessWrapperRuntimeSessionID(task, string(agent.Spec.Runtime.Type))),
		harnessWrapperCorrelationIDAnno: string(task.UID),
	}
	if !harnessWrapperPlannedTurnMatchesTask(task, agent, 1) {
		t.Fatal("expected planned turn to match task identity")
	}
	task.Annotations[harnessWrapperTurnIDAnnotation] = "other-turn"
	if harnessWrapperPlannedTurnMatchesTask(task, agent, 1) {
		t.Fatal("expected copied turn id to be rejected")
	}
}

// The persisted execution-event SessionName for a harness task must be EMPTY
// when the task has no real SessionRef, so a SessionRef-less task named "foo"
// cannot collide its events into a real Session "foo". The protocol-level
// harnessWrapperSessionName still falls back to the task name (a non-empty
// identifier is required on the wire), but that value must NOT be the one
// persisted as the event session key.
func TestHarnessEventSessionNameEmptyWithoutRealSessionRef(t *testing.T) {
	r := &TaskReconciler{}
	task, _ := harnessWrapperTaskAndAgent()
	task.Spec.SessionRef = nil

	if got := r.executionEventSessionName(context.Background(), task); got != "" {
		t.Fatalf("executionEventSessionName for SessionRef-less task = %q, want empty (no collision into a real Session)", got)
	}
	// The protocol identifier helper still returns a non-empty value (the task name).
	if got := harnessWrapperSessionName(task); got != task.Name {
		t.Fatalf("harnessWrapperSessionName = %q, want task name %q for the protocol request", got, task.Name)
	}
}

func TestHarnessWrapperTurnMetadataCarriesTaskTimeout(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.Timeout = &metav1.Duration{Duration: 45 * time.Minute}
	r := newUnitReconciler(newTestScheme(), task, agent)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	if request.Metadata["timeoutSeconds"] != "2700" {
		t.Fatalf("metadata timeoutSeconds = %q, want 2700", request.Metadata["timeoutSeconds"])
	}
}

func TestHarnessWrapperTurnRequestResolvesTaskValueFromEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.Env = []corev1.EnvVar{
		{
			Name: "CONFIG_VALUE",
			ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "task-config"},
				Key:                  "setting",
			}},
		},
		{
			Name:      "TASK_NAME",
			ValueFrom: &corev1.EnvVarSource{FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.name"}},
		},
	}
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "task-config", Namespace: task.Namespace},
		Data:       map[string]string{"setting": "from-config"},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, cm)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env["CONFIG_VALUE"] != "from-config" {
		t.Fatalf("CONFIG_VALUE = %q, want from-config", env["CONFIG_VALUE"])
	}
	if env["TASK_NAME"] != task.Name {
		t.Fatalf("TASK_NAME = %q, want %q", env["TASK_NAME"], task.Name)
	}
}

func TestHarnessWrapperTurnRequestOmitsOptionalMissingValueFromEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	optional := true
	task.Spec.Env = []corev1.EnvVar{
		{
			Name: "OPTIONAL_CONFIG",
			ValueFrom: &corev1.EnvVarSource{ConfigMapKeyRef: &corev1.ConfigMapKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: "missing-config"},
				Key:                  "setting",
				Optional:             &optional,
			}},
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	for _, item := range request.Input.Env {
		if item.Name == "OPTIONAL_CONFIG" {
			t.Fatalf("OPTIONAL_CONFIG was emitted with value %q, want omitted", item.Value)
		}
	}
}

func TestHarnessWrapperTurnRequestCarriesWorkspaceGitSecretEnv(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{Workspace: &corev1alpha1.WorkspaceConfig{
		GitRepo:      "https://github.com/orka-agents/orka",
		GitSecretRef: &corev1.LocalObjectReference{Name: "git-credentials"},
	}}
	gitSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "git-credentials", Namespace: task.Namespace},
		Data: map[string][]byte{
			"token":    []byte("git-token-value"),
			"username": []byte("git-user"),
		},
	}
	agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: "agent-runtime-secret"}
	agentSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "agent-runtime-secret", Namespace: task.Namespace},
		Data: map[string][]byte{
			workerenv.GitToken: []byte("agent-token-value"),
		},
	}
	r := newUnitReconciler(newTestScheme(), task, agent, gitSecret, agentSecret)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	if env[workerenv.GitToken] != "git-token-value" {
		t.Fatalf("git token env missing or wrong: %#v", env)
	}
	if env[workerenv.GitHubToken] == "git-token-value" {
		t.Fatalf("workspace git token should not overwrite runtime %s: %#v", workerenv.GitHubToken, env)
	}
	if env[workerenv.GitUsername] != "git-user" {
		t.Fatalf("git username env = %q, want git-user", env[workerenv.GitUsername])
	}
	if env[workerenv.GitAskpass] == "" {
		t.Fatalf("%s missing from harness turn env", workerenv.GitAskpass)
	}
}

func TestHarnessWrapperTurnRequestCarriesSafeEnvAndWorkspaceMetadata(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.Env = []corev1.EnvVar{{Name: workerenv.PRBaseSHA, Value: "base-sha"}, {Name: "ORKA_SECURITY_STAGE", Value: "review"}}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{Workspace: &corev1alpha1.WorkspaceConfig{
		GitRepo:      "https://github.com/orka-agents/orka",
		Branch:       "main",
		SubPath:      "docs",
		ForkRepo:     "https://github.com/orka-agents/orka-fork",
		PRBaseBranch: "contract",
		PushBranch:   "agent/test-branch",
	}}
	r := newUnitReconciler(newTestScheme(), task, agent)
	r.JobBuilder = &JobBuilder{ControllerURL: "http://orka-api.test:8080"}
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	for key, want := range map[string]string{
		"gitRepo":          "https://github.com/orka-agents/orka",
		"gitBranch":        "main",
		"workspaceSubPath": "docs",
		"forkRepo":         "https://github.com/orka-agents/orka-fork",
		"prBaseBranch":     "contract",
		"pushBranch":       "agent/test-branch",
		"prBaseSHA":        "base-sha",
	} {
		if got := request.Metadata[key]; got != want {
			t.Fatalf("metadata[%s] = %q, want %q", key, got, want)
		}
	}
	env := map[string]string{}
	for _, item := range request.Input.Env {
		env[item.Name] = item.Value
	}
	for key, want := range map[string]string{
		workerenv.ControllerURL:  "http://orka-api.test:8080",
		workerenv.ResultEndpoint: "http://orka-api.test:8080/internal/v1/results/default/harness-task",
		workerenv.PRBaseSHA:      "base-sha",
		"ORKA_SECURITY_STAGE":    "review",
	} {
		if got := env[key]; got != want {
			t.Fatalf("env[%s] = %q, want %q", key, got, want)
		}
	}
}

func TestValidateHarnessWrapperTaskEnvRejectsSecretAndUnsupportedValueFrom(t *testing.T) {
	if err := validateHarnessWrapperTaskEnv([]corev1.EnvVar{{Name: "ORKA_SECURITY_STAGE", Value: "review"}}); err != nil {
		t.Fatalf("safe env rejected: %v", err)
	}
	if err := validateHarnessWrapperTaskEnv([]corev1.EnvVar{{Name: "API_TOKEN", Value: "secret"}}); err == nil {
		t.Fatal("expected secret-shaped env name to be rejected")
	}
	if err := validateHarnessWrapperTaskEnv([]corev1.EnvVar{{Name: "SAFE", ValueFrom: &corev1.EnvVarSource{}}}); err == nil {
		t.Fatal("expected valueFrom env to be rejected")
	}
}

func TestHarnessWrapperCapabilitiesReadErrorRetryable(t *testing.T) {
	if !harnessWrapperCapabilitiesErrorIsRetryable(fmt.Errorf("read harness runtime capabilities: boom")) {
		t.Fatal("expected capabilities read error to be retryable")
	}
	if harnessWrapperCapabilitiesErrorIsRetryable(fmt.Errorf("read harness runtime capabilities: get failed (404): not found")) {
		t.Fatal("expected permanent capabilities 404 to remain terminal")
	}
}

func TestHarnessWrapperStreamMissingTurnErrorClassification(t *testing.T) {
	if !harnessWrapperStreamErrorIsMissingTurn(fmt.Errorf("stream_frames failed (404): turn not found")) {
		t.Fatal("404 turn-not-found stream error should be classified as retryable missing turn")
	}
	if harnessWrapperStreamErrorIsMissingTurn(harness.ClientError{
		Op:         "stream_frames",
		StatusCode: http.StatusGone,
		Message:    "turn not found",
	}) {
		t.Fatal("typed 410 turn-not-found stream error should remain terminal")
	}
	for _, message := range []string{
		"stream_frames failed (410): terminal turn expired from runtime retention",
		"stream_frames failed (410): turn not found",
		"stream_frames failed (401): unauthorized",
	} {
		if harnessWrapperStreamErrorIsMissingTurn(fmt.Errorf("%s", message)) {
			t.Fatalf("%q should not be classified as retryable missing turn", message)
		}
	}
}

func TestHarnessWrapperCancelMissingTurnErrorClassification(t *testing.T) {
	for _, message := range []string{
		"cancel_turn failed (404): turn not found",
		"cancel_turn failed (410): terminal turn expired from runtime retention",
	} {
		if !harnessWrapperCancelErrorIsMissingTurn(fmt.Errorf("%s", message)) {
			t.Fatalf("%q should be ignored during cancellation", message)
		}
	}
}

func TestHarnessWrapperBrokeredPauseExcludesTerminalGone(t *testing.T) {
	err := fmt.Errorf("continue brokered tool call \"call-1\": stream_frames failed (410): gone")
	if harnessWrapperStreamErrorIsBrokeredPause(err) {
		t.Fatal("terminal 410 continuation error should not be classified as a brokered pause")
	}
	if !harnessWrapperStreamErrorIsBrokeredPause(fmt.Errorf("continue brokered tool call \"call-1\": approval pending")) {
		t.Fatal("ordinary continuation pause should remain classified as brokered pause")
	}
}

func TestHarnessWrapperStreamTerminalErrorClassification(t *testing.T) {
	for _, message := range []string{
		"stream_frames failed (410): terminal turn expired from runtime retention",
		"harness frame identity does not match running turn",
		"invalid harness frame: turn completed payload is required",
		"stream_frames failed: decode harness frame: invalid character",
	} {
		if !harnessWrapperStreamErrorIsTerminal(fmt.Errorf("%s", message)) {
			t.Fatalf("%q should be terminal", message)
		}
	}
}

func TestHarnessWrapperRuntimeSessionIdentityUsesUIDWithoutExplicitSession(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	identity := harnessWrapperRuntimeSessionIdentity(task, agent, string(corev1alpha1.AgentRuntimeClaude))
	got := string(identity.ID)
	if !strings.Contains(got, string(task.UID)) {
		t.Fatalf("runtime session id = %q, want task UID", got)
	}
	if identity.Owner.Namespace != task.Namespace || identity.Owner.SessionName != task.Name+":"+string(task.UID) || identity.Owner.ActiveTask != task.Name || identity.Owner.AgentName != agent.Name || identity.Owner.Provider != harness.ProviderKindKubernetesService {
		t.Fatalf("runtime session owner = %#v, want task-scoped owner metadata", identity.Owner)
	}
	task.Spec.SessionRef = &corev1alpha1.SessionReference{Name: "shared-session"}
	identity = harnessWrapperRuntimeSessionIdentity(task, agent, string(corev1alpha1.AgentRuntimeClaude))
	got = string(identity.ID)
	if strings.Contains(got, string(task.UID)) || !strings.Contains(got, "shared-session") {
		t.Fatalf("runtime session id = %q, want explicit shared session without UID", got)
	}
	if identity.Owner.SessionName != "shared-session" || identity.Owner.ActiveTask != task.Name {
		t.Fatalf("runtime session owner = %#v, want shared session owner with active task", identity.Owner)
	}
	if got := harnessWrapperRuntimeSessionID(task, string(corev1alpha1.AgentRuntimeClaude)); got != identity.ID {
		t.Fatalf("compat runtime session id = %q, want identity id %q", got, identity.ID)
	}
}

func TestCancelHarnessWrapperPlannedMissingTurnIsIgnored(t *testing.T) {
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)
	task, agent := harnessWrapperTaskAndAgent()
	request, err := (&TaskReconciler{}).harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(request.TurnID),
		harnessWrapperRuntimeAnnotation: string(request.RuntimeSessionID),
		harnessWrapperCorrelationIDAnno: request.CorrelationID,
		harnessWrapperStartedAnno:       testFalseValue,
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	if err := r.cancelHarnessWrapperTurn(context.Background(), task, "test"); err != nil {
		t.Fatalf("cancelHarnessWrapperTurn() error = %v, want nil for missing planned turn", err)
	}
}

func TestHarnessWrapperTurnAnnotationsMustMatchCurrentAttempt(t *testing.T) {
	task, _ := harnessWrapperTaskAndAgent()
	task.Status.Attempts = 2
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(harnessWrapperTurnID(task, 2)),
		harnessWrapperRuntimeAnnotation: "attacker-controlled",
		harnessWrapperCorrelationIDAnno: string(task.UID),
	}
	if !harnessWrapperTurnAnnotationsMatchTaskAttempt(task, 2) {
		t.Fatal("expected current task/attempt turn annotations to match")
	}
	task.Annotations[harnessWrapperTurnIDAnnotation] = string(harnessWrapperTurnID(task, 1))
	if harnessWrapperTurnAnnotationsMatchTaskAttempt(task, 2) {
		t.Fatal("expected stale/copied turn id to be rejected")
	}
}

func TestCancelHarnessWrapperStartedMissingTurnIsIgnored(t *testing.T) {
	cfg := cliwrapper.DefaultConfig()
	cfg.AllowUnauthenticated = true
	server, err := cliwrapper.NewServer(cfg, &cliwrapper.FakeAdapter{Behavior: cliwrapper.FakeBehaviorSuccess})
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(server.Handler())
	defer srv.Close()
	t.Setenv(harnessWrapperEndpointEnv, srv.URL)
	task, agent := harnessWrapperTaskAndAgent()
	task.Status.Attempts = 1
	request, err := (&TaskReconciler{}).harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatal(err)
	}
	task.Annotations = map[string]string{
		harnessWrapperTurnIDAnnotation:  string(request.TurnID),
		harnessWrapperRuntimeAnnotation: string(request.RuntimeSessionID),
		harnessWrapperCorrelationIDAnno: request.CorrelationID,
		harnessWrapperStartedAnno:       scheduledRunLabelValue,
	}
	r := newUnitReconciler(newTestScheme(), task, agent)
	if err := r.cancelHarnessWrapperTurn(context.Background(), task, "test"); err != nil {
		t.Fatalf("cancelHarnessWrapperTurn() error = %v, want nil for missing started turn", err)
	}
}

func TestHarnessWrapperStartTurnErrorClassification(t *testing.T) {
	if !harnessWrapperStartTurnErrorIsRetryable(fmt.Errorf("post failed: connection refused")) {
		t.Fatal("expected transport start error to be retryable")
	}
	if harnessWrapperStartTurnErrorIsRetryable(fmt.Errorf("start_turn failed (401): unauthorized")) {
		t.Fatal("expected auth start error to remain terminal")
	}
	if harnessWrapperStartTurnErrorIsRetryable(
		fmt.Errorf("start_turn failed (410): runtime session unavailable after unconfirmed hosted cancellation"),
	) {
		t.Fatal("expected quarantined runtime session error to remain terminal")
	}
}

func TestHarnessWrapperTurnMetadataDefaultsMaxTurns(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	r := newUnitReconciler(newTestScheme(), task, agent)
	request, err := r.harnessWrapperStartTurnRequest(context.Background(), task, agent, time.Now(), 1)
	if err != nil {
		t.Fatalf("harnessWrapperStartTurnRequest: %v", err)
	}
	if request.Metadata["maxTurns"] != "50" {
		t.Fatalf("metadata maxTurns = %q, want 50", request.Metadata["maxTurns"])
	}
}

func TestSessionMessagesPromptIncludesCurrentTurnOnce(t *testing.T) {
	prompt := sessionMessagesPrompt([]store.SessionMessage{
		{Role: "user", Content: "first"},
		{Role: "assistant", Content: "reply"},
		{Role: "user", Content: "current"},
	})
	if strings.Count(prompt, "current") != 1 || !strings.Contains(prompt, "ASSISTANT:\nreply") {
		t.Fatalf("sessionMessagesPrompt() = %q", prompt)
	}
}

func TestSessionMessagesPromptIncludesGatewaySenderProvenance(t *testing.T) {
	prompt := sessionMessagesPrompt([]store.SessionMessage{{
		Role: "user", Content: "current", SourceType: "gateway-event",
		Metadata: map[string]string{
			"senderId": "user-1", "senderDisplayName": "User One", "accountId": "acct",
			"contextId": "room", "threadId": "thread-1",
		},
	}})
	for _, want := range []string{`senderId="user-1"`, `senderDisplayName="User One"`, `contextId="room"`, "current"} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("sessionMessagesPrompt() = %q, want %q", prompt, want)
		}
	}
}

func TestHarnessWrapperPromptIncludedUsesTaskScopedRuntimeSession(t *testing.T) {
	task, agent := harnessWrapperTaskAndAgent()
	task.Spec.SessionRef = &corev1alpha1.SessionReference{
		Name: "canonical-session", PromptIncluded: true, ThroughMessageID: "message-1",
	}
	identity := harnessWrapperRuntimeSessionIdentity(task, agent, "codex")
	if identity.Owner.SessionName == "canonical-session" || !strings.Contains(identity.Owner.SessionName, task.Name) {
		t.Fatalf("runtime session owner = %#v, want task-scoped identity", identity.Owner)
	}
}

func TestSessionMessagesPromptStaysBelowExecStringLimit(t *testing.T) {
	prompt := sessionMessagesPrompt([]store.SessionMessage{
		{Role: "user", Content: strings.Repeat("a", 64<<10)},
		{Role: "assistant", Content: strings.Repeat("b", 64<<10)},
		{Role: "user", Content: strings.Repeat("c", 64<<10)},
	})
	if len(prompt) == 0 || len(prompt) > 96<<10 {
		t.Fatalf("prompt length = %d, want 1..%d", len(prompt), 96<<10)
	}
}

func TestSessionMessagesPromptTruncatesOnUTF8Boundary(t *testing.T) {
	prompt := sessionMessagesPrompt([]store.SessionMessage{{
		Role: "user", Content: strings.Repeat("🙂", 30_000),
	}})
	if prompt == "" || !utf8.ValidString(prompt) {
		t.Fatalf("sessionMessagesPrompt() produced invalid UTF-8")
	}
}

func TestSessionMessagesPromptDropsLeadingOrphanAssistant(t *testing.T) {
	prompt := sessionMessagesPrompt([]store.SessionMessage{
		{Role: "user", Content: strings.Repeat("q", 64<<10)},
		{Role: "assistant", Content: "orphaned reply"},
		{Role: "user", Content: strings.Repeat("c", 64<<10)},
	})
	if strings.Contains(prompt, "orphaned reply") || !strings.Contains(prompt, "USER:\n") {
		t.Fatalf("sessionMessagesPrompt() kept an orphan assistant turn: %q", prompt)
	}
}
