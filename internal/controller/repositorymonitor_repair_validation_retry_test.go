/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"maps"
	"slices"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
)

const repairValidationTestCommand = "make test"

type repairValidationRetryHarness struct {
	t          *testing.T
	ctx        context.Context
	store      *storageclient.Client
	client     client.Client
	reconciler *RepositoryMonitorReconciler
	monitor    *corev1alpha1.RepositoryMonitor
	pr         repositoryMonitorPullRequest
	jobID      string
}

func newRepairValidationRetryHarness(t *testing.T, name string, repairCommand string, maxRetries *int32) *repairValidationRetryHarness {
	t.Helper()
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	monitor, _ := repositoryMonitorInventoryTestObjects(name)
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Repair.MaxValidationRetries = maxRetries
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	if repairCommand != "" {
		monitor.Spec.Validation.Image = "ghcr.io/orka-agents/validation@sha256:" + strings.Repeat("a", 64)
		monitor.Spec.Validation.RepairCommand = repairCommand
	}
	configureRepositoryMonitorTestWriteCredentials(monitor)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).
		WithObjects(monitor, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: monitor.Namespace, UID: "namespace-uid"}}).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
	command := &store.CommandEvent{ID: "cmd-" + name, Intent: repositoryMonitorCommandIntentFix, Source: "api"}
	pr := repositoryMonitorPullRequest{Number: 41, HeadSHA: "head41", BaseSHA: "base41", HeadBranch: "feature-fix", HeadRepo: "orka-agents/orka", BaseBranch: "main"}
	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "41", Number: 41, HeadSHA: pr.HeadSHA}
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatal(err)
	}
	if created, _, err := reconciler.createRepositoryMonitorRepairTaskWithEvidence(ctx, monitor, &store.MonitorRun{ID: "run-" + name}, command, "orka-agents", "orka", pr, item, 1, 1); err != nil || created != 1 {
		t.Fatalf("createRepositoryMonitorRepairTaskWithEvidence() = (%d, %v)", created, err)
	}
	return &repairValidationRetryHarness{
		t: t, ctx: ctx, store: monitorStore, client: cl, reconciler: reconciler, monitor: monitor, pr: pr,
		jobID: "repair-" + repositoryMonitorShortHash(command.ID),
	}
}

func (h *repairValidationRetryHarness) job() *store.RepairJob {
	h.t.Helper()
	job, err := h.store.GetRepairJob(h.ctx, h.monitor.Namespace, h.jobID)
	if err != nil {
		h.t.Fatal(err)
	}
	return job
}

func (h *repairValidationRetryHarness) task(name string) *corev1alpha1.Task {
	h.t.Helper()
	var task corev1alpha1.Task
	if err := h.client.Get(h.ctx, types.NamespacedName{Namespace: h.monitor.Namespace, Name: name}, &task); err != nil {
		h.t.Fatalf("Get Task %s: %v", name, err)
	}
	return &task
}

func (h *repairValidationRetryHarness) failValidation(name string, reason corev1alpha1.TaskDeliveryReason, message string) {
	h.t.Helper()
	task := h.task(name)
	task.Status.Phase = corev1alpha1.TaskPhaseFailed
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{
		State: corev1alpha1.TaskDeliveryStateDeliveryConflict, Outcome: corev1alpha1.TaskDeliveryOutcomeDeliveryConflict,
		Reason: reason, Message: message,
	}
	if err := h.client.Status().Update(h.ctx, task); err != nil {
		h.t.Fatal(err)
	}
}

func (h *repairValidationRetryHarness) ingest() {
	h.t.Helper()
	if _, err := h.reconciler.ingestCompletedRepositoryMonitorRepairTasks(h.ctx, h.monitor); err != nil {
		h.t.Fatalf("ingestCompletedRepositoryMonitorRepairTasks() error = %v", err)
	}
}

func (h *repairValidationRetryHarness) pushMutation() *store.GitHubMutationRecord {
	h.t.Helper()
	mutation, err := h.store.GetGitHubMutationRecord(h.ctx, h.monitor.Namespace, "ghmut-"+repositoryMonitorShortHash(h.jobID+"-push"))
	if err != nil {
		h.t.Fatal(err)
	}
	return mutation
}

