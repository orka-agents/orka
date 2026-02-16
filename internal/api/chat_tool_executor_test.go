/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/controller"
	"github.com/sozercan/orka/internal/llm"
	"github.com/sozercan/orka/internal/store"
)

// --- test helpers ---

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	return s
}

func newTestExecutor(objs ...runtime.Object) *ToolExecutor {
	scheme := testScheme()
	cb := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		cb = cb.WithRuntimeObjects(o)
	}
	c := cb.Build()
	sm := controller.NewSessionManager(&fakeSessionStore{})
	return NewToolExecutor(c, sm, "default", "sess-12345678", "", false, 5, 30*time.Second, &fakeResultStore{})
}

// fakeResultStore implements store.ResultStore for testing.
type fakeResultStore struct {
	data     map[string][]byte
	errOnGet error
}

func (f *fakeResultStore) SaveResult(_ context.Context, namespace, taskName string, data []byte) error {
	if f.data == nil {
		f.data = make(map[string][]byte)
	}
	f.data[namespace+"/"+taskName] = data
	return nil
}

func (f *fakeResultStore) GetResult(_ context.Context, namespace, taskName string) ([]byte, error) {
	if f.errOnGet != nil {
		return nil, f.errOnGet
	}
	if f.data == nil {
		return nil, store.ErrNotFound
	}
	d, ok := f.data[namespace+"/"+taskName]
	if !ok {
		return nil, store.ErrNotFound
	}
	return d, nil
}

func (f *fakeResultStore) DeleteResult(_ context.Context, _, _ string) error {
	return nil
}

// fakeSessionStore implements store.SessionStore for testing.
type fakeSessionStore struct {
	deleted  map[string]bool
	errOnDel error
}

func (f *fakeSessionStore) CreateSession(_ context.Context, _ *store.SessionRecord) error { return nil }
func (f *fakeSessionStore) GetSession(_ context.Context, _, _ string) (*store.SessionRecord, error) {
	return &store.SessionRecord{}, nil
}
func (f *fakeSessionStore) ListSessions(_ context.Context, _ string) ([]store.SessionMetadata, error) {
	return nil, nil
}
func (f *fakeSessionStore) DeleteSession(_ context.Context, ns, name string) error {
	if f.errOnDel != nil {
		return f.errOnDel
	}
	if f.deleted == nil {
		f.deleted = make(map[string]bool)
	}
	f.deleted[ns+"/"+name] = true
	return nil
}
func (f *fakeSessionStore) AcquireLock(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeSessionStore) ReleaseLock(_ context.Context, _, _, _ string) error { return nil }
func (f *fakeSessionStore) IsLocked(_ context.Context, _, _, _ string) (bool, error) {
	return false, nil
}
func (f *fakeSessionStore) AppendMessages(_ context.Context, _, _ string, _ []store.SessionMessage) error {
	return nil
}
func (f *fakeSessionStore) LoadTranscript(_ context.Context, _, _ string, _ int) ([]store.SessionMessage, error) {
	return nil, nil
}
func (f *fakeSessionStore) UpdateTokenCounts(_ context.Context, _, _ string, _, _ int) error {
	return nil
}

func mustJSON(v map[string]any) json.RawMessage {
	b, _ := json.Marshal(v)
	return b
}

// --- Tests: helper functions ---

