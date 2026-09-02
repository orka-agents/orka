package controller

import (
	"context"
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

const repositoryMonitorValidationTestImage = "ghcr.io/example/repo-validation@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

const repositoryMonitorValidationTestSecret = "ghp_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"

func TestRepositoryMonitorReviewValidationGatesPassedVerdict(t *testing.T) {
	tests := []struct {
		name               string
		validationTask     func(*corev1alpha1.RepositoryMonitor, *corev1alpha1.Task) *corev1alpha1.Task
		wantHandled        bool
		wantVerdict        string
		wantStatus         string
		wantAutomergeState string
		wantEvidence       string
		wantCommand        string
		wantFresh          bool
	}{
		{
			name: "passing validation preserves passed verdict",
			validationTask: func(monitor *corev1alpha1.RepositoryMonitor, reviewTask *corev1alpha1.Task) *corev1alpha1.Task {
				return repositoryMonitorValidationTaskForTest(monitor, reviewTask, corev1alpha1.TaskPhaseSucceeded, reviewTask.Annotations[labels.AnnotationMonitorHeadSHA])
			},
			wantHandled:        true,
			wantVerdict:        repositoryMonitorReviewVerdictPassed,
			wantStatus:         repositoryMonitorValidationStatusPassed,
			wantAutomergeState: repositoryMonitorAutomergeStateMergeReady,
			wantEvidence:       "go test ./...: ok",
			wantCommand:        "go test ./...",
			wantFresh:          true,
		},
		{
			name: "failed validation blocks passed verdict",
			validationTask: func(monitor *corev1alpha1.RepositoryMonitor, reviewTask *corev1alpha1.Task) *corev1alpha1.Task {
				task := repositoryMonitorValidationTaskForTest(monitor, reviewTask, corev1alpha1.TaskPhaseFailed, reviewTask.Annotations[labels.AnnotationMonitorHeadSHA])
				task.Status.Message = "command exited with status 1"
				task.Status.ExecutionOutcome = &corev1alpha1.TaskWorkloadExecutionOutcome{Phase: corev1alpha1.TaskPhaseFailed, Attempt: 1}
				return task
			},
			wantHandled:  true,
			wantVerdict:  repositoryMonitorReviewVerdictNeedsChanges,
			wantStatus:   repositoryMonitorValidationStatusFailed,
			wantEvidence: "status 1",
			wantCommand:  "go test ./...",
			wantFresh:    true,
		},
		{
			name: "infrastructure validation failure remains retryable",
			validationTask: func(monitor *corev1alpha1.RepositoryMonitor, reviewTask *corev1alpha1.Task) *corev1alpha1.Task {
				task := repositoryMonitorValidationTaskForTest(monitor, reviewTask, corev1alpha1.TaskPhaseFailed, reviewTask.Annotations[labels.AnnotationMonitorHeadSHA])
				task.Status.Message = "pod stuck: ErrImageNeverPull"
				return task
			},
			wantHandled:  true,
			wantVerdict:  repositoryMonitorReviewVerdictNeedsHuman,
			wantStatus:   repositoryMonitorValidationStatusUnavailable,
			wantEvidence: "ErrImageNeverPull",
			wantCommand:  "go test ./...",
		},
		{
			name:         "missing validation blocks passed verdict",
			wantHandled:  true,
			wantVerdict:  repositoryMonitorReviewVerdictNeedsHuman,
			wantStatus:   repositoryMonitorValidationStatusNotRun,
			wantEvidence: "did not run",
			wantFresh:    true,
		},
		{
			name: "stale validation checkout blocks passed verdict",
			validationTask: func(monitor *corev1alpha1.RepositoryMonitor, reviewTask *corev1alpha1.Task) *corev1alpha1.Task {
				return repositoryMonitorValidationTaskForTest(monitor, reviewTask, corev1alpha1.TaskPhaseSucceeded, "different-head")
			},
			wantHandled:  true,
			wantVerdict:  repositoryMonitorReviewVerdictNeedsChanges,
			wantStatus:   repositoryMonitorValidationStatusFailed,
			wantEvidence: "exact reviewed head",
			wantFresh:    true,
		},
		{
			name: "removed validation image annotation is rejected",
			validationTask: func(monitor *corev1alpha1.RepositoryMonitor, reviewTask *corev1alpha1.Task) *corev1alpha1.Task {
				task := repositoryMonitorValidationTaskForTest(monitor, reviewTask, corev1alpha1.TaskPhaseSucceeded, reviewTask.Annotations[labels.AnnotationMonitorHeadSHA])
				delete(task.Annotations, labels.AnnotationRepositoryValidationImage)
				return task
			},
			wantHandled:  true,
			wantVerdict:  repositoryMonitorReviewVerdictNeedsChanges,
			wantStatus:   repositoryMonitorValidationStatusFailed,
			wantEvidence: "repository-validation-image",
			wantFresh:    true,
		},
		{
			name: "altered validation image annotation is rejected",
			validationTask: func(monitor *corev1alpha1.RepositoryMonitor, reviewTask *corev1alpha1.Task) *corev1alpha1.Task {
				task := repositoryMonitorValidationTaskForTest(monitor, reviewTask, corev1alpha1.TaskPhaseSucceeded, reviewTask.Annotations[labels.AnnotationMonitorHeadSHA])
				task.Annotations[labels.AnnotationRepositoryValidationImage] = "ghcr.io/example/other-validation@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				return task
			},
			wantHandled:  true,
			wantVerdict:  repositoryMonitorReviewVerdictNeedsChanges,
			wantStatus:   repositoryMonitorValidationStatusFailed,
			wantEvidence: "repository-validation-image",
			wantFresh:    true,
		},
		{
			name: "credential-like validation command is rejected without persistence",
			validationTask: func(monitor *corev1alpha1.RepositoryMonitor, reviewTask *corev1alpha1.Task) *corev1alpha1.Task {
				task := repositoryMonitorValidationTaskForTest(monitor, reviewTask, corev1alpha1.TaskPhaseSucceeded, reviewTask.Annotations[labels.AnnotationMonitorHeadSHA])
				task.Spec.Args = []string{"TOKEN=" + repositoryMonitorValidationTestSecret + " go test ./..."}
				return task
			},
			wantHandled:  true,
			wantVerdict:  repositoryMonitorReviewVerdictNeedsChanges,
			wantStatus:   repositoryMonitorValidationStatusFailed,
			wantEvidence: "credential-like",
			wantFresh:    true,
		},
		{
			name: "running validation defers review ingestion",
			validationTask: func(monitor *corev1alpha1.RepositoryMonitor, reviewTask *corev1alpha1.Task) *corev1alpha1.Task {
				return repositoryMonitorValidationTaskForTest(monitor, reviewTask, corev1alpha1.TaskPhaseRunning, reviewTask.Annotations[labels.AnnotationMonitorHeadSHA])
			},
			wantHandled: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			monitorStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatal(err)
			}

			monitor := repositoryMonitorReviewIngestTestMonitor("validation-" + repositoryMonitorShortHash(tt.name))
			monitor.Spec.Validation.Image = repositoryMonitorValidationTestImage
			reviewTask := repositoryMonitorReviewIngestTestTask("review-"+repositoryMonitorShortHash(tt.name), monitor.Name, 1, repositoryMonitorTestHeadSHA)
			repositoryMonitorBindValidationForTest(reviewTask)
			objects := []client.Object{monitor, reviewTask}
			var validationTask *corev1alpha1.Task
			if tt.validationTask != nil {
				validationTask = tt.validationTask(monitor, reviewTask)
				objects = append(objects, validationTask)
			}
			k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
			reconciler := &RepositoryMonitorReconciler{Client: k8sClient, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore}

			item := &store.MonitorItem{
				MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
				Kind: repositoryMonitorPullRequestKind, ItemKey: "1", Number: 1,
				State: repositoryMonitorItemStateOpen, HeadSHA: repositoryMonitorTestHeadSHA,
				LastVerdict: repositoryMonitorRunPhaseQueued, LastReviewID: reviewTask.Name,
			}
			if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
				t.Fatal(err)
			}
			if err := monitorStore.SaveResult(ctx, reviewTask.Namespace, reviewTask.Name, repositoryMonitorReviewResultEnvelope(t, 1, repositoryMonitorTestHeadSHA, repositoryMonitorReviewVerdictPassed)); err != nil {
				t.Fatal(err)
			}
			if validationTask != nil && validationTask.Status.ResultRef != nil && validationTask.Status.ResultRef.Available {
				if err := monitorStore.SaveResult(ctx, validationTask.Namespace, validationTask.Name, []byte("go test ./...: ok")); err != nil {
					t.Fatal(err)
				}
			}

			handled, err := reconciler.ingestCompletedRepositoryMonitorReviewTask(ctx, monitor, item, reviewTask)
			if err != nil {
				t.Fatalf("ingestCompletedRepositoryMonitorReviewTask() error = %v", err)
			}
			if handled != tt.wantHandled {
				t.Fatalf("handled = %v, want %v", handled, tt.wantHandled)
			}
			records, _, err := monitorStore.ListReviewRecords(ctx, store.ReviewRecordFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Number: 1, Limit: 5})
			if err != nil {
				t.Fatal(err)
			}
			if !tt.wantHandled {
				if len(records) != 0 {
					t.Fatalf("records = %#v, want no record while validation runs", records)
				}
				return
			}
			if len(records) != 1 {
				t.Fatalf("records = %#v, want one review record", records)
			}
			record := records[0]
			if record.Verdict != tt.wantVerdict || record.ValidationStatus != tt.wantStatus || record.ValidationImage != repositoryMonitorValidationTestImage {
				t.Fatalf("record verdict/validation = %q/%q image %q, want %q/%q image %q", record.Verdict, record.ValidationStatus, record.ValidationImage, tt.wantVerdict, tt.wantStatus, repositoryMonitorValidationTestImage)
			}
			if tt.wantEvidence != "" && !strings.Contains(record.ValidationEvidence, tt.wantEvidence) {
				t.Fatalf("validation evidence = %q, want containing %q", record.ValidationEvidence, tt.wantEvidence)
			}
			if record.ValidationCommand != tt.wantCommand {
				t.Fatalf("validation command = %q, want %q", record.ValidationCommand, tt.wantCommand)
			}
			if strings.Contains(record.ValidationCommand, repositoryMonitorValidationTestSecret) || strings.Contains(record.ValidationEvidence, repositoryMonitorValidationTestSecret) {
				t.Fatal("validation record persisted credential-like command content")
			}
			updated, err := monitorStore.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "1")
			if err != nil {
				t.Fatal(err)
			}
			if updated.AutomergeState != tt.wantAutomergeState {
				t.Fatalf("automerge state = %q, want %q", updated.AutomergeState, tt.wantAutomergeState)
			}
			fresh, err := reconciler.repositoryMonitorReviewedHeadFresh(ctx, monitor, updated, repositoryMonitorTestHeadSHA)
			if err != nil {
				t.Fatal(err)
			}
			if fresh != tt.wantFresh {
				t.Fatalf("fresh = %v, want %v for validation status %q", fresh, tt.wantFresh, record.ValidationStatus)
			}
		})
	}
}

