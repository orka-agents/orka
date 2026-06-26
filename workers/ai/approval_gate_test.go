/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/approvals"
	"github.com/sozercan/orka/internal/contexttoken"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/llm"
	toolspkg "github.com/sozercan/orka/internal/tools"
	"github.com/sozercan/orka/internal/worker"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/workers/common"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	gatedDispatchTool    = "dispatch_work_order"
	declineHandledResult = "decline handled"
	correctedResult      = "corrected"
	doneResult           = "done"
	approvalTestAuthKey  = "api_key"
)

func TestApprovalGatePrescansBatchBeforeExecutingGatedTool(t *testing.T) {
	for _, tt := range []struct {
		name  string
		calls []llm.ToolCall
	}{
		{
			name: "dispatch before request approval",
			calls: []llm.ToolCall{
				{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`{"incident":"inc-1"}`),
				},
				{
					ID:        "call-approval",
					Name:      "request_approval",
					Arguments: json.RawMessage(`{"action":"approve dispatch"}`),
				},
			},
		},
		{
			name: "request approval before dispatch",
			calls: []llm.ToolCall{
				{
					ID:        "call-approval",
					Name:      "request_approval",
					Arguments: json.RawMessage(`{"action":"approve dispatch"}`),
				},
				{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`{"incident":"inc-1"}`),
				},
			},
		},
		{
			name: "dispatch only",
			calls: []llm.ToolCall{{
				ID:        "call-dispatch",
				Name:      gatedDispatchTool,
				Arguments: json.RawMessage(`{"incident":"inc-1"}`),
			}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			restore := replaceDefaultToolRegistryForTest(t)
			defer restore()
			var executions atomic.Int32
			toolspkg.DefaultRegistry.Register(recordingTool{
				name: gatedDispatchTool,
				onExecute: func(json.RawMessage) {
					executions.Add(1)
				},
			})
			toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
			t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
			t.Setenv(workerenv.AutonomousMode, "true")
			recorder := common.NewFakeEventRecorder()

			result, err := executeAgentLoopWithEvents(
				context.Background(),
				&mockProvider{response: &llm.CompletionResponse{
					Content:    "need action",
					ToolCalls:  tt.calls,
					StopReason: "tool_use",
				}},
				[]llm.Message{{Role: "user", Content: "handle incident"}},
				"",
				"test-model",
				toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool, "request_approval"}),
				nil,
				nil,
				recorder,
				&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
			)
			if err != nil {
				t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
			}
			if executions.Load() != 0 {
				t.Fatalf("gated tool executed before approval")
			}
			if !strings.Contains(result, "approval requested") {
				t.Fatalf("result = %q, want approval requested", result)
			}
			assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeApprovalRequested})
		})
	}
}

func TestApprovalGateExecutesApprovedMatchingTargetWithIdempotencyKey(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var gotArgs json.RawMessage
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(args json.RawMessage) {
			gotArgs = append(json.RawMessage(nil), args...)
		},
	})
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target := approvalTargetForTest(t, args)
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{Content: doneResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}),
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != doneResult {
		t.Fatalf("result = %q, want done", result)
	}
	var body map[string]any
	if err := json.Unmarshal(gotArgs, &body); err != nil {
		t.Fatalf("executed args JSON: %v", err)
	}
	if body["idempotencyKey"] != target.ApprovalID {
		t.Fatalf("executed args = %#v, want idempotencyKey %s", body, target.ApprovalID)
	}
}

func TestApprovalGateMarksApprovedCustomToolFiredWhenAttemptFails(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		http.Error(w, "applied then failed", http.StatusInternalServerError)
	}))
	defer toolServer.Close()
	customTool := approvalTestCustomTool(toolServer.URL)
	args := json.RawMessage(`{"incident":"inc-1"}`)
	specDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		specDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching twice",
				ToolCalls: []llm.ToolCall{
					{ID: "call-dispatch-1", Name: gatedDispatchTool, Arguments: args},
					{ID: "call-dispatch-2", Name: gatedDispatchTool, Arguments: args},
				},
				StopReason: "tool_use",
			},
			{Content: doneResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != doneResult {
		t.Fatalf("result = %q, want done", result)
	}
	if executions.Load() != 1 {
		t.Fatalf("custom tool executions = %d, want one attempted side effect", executions.Load())
	}
}

func TestApprovalGatePreScanRejectsRequiredToolThatIsNotEnabled(t *testing.T) {
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "hallucinated tool",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`{"incident":"inc-1"}`),
				}},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{},
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want %s", result, correctedResult)
	}
}

func TestApprovalGatePreScanReturnsMalformedGatedArgsToModel(t *testing.T) {
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "bad gated args",
				ToolCalls: []llm.ToolCall{
					{
						ID:        "call-dispatch",
						Name:      gatedDispatchTool,
						Arguments: json.RawMessage(`{"unterminated"`),
					},
					{
						ID:        "call-other",
						Name:      "other_tool",
						Arguments: json.RawMessage(`{"ok":true}`),
					},
				},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}, {Name: "other_tool"}},
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want corrected", result)
	}
}

func TestApprovalGatePreScanReturnsNonObjectGatedArgsToModel(t *testing.T) {
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "bad gated args",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`[]`),
				}},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want corrected", result)
	}
}