func TestGetStringArg(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		key  string
		want string
	}{
		{"present string", map[string]any{"k": "val"}, "k", "val"},
		{"missing key", map[string]any{}, "k", ""},
		{"non-string value", map[string]any{"k": 42}, "k", "42"},
		{"bool value", map[string]any{"k": true}, "k", "true"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getStringArg(tt.args, tt.key)
			if got != tt.want {
				t.Errorf("getStringArg() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetStringArgDefault(t *testing.T) {
	tests := []struct {
		name       string
		args       map[string]any
		key        string
		defaultVal string
		want       string
	}{
		{"present", map[string]any{"k": "val"}, "k", "def", "val"},
		{"missing uses default", map[string]any{}, "k", "def", "def"},
		{"empty string uses default", map[string]any{"k": ""}, "k", "def", "def"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getStringArgDefault(tt.args, tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("getStringArgDefault() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestGetIntArg(t *testing.T) {
	tests := []struct {
		name       string
		args       map[string]any
		key        string
		defaultVal int
		want       int
	}{
		{"float64 value", map[string]any{"k": float64(42)}, "k", 0, 42},
		{"int value", map[string]any{"k": int(7)}, "k", 0, 7},
		{"int64 value", map[string]any{"k": int64(99)}, "k", 0, 99},
		{"missing uses default", map[string]any{}, "k", 10, 10},
		{"string falls back to default", map[string]any{"k": "abc"}, "k", 5, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getIntArg(tt.args, tt.key, tt.defaultVal)
			if got != tt.want {
				t.Errorf("getIntArg() = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestGetStringSliceArg(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		key  string
		want []string
	}{
		{"string slice", map[string]any{"k": []any{"a", "b"}}, "k", []string{"a", "b"}},
		{"missing key", map[string]any{}, "k", nil},
		{"non-slice value", map[string]any{"k": "scalar"}, "k", nil},
		{"mixed types", map[string]any{"k": []any{"a", 42}}, "k", []string{"a", "42"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := getStringSliceArg(tt.args, tt.key)
			if tt.want == nil {
				if got != nil {
					t.Errorf("getStringSliceArg() = %v, want nil", got)
				}
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("getStringSliceArg() len = %d, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("getStringSliceArg()[%d] = %q, want %q", i, got[i], tt.want[i])
				}
			}
		})
	}
}

func TestToolError(t *testing.T) {
	r := toolError("my_type", "something failed", "try again")
	if r.Success {
		t.Fatal("expected Success=false")
	}
	if r.ErrorType != "my_type" {
		t.Errorf("ErrorType = %q, want %q", r.ErrorType, "my_type")
	}
	if r.Error != "something failed" {
		t.Errorf("Error = %q, want %q", r.Error, "something failed")
	}
	if r.Suggestion != "try again" {
		t.Errorf("Suggestion = %q, want %q", r.Suggestion, "try again")
	}
}

func TestClassifyK8sError(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		wantType string
	}{
		{
			"not found",
			apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "tasks"}, "my-task"),
			"not_found",
		},
		{
			"already exists",
			apierrors.NewAlreadyExists(schema.GroupResource{Group: "", Resource: "tasks"}, "my-task"),
			"already_exists",
		},
		{
			"forbidden",
			apierrors.NewForbidden(schema.GroupResource{Group: "", Resource: "tasks"}, "my-task", fmt.Errorf("denied")),
			"permission_denied",
		},
		{
			"generic error",
			fmt.Errorf("some random error"),
			"internal_error",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := classifyK8sError(tt.err)
			if r.Success {
				t.Fatal("expected Success=false")
			}
			if r.ErrorType != tt.wantType {
				t.Errorf("ErrorType = %q, want %q", r.ErrorType, tt.wantType)
			}
		})
	}
}

func TestMarshalResult(t *testing.T) {
	t.Run("success", func(t *testing.T) {
		r := ToolResult{Success: true, Data: "hello"}
		s, err := marshalResult(r)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(s, `"success":true`) {
			t.Errorf("marshalResult() = %s, expected success:true", s)
		}
	})
	t.Run("error result", func(t *testing.T) {
		r := toolError("test", "msg", "sug")
		s, err := marshalResult(r)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(s, `"success":false`) {
			t.Errorf("marshalResult() = %s, expected success:false", s)
		}
	})
}

// --- Tests: ToolExecutor construction & basic methods ---

func TestNewToolExecutor(t *testing.T) {
	e := newTestExecutor()
	if e == nil {
		t.Fatal("expected non-nil executor")
	}
	if e.namespace != "default" {
		t.Errorf("namespace = %q, want %q", e.namespace, "default")
	}
	if e.sessionID != "sess-12345678" {
		t.Errorf("sessionID = %q, want %q", e.sessionID, "sess-12345678")
	}
	if e.maxTasks != 5 {
		t.Errorf("maxTasks = %d, want %d", e.maxTasks, 5)
	}
}

func TestGenerateTaskName(t *testing.T) {
	e := newTestExecutor()
	name1 := e.generateTaskName()
	name2 := e.generateTaskName()

	if !strings.HasPrefix(name1, "chat-sess-123-") {
		t.Errorf("name1 = %q, expected prefix chat-sess-123-", name1)
	}
	if name1 == name2 {
		t.Errorf("expected unique names, got %q twice", name1)
	}
}

func TestTaskLabels(t *testing.T) {
	e := newTestExecutor()
	labels := e.taskLabels()
	if labels["orka.ai/created-by"] != "orchestrator" {
		t.Errorf("created-by label = %q", labels["orka.ai/created-by"])
	}
	if labels["orka.ai/chat-session"] != "sess-12345678" {
		t.Errorf("chat-session label = %q", labels["orka.ai/chat-session"])
	}
}

func TestCheckTaskLimit(t *testing.T) {
	e := newTestExecutor()
	e.maxTasks = 2
	e.tasksCreated = 0

	if r := e.checkTaskLimit(); r != nil {
		t.Fatalf("expected nil when under limit, got %+v", r)
	}

	e.tasksCreated = 2
	r := e.checkTaskLimit()
	if r == nil {
		t.Fatal("expected non-nil when at limit")
	}
	if r.ErrorType != "limit_reached" {
		t.Errorf("ErrorType = %q, want limit_reached", r.ErrorType)
	}
}

func TestCheckNamespaceScope(t *testing.T) {
	tests := []struct {
		name      string
		watchNS   string
		enforceNS bool
		execNS    string
		targetNS  string
		expectErr bool
	}{
		{"allowed - same namespace", "", false, "default", "default", false},
		{"allowed - no restrictions", "", false, "default", "other", false},
		{"watchNamespace mismatch", "prod", false, "default", "dev", true},
		{"watchNamespace match", "prod", false, "default", "prod", false},
		{"enforce isolation mismatch", "", true, "default", "other", true},
		{"enforce isolation match", "", true, "default", "default", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestExecutor()
			e.watchNamespace = tt.watchNS
			e.enforceNamespaceIsolation = tt.enforceNS
			e.namespace = tt.execNS
			r := e.checkNamespaceScope(tt.targetNS)
			if tt.expectErr && r == nil {
				t.Fatal("expected error result")
			}
			if !tt.expectErr && r != nil {
				t.Fatalf("expected nil, got %+v", r)
			}
		})
	}
}

// --- Tests: Execute dispatch ---

func TestExecute_InvalidJSON(t *testing.T) {
	e := newTestExecutor()
	tc := llm.ToolCall{
		ID:        "1",
		Name:      "create_ai_task",
		Arguments: json.RawMessage(`{invalid`),
	}
	result, err := e.Execute(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "invalid_arguments") {
		t.Errorf("expected invalid_arguments error, got: %s", result)
	}
}

func TestExecute_UnknownTool(t *testing.T) {
	e := newTestExecutor()
	tc := llm.ToolCall{
		ID:        "1",
		Name:      "nonexistent_tool",
		Arguments: mustJSON(map[string]any{}),
	}
	result, err := e.Execute(context.Background(), tc)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result, "unknown_tool") {
		t.Errorf("expected unknown_tool error, got: %s", result)
	}
}

func TestExecute_Dispatch(t *testing.T) {
	tests := []struct {
		name     string
		toolName string
		args     map[string]any
		contains string
	}{
		{"create_ai_task missing prompt", "create_ai_task", map[string]any{}, "prompt is required"},
		{"create_agent_task missing prompt", "create_agent_task", map[string]any{}, "prompt is required"},
		{"create_agent_task missing agentRef", "create_agent_task", map[string]any{"prompt": "test"}, "agentRef is required"},
		{"check_task_progress missing name", "check_task_progress", map[string]any{}, "name is required"},
		{"fetch_task_output missing name", "fetch_task_output", map[string]any{}, "name is required"},
		{"wait_for_task missing name", "wait_for_task", map[string]any{}, "name is required"},
		{"cancel_task missing name", "cancel_task", map[string]any{}, "name is required"},
		{"create_agent missing name", "create_agent", map[string]any{}, "name is required"},
		{"update_agent missing name", "update_agent", map[string]any{}, "name is required"},
		{"delete_agent missing name", "delete_agent", map[string]any{}, "name is required"},
		{"create_tool missing name", "create_tool", map[string]any{}, "name is required"},
		{"create_tool missing desc", "create_tool", map[string]any{"name": "t1"}, "description is required"},
		{"create_tool missing url", "create_tool", map[string]any{"name": "t1", "description": "d"}, "url is required"},
		{"delete_tool missing name", "delete_tool", map[string]any{}, "name is required"},
		{"delete_session missing id", "delete_session", map[string]any{}, "sessionId is required"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestExecutor()
			tc := llm.ToolCall{
				ID:        "1",
				Name:      tt.toolName,
				Arguments: mustJSON(tt.args),
			}
			result, err := e.Execute(context.Background(), tc)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(result, tt.contains) {
				t.Errorf("expected %q in result, got: %s", tt.contains, result)
			}
		})
	}
}

// --- Tests: executeCreateAITask ---

func TestExecuteCreateAITask_Success(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"prompt": "summarize this",
	}
	r := e.executeCreateAITask(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if data["phase"] != "Pending" {
		t.Errorf("phase = %v, want Pending", data["phase"])
	}
	if data["message"] != taskCreatedMsg {
		t.Errorf("message = %v", data["message"])
	}
}

func TestExecuteCreateAITask_WithSchedule(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"prompt":   "do something",
		"schedule": "*/5 * * * *",
	}
	r := e.executeCreateAITask(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if !strings.Contains(data["message"].(string), "Recurring task scheduled") {
		t.Errorf("message = %v, expected scheduled message", data["message"])
	}
}

func TestExecuteCreateAITask_InvalidTimeout(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"prompt":  "test",
		"timeout": "not-a-duration",
	}
	r := e.executeCreateAITask(context.Background(), args)
	if r.Success {
		t.Fatal("expected failure for invalid timeout")
	}
	if r.ErrorType != "invalid_arguments" {
		t.Errorf("ErrorType = %q", r.ErrorType)
	}
}

func TestExecuteCreateAITask_TaskLimit(t *testing.T) {
	e := newTestExecutor()
	e.maxTasks = 0
	r := e.executeCreateAITask(context.Background(), map[string]any{"prompt": "test"})
	if r.Success {
		t.Fatal("expected failure due to task limit")
	}
	if r.ErrorType != "limit_reached" {
		t.Errorf("ErrorType = %q", r.ErrorType)
	}
}

func TestExecuteCreateAITask_WithOptionalFields(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"prompt":      "test",
		"agentRef":    "my-agent",
		"providerRef": "my-provider",
		"timeout":     "5m",
		"priority":    float64(10),
		"sessionRef":  "my-session",
	}
	r := e.executeCreateAITask(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}
}

// --- Tests: executeCreateContainerTask ---

func TestExecuteCreateContainerTask_Success(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"image":   "alpine:latest",
		"command": []any{"echo", "hello"},
		"args":    []any{"world"},
	}
	r := e.executeCreateContainerTask(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if data["phase"] != "Pending" {
		t.Errorf("phase = %v", data["phase"])
	}
}

func TestExecuteCreateContainerTask_TaskLimit(t *testing.T) {
	e := newTestExecutor()
	e.maxTasks = 0
	r := e.executeCreateContainerTask(context.Background(), map[string]any{"image": "alpine"})
	if r.Success {
		t.Fatal("expected failure")
	}
}

func TestExecuteCreateContainerTask_WithSchedule(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"image":    "alpine",
		"schedule": "0 * * * *",
	}
	r := e.executeCreateContainerTask(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if !strings.Contains(data["message"].(string), "Recurring") {
		t.Errorf("expected recurring message, got %v", data["message"])
	}
}

func TestExecuteCreateContainerTask_InvalidTimeout(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"image":   "alpine",
		"timeout": "bad",
	}
	r := e.executeCreateContainerTask(context.Background(), args)
	if r.Success {
		t.Fatal("expected failure for invalid timeout")
	}
}