func TestRepositoryMonitorReviewValidationRequiresTaskImageBinding(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor := repositoryMonitorReviewIngestTestMonitor("missing-validation-binding")
	monitor.Spec.Validation.Image = repositoryMonitorValidationTestImage
	reviewTask := repositoryMonitorReviewIngestTestTask("missing-validation-binding-review", monitor.Name, 1, repositoryMonitorTestHeadSHA)
	reviewTask.Spec.Workspace = &corev1alpha1.WorkspaceConfig{Intent: corev1alpha1.WorkspaceIntentRead, GitRepo: repositoryMonitorTestRepoURL, Ref: repositoryMonitorTestHeadSHA}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(monitor, reviewTask).Build()
	reconciler := &RepositoryMonitorReconciler{Client: k8sClient, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore}
	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "1", Number: 1, State: repositoryMonitorItemStateOpen, HeadSHA: repositoryMonitorTestHeadSHA, LastVerdict: repositoryMonitorRunPhaseQueued, LastReviewID: reviewTask.Name}
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if err := monitorStore.SaveResult(ctx, reviewTask.Namespace, reviewTask.Name, repositoryMonitorReviewResultEnvelope(t, 1, repositoryMonitorTestHeadSHA, repositoryMonitorReviewVerdictPassed)); err != nil {
		t.Fatal(err)
	}
	handled, err := reconciler.ingestCompletedRepositoryMonitorReviewTask(ctx, monitor, item, reviewTask)
	if err != nil || !handled {
		t.Fatalf("ingest = (%v, %v), want handled", handled, err)
	}
	records, _, err := monitorStore.ListReviewRecords(ctx, store.ReviewRecordFilter{Namespace: monitor.Namespace, MonitorName: monitor.Name, Number: 1, Limit: 1})
	if err != nil || len(records) != 1 {
		t.Fatalf("records = %#v, err = %v", records, err)
	}
	if records[0].Verdict != repositoryMonitorReviewVerdictNeedsChanges || records[0].ValidationStatus != repositoryMonitorValidationStatusFailed || records[0].ValidationImage != "" || !strings.Contains(records[0].ValidationEvidence, "missing") {
		t.Fatalf("record = %#v, want missing image binding to fail closed", records[0])
	}
	updated, err := monitorStore.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := reconciler.repositoryMonitorReviewedHeadFresh(ctx, monitor, updated, repositoryMonitorTestHeadSHA)
	if err != nil {
		t.Fatal(err)
	}
	if fresh {
		t.Fatal("review created without the configured validation binding remained fresh")
	}
}