//nolint:gocyclo // One ordered scenario: fail, retry, pass, ingest.
func TestRepositoryMonitorRepairValidationFailThenPass(t *testing.T) {
	h := newRepairValidationRetryHarness(t, "repair-validation-retry", repairValidationTestCommand, nil)
	first := h.job().TaskName
	firstTask := h.task(first)
	h.failValidation(first, repositoryMonitorRepairValidationFailedDeliveryReason, "Validation command exited with status 2.")
	h.ingest()

	job := h.job()
	wantRetry := repositoryMonitorRepairValidationRetryTaskName(first, 1)
	if job.Phase != repositoryMonitorRepairPhaseQueued || job.TaskName != wantRetry || job.ValidationAttempts != 1 || job.LastError != "" {
		t.Fatalf("job after failed validation = %+v, want queued retry %s", job, wantRetry)
	}
	if mutation := h.pushMutation(); mutation.Status == repositoryMonitorRunPhaseFailed || mutation.Status == repositoryMonitorRunPhaseSucceeded {
		t.Fatalf("push mutation was terminalized on a retried repair: %+v", mutation)
	}
	retry := h.task(wantRetry)
	if retry.Annotations[repositoryMonitorRepairValidationAttemptAnnotation] != "1" ||
		retry.Annotations[repositoryMonitorRepairValidationOriginalTaskAnnotation] != first {
		t.Fatalf("retry annotations = %v", retry.Annotations)
	}
	if !metav1.IsControlledBy(retry, h.monitor) {
		t.Fatal("retry Task is not controlled by the monitor")
	}
	if retry.Spec.SessionRef == nil || firstTask.Spec.SessionRef == nil || *retry.Spec.SessionRef != *firstTask.Spec.SessionRef {
		t.Fatalf("retry session = %#v, want %#v", retry.Spec.SessionRef, firstTask.Spec.SessionRef)
	}
	if !strings.Contains(retry.Spec.Prompt, repairValidationTestCommand) || !strings.Contains(retry.Spec.Prompt, "exited with status 2") ||
		!strings.Contains(retry.Spec.Prompt, "Nothing was pushed") {
		t.Fatalf("retry prompt lacks validation feedback: %q", retry.Spec.Prompt)
	}
	if _, ok := repositoryMonitorRepairPromptInput(retry.Spec.Prompt); !ok {
		t.Fatal("retry prompt input is no longer parseable")
	}
	if repositoryMonitorRepairPromptWithoutValidationFeedback(retry.Spec.Prompt) != firstTask.Spec.Prompt {
		t.Fatal("stripping the feedback section does not restore the original prompt")
	}
	if item, err := h.store.GetMonitorItem(h.ctx, h.monitor.Namespace, h.monitor.Name, repositoryMonitorPullRequestKind, "41"); err != nil || item.RepairState != repositoryMonitorRepairPhaseQueued {
		t.Fatalf("item = %+v, %v; want queued repair", item, err)
	}

	// Re-ingesting before the retry finishes is a no-op.
	h.ingest()
	if got := h.job(); got.TaskName != wantRetry || got.Phase != repositoryMonitorRepairPhaseQueued {
		t.Fatalf("job changed while retry is running: %+v", got)
	}

	markRepositoryMonitorTestTaskDelivered(t, h.ctx, h.client, wantRetry, h.pr.HeadBranch, "pushed41")
	h.ingest()
	job = h.job()
	if job.Phase != repositoryMonitorRepairPhaseSucceeded || job.PushedSHA != "pushed41" || job.LastError != "" {
		t.Fatalf("job after passing retry = %+v", job)
	}
	if mutation := h.pushMutation(); mutation.Status != repositoryMonitorRunPhaseSucceeded || mutation.ExternalID != "pushed41" {
		t.Fatalf("push mutation = %+v", mutation)
	}
	// The pushed repair keeps its pre-push failure in the shared budget.
	prePush, err := h.reconciler.repositoryMonitorRepairJobPrePushValidationFailures(h.ctx, h.monitor, job)
	if err != nil || prePush != 1 {
		t.Fatalf("pre-push failures = %d, %v; want 1", prePush, err)
	}
	attempts, err := h.reconciler.repositoryMonitorRepairValidationAttemptCount(h.ctx, h.monitor, job)
	if err != nil || attempts != 1 {
		t.Fatalf("shared validation attempts = %d, %v; want 1", attempts, err)
	}
}