// --- Tests: executeCreateAgentTask ---

func TestExecuteCreateAgentTask_Success(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"prompt":   "do work",
		"agentRef": "my-agent",
	}
	r := e.executeCreateAgentTask(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}
}

func TestExecuteCreateAgentTask_MissingPrompt(t *testing.T) {
	e := newTestExecutor()
	r := e.executeCreateAgentTask(context.Background(), map[string]any{"agentRef": "a"})
	if r.Success {
		t.Fatal("expected failure")
	}
}

func TestExecuteCreateAgentTask_MissingAgentRef(t *testing.T) {
	e := newTestExecutor()
	r := e.executeCreateAgentTask(context.Background(), map[string]any{"prompt": "test"})
	if r.Success {
		t.Fatal("expected failure")
	}
}

func TestExecuteCreateAgentTask_WithWorkspace(t *testing.T) {
	// Seed a git secret
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "github-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"token": []byte("ghp_test"),
		},
	}
	e := newTestExecutor(secret)
	args := map[string]any{
		"prompt":   "work on repo",
		"agentRef": "my-agent",
		"workspace": map[string]any{
			"gitRepo":    "https://github.com/org/repo",
			"branch":     "main",
			"subPath":    "src",
			"pushBranch": "feature-1",
		},
		"maxTurns": float64(10),
	}
	r := e.executeCreateAgentTask(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}
}