func TestRepositoryMonitorReviewValidationIgnoresUnexpectedMatchingTasks(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor := repositoryMonitorReviewIngestTestMonitor("validation-exact-child")
	monitor.Spec.Validation.Image = repositoryMonitorValidationTestImage
	reviewTask := repositoryMonitorReviewIngestTestTask("validation-exact-child-review", monitor.Name, 1, repositoryMonitorTestHeadSHA)
	repositoryMonitorBindValidationForTest(reviewTask)
	validationTask := repositoryMonitorValidationTaskForTest(monitor, reviewTask, corev1alpha1.TaskPhaseRunning, repositoryMonitorTestHeadSHA)
	unexpected := validationTask.DeepCopy()
	unexpected.Name = "unexpected-validation-task"
	unexpected.UID = types.UID("unexpected-validation-task")
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(monitor, reviewTask, validationTask, unexpected).Build()
	reconciler := &RepositoryMonitorReconciler{Client: k8sClient, Scheme: scheme}

	result, pending, err := reconciler.repositoryMonitorReviewValidation(ctx, monitor, reviewTask)
	if err != nil {
		t.Fatalf("repositoryMonitorReviewValidation() error = %v", err)
	}
	if !pending || result.TaskName != validationTask.Name || result.Status != repositoryMonitorValidationStatusNotRun {
		t.Fatalf("validation result = %#v, pending = %v, want exact child pending", result, pending)
	}
}