func TestRepositoryMonitorRepairValidationExhaustsBudget(t *testing.T) {
	maxRetries := int32(1)
	h := newRepairValidationRetryHarness(t, "repair-validation-exhausted", repairValidationTestCommand, &maxRetries)
	first := h.job().TaskName
	h.failValidation(first, repositoryMonitorRepairValidationFailedDeliveryReason, "Validation command exited with status 1.")
	h.ingest()
	retry := h.job().TaskName
	if retry == first {
		t.Fatal("first failure did not create a retry")
	}
	h.failValidation(retry, repositoryMonitorRepairValidationFailedDeliveryReason, "Validation command exited with status 1.")
	h.ingest()

	job := h.job()
	if job.Phase != repositoryMonitorRepairPhaseFailed || job.LastError != repositoryMonitorRepairValidationFailedError ||
		job.PushedSHA != "" || job.ValidationAttempts != 2 || job.TaskName != retry {
		t.Fatalf("job after exhausted budget = %+v", job)
	}
	var tasks corev1alpha1.TaskList
	if err := h.client.List(h.ctx, &tasks); err != nil || len(tasks.Items) != 2 {
		t.Fatalf("tasks = %d, %v; want no further retry", len(tasks.Items), err)
	}
	if mutation := h.pushMutation(); mutation.Status != repositoryMonitorRunPhaseFailed || mutation.ExternalID != "" ||
		mutation.Error != repositoryMonitorRepairValidationFailedError {
		t.Fatalf("push mutation = %+v", mutation)
	}
	item, err := h.store.GetMonitorItem(h.ctx, h.monitor.Namespace, h.monitor.Name, repositoryMonitorPullRequestKind, "41")
	if err != nil || item.RepairState != repositoryMonitorRepairPhaseFailed {
		t.Fatalf("item = %+v, %v; want failed repair", item, err)
	}
	status, description, err := h.reconciler.repositoryMonitorReadyOutcome(h.ctx, h.monitor, h.pr, item)
	if err != nil || status != repositoryMonitorStatusFailure ||
		description != "Repair did not pass spec.validation.repairCommand after 2 attempts; nothing was pushed." {
		t.Fatalf("readiness = %q %q %v", status, description, err)
	}
	if len(description) > 140 {
		t.Fatalf("readiness description exceeds the GitHub status limit: %d", len(description))
	}
}

func TestRepositoryMonitorRepairValidationUnavailableFailsClosed(t *testing.T) {
	h := newRepairValidationRetryHarness(t, "repair-validation-unavailable", repairValidationTestCommand, nil)
	first := h.job().TaskName
	h.failValidation(first, repositoryMonitorRepairValidationUnavailableDeliveryReason, "Validation image could not be pulled.")
	h.ingest()

	job := h.job()
	if job.Phase != repositoryMonitorRepairPhaseFailed || job.LastError != repositoryMonitorRepairValidationUnavailableError ||
		job.PushedSHA != "" || job.TaskName != first {
		t.Fatalf("job after unavailable validation = %+v", job)
	}
	var tasks corev1alpha1.TaskList
	if err := h.client.List(h.ctx, &tasks); err != nil || len(tasks.Items) != 1 {
		t.Fatalf("tasks = %d, %v; want no retry", len(tasks.Items), err)
	}
	item, err := h.store.GetMonitorItem(h.ctx, h.monitor.Namespace, h.monitor.Name, repositoryMonitorPullRequestKind, "41")
	if err != nil {
		t.Fatal(err)
	}
	status, description, err := h.reconciler.repositoryMonitorReadyOutcome(h.ctx, h.monitor, h.pr, item)
	if err != nil || status != repositoryMonitorStatusFailure || description != "Repair validation could not run; nothing was pushed." {
		t.Fatalf("readiness = %q %q %v", status, description, err)
	}
}

// A monitor without repairCommand ingests a delivered repair exactly as
// before: no retry Task, no validation annotations, no validation failures.
func TestRepositoryMonitorRepairWithoutRepairCommandIsUnchanged(t *testing.T) {
	h := newRepairValidationRetryHarness(t, "repair-without-command", "", nil)
	first := h.job().TaskName
	task := h.task(first)
	for key := range task.Annotations {
		if strings.Contains(key, "validation") {
			t.Fatalf("ungated repair Task carries validation annotation %q", key)
		}
	}
	markRepositoryMonitorTestTaskDelivered(t, h.ctx, h.client, first, h.pr.HeadBranch, "pushed41")
	h.ingest()
	job := h.job()
	if job.Phase != repositoryMonitorRepairPhaseSucceeded || job.PushedSHA != "pushed41" || job.LastError != "" ||
		job.ValidationAttempts != 0 || job.TaskName != first {
		t.Fatalf("job = %+v", job)
	}
	var tasks corev1alpha1.TaskList
	if err := h.client.List(h.ctx, &tasks); err != nil || len(tasks.Items) != 1 {
		t.Fatalf("tasks = %d, %v; want only the repair Task", len(tasks.Items), err)
	}
	if prePush, err := h.reconciler.repositoryMonitorRepairJobPrePushValidationFailures(h.ctx, h.monitor, job); err != nil || prePush != 0 {
		t.Fatalf("pre-push failures = %d, %v", prePush, err)
	}
}