func TestExecuteCreateAgentTask_WithExplicitGitSecret(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"prompt":   "work on repo",
		"agentRef": "my-agent",
		"workspace": map[string]any{
			"gitRepo":      "https://github.com/org/repo",
			"gitSecretRef": "my-secret",
		},
	}
	r := e.executeCreateAgentTask(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}
}

// --- Tests: executeCheckTaskProgress ---

func TestExecuteCheckTaskProgress_Success(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-task",
			Namespace: "default",
		},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseRunning,
			Message: "running",
		},
	}
	e := newTestExecutor(task)
	r := e.executeCheckTaskProgress(context.Background(), map[string]any{"name": "my-task"})
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if data["name"] != "my-task" {
		t.Errorf("name = %v", data["name"])
	}
}

func TestExecuteCheckTaskProgress_NotFound(t *testing.T) {
	e := newTestExecutor()
	r := e.executeCheckTaskProgress(context.Background(), map[string]any{"name": "nonexistent"})
	if r.Success {
		t.Fatal("expected failure")
	}
	if r.ErrorType != "not_found" {
		t.Errorf("ErrorType = %q", r.ErrorType)
	}
}

func TestExecuteCheckTaskProgress_WithConditions(t *testing.T) {
	now := metav1.Now()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cond-task",
			Namespace: "default",
		},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseRunning,
			StartTime: &now,
			Conditions: []metav1.Condition{
				{Type: "Ready", Status: "True", Reason: "Scheduled", Message: "ok"},
			},
		},
	}
	e := newTestExecutor(task)
	r := e.executeCheckTaskProgress(context.Background(), map[string]any{"name": "cond-task"})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if _, ok := data["duration"]; !ok {
		t.Error("expected duration field")
	}
	if _, ok := data["conditions"]; !ok {
		t.Error("expected conditions field")
	}
}