func repositoryMonitorBindValidationForTest(task *corev1alpha1.Task) {
	task.Annotations[labels.AnnotationAgentReadOnly] = scheduledRunLabelValue
	task.Annotations[labels.AnnotationMonitorRunID] = "run-validation"
	task.Annotations[labels.AnnotationRepositoryValidationImage] = repositoryMonitorValidationTestImage
	task.Labels = map[string]string{
		labels.LabelCreatedBy:         "repository-monitor",
		labels.LabelRepositoryMonitor: labels.SelectorValue(task.Annotations[labels.AnnotationRepositoryMonitorName]),
		labels.LabelMonitorRun:        "run-validation",
		labels.LabelGitHubRepository:  labels.SelectorValue(task.Annotations[labels.AnnotationGitHubRepository]),
		labels.LabelGitHubTarget:      labels.SelectorValue(repositoryMonitorPullRequestKind),
		labels.LabelGitHubNumber:      task.Annotations[labels.AnnotationMonitorItemNumber],
	}
	task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: append(readOnlyAgentAllowedTools(), tools.RunValidationToolName, repositoryMonitorWaitForTasksToolName)}
	task.Spec.Workspace = &corev1alpha1.WorkspaceConfig{
		Intent:  corev1alpha1.WorkspaceIntentRead,
		GitRepo: repositoryMonitorTestRepoURL,
		Ref:     task.Annotations[labels.AnnotationMonitorHeadSHA],
	}
}

