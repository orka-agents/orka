package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

func TestRequireToolApprovalPreservesDigestWhenSafeContentIsOversized(t *testing.T) {
	eventStore := store.NewFakeExecutionEventStore()
	req := toolApprovalRequest{
		Action:           createPullRequestApprovalAction,
		ToolName:         "create_pull_request",
		RiskSummary:      "create PR",
		SafeSummary:      "approval requested",
		Seed:             "oversized-seed",
		ApprovalTaskName: createPullRequestApprovalTaskName(),
		SafeContent:      map[string]any{"title": strings.Repeat("x", events.MaxExecutionEventContentJSONBytes)},
	}
	ctx := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, TaskID: createPullRequestApprovalTaskName(), ToolCallID: "tool-call-large", ExecutionEventStore: eventStore})
	check, err := requireToolApproval(ctx, req)
	if err != nil {
		t.Fatalf("requireToolApproval: %v", err)
	}
	var approvalResult approvalRequiredToolResult
	if err := json.Unmarshal([]byte(check.Result), &approvalResult); err != nil {
		t.Fatalf("unmarshal approval result: %v", err)
	}
	listed := listCreatePRApprovalEvents(t, eventStore, createPullRequestApprovalTaskName())
	if len(listed) != 1 {
		t.Fatalf("approval events = %#v, want one", listed)
	}
	var content map[string]any
	if err := json.Unmarshal(listed[0].Content, &content); err != nil {
		t.Fatalf("unmarshal approval content: %v", err)
	}
	if content["requestDigest"] == "" || content["safeContentTruncated"] != true {
		t.Fatalf("approval content = %#v, want digest with safeContentTruncated", content)
	}
	approvedContent, _ := json.Marshal(map[string]string{"approvalID": approvalResult.ApprovalID, "decision": "approve"})
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: defaultNamespace, StreamType: store.ExecutionEventStreamTypeTask, StreamID: createPullRequestApprovalTaskName(),
		TaskName: createPullRequestApprovalTaskName(), Type: events.ExecutionEventTypeApprovalApproved, ToolCallID: approvalResult.ApprovalID, Content: approvedContent,
	}); err != nil {
		t.Fatalf("append approval: %v", err)
	}
	approved, err := requireToolApproval(ctx, req)
	if err != nil {
		t.Fatalf("requireToolApproval approved: %v", err)
	}
	if !approved.Approved {
		t.Fatalf("approved check = %#v, want approved", approved)
	}
}