// --- Tests: executeFetchTaskOutput ---

func TestExecuteFetchTaskOutput_NoResult(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "no-result",
			Namespace: "default",
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
		},
	}
	e := newTestExecutor(task)
	r := e.executeFetchTaskOutput(context.Background(), map[string]any{"name": "no-result"})
	if r.Success {
		t.Fatal("expected failure - no result available")
	}
}

func TestExecuteFetchTaskOutput_Success(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "done-task",
			Namespace: "default",
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}
	rs := &fakeResultStore{
		data: map[string][]byte{
			"default/done-task": []byte("result output here"),
		},
	}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()
	sm := controller.NewSessionManager(&fakeSessionStore{})
	e := NewToolExecutor(c, sm, "default", "sess-123", "", false, 5, 30*time.Second, rs)

	r := e.executeFetchTaskOutput(context.Background(), map[string]any{"name": "done-task"})
	if !r.Success {
		t.Fatalf("expected success, got error: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if data["output"] != "result output here" {
		t.Errorf("output = %v", data["output"])
	}
}

func TestExecuteFetchTaskOutput_StoreNotFound(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "store-miss",
			Namespace: "default",
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}
	rs := &fakeResultStore{} // empty store
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()
	sm := controller.NewSessionManager(&fakeSessionStore{})
	e := NewToolExecutor(c, sm, "default", "sess-123", "", false, 5, 30*time.Second, rs)

	r := e.executeFetchTaskOutput(context.Background(), map[string]any{"name": "store-miss"})
	if r.Success {
		t.Fatal("expected failure")
	}
	if r.ErrorType != "not_found" {
		t.Errorf("ErrorType = %q, want not_found", r.ErrorType)
	}
}