func repositoryMonitorValidationTaskForTest(monitor *corev1alpha1.RepositoryMonitor, reviewTask *corev1alpha1.Task, phase corev1alpha1.TaskPhase, workspaceRef string) *corev1alpha1.Task {
	controller := true
	resultAvailable := phase == corev1alpha1.TaskPhaseSucceeded
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      tools.RepositoryValidationTaskName(reviewTask.Name),
			Namespace: reviewTask.Namespace,
			Labels: map[string]string{
				labels.LabelCreatedBy:         "repository-monitor",
				labels.LabelPurpose:           repositoryMonitorValidationPurpose,
				labels.LabelParentTask:        labels.SelectorValue(reviewTask.Name),
				labels.LabelRepositoryMonitor: labels.SelectorValue(monitor.Name),
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName:            reviewTask.Name,
				labels.AnnotationRepositoryMonitorName:     reviewTask.Annotations[labels.AnnotationRepositoryMonitorName],
				labels.AnnotationMonitorRunID:              reviewTask.Annotations[labels.AnnotationMonitorRunID],
				labels.AnnotationMonitorItemKind:           reviewTask.Annotations[labels.AnnotationMonitorItemKind],
				labels.AnnotationMonitorItemNumber:         reviewTask.Annotations[labels.AnnotationMonitorItemNumber],
				labels.AnnotationMonitorHeadSHA:            reviewTask.Annotations[labels.AnnotationMonitorHeadSHA],
				labels.AnnotationGitHubRepository:          reviewTask.Annotations[labels.AnnotationGitHubRepository],
				labels.AnnotationRepositoryValidationImage: repositoryMonitorValidationTestImage,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor",
				Name: monitor.Name, UID: monitor.UID, Controller: &controller,
			}},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   repositoryMonitorValidationTestImage,
			Command: []string{"/bin/sh", "-c"},
			Args:    []string{"go test ./..."},
			Workspace: &corev1alpha1.WorkspaceConfig{
				Intent:  corev1alpha1.WorkspaceIntentRead,
				GitRepo: reviewTask.Spec.Workspace.GitRepo,
				Ref:     workspaceRef,
			},
		},
		Status: corev1alpha1.TaskStatus{
			Phase: phase,
			ResultRef: &corev1alpha1.ResultReference{
				Available: resultAvailable,
			},
		},
	}
}