func TestApprovalGatePrepareApprovedCallRefreshesAuthRefBeforeConsumingApproval(t *testing.T) {
	customTool := approvalTestCustomTool("https://tools.example.test/dispatch")
	customTool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	customTool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "uid-1",
		approvalAuthRefResourceVersionAnnotation: "10",
	}
	args := json.RawMessage(`{"incident":"inc-1"}`)
	oldSpecDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("old target spec digest: %v", err)
	}
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		oldSpecDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	gate := &approvalGate{
		namespace: "default",
		taskName:  "incident-task",
		taskUID:   "task-uid-1",
		required:  map[string]struct{}{gatedDispatchTool: {}},
		resolved:  []approvals.ResolvedApproval{resolvedApprovalForTarget(target)},
		firedKeys: map[string]bool{},
		refreshTarget: func(_ context.Context, _ string, tool *corev1alpha1.Tool) {
			tool.Annotations[approvalAuthRefResourceVersionAnnotation] = "11"
		},
	}

	_, _, _, err = gate.prepareApprovedCall(context.Background(), gatedDispatchTool, args, customTool)
	if err == nil || !strings.Contains(err.Error(), "is not resolved") {
		t.Fatalf("prepareApprovedCall() error = %v, want unresolved after auth ref version refresh", err)
	}
}

func TestApprovalGatePrepareApprovedCallSkipsUngatedMalformedArgs(t *testing.T) {
	args := json.RawMessage(`{"unterminated"`)
	for _, tt := range []struct {
		name     string
		resolved []approvals.ResolvedApproval
	}{
		{name: "no resolved approvals"},
		{
			name:     "unrelated resolved approval",
			resolved: []approvals.ResolvedApproval{{ID: "other", Status: approvals.StatusApproved}},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			gate := &approvalGate{resolved: tt.resolved}
			gotArgs, approvalKey, alreadyFired, err := gate.prepareApprovedCall(context.Background(), "ungated_tool", args, nil)
			if err != nil {
				t.Fatalf("prepareApprovedCall() error = %v", err)
			}
			if string(gotArgs) != string(args) || approvalKey != "" || alreadyFired {
				t.Fatalf("prepareApprovedCall() = args %q key %q fired %t", gotArgs, approvalKey, alreadyFired)
			}
		})
	}
}

func TestSplitBlockingApprovalOverflowDoesNotMutateInput(t *testing.T) {
	const (
		firstApprovalID  = "first"
		secondApprovalID = "second"
	)
	target := approvalTargetForTest(t, json.RawMessage(`{"incident":"inc-1"}`))
	first := resolvedApprovalForTarget(target)
	first.ID = firstApprovalID
	second := resolvedApprovalForTarget(target)
	second.ID = secondApprovalID
	input := []approvals.ResolvedApproval{approvals.BlockingOverflowResolvedApproval(), first, second}

	filtered, overflow := splitBlockingApprovalOverflow(input)
	if !overflow {
		t.Fatal("overflow = false, want true")
	}
	if len(filtered) != 2 || filtered[0].ID != firstApprovalID || filtered[1].ID != secondApprovalID {
		t.Fatalf("filtered = %#v, want first/second", filtered)
	}
	inputPreserved := approvals.IsResolvedApprovalBlockingOverflow(input[0]) &&
		input[1].ID == firstApprovalID &&
		input[2].ID == secondApprovalID
	if !inputPreserved {
		t.Fatalf("input mutated by splitBlockingApprovalOverflow: %#v", input)
	}
}

func TestApprovalGateResolvedDecisionPrefersExactApprovalID(t *testing.T) {
	target := approvalTargetForTest(t, json.RawMessage(`{"incident":"inc-1"}`))
	legacy := resolvedApprovalForTarget(target)
	legacy.ID = "legacy-approval-id"
	legacy.TaskUID = ""
	legacy.Status = approvals.StatusApproved
	exact := resolvedApprovalForTarget(target)
	exact.Status = approvals.StatusDeclined
	gate := &approvalGate{resolved: []approvals.ResolvedApproval{legacy, exact}}

	got, found := gate.resolvedDecision(target)
	if !found {
		t.Fatal("resolvedDecision() did not match exact decision")
	}
	if got.ID != exact.ID || got.Status != approvals.StatusDeclined {
		t.Fatalf("resolvedDecision() = %#v, want exact declined decision", got)
	}
}

func TestApprovalGateStaleDecisionTreatsMissingSpecDigestAsStale(t *testing.T) {
	target := approvalTargetForTest(t, json.RawMessage(`{"incident":"inc-1"}`))
	legacy := resolvedApprovalForTarget(target)
	legacy.TargetSpecDigest = ""
	gate := &approvalGate{resolved: []approvals.ResolvedApproval{legacy}}

	got, stale := gate.staleDecisionForTarget(approvals.ApprovalTarget{
		TaskUID:          target.TaskUID,
		TargetTool:       target.TargetTool,
		TargetArgsDigest: target.TargetArgsDigest,
		TargetSpecDigest: "current-spec-digest",
	})
	if !stale {
		t.Fatal("staleDecisionForTarget() stale = false, want true for missing legacy spec digest")
	}
	if got.ID != legacy.ID {
		t.Fatalf("staleDecisionForTarget() ID = %q, want %q", got.ID, legacy.ID)
	}
}

func TestApprovalGateResolvedDecisionIgnoresArbitraryUntaggedDigestMatch(t *testing.T) {
	target := approvalTargetForTest(t, json.RawMessage(`{"incident":"inc-1"}`))
	legacy := resolvedApprovalForTarget(target)
	legacy.ID = "unrelated-legacy-id"
	legacy.TaskUID = ""
	gate := &approvalGate{namespace: "default", taskName: "incident-task", resolved: []approvals.ResolvedApproval{legacy}}

	if got, found := gate.resolvedDecision(target); found {
		t.Fatalf("resolvedDecision() matched arbitrary legacy decision: %#v", got)
	}
}