func TestExecuteFetchTaskOutput_StoreError(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "store-err",
			Namespace: "default",
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{
				Available: true,
			},
		},
	}
	rs := &fakeResultStore{errOnGet: errors.New("db error")}
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()
	sm := controller.NewSessionManager(&fakeSessionStore{})
	e := NewToolExecutor(c, sm, "default", "sess-123", "", false, 5, 30*time.Second, rs)

	r := e.executeFetchTaskOutput(context.Background(), map[string]any{"name": "store-err"})
	if r.Success {
		t.Fatal("expected failure")
	}
	if r.ErrorType != "internal_error" {
		t.Errorf("ErrorType = %q, want internal_error", r.ErrorType)
	}
}

// --- Tests: executeWaitForTask ---

func TestExecuteWaitForTask_AlreadyComplete(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "done",
			Namespace: "default",
		},
		Status: corev1alpha1.TaskStatus{
			Phase:   corev1alpha1.TaskPhaseSucceeded,
			Message: "done",
		},
	}
	e := newTestExecutor(task)
	r := e.executeWaitForTask(context.Background(), map[string]any{"name": "done", "timeout": float64(1)})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteWaitForTask_NotFound(t *testing.T) {
	e := newTestExecutor()
	r := e.executeWaitForTask(context.Background(), map[string]any{"name": "missing"})
	if r.Success {
		t.Fatal("expected failure")
	}
}

// --- Tests: executeCancelTask ---

func TestExecuteCancelTask_Success(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cancel-me",
			Namespace: "default",
		},
	}
	e := newTestExecutor(task)
	r := e.executeCancelTask(context.Background(), map[string]any{"name": "cancel-me"})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if !strings.Contains(data["message"].(string), "cancelled") {
		t.Errorf("message = %v", data["message"])
	}
}

func TestExecuteCancelTask_NotFound(t *testing.T) {
	e := newTestExecutor()
	r := e.executeCancelTask(context.Background(), map[string]any{"name": "nope"})
	if r.Success {
		t.Fatal("expected failure")
	}
}

// --- Tests: executeListAgents ---

func TestExecuteListAgents_Empty(t *testing.T) {
	e := newTestExecutor()
	r := e.executeListAgents(context.Background(), map[string]any{})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteListAgents_WithAgents(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-agent",
			Namespace: "default",
			Annotations: map[string]string{
				"description": "A test agent",
			},
		},
		Spec: corev1alpha1.AgentSpec{
			Model: &corev1alpha1.ModelConfig{
				Provider: "openai",
				Name:     "gpt-4",
			},
			Tools: []corev1alpha1.ToolReference{
				{Name: "tool1"},
			},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type: "copilot",
			},
		},
	}
	e := newTestExecutor(agent)
	r := e.executeListAgents(context.Background(), map[string]any{})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
	agents := r.Data.([]map[string]any)
	if len(agents) != 1 {
		t.Fatalf("expected 1 agent, got %d", len(agents))
	}
	if agents[0]["name"] != "test-agent" {
		t.Errorf("name = %v", agents[0]["name"])
	}
	if agents[0]["description"] != "A test agent" {
		t.Errorf("description = %v", agents[0]["description"])
	}
}

// --- Tests: executeListTools ---

func TestExecuteListTools_Empty(t *testing.T) {
	e := newTestExecutor()
	r := e.executeListTools(context.Background(), map[string]any{})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteListTools_WithTools(t *testing.T) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "my-tool",
			Namespace: "default",
		},
		Spec: corev1alpha1.ToolSpec{
			Description: "does stuff",
		},
	}
	e := newTestExecutor(tool)
	r := e.executeListTools(context.Background(), map[string]any{})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
	tools := r.Data.([]map[string]any)
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %d", len(tools))
	}
}