func TestRenderRepositoryMonitorReviewBodyIncludesValidationEvidence(t *testing.T) {
	monitor := repositoryMonitorReviewIngestTestMonitor("render-validation")
	item := &store.MonitorItem{Number: 1}
	task := repositoryMonitorReviewIngestTestTask("render-validation-task", monitor.Name, 1, repositoryMonitorTestHeadSHA)
	record := &store.ReviewRecord{
		ID: "review-1", HeadSHA: repositoryMonitorTestHeadSHA,
		Verdict: repositoryMonitorReviewVerdictPassed, Confidence: repositoryMonitorReviewConfidenceHigh,
		FindingsJSON: "[]", ValidationStatus: repositoryMonitorValidationStatusPassed,
		ValidationImage: repositoryMonitorValidationTestImage, ValidationCommand: "go test ./...",
		ValidationEvidence: "ok\nall packages passed",
	}
	body := renderRepositoryMonitorReviewBody(monitor, item, task, record, "publish-1", nil)
	renderedImage := strings.Replace(repositoryMonitorValidationTestImage, "@", "@\u200b", 1)
	for _, want := range []string{"**Status:** passed", renderedImage, "go test ./...", "> all packages passed"} {
		if !strings.Contains(body, want) {
			t.Fatalf("rendered body missing %q:\n%s", want, body)
		}
	}
}

func TestBoundRepositoryMonitorValidationEvidenceRedactsCredentials(t *testing.T) {
	secret := "ghp_" + strings.Repeat("a", 30)
	evidence := boundRepositoryMonitorValidationEvidence("validation failed with token=" + secret)
	if strings.Contains(evidence, secret) || !strings.Contains(evidence, "[REDACTED]") {
		t.Fatalf("validation evidence was not redacted: %q", evidence)
	}
}

func TestRepositoryMonitorValidationAllowsAutomergeRequiresCurrentPassedValidation(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := repositoryMonitorReviewIngestTestMonitor("validation-automerge")
	monitor.Spec.Validation.Image = repositoryMonitorValidationTestImage
	item := &store.MonitorItem{
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		HeadSHA:          repositoryMonitorTestHeadSHA,
		LastReviewID:     "old-image-review",
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}

	for _, record := range []*store.ReviewRecord{
		{
			ID: "old-image-review", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
			Kind: repositoryMonitorPullRequestKind, Number: item.Number, HeadSHA: item.HeadSHA,
			Verdict: repositoryMonitorReviewVerdictPassed, ValidationStatus: repositoryMonitorValidationStatusPassed,
			ValidationImage: "ghcr.io/example/repo-validation@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		},
		{
			ID: "failed-validation-review", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
			Kind: repositoryMonitorPullRequestKind, Number: item.Number, HeadSHA: item.HeadSHA,
			Verdict: repositoryMonitorReviewVerdictPassed, ValidationStatus: repositoryMonitorValidationStatusFailed,
			ValidationImage: repositoryMonitorValidationTestImage,
		},
		{
			ID: "passed-validation-review", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name,
			Kind: repositoryMonitorPullRequestKind, Number: item.Number, HeadSHA: item.HeadSHA,
			Verdict: repositoryMonitorReviewVerdictPassed, ValidationStatus: repositoryMonitorValidationStatusPassed,
			ValidationImage: repositoryMonitorValidationTestImage,
		},
	} {
		if err := monitorStore.CreateReviewRecord(ctx, record); err != nil {
			t.Fatal(err)
		}
	}

	if reconciler.repositoryMonitorValidationAllowsAutomerge(ctx, monitor, item, item.HeadSHA) {
		t.Fatal("review using an old validation image allowed automerge")
	}
	item.LastReviewID = "failed-validation-review"
	if reconciler.repositoryMonitorValidationAllowsAutomerge(ctx, monitor, item, item.HeadSHA) {
		t.Fatal("failed validation allowed automerge")
	}
	item.LastReviewID = "passed-validation-review"
	if !reconciler.repositoryMonitorValidationAllowsAutomerge(ctx, monitor, item, item.HeadSHA) {
		t.Fatal("current passed validation did not allow automerge")
	}
}