func TestApprovalGateResolvedDecisionMatchesLegacyUntaggedApproval(t *testing.T) {
	target := approvalTargetForTest(t, json.RawMessage(`{"incident":"inc-1"}`))
	legacy := resolvedApprovalForTarget(target)
	legacy.ID = approvals.ApprovalID(
		"default",
		"incident-task",
		"",
		target.TargetTool,
		target.TargetArgsDigest,
		target.TargetSpecDigest,
	)
	legacy.TaskUID = ""
	gate := &approvalGate{namespace: "default", taskName: "incident-task", resolved: []approvals.ResolvedApproval{legacy}}

	got, found := gate.resolvedDecision(target)
	if !found {
		t.Fatal("resolvedDecision() did not match legacy untagged decision")
	}
	if got.ID != legacy.ID {
		t.Fatalf("resolvedDecision() ID = %q, want legacy ID %q", got.ID, legacy.ID)
	}
}

func TestApprovalGateMismatchedCustomToolSpecRequestsNewApproval(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()

	args := json.RawMessage(`{"incident":"inc-1"}`)
	oldTool := approvalTestCustomTool(toolServer.URL + "/old")
	oldSpecDigest, err := approvalTargetSpecDigest(oldTool)
	if err != nil {
		t.Fatalf("old spec digest: %v", err)
	}
	oldTarget, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		oldSpecDigest,
	)
	if err != nil {
		t.Fatalf("old approval target: %v", err)
	}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(oldTarget)})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")
	recorder := common.NewFakeEventRecorder()
	currentTool := approvalTestCustomTool(toolServer.URL + "/new")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{response: &llm.CompletionResponse{
			Content: "dispatching",
			ToolCalls: []llm.ToolCall{{
				ID:        "call-dispatch",
				Name:      gatedDispatchTool,
				Arguments: args,
			}},
			StopReason: "tool_use",
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: currentTool},
		worker.NewToolExecutor(),
		recorder,
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("mismatched custom tool spec executed gated tool")
	}
	if !strings.Contains(result, "approval requested") {
		t.Fatalf("result = %q, want new approval requested", result)
	}
	assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeApprovalRequested})
}

func TestApprovalGateMismatchedResolvedApprovalRequestsNewApproval(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var executions atomic.Int32
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(json.RawMessage) {
			executions.Add(1)
		},
	})
	oldTarget := approvalTargetForTest(t, json.RawMessage(`{"incident":"old"}`))
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(oldTarget)})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")
	recorder := common.NewFakeEventRecorder()

	_, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{response: &llm.CompletionResponse{
			Content: "dispatching",
			ToolCalls: []llm.ToolCall{{
				ID:        "call-dispatch",
				Name:      gatedDispatchTool,
				Arguments: json.RawMessage(`{"incident":"new"}`),
			}},
			StopReason: "tool_use",
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}),
		nil,
		nil,
		recorder,
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if executions.Load() != 0 {
		t.Fatalf("mismatched approval executed gated tool")
	}
	assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeApprovalRequested})
}

func TestApprovalGateSkipsDuplicateApprovedFireWithinWorker(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var executions atomic.Int32
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(json.RawMessage) {
			executions.Add(1)
		},
	})
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target := approvalTargetForTest(t, args)
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	_, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{
				Content: "dispatching again",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-2",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{Content: doneResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}),
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if executions.Load() != 1 {
		t.Fatalf("executions = %d, want 1", executions.Load())
	}
}

func TestResolvedApprovalsContextIsReadable(t *testing.T) {
	got := formatResolvedApprovalsContext([]approvals.ResolvedApproval{{
		ID:                "k-1",
		TargetTool:        "dispatch_work_order",
		Status:            approvals.StatusApproved,
		Actor:             "reviewer-a",
		DecisionTime:      "2026-06-23T10:00:00Z",
		Reason:            "safe to dispatch",
		TargetArgsPreview: json.RawMessage(`{"incident":"inc-1"}`),
	}})
	wantParts := []string{
		"## Resolved Human Approvals",
		"APPROVED k-1",
		"dispatch_work_order",
		"reviewer-a",
		`args={"incident":"inc-1"}`,
		"safe to dispatch",
	}
	for _, want := range wantParts {
		if !strings.Contains(got, want) {
			t.Fatalf("resolved approval context missing %q:\n%s", want, got)
		}
	}
}

func approvalTargetForTest(t *testing.T, args json.RawMessage) approvals.ApprovalTarget {
	t.Helper()
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
	)
	if err != nil {
		t.Fatalf("NewApprovalTarget() error = %v", err)
	}
	return target
}

func TestApprovalTargetSpecDigestKeepsHTTPToolSpecDigest(t *testing.T) {
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	got, err := approvalTargetSpecDigest(tool)
	if err != nil {
		t.Fatalf("approvalTargetSpecDigest() error = %v", err)
	}
	want, err := approvals.TargetSpecDigest(tool.Spec)
	if err != nil {
		t.Fatalf("TargetSpecDigest() error = %v", err)
	}
	if got != want {
		t.Fatalf("HTTP tool digest = %q, want raw spec digest %q", got, want)
	}
}

func TestApprovalTargetArgumentsRejectsBodyAuthKeyURLInterpolation(t *testing.T) {
	customTool := approvalTestCustomTool("https://tools.example.test/items/{{" + approvalTestAuthKey + "}}")
	customTool.Spec.HTTP.AuthInject = approvalAuthInjectBody
	customTool.Spec.HTTP.AuthBodyKey = approvalTestAuthKey
	customTool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	customTool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "uid-1",
		approvalAuthRefResourceVersionAnnotation: "10",
	}

	_, err := approvalTargetArguments(
		json.RawMessage(fmt.Sprintf(`{%q:"model-supplied"}`, approvalTestAuthKey)),
		customTool,
	)
	if err == nil || !strings.Contains(err.Error(), "body auth key") {
		t.Fatalf("approvalTargetArguments() error = %v, want body auth URL interpolation rejection", err)
	}
}