// --- Tests: executeListTasks ---

func TestExecuteListTasks_Empty(t *testing.T) {
	e := newTestExecutor()
	r := e.executeListTasks(context.Background(), map[string]any{})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteListTasks_WithFilter(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "t1",
			Namespace:         "default",
			CreationTimestamp: metav1.Now(),
		},
		Status: corev1alpha1.TaskStatus{
			Phase: corev1alpha1.TaskPhaseSucceeded,
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI,
		},
	}
	e := newTestExecutor(task)

	// Filter matching
	r := e.executeListTasks(context.Background(), map[string]any{"status": "Succeeded"})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
	tasks := r.Data.([]map[string]any)
	if len(tasks) != 1 {
		t.Errorf("expected 1 task, got %d", len(tasks))
	}

	// Filter non-matching
	r2 := e.executeListTasks(context.Background(), map[string]any{"status": "Failed"})
	if !r2.Success {
		t.Fatalf("expected success: %s", r2.Error)
	}
	tasks2 := r2.Data.([]map[string]any)
	if len(tasks2) != 0 {
		t.Errorf("expected 0 tasks, got %d", len(tasks2))
	}
}

// --- Tests: executeCreateAgent ---

func TestExecuteCreateAgent_Success(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"name": "new-agent",
	}
	r := e.executeCreateAgent(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if data["name"] != "new-agent" {
		t.Errorf("name = %v", data["name"])
	}
}

func TestExecuteCreateAgent_WithModel(t *testing.T) {
	tests := []struct {
		name  string
		model any
	}{
		{"map model", map[string]any{"name": "gpt-4", "provider": "openai", "temperature": float64(0.7)}},
		{"string model with provider", "openai/gpt-4"},
		{"string model without provider", "gpt-4"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := newTestExecutor()
			args := map[string]any{
				"name":  "agent-" + tt.name,
				"model": tt.model,
			}
			r := e.executeCreateAgent(context.Background(), args)
			if !r.Success {
				t.Fatalf("expected success: %s", r.Error)
			}
		})
	}
}