func TestRepositoryMonitorRepairValidationRetryNames(t *testing.T) {
	long := "monrepair-" + strings.Repeat("x", 80)
	for failures := 1; failures <= 12; failures++ {
		name := repositoryMonitorRepairValidationRetryTaskName(long, failures)
		if len(name) > 63 || !repositoryMonitorRepairValidationRetryNamePattern.MatchString(name) {
			t.Fatalf("retry name %q is invalid", name)
		}
	}
	if got := repositoryMonitorRepairValidationRetryTaskName("monrepair-a-1-cmd", 2); got != "monrepair-a-1-cmd-rv2" {
		t.Fatalf("retry name = %q", got)
	}
	prompt := "Fix it." + repositoryMonitorRepairPromptInputMarker + `{"headSHA":"h"}`
	once := repositoryMonitorRepairPromptWithValidationFeedback(prompt, "make test", "first", 1, 2)
	twice := repositoryMonitorRepairPromptWithValidationFeedback(once, "make test", "second", 2, 2)
	if strings.Contains(twice, "first") || strings.Count(twice, repositoryMonitorRepairValidationFeedbackStart) != 1 ||
		!strings.HasSuffix(twice, repositoryMonitorRepairPromptInputMarker+`{"headSHA":"h"}`) {
		t.Fatalf("feedback was not replaced: %q", twice)
	}
}

// The command's output is hidden, so the retry prompt must carry the whole
// accepted command, not a prefix.
func TestRepositoryMonitorRepairValidationFeedbackKeepsTheFullCommand(t *testing.T) {
	command := "make lint && " + strings.Repeat("go test ./pkg/x/... && ", 200) + "make e2e-last-check"
	if !tools.ValidRepositoryRepairValidationCommand(command) {
		t.Fatalf("test command of %d bytes is not an accepted repairCommand", len(command))
	}
	prompt := repositoryMonitorRepairPromptWithValidationFeedback("Fix the PR."+repositoryMonitorRepairPromptInputMarker+"{}", command, "exit 1", 1, 2)
	if !strings.Contains(prompt, command) {
		t.Fatalf("retry prompt does not contain the full %d-byte command", len(command))
	}
}

func TestRepositoryMonitorRepairValidationRetryDropsRuntimeMetadata(t *testing.T) {
	h := newRepairValidationRetryHarness(t, "repair-validation-retry-metadata", repairValidationTestCommand, nil)
	first := h.job().TaskName
	created := h.task(first)
	wantLabels := maps.Clone(created.Labels)
	wantAnnotations := maps.Clone(created.Annotations)
	// Every key the monitor sets at creation must survive a retry.
	for key := range wantLabels {
		if !slices.Contains(repositoryMonitorRepairTaskLabelKeys, key) {
			t.Fatalf("repair Task label %q is not carried to a validation retry", key)
		}
	}
	for key := range wantAnnotations {
		if !slices.Contains(repositoryMonitorRepairTaskAnnotationKeys, key) {
			t.Fatalf("repair Task annotation %q is not carried to a validation retry", key)
		}
	}

	// Metadata the attempt picked up while it ran belongs to that attempt.
	created.Labels["acp.workspace.orka.ai/runtime-pool"] = "pool-a"
	created.Annotations["acp.workspace.orka.ai/attachment-epoch"] = "3"
	created.Annotations["acp.workspace.orka.ai/last-settled-task-uid"] = string(created.UID)
	created.Annotations[labels.AnnotationCancelRequested] = "true"
	created.Annotations[labels.AnnotationTransactionTokenSecret] = "attempt-token"
	created.Annotations[labels.AnnotationTraceParent] = "00-0123456789abcdef0123456789abcdef-0123456789abcdef-01"
	if err := h.client.Update(h.ctx, created); err != nil {
		t.Fatal(err)
	}
	h.failValidation(first, repositoryMonitorRepairValidationFailedDeliveryReason, "Validation command exited with status 2.")
	h.ingest()

	retry := h.task(repositoryMonitorRepairValidationRetryTaskName(first, 1))
	wantAnnotations[repositoryMonitorRepairValidationAttemptAnnotation] = "1"
	wantAnnotations[repositoryMonitorRepairValidationOriginalTaskAnnotation] = first
	if !maps.Equal(retry.Labels, wantLabels) {
		t.Fatalf("retry labels = %v, want %v", retry.Labels, wantLabels)
	}
	if !maps.Equal(retry.Annotations, wantAnnotations) {
		t.Fatalf("retry annotations = %v, want %v", retry.Annotations, wantAnnotations)
	}
}