func TestApprovalTargetArgumentsKeepsBodyAuthKeyWithoutAuthRef(t *testing.T) {
	customTool := approvalTestCustomTool("https://tools.example.test/dispatch")
	customTool.Spec.HTTP.AuthInject = approvalAuthInjectBody
	customTool.Spec.HTTP.AuthBodyKey = approvalTestAuthKey

	got, err := approvalTargetArguments(
		json.RawMessage(fmt.Sprintf(`{"incident":"inc-1",%q:"model-supplied"}`, approvalTestAuthKey)),
		customTool,
	)
	if err != nil {
		t.Fatalf("approvalTargetArguments() error = %v", err)
	}
	if !strings.Contains(string(got), approvalTestAuthKey) {
		t.Fatalf("approvalTargetArguments() = %s, want body auth key retained without auth ref", got)
	}
}

func TestApprovalTargetArgumentsRejectsReservedURLField(t *testing.T) {
	customTool := approvalTestCustomTool("https://tools.example.test/items/{{id}}")
	_, err := approvalTargetArguments(
		json.RawMessage(`{"id":1,"__orkaApprovalURL":"user-value"}`),
		customTool,
	)
	if err == nil || !strings.Contains(err.Error(), approvalTargetURLField) {
		t.Fatalf("approvalTargetArguments() error = %v, want reserved URL field rejection", err)
	}
}

func TestApprovalTargetArgumentsHashesURLInterpolationAsExecuted(t *testing.T) {
	customTool := approvalTestCustomTool("https://tools.example.test/items/{{id}}")
	numeric, err := approvalTargetArguments(json.RawMessage(`{"id":1,"op":"delete"}`), customTool)
	if err != nil {
		t.Fatalf("approvalTargetArguments(numeric) error = %v", err)
	}
	stringID, err := approvalTargetArguments(json.RawMessage(`{"id":"1","op":"delete"}`), customTool)
	if err != nil {
		t.Fatalf("approvalTargetArguments(string) error = %v", err)
	}
	numericDigest, err := approvals.TargetArgsDigest(numeric)
	if err != nil {
		t.Fatalf("numeric digest: %v", err)
	}
	stringDigest, err := approvals.TargetArgsDigest(stringID)
	if err != nil {
		t.Fatalf("string digest: %v", err)
	}
	if numericDigest != stringDigest {
		t.Fatalf("URL-interpolated numeric/string target digests differ: %s != %s", numericDigest, stringDigest)
	}
	if strings.Contains(string(numeric), `"id"`) || !strings.Contains(string(numeric), approvalTargetURLField) {
		t.Fatalf("URL-interpolated target args = %s, want id removed and URL field added", numeric)
	}
}

func TestFormatResolvedApprovalsContextSkipsBlockingOverflowSentinel(t *testing.T) {
	got := formatResolvedApprovalsContext([]approvals.ResolvedApproval{
		approvals.BlockingOverflowResolvedApproval(),
	})
	if got != "" {
		t.Fatalf("formatResolvedApprovalsContext() = %q, want sentinel omitted", got)
	}
}

func TestBindApprovalAuthRefVersionClearsStaleAnnotationsWhenUnavailable(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	tool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "missing-auth", Key: "authref"}
	tool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "stale-uid",
		approvalAuthRefResourceVersionAnnotation: "9",
	}

	bindApprovalAuthRefVersion(
		context.Background(),
		ctrlfake.NewClientBuilder().WithScheme(scheme).Build(),
		"default",
		tool,
	)

	if got := tool.Annotations[approvalAuthRefUIDAnnotation]; got != "" {
		t.Fatalf("stale auth ref UID annotation = %q, want cleared", got)
	}
	if got := tool.Annotations[approvalAuthRefResourceVersionAnnotation]; got != "" {
		t.Fatalf("stale auth ref resourceVersion annotation = %q, want cleared", got)
	}
}

func TestApprovalTargetSpecDigestRejectsMountedCredentialSource(t *testing.T) {
	root := t.TempDir()
	oldRoots := approvalMountRoots
	approvalMountRoots = []string{root}
	t.Cleanup(func() { approvalMountRoots = oldRoots })
	if err := os.WriteFile(filepath.Join(root, "authref"), []byte("test"), 0o600); err != nil {
		t.Fatalf("write mounted auth ref marker: %v", err)
	}
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	tool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	tool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "uid-1",
		approvalAuthRefResourceVersionAnnotation: "10",
	}
	_, err := approvalTargetSpecDigest(tool)
	if err == nil || !strings.Contains(err.Error(), "mounted credential source") {
		t.Fatalf("approvalTargetSpecDigest() error = %v, want mounted credential source rejection", err)
	}
}

func TestApprovalTargetSpecDigestRejectsBodyAuthWithoutBodyKey(t *testing.T) {
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	tool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	tool.Spec.HTTP.AuthInject = approvalAuthInjectBody
	tool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "uid-1",
		approvalAuthRefResourceVersionAnnotation: "10",
	}
	_, err := approvalTargetSpecDigest(tool)
	if err == nil || !strings.Contains(err.Error(), "authBodyKey is required") {
		t.Fatalf("approvalTargetSpecDigest() error = %v, want authBodyKey rejection", err)
	}
}

func TestApprovalTargetSpecDigestRejectsBodyAuthKeyURLInterpolation(t *testing.T) {
	tool := approvalTestCustomTool("https://tools.example.test/items/{{" + approvalTestAuthKey + "}}")
	tool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	tool.Spec.HTTP.AuthInject = approvalAuthInjectBody
	tool.Spec.HTTP.AuthBodyKey = approvalTestAuthKey
	tool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "uid-1",
		approvalAuthRefResourceVersionAnnotation: "10",
	}
	_, err := approvalTargetSpecDigest(tool)
	if err == nil || !strings.Contains(err.Error(), "body auth key") {
		t.Fatalf("approvalTargetSpecDigest() error = %v, want body auth URL interpolation rejection", err)
	}
}