func TestExecuteCreateAgent_WithSystemPromptAndTools(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"name":         "agent-full",
		"systemPrompt": "You are helpful",
		"tools":        []any{"tool1", "tool2"},
	}
	r := e.executeCreateAgent(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteCreateAgent_WithRuntime(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"name": "runtime-agent",
		"runtime": map[string]any{
			"type": "copilot",
		},
	}
	r := e.executeCreateAgent(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteCreateAgent_WithCoordination(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"name": "coord-agent",
		"coordination": map[string]any{
			"enabled":               true,
			"maxConcurrentChildren": float64(3),
			"maxDepth":              float64(2),
			"allowedAgents": []any{
				map[string]any{"name": "helper", "namespace": "default"},
			},
		},
	}
	r := e.executeCreateAgent(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteCreateAgent_NamespaceRestriction(t *testing.T) {
	e := newTestExecutor()
	e.enforceNamespaceIsolation = true
	args := map[string]any{
		"name":      "bad-ns",
		"namespace": "other-ns",
	}
	r := e.executeCreateAgent(context.Background(), args)
	if r.Success {
		t.Fatal("expected failure due to namespace restriction")
	}
	if r.ErrorType != "permission_denied" {
		t.Errorf("ErrorType = %q", r.ErrorType)
	}
}

func TestExecuteCreateAgent_WithInitialPrompt(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"name":          "init-agent",
		"initialPrompt": "do initial work",
	}
	r := e.executeCreateAgent(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if data["message"] != "Agent created and task started" {
		t.Errorf("message = %v", data["message"])
	}
}

// --- Tests: executeUpdateAgent ---

func TestExecuteUpdateAgent_Success(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "upd-agent",
			Namespace: "default",
		},
		Spec: corev1alpha1.AgentSpec{},
	}
	e := newTestExecutor(agent)
	args := map[string]any{
		"name":         "upd-agent",
		"model":        "openai/gpt-4o",
		"systemPrompt": "Be concise",
		"tools":        []any{"search"},
	}
	r := e.executeUpdateAgent(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteUpdateAgent_NotFound(t *testing.T) {
	e := newTestExecutor()
	r := e.executeUpdateAgent(context.Background(), map[string]any{"name": "missing"})
	if r.Success {
		t.Fatal("expected failure")
	}
}

// --- Tests: executeDeleteAgent ---

func TestExecuteDeleteAgent_Success(t *testing.T) {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "del-agent",
			Namespace: "default",
		},
	}
	e := newTestExecutor(agent)
	r := e.executeDeleteAgent(context.Background(), map[string]any{"name": "del-agent"})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteDeleteAgent_NotFound(t *testing.T) {
	e := newTestExecutor()
	r := e.executeDeleteAgent(context.Background(), map[string]any{"name": "missing"})
	if r.Success {
		t.Fatal("expected failure")
	}
}

// --- Tests: executeCreateTool ---

func TestExecuteCreateTool_Success(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"name":        "my-tool",
		"description": "does stuff",
		"url":         "https://example.com/api",
		"method":      "GET",
	}
	r := e.executeCreateTool(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteCreateTool_DefaultMethod(t *testing.T) {
	e := newTestExecutor()
	args := map[string]any{
		"name":        "my-tool2",
		"description": "does more stuff",
		"url":         "https://example.com/api",
	}
	r := e.executeCreateTool(context.Background(), args)
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteCreateTool_NamespaceRestriction(t *testing.T) {
	e := newTestExecutor()
	e.enforceNamespaceIsolation = true
	args := map[string]any{
		"name":        "t",
		"description": "d",
		"url":         "http://x",
		"namespace":   "other",
	}
	r := e.executeCreateTool(context.Background(), args)
	if r.Success {
		t.Fatal("expected failure")
	}
}

// --- Tests: executeDeleteTool ---

func TestExecuteDeleteTool_Success(t *testing.T) {
	tool := &corev1alpha1.Tool{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "del-tool",
			Namespace: "default",
		},
	}
	e := newTestExecutor(tool)
	r := e.executeDeleteTool(context.Background(), map[string]any{"name": "del-tool"})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
}

func TestExecuteDeleteTool_NotFound(t *testing.T) {
	e := newTestExecutor()
	r := e.executeDeleteTool(context.Background(), map[string]any{"name": "missing"})
	if r.Success {
		t.Fatal("expected failure")
	}
}

// --- Tests: executeDeleteSession ---

func TestExecuteDeleteSession_Success(t *testing.T) {
	e := newTestExecutor()
	r := e.executeDeleteSession(context.Background(), map[string]any{"sessionId": "sess-abc"})
	if !r.Success {
		t.Fatalf("expected success: %s", r.Error)
	}
	data := r.Data.(map[string]any)
	if data["sessionId"] != "sess-abc" {
		t.Errorf("sessionId = %v", data["sessionId"])
	}
}

// --- Tests: findGitSecret ---

func TestFindGitSecret_Found(t *testing.T) {
	tests := []struct {
		name       string
		secretName string
		dataKey    string
	}{
		{"github-credentials with token", "github-credentials", "token"},
		{"git-credentials with password", "git-credentials", "password"},
		{"github-token with token", "github-token", "token"},
		{"git-token with token", "git-token", "token"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{
					Name:      tt.secretName,
					Namespace: "default",
				},
				Data: map[string][]byte{
					tt.dataKey: []byte("secret-value"),
				},
			}
			e := newTestExecutor(secret)
			got := e.findGitSecret(context.Background(), "default")
			if got != tt.secretName {
				t.Errorf("findGitSecret() = %q, want %q", got, tt.secretName)
			}
		})
	}
}

func TestFindGitSecret_NotFound(t *testing.T) {
	e := newTestExecutor()
	got := e.findGitSecret(context.Background(), "default")
	if got != "" {
		t.Errorf("findGitSecret() = %q, want empty", got)
	}
}

func TestFindGitSecret_WrongDataKey(t *testing.T) {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "github-credentials",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"username": []byte("user"),
		},
	}
	e := newTestExecutor(secret)
	got := e.findGitSecret(context.Background(), "default")
	if got != "" {
		t.Errorf("findGitSecret() = %q, want empty (no token/password key)", got)
	}
}
