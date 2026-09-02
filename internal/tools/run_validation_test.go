package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const (
	runValidationTestNamespace = "validation-team"
	runValidationTestImage     = "ghcr.io/example/go-ci@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	runValidationTestHeadSHA   = "0123456789abcdef0123456789abcdef01234567"
)

func TestRunValidationToolCreatesOneScopedExactHeadTask(t *testing.T) {
	monitor, parent := runValidationFixtures()
	k8sClient := newFakeClient(monitor, parent)
	bindingStore := newRunValidationBindingStore()
	tool := NewRunValidationTool(k8sClient)
	ctx := WithToolContext(context.Background(), &ToolContext{
		Brokered:                     true,
		Namespace:                    parent.Namespace,
		TaskID:                       parent.Name,
		TaskUID:                      string(parent.UID),
		RepositoryValidationBindings: bindingStore,
	})

	result, err := tool.Execute(ctx, json.RawMessage(`{"command":"go test ./... && golangci-lint run"}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	parsed := parseRunValidationResult(t, result)
	if !parsed.Success {
		t.Fatalf("Execute() result = %#v, want success", parsed)
	}
	data, ok := parsed.Data.(map[string]any)
	if !ok {
		t.Fatalf("Execute() data = %#v, want object", parsed.Data)
	}
	validationName, _ := data["task"].(string)
	if validationName == "" {
		t.Fatalf("Execute() data = %#v, want task name", data)
	}

	validationTask := &corev1alpha1.Task{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: parent.Namespace, Name: validationName}, validationTask); err != nil {
		t.Fatalf("get validation Task: %v", err)
	}
	if validationTask.Spec.Type != corev1alpha1.TaskTypeContainer || validationTask.Spec.Image != runValidationTestImage {
		t.Fatalf("validation task type/image = %q/%q", validationTask.Spec.Type, validationTask.Spec.Image)
	}
	if !slices.Equal(validationTask.Spec.Command, []string{"/bin/sh", "-c"}) || !slices.Equal(validationTask.Spec.Args, []string{"go test ./... && golangci-lint run"}) {
		t.Fatalf("validation command = %#v %#v", validationTask.Spec.Command, validationTask.Spec.Args)
	}
	if validationTask.Spec.Workspace == nil || validationTask.Spec.Workspace.Intent != corev1alpha1.WorkspaceIntentRead || validationTask.Spec.Workspace.GitRepo != parent.Spec.Workspace.GitRepo || validationTask.Spec.Workspace.Ref != runValidationTestHeadSHA {
		t.Fatalf("validation workspace = %#v, want exact parent head", validationTask.Spec.Workspace)
	}
	if validationTask.Spec.Workspace.PublicationCredentialRef != nil || validationTask.Spec.Workspace.ForgeCredentialRef != nil || validationTask.Spec.Workspace.PushBranch != "" {
		t.Fatalf("validation workspace has publication capability: %#v", validationTask.Spec.Workspace)
	}
	if !metav1.IsControlledBy(validationTask, monitor) || labels.ParentTaskName(validationTask.Labels, validationTask.Annotations) != parent.Name {
		t.Fatalf("validation provenance = owners %#v labels %#v annotations %#v", validationTask.OwnerReferences, validationTask.Labels, validationTask.Annotations)
	}
	if len(bindingStore.events) != 1 {
		t.Fatalf("binding event count = %d, want 1", len(bindingStore.events))
	}
	for _, event := range bindingStore.events {
		if strings.Contains(event.MetadataJSON, "go test") || strings.Contains(event.MetadataJSON, "golangci-lint") {
			t.Fatalf("binding metadata contains the raw command: %s", event.MetadataJSON)
		}
	}
	binding, err := FindRepositoryValidationCommandBinding(ctx, bindingStore, parent.Namespace, validationTask.Name)
	if err != nil {
		t.Fatalf("FindRepositoryValidationCommandBinding() error = %v", err)
	}
	if !binding.MatchesReview(parent, monitor, runValidationTestImage, runValidationTestHeadSHA) ||
		!binding.MatchesCommand("go test ./... && golangci-lint run") {
		t.Fatalf("durable validation binding = %#v, want exact review and command", binding)
	}

	result, err = tool.Execute(ctx, json.RawMessage(`{"command":"go test ./... && golangci-lint run"}`))
	if err != nil || !parseRunValidationResult(t, result).Success {
		t.Fatalf("idempotent Execute() = (%s, %v), want success", result, err)
	}
	result, err = tool.Execute(ctx, json.RawMessage(`{"command":"go test ./internal/..."}`))
	if err != nil {
		t.Fatalf("conflicting Execute() error = %v", err)
	}
	conflict := parseRunValidationResult(t, result)
	if conflict.Success || conflict.ErrorType != "validation_task_conflict" {
		t.Fatalf("conflicting Execute() = %#v, want validation_task_conflict", conflict)
	}

	var tasks corev1alpha1.TaskList
	if err := k8sClient.List(ctx, &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 2 {
		t.Fatalf("Task count = %d, want parent plus one validation Task", len(tasks.Items))
	}
}

func TestRepositoryValidationTaskNameKeepsValidationSuffixWhenTruncated(t *testing.T) {
	name := RepositoryValidationTaskName(strings.Repeat("review", 20))
	if len(name) > 63 || !strings.HasSuffix(name, "-validation") {
		t.Fatalf("RepositoryValidationTaskName() = %q, want <=63 characters ending in -validation", name)
	}
}

func TestRunValidationToolFailsClosedWithoutCommandBindingStore(t *testing.T) {
	monitor, parent := runValidationFixtures()
	k8sClient := newFakeClient(monitor, parent)
	ctx := WithToolContext(context.Background(), &ToolContext{
		Brokered: true, Namespace: parent.Namespace, TaskID: parent.Name, TaskUID: string(parent.UID),
	})

	result, err := NewRunValidationTool(k8sClient).Execute(ctx, json.RawMessage(`{"command":"go test ./..."}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	parsed := parseRunValidationResult(t, result)
	if parsed.Success || parsed.ErrorType != internalErrorType || !strings.Contains(parsed.Error, "persist") {
		t.Fatalf("Execute() = %#v, want command binding persistence failure", parsed)
	}
	var tasks corev1alpha1.TaskList
	if err := k8sClient.List(ctx, &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 1 || tasks.Items[0].Name != parent.Name {
		t.Fatalf("missing command binding store created a validation Task: %#v", tasks.Items)
	}
}

func TestRunValidationToolDescriptionBindsWaitTimeout(t *testing.T) {
	want := fmt.Sprintf("timeout %q", RepositoryValidationTimeout.String())
	if description := NewRunValidationTool(nil).Description(); !strings.Contains(description, want) {
		t.Fatalf("Description() = %q, want containing %q", description, want)
	}
}

func TestRunValidationToolRejectsUnboundReviewTask(t *testing.T) {
	monitor, parent := runValidationFixtures()
	parent.Annotations[labels.AnnotationRepositoryValidationImage] = "ghcr.io/example/unapproved:latest"
	k8sClient := newFakeClient(monitor, parent)
	ctx := WithToolContext(context.Background(), &ToolContext{
		Brokered: true, Namespace: parent.Namespace, TaskID: parent.Name, TaskUID: string(parent.UID),
	})

	result, err := NewRunValidationTool(k8sClient).Execute(ctx, json.RawMessage(`{"command":"go test ./..."}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	parsed := parseRunValidationResult(t, result)
	if parsed.Success || parsed.ErrorType != "validation_not_authorized" {
		t.Fatalf("Execute() = %#v, want validation_not_authorized", parsed)
	}
	var tasks corev1alpha1.TaskList
	if err := k8sClient.List(ctx, &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 1 || tasks.Items[0].Name != parent.Name {
		t.Fatalf("unexpected Task mutation: %#v", tasks.Items)
	}
}

func TestRunValidationToolRejectsMutableValidationImage(t *testing.T) {
	monitor, parent := runValidationFixtures()
	monitor.Spec.Validation.Image = "ghcr.io/example/go-ci:latest"
	parent.Annotations[labels.AnnotationRepositoryValidationImage] = monitor.Spec.Validation.Image
	k8sClient := newFakeClient(monitor, parent)
	ctx := WithToolContext(context.Background(), &ToolContext{
		Brokered: true, Namespace: parent.Namespace, TaskID: parent.Name, TaskUID: string(parent.UID),
	})

	result, err := NewRunValidationTool(k8sClient).Execute(ctx, json.RawMessage(`{"command":"go test ./..."}`))
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	parsed := parseRunValidationResult(t, result)
	if parsed.Success || parsed.ErrorType != "validation_not_authorized" || !strings.Contains(parsed.Error, "digest-pinned") {
		t.Fatalf("Execute() = %#v, want digest-pinned validation rejection", parsed)
	}
}

func TestRunValidationToolRejectsCredentialLikeCommand(t *testing.T) {
	monitor, parent := runValidationFixtures()
	k8sClient := newFakeClient(monitor, parent)
	ctx := WithToolContext(context.Background(), &ToolContext{
		Brokered: true, Namespace: parent.Namespace, TaskID: parent.Name, TaskUID: string(parent.UID),
	})
	secret := "ghp_" + strings.Repeat("a", 30)
	raw, err := json.Marshal(map[string]string{"command": "TOKEN=" + secret + " go test ./..."})
	if err != nil {
		t.Fatal(err)
	}

	result, err := NewRunValidationTool(k8sClient).Execute(ctx, raw)
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	parsed := parseRunValidationResult(t, result)
	if parsed.Success || parsed.ErrorType != "invalid_arguments" || strings.Contains(result, secret) {
		t.Fatalf("Execute() = %#v, want credential-like command rejected without echoing the value", parsed)
	}
	var tasks corev1alpha1.TaskList
	if err := k8sClient.List(ctx, &tasks); err != nil {
		t.Fatal(err)
	}
	if len(tasks.Items) != 1 || tasks.Items[0].Name != parent.Name {
		t.Fatalf("credential-like command created a validation Task: %#v", tasks.Items)
	}
}

func runValidationFixtures() (*corev1alpha1.RepositoryMonitor, *corev1alpha1.Task) {
	monitor := &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: "repo", Namespace: runValidationTestNamespace, UID: types.UID("monitor-uid")},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL:    "https://github.com/example/repo",
			Validation: corev1alpha1.RepositoryMonitorValidationSpec{Image: runValidationTestImage},
		},
	}
	controller := true
	parent := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "monrev-repo-17-head-run",
			Namespace: runValidationTestNamespace,
			UID:       types.UID("review-task-uid"),
			Labels: map[string]string{
				labels.LabelCreatedBy:         "repository-monitor",
				labels.LabelRepositoryMonitor: labels.SelectorValue(monitor.Name),
				labels.LabelMonitorRun:        "run-1",
				labels.LabelGitHubRepository:  "example-repo",
				labels.LabelGitHubTarget:      "pull-request",
				labels.LabelGitHubNumber:      "17",
			},
			Annotations: map[string]string{
				labels.AnnotationAgentReadOnly:             trueStr,
				labels.AnnotationRepositoryMonitorName:     monitor.Name,
				labels.AnnotationRepositoryValidationImage: runValidationTestImage,
				labels.AnnotationMonitorRunID:              "run-1",
				labels.AnnotationMonitorItemKind:           "pull_request",
				labels.AnnotationMonitorItemNumber:         "17",
				labels.AnnotationMonitorHeadSHA:            runValidationTestHeadSHA,
				labels.AnnotationGitHubRepository:          "example/repo",
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor",
				Name: monitor.Name, UID: monitor.UID, Controller: &controller,
			}},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent,
			AgentRuntime: &corev1alpha1.AgentRuntimeSpec{
				AllowedTools: []string{"Read(/workspace/**)", RunValidationToolName, "wait_for_tasks"},
			},
			Workspace: &corev1alpha1.WorkspaceConfig{
				Intent:            corev1alpha1.WorkspaceIntentRead,
				GitRepo:           "https://github.com/example/repo",
				Ref:               runValidationTestHeadSHA,
				ReadCredentialRef: &corev1alpha1.WorkspaceCredentialReference{Name: "repo-read"},
			},
		},
	}
	return monitor, parent
}

func parseRunValidationResult(t *testing.T, value string) ChatToolResult {
	t.Helper()
	var result ChatToolResult
	if err := json.Unmarshal([]byte(value), &result); err != nil {
		t.Fatalf("decode tool result %q: %v", value, err)
	}
	return result
}

type runValidationBindingStore struct {
	events map[string]store.MonitorEvent
}

func newRunValidationBindingStore() *runValidationBindingStore {
	return &runValidationBindingStore{events: make(map[string]store.MonitorEvent)}
}

func (s *runValidationBindingStore) CreateMonitorEvent(_ context.Context, event *store.MonitorEvent) error {
	if event == nil {
		return errors.New("monitor event is required")
	}
	if _, exists := s.events[event.ID]; exists {
		return errors.New("monitor event already exists")
	}
	s.events[event.ID] = *event
	return nil
}

func (s *runValidationBindingStore) ListMonitorEvents(_ context.Context, filter store.MonitorEventFilter) ([]store.MonitorEvent, string, error) {
	events := make([]store.MonitorEvent, 0, len(s.events))
	for _, event := range s.events {
		if event.MonitorNamespace != filter.Namespace ||
			(filter.MonitorName != "" && event.MonitorName != filter.MonitorName) ||
			(filter.RunID != "" && event.RunID != filter.RunID) ||
			(filter.ItemKind != "" && event.ItemKind != filter.ItemKind) ||
			(filter.ItemNumber != 0 && event.ItemNumber != filter.ItemNumber) ||
			(filter.EventType != "" && event.EventType != filter.EventType) {
			continue
		}
		events = append(events, event)
	}
	return events, "", nil
}