func TestApprovalTargetSpecDigestRejectsTransactionTokenHeader(t *testing.T) {
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	tool.Spec.HTTP.Headers = map[string]string{contexttoken.HeaderName: "static"}
	_, err := approvalTargetSpecDigest(tool)
	if err == nil || !strings.Contains(err.Error(), "reserved header") {
		t.Fatalf("approvalTargetSpecDigest() error = %v, want reserved transaction-token header rejection", err)
	}
}

func bindApprovalAuthRefVersionForTest(
	t *testing.T,
	tool *corev1alpha1.Tool,
	value string,
	resourceVersion string,
) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "dispatch-auth",
			Namespace:       "default",
			UID:             "uid-1",
			ResourceVersion: resourceVersion,
		},
		Data: map[string][]byte{"authref": []byte(value)},
	}
	bindApprovalAuthRefVersion(
		context.Background(),
		ctrlfake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build(),
		"default",
		tool,
	)
}

func approvalAuthRefToolForTest() *corev1alpha1.Tool {
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	tool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	return tool
}

func TestBindApprovalAuthRefVersionUpdatesRotatedAuthRef(t *testing.T) {
	tool := approvalAuthRefToolForTest()
	tool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "uid-1",
		approvalAuthRefResourceVersionAnnotation: "10",
	}

	bindApprovalAuthRefVersionForTest(t, tool, "value", "11")

	if got := tool.Annotations[approvalAuthRefResourceVersionAnnotation]; got != "11" {
		t.Fatalf("auth ref resourceVersion annotation = %q, want refreshed version 11", got)
	}
}

func TestBindApprovalAuthRefVersionSkipsEmptyAuthRefValue(t *testing.T) {
	tool := approvalAuthRefToolForTest()
	tool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "stale-uid",
		approvalAuthRefResourceVersionAnnotation: "9",
	}

	bindApprovalAuthRefVersionForTest(t, tool, "  ", "10")

	if got := tool.Annotations[approvalAuthRefUIDAnnotation]; got != "" {
		t.Fatalf("auth ref UID annotation = %q, want unset for empty auth ref value", got)
	}
}

func TestApprovalTargetSpecDigestRejectsStaticIdempotencyHeader(t *testing.T) {
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	tool.Spec.HTTP.Headers = map[string]string{"idempotency-key": "static"}
	_, err := approvalTargetSpecDigest(tool)
	if err == nil || !strings.Contains(err.Error(), "reserved header") {
		t.Fatalf("approvalTargetSpecDigest() error = %v, want reserved header rejection", err)
	}
}

func TestApprovalTargetSpecDigestRequiresAuthSecretVersion(t *testing.T) {
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	tool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	_, err := approvalTargetSpecDigest(tool)
	if err == nil || !strings.Contains(err.Error(), "auth secret version is not available") {
		t.Fatalf("approvalTargetSpecDigest() error = %v, want auth secret version rejection", err)
	}
}

func TestApprovalTargetSpecDigestReadsLegacyAuthRefAnnotations(t *testing.T) {
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	tool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	tool.Annotations = map[string]string{
		legacyApprovalAuthRefUIDAnnotation:             "legacy-uid-1",
		legacyApprovalAuthRefResourceVersionAnnotation: "10",
	}
	if _, err := approvalTargetSpecDigest(tool); err != nil {
		t.Fatalf("approvalTargetSpecDigest() error = %v", err)
	}
}

func TestApprovalTargetSpecDigestIncludesAuthSecretVersion(t *testing.T) {
	tool := approvalTestCustomTool("https://tools.example.test/dispatch")
	tool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	tool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "uid-1",
		approvalAuthRefResourceVersionAnnotation: "10",
	}
	first, err := approvalTargetSpecDigest(tool)
	if err != nil {
		t.Fatalf("approvalTargetSpecDigest() error = %v", err)
	}
	tool.Annotations[approvalAuthRefResourceVersionAnnotation] = "11"
	second, err := approvalTargetSpecDigest(tool)
	if err != nil {
		t.Fatalf("approvalTargetSpecDigest() second error = %v", err)
	}
	if first == second {
		t.Fatalf("auth secret resourceVersion change did not affect approval target digest")
	}
}

func TestApprovalTargetSpecDigestIncludesMCPStatusEndpoint(t *testing.T) {
	tool := &corev1alpha1.Tool{
		Spec: corev1alpha1.ToolSpec{
			Description: "mcp tool",
			MCP: &corev1alpha1.MCPToolServer{
				SubstrateActor: &corev1alpha1.SubstrateMCPActor{
					TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "template-a"},
				},
			},
		},
		Status: corev1alpha1.ToolStatus{
			Endpoint: "http://actor-a/mcp",
			Actor:    &corev1alpha1.ToolActorStatus{RouteHost: "actor-a.test"},
		},
	}
	first, err := approvalTargetSpecDigest(tool)
	if err != nil {
		t.Fatalf("approvalTargetSpecDigest() error = %v", err)
	}
	tool.Status.Endpoint = "http://actor-b/mcp"
	second, err := approvalTargetSpecDigest(tool)
	if err != nil {
		t.Fatalf("approvalTargetSpecDigest() second error = %v", err)
	}
	if first == second {
		t.Fatalf("MCP status endpoint change did not affect approval target digest")
	}
}