func TestCreatePullRequestTool_ApprovalRequiredDoesNotCreatePR(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"html_url":"https://github.com/sozercan/ayna/pull/42","number":42}`) //nolint:errcheck
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	task, secretObj := createPullRequestTestTaskAndSecret()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secretObj).Build()
	eventStore := store.NewFakeExecutionEventStore()
	tool := &CreatePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	args := createPullRequestApprovalTestArgs()
	argsJSON, _ := json.Marshal(args)
	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace:           defaultNamespace,
		TaskID:              createPullRequestApprovalTaskName(),
		ToolCallID:          "tool-call-1",
		ExecutionEventStore: eventStore,
	})

	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var approvalResult approvalRequiredToolResult
	if err := json.Unmarshal([]byte(result), &approvalResult); err != nil {
		t.Fatalf("unmarshal approval result: %v", err)
	}
	if approvalResult.ErrorType != approvalRequiredErrorType || approvalResult.ApprovalID == "" {
		t.Fatalf("approval result = %#v, want approval_required with id", approvalResult)
	}
	if requests != 0 {
		t.Fatalf("GitHub API requests = %d, want 0 before approval", requests)
	}

	approvalEvents := listCreatePRApprovalEvents(t, eventStore, createPullRequestApprovalTaskName())
	if len(approvalEvents) != 1 || approvalEvents[0].Type != events.ExecutionEventTypeApprovalRequested {
		t.Fatalf("approval events = %#v, want one ApprovalRequested", approvalEvents)
	}
	var content map[string]any
	if err := json.Unmarshal(approvalEvents[0].Content, &content); err != nil {
		t.Fatalf("unmarshal approval event content: %v", err)
	}
	if content["approvalID"] != approvalResult.ApprovalID || content["action"] != createPullRequestApprovalAction {
		t.Fatalf("approval content = %#v, want id/action", content)
	}
	if approvalEvents[0].ToolCallID != approvalResult.ApprovalID {
		t.Fatalf("approval event ToolCallID = %q, want stable approval id %q", approvalEvents[0].ToolCallID, approvalResult.ApprovalID)
	}
	if strings.Contains(string(approvalEvents[0].Content), "Implements #19") || strings.Contains(string(approvalEvents[0].Content), "unit-test-value") {
		t.Fatalf("approval event content leaked PR body or credential value: %s", approvalEvents[0].Content)
	}

	// Retrying the same tool call is idempotent while the approval remains pending:
	// it returns the same approval id and does not append a duplicate request.
	retryCtx := WithToolContext(context.Background(), &ToolContext{
		Namespace:           defaultNamespace,
		TaskID:              createPullRequestApprovalTaskName(),
		ToolCallID:          "tool-call-2",
		ExecutionEventStore: eventStore,
	})
	result, err = tool.Execute(retryCtx, argsJSON)
	if err != nil {
		t.Fatalf("Execute() retry error = %v", err)
	}
	var retryResult approvalRequiredToolResult
	if err := json.Unmarshal([]byte(result), &retryResult); err != nil {
		t.Fatalf("unmarshal retry approval result: %v", err)
	}
	if retryResult.ApprovalID != approvalResult.ApprovalID {
		t.Fatalf("retry approval id = %q, want %q", retryResult.ApprovalID, approvalResult.ApprovalID)
	}
	if got := len(listCreatePRApprovalEvents(t, eventStore, createPullRequestApprovalTaskName())); got != 1 {
		t.Fatalf("approval event count after retry = %d, want 1", got)
	}
}

func TestCreatePullRequestTool_ApprovedPathCreatesPR(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		w.WriteHeader(http.StatusCreated)
		fmt.Fprint(w, `{"html_url":"https://github.com/sozercan/ayna/pull/42","number":42}`) //nolint:errcheck
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	task, secretObj := createPullRequestTestTaskAndSecret()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secretObj).Build()
	eventStore := store.NewFakeExecutionEventStore()
	tool := &CreatePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	args := createPullRequestApprovalTestArgs()
	toolCallID := "tool-call-approved"
	approvalID := createPullRequestApprovalID(createPullRequestApprovalTaskName(), args, server.URL)
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalRequested, approvalID, args, server.URL)
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalApproved, approvalID, args, server.URL)

	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace:           defaultNamespace,
		TaskID:              createPullRequestApprovalTaskName(),
		ToolCallID:          toolCallID,
		ExecutionEventStore: eventStore,
	})
	argsJSON, _ := json.Marshal(args)
	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var prResult CreatePullRequestResult
	if err := json.Unmarshal([]byte(result), &prResult); err != nil {
		t.Fatalf("unmarshal PR result: %v", err)
	}
	if prResult.PRNumber != 42 || requests != 1 {
		t.Fatalf("PR result=%#v requests=%d, want PR #42 and one request", prResult, requests)
	}
}

func TestCreatePullRequestTool_DeclinedApprovalDoesNotCreatePR(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	task, secretObj := createPullRequestTestTaskAndSecret()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secretObj).Build()
	eventStore := store.NewFakeExecutionEventStore()
	tool := &CreatePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	args := createPullRequestApprovalTestArgs()
	toolCallID := "tool-call-declined"
	approvalID := createPullRequestApprovalID(createPullRequestApprovalTaskName(), args, server.URL)
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalRequested, approvalID, args, server.URL)
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalDeclined, approvalID, args, server.URL)

	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace:           defaultNamespace,
		TaskID:              createPullRequestApprovalTaskName(),
		ToolCallID:          toolCallID,
		ExecutionEventStore: eventStore,
	})
	argsJSON, _ := json.Marshal(args)
	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var approvalResult approvalRequiredToolResult
	if err := json.Unmarshal([]byte(result), &approvalResult); err != nil {
		t.Fatalf("unmarshal approval result: %v", err)
	}
	if approvalResult.ErrorType != approvalDeniedErrorType || approvalResult.Status != "declined" {
		t.Fatalf("approval result = %#v, want declined denial", approvalResult)
	}
	if requests != 0 {
		t.Fatalf("GitHub API requests = %d, want 0 after declined approval", requests)
	}
}

func TestCreatePullRequestTool_ExpiredApprovalCanBeReissued(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	task, secretObj := createPullRequestTestTaskAndSecret()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secretObj).Build()
	eventStore := store.NewFakeExecutionEventStore()
	tool := &CreatePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}
	args := createPullRequestApprovalTestArgs()
	approvalID := createPullRequestApprovalID(createPullRequestApprovalTaskName(), args, server.URL)
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalRequested, approvalID, args, server.URL)
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalExpired, approvalID, args, server.URL)
	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace: defaultNamespace, TaskID: createPullRequestApprovalTaskName(), ToolCallID: "expired-retry", ExecutionEventStore: eventStore,
	})
	argsJSON, _ := json.Marshal(args)
	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var approvalResult approvalRequiredToolResult
	if err := json.Unmarshal([]byte(result), &approvalResult); err != nil {
		t.Fatalf("unmarshal approval result: %v", err)
	}
	if approvalResult.ErrorType != approvalRequiredErrorType || approvalResult.ApprovalID == approvalID || requests != 0 {
		t.Fatalf("approval result=%#v requests=%d, want fresh approval id and no GitHub call", approvalResult, requests)
	}
	listed := listCreatePRApprovalEvents(t, eventStore, createPullRequestApprovalTaskName())
	if len(listed) != 3 || listed[2].Type != events.ExecutionEventTypeApprovalRequested {
		t.Fatalf("approval events=%#v, want reissued ApprovalRequested", listed)
	}
}

func TestCreatePullRequestTool_ProxyApprovalUsesTargetTaskWhenContextHasNoTaskID(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	task, secretObj := createPullRequestTestTaskAndSecret()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secretObj).Build()
	eventStore := store.NewFakeExecutionEventStore()
	tool := &CreatePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}
	args := createPullRequestApprovalTestArgs()
	ctx := WithToolContext(context.Background(), &ToolContext{Namespace: defaultNamespace, ExecutionEventStore: eventStore})
	argsJSON, _ := json.Marshal(args)
	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var approvalResult approvalRequiredToolResult
	if err := json.Unmarshal([]byte(result), &approvalResult); err != nil {
		t.Fatalf("unmarshal approval result: %v", err)
	}
	if approvalResult.ErrorType != approvalRequiredErrorType || approvalResult.TaskName != args.TaskName || requests != 0 {
		t.Fatalf("approval result=%#v requests=%d, want approval on target task and no GitHub call", approvalResult, requests)
	}
	listed := listCreatePRApprovalEvents(t, eventStore, args.TaskName)
	if len(listed) != 1 || listed[0].TaskName != args.TaskName {
		t.Fatalf("approval events=%#v, want event attached to target task", listed)
	}
}

func TestCreatePullRequestTool_ApprovalForDifferentRepositoryRequiresNewApproval(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	task, secretObj := createPullRequestTestTaskAndSecret()
	task.Spec.AgentRuntime.Workspace.GitRepo = "https://github.com/sozercan/other.git"
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secretObj).Build()
	eventStore := store.NewFakeExecutionEventStore()
	tool := &CreatePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	args := createPullRequestApprovalTestArgs()
	oldApprovalID := createPullRequestApprovalID(createPullRequestApprovalTaskName(), args, server.URL)
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalRequested, oldApprovalID, args, server.URL)
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalApproved, oldApprovalID, args, server.URL)

	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace:           defaultNamespace,
		TaskID:              createPullRequestApprovalTaskName(),
		ToolCallID:          "tool-call-repo-change",
		ExecutionEventStore: eventStore,
	})
	argsJSON, _ := json.Marshal(args)
	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var approvalResult approvalRequiredToolResult
	if err := json.Unmarshal([]byte(result), &approvalResult); err != nil {
		t.Fatalf("unmarshal approval result: %v", err)
	}
	if approvalResult.ErrorType != approvalRequiredErrorType || requests != 0 {
		t.Fatalf("approval result=%#v requests=%d, want fresh approval and no GitHub call", approvalResult, requests)
	}
	approvalEvents := listCreatePRApprovalEvents(t, eventStore, createPullRequestApprovalTaskName())
	if len(approvalEvents) != 3 {
		t.Fatalf("approval event count=%d, want old request+approval plus new request", len(approvalEvents))
	}
	if !strings.Contains(string(approvalEvents[2].Content), "sozercan/other") {
		t.Fatalf("new approval content = %s, want changed repository", approvalEvents[2].Content)
	}
}

func TestCreatePullRequestTool_ApprovedMismatchedRequestDoesNotCreatePR(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		w.WriteHeader(http.StatusCreated)
	}))
	defer server.Close()

	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	task, secretObj := createPullRequestTestTaskAndSecret()
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task, secretObj).Build()
	eventStore := store.NewFakeExecutionEventStore()
	tool := &CreatePullRequestTool{k8sClient: k8sClient, apiBaseURL: server.URL}

	args := createPullRequestApprovalTestArgs()
	approvalID := createPullRequestApprovalID(createPullRequestApprovalTaskName(), args, server.URL)
	content, _ := json.Marshal(map[string]string{
		"approvalID":    approvalID,
		"action":        createPullRequestApprovalAction,
		"tool":          createPullRequestToolName,
		"requestDigest": "mismatched",
	})
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  defaultNamespace,
		StreamType: events.ExecutionEventStreamTypeTask,
		StreamID:   createPullRequestApprovalTaskName(),
		TaskName:   createPullRequestApprovalTaskName(),
		ToolName:   createPullRequestToolName,
		Type:       events.ExecutionEventTypeApprovalRequested,
		Severity:   events.ExecutionEventSeverityInfo,
		Content:    content,
	}); err != nil {
		t.Fatalf("AppendExecutionEvent(request): %v", err)
	}
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalApproved, approvalID, args, server.URL)

	ctx := WithToolContext(context.Background(), &ToolContext{
		Namespace:           defaultNamespace,
		TaskID:              createPullRequestApprovalTaskName(),
		ToolCallID:          "tool-call-approved-mismatch",
		ExecutionEventStore: eventStore,
	})
	argsJSON, _ := json.Marshal(args)
	result, err := tool.Execute(ctx, argsJSON)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	var approvalResult approvalRequiredToolResult
	if err := json.Unmarshal([]byte(result), &approvalResult); err != nil {
		t.Fatalf("unmarshal approval result: %v", err)
	}
	if approvalResult.ErrorType != approvalDeniedErrorType || requests != 0 {
		t.Fatalf("approval result=%#v requests=%d, want denied without GitHub call", approvalResult, requests)
	}
}

func createPullRequestApprovalTaskName() string {
	return "orchestrator-task"
}

func createPullRequestApprovalID(taskName string, args CreatePullRequestArgs, baseURL string) string {
	target := createPullRequestApprovalTargetForTest(baseURL)
	return deterministicApprovalID(defaultNamespace, taskName, createPullRequestApprovalAction, createPullRequestApprovalSeed(args, target))
}

func createPullRequestApprovedContext(t *testing.T, args CreatePullRequestArgs, baseURL ...string) context.Context {
	t.Helper()
	eventStore := store.NewFakeExecutionEventStore()
	toolCallID := "approved-fixture"
	apiBaseURL := githubAPIBaseURL
	if len(baseURL) > 0 && strings.TrimSpace(baseURL[0]) != "" {
		apiBaseURL = strings.TrimSpace(baseURL[0])
	}
	approvalID := createPullRequestApprovalID(createPullRequestApprovalTaskName(), args, apiBaseURL)
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalRequested, approvalID, args, apiBaseURL)
	appendCreatePRApprovalEvent(t, eventStore, events.ExecutionEventTypeApprovalApproved, approvalID, args, apiBaseURL)
	return WithToolContext(context.Background(), &ToolContext{
		Namespace:           defaultNamespace,
		TaskID:              createPullRequestApprovalTaskName(),
		ToolCallID:          toolCallID,
		ExecutionEventStore: eventStore,
	})
}

func createPullRequestApprovalTestArgs() CreatePullRequestArgs {
	return CreatePullRequestArgs{
		TaskName:   testCoderTaskName,
		HeadBranch: testEditMessageBranch,
		BaseBranch: testBranch,
		Title:      testEditCommitTitle,
		Body:       "Implements #19",
	}
}

func createPullRequestTestTaskAndSecret() (*corev1alpha1.Task, *corev1.Secret) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: testCoderTaskName, Namespace: defaultNamespace},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				Workspace: &corev1alpha1.WorkspaceConfig{
					GitRepo: testSozercanAynaRepoURL,
					Branch:  testBranch,
					GitSecretRef: &corev1.LocalObjectReference{
						Name: testGitCredsSecretName,
					},
				},
			},
		},
	}
	secretObj := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: testGitCredsSecretName, Namespace: defaultNamespace},
		Data:       map[string][]byte{tokenKey: []byte("unit-test-value")},
	}
	return task, secretObj
}

func appendCreatePRApprovalEvent(t *testing.T, eventStore store.ExecutionEventStore, typ, approvalID string, args CreatePullRequestArgs, baseURL string) {
	t.Helper()
	target := createPullRequestApprovalTargetForTest(baseURL)
	request := toolApprovalRequest{
		Action:      createPullRequestApprovalAction,
		ToolName:    createPullRequestToolName,
		RiskSummary: createPullRequestRiskSummary(args, target),
		Seed:        createPullRequestApprovalSeed(args, target),
		SafeContent: map[string]any{
			"targetTaskName": args.TaskName,
			"headBranch":     args.HeadBranch,
			"baseBranch":     args.BaseBranch,
			"title":          args.Title,
			"repository":     target.Repository(),
			"apiBaseURL":     target.BaseURL,
		},
	}
	content, err := json.Marshal(map[string]string{
		"approvalID":    approvalID,
		"action":        createPullRequestApprovalAction,
		"tool":          createPullRequestToolName,
		"requestDigest": approvalRequestDigest(request),
	})
	if err != nil {
		t.Fatalf("marshal approval content: %v", err)
	}
	if _, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  defaultNamespace,
		StreamType: events.ExecutionEventStreamTypeTask,
		StreamID:   createPullRequestApprovalTaskName(),
		TaskName:   createPullRequestApprovalTaskName(),
		ToolName:   createPullRequestToolName,
		Type:       typ,
		Severity:   events.ExecutionEventSeverityInfo,
		Content:    content,
	}); err != nil {
		t.Fatalf("AppendExecutionEvent(%s): %v", typ, err)
	}
}

func createPullRequestApprovalTargetForTest(baseURL string) createPullRequestApprovalTarget {
	if strings.TrimSpace(baseURL) == "" {
		baseURL = githubAPIBaseURL
	}
	return createPullRequestApprovalTarget{Owner: "sozercan", Repo: "ayna", BaseURL: strings.TrimSpace(baseURL)}
}

func listCreatePRApprovalEvents(t *testing.T, eventStore store.ExecutionEventStore, taskName string) []store.ExecutionEvent {
	t.Helper()
	listed, err := listApprovalEvents(context.Background(), eventStore, defaultNamespace, taskName)
	if err != nil {
		t.Fatalf("list approval events: %v", err)
	}
	return listed
}