func approvalTestCustomTool(url string) *corev1alpha1.Tool {
	return &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{Name: gatedDispatchTool},
		Spec: corev1alpha1.ToolSpec{
			Description: "approval-gated custom tool",
			HTTP: &corev1alpha1.HTTPExecution{
				URL:    url,
				Method: http.MethodPost,
			},
		},
	}
}

func resolvedApprovalForTarget(target approvals.ApprovalTarget) approvals.ResolvedApproval {
	return approvals.ResolvedApproval{
		ID:               target.ApprovalID,
		TaskUID:          target.TaskUID,
		TargetTool:       target.TargetTool,
		TargetArgsDigest: target.TargetArgsDigest,
		TargetSpecDigest: target.TargetSpecDigest,
		Status:           approvals.StatusApproved,
	}
}

func setResolvedApprovalsEnv(t *testing.T, resolved []approvals.ResolvedApproval) {
	t.Helper()
	data, err := json.Marshal(resolved)
	if err != nil {
		t.Fatalf("marshal resolved approvals: %v", err)
	}
	t.Setenv(workerenv.ResolvedApprovals, string(data))
}

type recordingTool struct {
	name      string
	onExecute func(json.RawMessage)
}

func (t recordingTool) Name() string        { return t.name }
func (t recordingTool) Description() string { return "recording tool" }
func (t recordingTool) Parameters() json.RawMessage {
	return json.RawMessage(`{"type":"object","properties":{}}`)
}
func (t recordingTool) Execute(_ context.Context, args json.RawMessage) (string, error) {
	if t.onExecute != nil {
		t.onExecute(args)
	}
	return `{"ok":true}`, nil
}

func TestApprovalToolingRequiresAutonomousMode(t *testing.T) {
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	_, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{response: &llm.CompletionResponse{Content: doneResult, StopReason: "end_turn"}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err == nil || !strings.Contains(err.Error(), "autonomous coordination mode") {
		t.Fatalf("error = %v, want autonomous-mode configuration error", err)
	}
}

func TestRequestApprovalToolValidationErrorReturnsWholeBatchToModel(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "bad approval",
				ToolCalls: []llm.ToolCall{
					{
						ID:        "call-approval",
						Name:      "request_approval",
						Arguments: json.RawMessage(`{"action":"missing target"}`),
					},
					{
						ID:        "call-other",
						Name:      "other_tool",
						Arguments: json.RawMessage(`{"ok":true}`),
					},
				},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{"request_approval"}),
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want corrected", result)
	}
}

func TestRequestApprovalToolDuplicateTerminalDecisionReturnsWholeBatchToModel(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")
	customTool := approvalTestCustomTool("https://tools.example.test/dispatch")
	args := json.RawMessage(`{"incident":"inc-1"}`)
	specDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		specDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "duplicate approval",
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call-approval",
						Name: "request_approval",
						Arguments: json.RawMessage(
							`{"action":"dispatch","targetTool":"dispatch_work_order","targetArguments":{"incident":"inc-1"}}`,
						),
					},
					{ID: "call-other", Name: "other_tool", Arguments: json.RawMessage(`{"ok":true}`)},
				},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{"request_approval"}),
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want %s", result, correctedResult)
	}
}

func TestRequestApprovalToolCallEmitsCustomToolSpecDigest(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")
	recorder := common.NewFakeEventRecorder()
	customTool := approvalTestCustomTool("https://tools.example.test/dispatch")
	wantDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{response: &llm.CompletionResponse{
			Content: "requesting approval",
			ToolCalls: []llm.ToolCall{{
				ID:   "call-approval",
				Name: "request_approval",
				Arguments: json.RawMessage(
					`{"action":"dispatch team","targetTool":"dispatch_work_order","targetArguments":{"incident":"inc-1"}}`,
				),
			}},
			StopReason: "tool_use",
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{"request_approval"}),
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		nil,
		recorder,
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if !strings.Contains(result, "approval requested") {
		t.Fatalf("result = %q, want approval requested", result)
	}
	assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeApprovalRequested})
	var target approvals.ApprovalTarget
	for _, event := range recorder.Events() {
		if event.Type != events.ExecutionEventTypeApprovalRequested {
			continue
		}
		if err := json.Unmarshal(event.Content, &target); err != nil {
			t.Fatalf("approval content JSON: %v", err)
		}
	}
	if target.TargetSpecDigest != wantDigest {
		t.Fatalf("TargetSpecDigest = %q, want %q", target.TargetSpecDigest, wantDigest)
	}
}

func TestRequestApprovalToolCallStopsWorkerAfterEmitting(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	t.Setenv(workerenv.AutonomousMode, "true")
	recorder := common.NewFakeEventRecorder()
	customTool := approvalTestCustomTool("https://tools.example.test/dispatch")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{response: &llm.CompletionResponse{
			Content: "requesting approval",
			ToolCalls: []llm.ToolCall{{
				ID:   "call-approval",
				Name: "request_approval",
				Arguments: json.RawMessage(
					`{"action":"dispatch team","targetTool":"dispatch_work_order","targetArguments":{"incident":"inc-1"}}`,
				),
			}},
			StopReason: "tool_use",
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{"request_approval"}),
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		nil,
		recorder,
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if !strings.Contains(result, "approval requested") {
		t.Fatalf("result = %q, want approval requested", result)
	}
	assertRecordedEventTypesEventually(t, recorder, []string{events.ExecutionEventTypeApprovalRequested})
}

func TestApprovalGateDeclinedDecisionDoesNotExecuteAndContinues(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var executions atomic.Int32
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(json.RawMessage) {
			executions.Add(1)
		},
	})
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target := approvalTargetForTest(t, args)
	declined := resolvedApprovalForTarget(target)
	declined.Status = approvals.StatusDeclined
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{declined})
	t.Setenv(workerenv.ApprovalRequiredTools, gatedDispatchTool)
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{Content: declineHandledResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}),
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != declineHandledResult {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined approval executed gated tool")
	}
}

func TestApprovalGatePreScanStaleExplicitCustomToolSpecBlocksWholeBatch(t *testing.T) {
	var otherExecutions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/other" {
			otherExecutions.Add(1)
		}
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	oldTool := approvalTestCustomTool(toolServer.URL + "/old")
	oldSpecDigest, err := approvalTargetSpecDigest(oldTool)
	if err != nil {
		t.Fatalf("old target spec digest: %v", err)
	}
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		oldSpecDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
	t.Setenv(workerenv.AutonomousMode, "true")
	currentTool := approvalTestCustomTool(toolServer.URL + "/new")
	otherTool := approvalTestCustomTool(toolServer.URL + "/other")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{
					{ID: "call-other", Name: "other_tool", Arguments: json.RawMessage(`{"ok":true}`)},
					{ID: "call-1", Name: gatedDispatchTool, Arguments: args},
				},
				StopReason: "tool_use",
			},
			{Content: "spec change handled", StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: "other_tool"}, {Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: currentTool, "other_tool": otherTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != "spec change handled" {
		t.Fatalf("result = %q, want spec change handled", result)
	}
	if otherExecutions.Load() != 0 {
		t.Fatalf("earlier batch tool executed before stale explicit approval was handled")
	}
}

func TestApprovalGateStaleExplicitCustomToolSpecDoesNotExecuteWithoutRequiredTools(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	oldTool := approvalTestCustomTool(toolServer.URL + "/old")
	oldSpecDigest, err := approvalTargetSpecDigest(oldTool)
	if err != nil {
		t.Fatalf("old target spec digest: %v", err)
	}
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		oldSpecDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{resolvedApprovalForTarget(target)})
	t.Setenv(workerenv.AutonomousMode, "true")
	currentTool := approvalTestCustomTool(toolServer.URL + "/new")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{Content: "spec change handled", StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: currentTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != "spec change handled" {
		t.Fatalf("result = %q, want spec change handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("stale explicit custom approval target executed custom tool")
	}
}

func TestApprovalGateBlockingOverflowFailsClosedForUngatedCustomTool(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{approvals.BlockingOverflowResolvedApproval()})
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`{"incident":"inc-1"}`),
				}},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: approvalTestCustomTool(toolServer.URL)},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want corrected", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("custom tool executed despite blocking approval overflow")
	}
}

func TestApprovalGateBlockingOverflowAllowsMatchingApproval(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	customTool := approvalTestCustomTool(toolServer.URL)
	args := json.RawMessage(`{"incident":"inc-1"}`)
	specDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		specDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{
		approvals.BlockingOverflowResolvedApproval(),
		resolvedApprovalForTarget(target),
	})
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content:    "dispatching",
				ToolCalls:  []llm.ToolCall{{ID: "call-dispatch", Name: gatedDispatchTool, Arguments: args}},
				StopReason: "tool_use",
			},
			{Content: doneResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != doneResult {
		t.Fatalf("result = %q, want done", result)
	}
	if executions.Load() != 1 {
		t.Fatalf("custom tool executions = %d, want matching approval to execute", executions.Load())
	}
}

func TestApprovalGateDeclinedExplicitCustomToolTargetDoesNotExecuteWithoutRequiredTools(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	customTool := approvalTestCustomTool(toolServer.URL)
	specDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		args,
		"dispatch",
		"",
		"",
		specDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	declined := resolvedApprovalForTarget(target)
	declined.Status = approvals.StatusDeclined
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{declined})
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{Content: declineHandledResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != declineHandledResult {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined explicit custom approval target executed custom tool")
	}
}

func TestApprovalGateResolvedHistoryFailsClosedOnTargetBuildError(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	customTool := approvalTestCustomTool(toolServer.URL)
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		json.RawMessage(`{"incident":"inc-1"}`),
		"dispatch",
		"",
		"",
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	declined := resolvedApprovalForTarget(target)
	declined.Status = approvals.StatusDeclined
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{declined})
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`{"incident":"inc-1","__orkaApprovalURL":"x"}`),
				}},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want corrected", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("custom tool executed despite target build error with approval history")
	}
}

func TestApprovalGateBlockingOverflowFailsClosedOnTargetBuildError(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	customTool := approvalTestCustomTool(toolServer.URL)
	customTool.Spec.HTTP.Headers = map[string]string{"idempotency-key": "static"}
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{approvals.BlockingOverflowResolvedApproval()})
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-dispatch",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`{"incident":"inc-1"}`),
				}},
				StopReason: "tool_use",
			},
			{Content: correctedResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != correctedResult {
		t.Fatalf("result = %q, want corrected", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("custom tool executed despite overflow and target build error")
	}
}

func TestApprovalGateDeclinedURLInterpolatedCustomToolTargetDoesNotExecuteWithoutRequiredTools(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	customTool := approvalTestCustomTool(toolServer.URL + "/items/{{id}}")
	specDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}
	targetArgs, err := approvalTargetArguments(json.RawMessage(`{"id":1,"op":"delete"}`), customTool)
	if err != nil {
		t.Fatalf("target args: %v", err)
	}
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		targetArgs,
		"delete item",
		"",
		"",
		specDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	declined := resolvedApprovalForTarget(target)
	declined.Status = approvals.StatusDeclined
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{declined})
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "deleting item",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-delete",
					Name:      gatedDispatchTool,
					Arguments: json.RawMessage(`{"id":"1","op":"delete"}`),
				}},
				StopReason: "tool_use",
			},
			{Content: declineHandledResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "delete item"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != declineHandledResult {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined URL-interpolated approval target executed custom tool")
	}
}

func TestRequestApprovalBodyAuthTargetMatchesSanitizedDecline(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	toolspkg.DefaultRegistry.Register(toolspkg.NewRequestApprovalTool())
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	customTool := approvalTestCustomTool(toolServer.URL)
	customTool.Spec.HTTP.AuthInject = approvalAuthInjectBody
	customTool.Spec.HTTP.AuthBodyKey = approvalTestAuthKey
	customTool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	customTool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "uid-1",
		approvalAuthRefResourceVersionAnnotation: "10",
	}
	specDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		json.RawMessage(`{"incident":"inc-1"}`),
		"dispatch",
		"",
		"",
		specDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	declined := resolvedApprovalForTarget(target)
	declined.Status = approvals.StatusDeclined
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{declined})
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "requesting declined dispatch",
				ToolCalls: []llm.ToolCall{
					{
						ID:   "call-approval",
						Name: "request_approval",
						Arguments: json.RawMessage(fmt.Sprintf(
							`{"action":"dispatch","targetTool":"dispatch_work_order",`+
								`"targetArguments":{"incident":"inc-1",%q:"model-supplied"}}`,
							approvalTestAuthKey,
						)),
					},
					{
						ID:        "call-dispatch",
						Name:      gatedDispatchTool,
						Arguments: json.RawMessage(fmt.Sprintf(`{"incident":"inc-1",%q:"model-supplied"}`, approvalTestAuthKey)),
					},
				},
				StopReason: "tool_use",
			},
			{Content: declineHandledResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{"request_approval"}),
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != declineHandledResult {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined body-auth request_approval target executed custom tool")
	}
}

func TestApprovalGateDeclinedExplicitCustomToolBodyAuthTargetDoesNotExecuteWithoutRequiredTools(t *testing.T) {
	var executions atomic.Int32
	toolServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		executions.Add(1)
		fmt.Fprint(w, `{"ok":true}`) //nolint:errcheck
	}))
	defer toolServer.Close()
	customTool := approvalTestCustomTool(toolServer.URL)
	customTool.Spec.HTTP.AuthInject = approvalAuthInjectBody
	customTool.Spec.HTTP.AuthBodyKey = approvalTestAuthKey
	customTool.Spec.HTTP.AuthSecretRef = &corev1alpha1.SecretKeySelector{Name: "dispatch-auth", Key: "authref"}
	customTool.Annotations = map[string]string{
		approvalAuthRefUIDAnnotation:             "uid-1",
		approvalAuthRefResourceVersionAnnotation: "10",
	}
	specDigest, err := approvalTargetSpecDigest(customTool)
	if err != nil {
		t.Fatalf("target spec digest: %v", err)
	}
	approvedArgs := json.RawMessage(`{"incident":"inc-1"}`)
	retryArgs := json.RawMessage(fmt.Sprintf(`{"incident":"inc-1",%q:"model-supplied"}`, approvalTestAuthKey))
	target, err := approvals.NewApprovalTarget(
		"default",
		"incident-task",
		"task-uid-1",
		gatedDispatchTool,
		approvedArgs,
		"dispatch",
		"",
		"",
		specDigest,
	)
	if err != nil {
		t.Fatalf("approval target: %v", err)
	}
	declined := resolvedApprovalForTarget(target)
	declined.Status = approvals.StatusDeclined
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{declined})
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      gatedDispatchTool,
					Arguments: retryArgs,
				}},
				StopReason: "tool_use",
			},
			{Content: declineHandledResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		[]llm.Tool{{Name: gatedDispatchTool}},
		map[string]*corev1alpha1.Tool{gatedDispatchTool: customTool},
		worker.NewToolExecutor(),
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != declineHandledResult {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined body-auth approval target executed custom tool")
	}
}

func TestApprovalGateDeclinedExplicitApprovalTargetDoesNotExecuteWithoutRequiredTools(t *testing.T) {
	restore := replaceDefaultToolRegistryForTest(t)
	defer restore()
	var executions atomic.Int32
	toolspkg.DefaultRegistry.Register(recordingTool{
		name: gatedDispatchTool,
		onExecute: func(json.RawMessage) {
			executions.Add(1)
		},
	})
	args := json.RawMessage(`{"incident":"inc-1"}`)
	target := approvalTargetForTest(t, args)
	declined := resolvedApprovalForTarget(target)
	declined.Status = approvals.StatusDeclined
	setResolvedApprovalsEnv(t, []approvals.ResolvedApproval{declined})
	t.Setenv(workerenv.AutonomousMode, "true")

	result, err := executeAgentLoopWithEvents(
		context.Background(),
		&mockProvider{responses: []*llm.CompletionResponse{
			{
				Content: "dispatching",
				ToolCalls: []llm.ToolCall{{
					ID:        "call-1",
					Name:      gatedDispatchTool,
					Arguments: args,
				}},
				StopReason: "tool_use",
			},
			{Content: declineHandledResult, StopReason: "end_turn"},
		}},
		[]llm.Message{{Role: "user", Content: "handle incident"}},
		"",
		"test-model",
		toolspkg.DefaultRegistry.ToLLMTools([]string{gatedDispatchTool}),
		nil,
		nil,
		common.NewFakeEventRecorder(),
		&toolspkg.ToolContext{Namespace: "default", TaskID: "incident-task", TaskUID: "task-uid-1"},
	)
	if err != nil {
		t.Fatalf("executeAgentLoopWithEvents() error = %v", err)
	}
	if result != declineHandledResult {
		t.Fatalf("result = %q, want decline handled", result)
	}
	if executions.Load() != 0 {
		t.Fatalf("declined explicit approval target executed gated tool")
	}
}
