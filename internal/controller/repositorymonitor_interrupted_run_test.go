/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
)

// heldControllerEpochManager returns an epoch manager that holds epoch as
// this process's current controller epoch.
func heldControllerEpochManager(epoch int64) *ControllerEpochManager {
	ready := make(chan struct{})
	close(ready)
	return &ControllerEpochManager{
		Name:     store.DefaultControllerEpochName,
		HolderID: "controller-current",
		current:  &store.ControllerEpoch{Name: store.DefaultControllerEpochName, Epoch: epoch, HolderID: "controller-current"},
		ready:    ready,
	}
}

func newInterruptedRunTestReconciler(t *testing.T, name string, epochs *ControllerEpochManager, githubURL string) (*RepositoryMonitorReconciler, *storageclient.Client, *corev1alpha1.RepositoryMonitor) {
	t.Helper()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta:   metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL:      repositoryMonitorTestRepoURL,
			Validation:   corev1alpha1.RepositoryMonitorValidationSpec{Image: repositoryMonitorValidationTestImage},
			GitSecretRef: &corev1.LocalObjectReference{Name: "github-token"},
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "default"},
		Data:       map[string][]byte{"token": []byte("test-token")},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	return &RepositoryMonitorReconciler{
		Client:                 cl,
		Scheme:                 scheme,
		Store:                  monitorStore,
		ControllerEpochManager: epochs,
		GitHubAPIBaseURL:       githubURL,
	}, monitorStore, monitor
}

func createRunningMonitorRun(t *testing.T, monitorStore *storageclient.Client, monitorName, runID string) {
	t.Helper()
	if err := monitorStore.CreateMonitorRun(context.Background(), &store.MonitorRun{
		ID:               runID,
		MonitorNamespace: "default",
		MonitorName:      monitorName,
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseRunning,
		StartedAt:        time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
}

func interruptedRunEvent(t *testing.T, monitorStore *storageclient.Client, monitorName, runID string) (store.MonitorEvent, map[string]any) {
	t.Helper()
	events, _, err := monitorStore.ListMonitorEvents(context.Background(), store.MonitorEventFilter{
		Namespace: "default", MonitorName: monitorName, RunID: runID, EventType: repositoryMonitorRunFailurePermanent, Limit: 10,
	})
	if err != nil {
		t.Fatalf("ListMonitorEvents() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("run_failed events = %#v, want exactly one", events)
	}
	metadata := map[string]any{}
	if err := json.Unmarshal([]byte(events[0].MetadataJSON), &metadata); err != nil {
		t.Fatalf("event metadata %q: %v", events[0].MetadataJSON, err)
	}
	return events[0], metadata
}

func TestRepositoryMonitorReconcileReclaimsRunFromPreviousControllerEpoch(t *testing.T) {
	ctx := context.Background()
	reconciler, monitorStore, monitor := newInterruptedRunTestReconciler(t, "previous-epoch", heldControllerEpochManager(7), "")
	// The run is running in storage, but this process, which holds the
	// current epoch, is not executing it: an earlier epoch claimed it.
	createRunningMonitorRun(t, monitorStore, monitor.Name, "orphan-run")

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "orphan-run")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	wantError := repositoryMonitorInterruptedRunError + ": no live controller is executing it; reclaimed by controller epoch 7 and marked failed"
	if run.Phase != repositoryMonitorRunPhaseFailed || run.CompletedAt == nil || run.Error != wantError {
		t.Fatalf("run = %#v, want failed with error %q", run, wantError)
	}
	if !repositoryMonitorFailedCommandRunRetryable(run.Error) || !repositoryMonitorRunErrorNeedsOutcomeVerification(run.Error) {
		t.Fatalf("run error %q should stay retryable and require outcome verification", run.Error)
	}
	event, metadata := interruptedRunEvent(t, monitorStore, monitor.Name, "orphan-run")
	if event.Summary != wantError {
		t.Fatalf("event summary = %q, want %q", event.Summary, wantError)
	}
	if metadata[eventReasonField] != "interrupted_run" || metadata["controllerEpoch"] != float64(7) {
		t.Fatalf("event metadata = %#v, want interrupted_run reclaimed by epoch 7", metadata)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: monitor.Name}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError || current.Status.LastRunID != "orphan-run" {
		t.Fatalf("status = %#v, want Error with orphan-run as last run", current.Status)
	}
	active, err := reconciler.activeMonitorRunExists(ctx, monitor)
	if err != nil {
		t.Fatalf("activeMonitorRunExists() error = %v", err)
	}
	if active {
		t.Fatal("monitor still has an active run, want the next run free to start")
	}
}

func TestRepositoryMonitorReconcileKeepsRunExecutingInCurrentControllerEpoch(t *testing.T) {
	ctx := context.Background()
	reconciler, monitorStore, monitor := newInterruptedRunTestReconciler(t, "current-epoch", heldControllerEpochManager(7), "")
	createRunningMonitorRun(t, monitorStore, monitor.Name, "live-run")
	key := repositoryMonitorRunKey("default", monitor.Name, "live-run")
	reconciler.markMonitorRunActive(key)

	staleRun, requeueAfter, err := reconciler.failStaleRunningMonitorRun(ctx, monitor)
	if err != nil {
		t.Fatalf("failStaleRunningMonitorRun() error = %v", err)
	}
	if staleRun != nil || requeueAfter <= 0 || requeueAfter > repositoryMonitorRunningRunTimeout {
		t.Fatalf("failStaleRunningMonitorRun() = %#v, %v; want live run kept with the timeout backstop", staleRun, requeueAfter)
	}
	run, err := monitorStore.GetMonitorRun(ctx, "default", "live-run")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseRunning || run.CompletedAt != nil || run.Error != "" {
		t.Fatalf("run = %#v, want still running", run)
	}

	// Once the execution in this process returns without recording
	// completion, nothing executes the run any more and it is reclaimed.
	reconciler.markMonitorRunInactive(key)
	staleRun, _, err = reconciler.failStaleRunningMonitorRun(ctx, monitor)
	if err != nil {
		t.Fatalf("failStaleRunningMonitorRun() after execution ended error = %v", err)
	}
	if staleRun == nil || staleRun.Phase != repositoryMonitorRunPhaseFailed || !strings.HasPrefix(staleRun.Error, repositoryMonitorInterruptedRunError) {
		t.Fatalf("failStaleRunningMonitorRun() = %#v, want run reclaimed as interrupted", staleRun)
	}
}

func TestRepositoryMonitorKeepsRunningRunUntilTimeoutWithoutControllerEpoch(t *testing.T) {
	ctx := context.Background()
	// Without a held controller epoch this process cannot rule out a live
	// executor elsewhere, so only the timeout backstop applies.
	reconciler, monitorStore, monitor := newInterruptedRunTestReconciler(t, "no-epoch", &ControllerEpochManager{ready: make(chan struct{})}, "")
	createRunningMonitorRun(t, monitorStore, monitor.Name, "unknown-run")

	staleRun, requeueAfter, err := reconciler.failStaleRunningMonitorRun(ctx, monitor)
	if err != nil {
		t.Fatalf("failStaleRunningMonitorRun() error = %v", err)
	}
	if staleRun != nil || requeueAfter <= 0 {
		t.Fatalf("failStaleRunningMonitorRun() = %#v, %v; want run kept until the timeout", staleRun, requeueAfter)
	}
	run, err := monitorStore.GetMonitorRun(ctx, "default", "unknown-run")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseRunning {
		t.Fatalf("run phase = %q, want running", run.Phase)
	}
}

func TestRepositoryMonitorRecordsTerminalStateWhenRunContextIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// The GitHub API call cancels the run's context, as a controller shutdown
	// does in the middle of a run.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cancel()
		<-r.Context().Done()
	}))
	t.Cleanup(server.Close)
	reconciler, monitorStore, monitor := newInterruptedRunTestReconciler(t, "cancelled", heldControllerEpochManager(3), server.URL)
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "cancelled-run",
		MonitorNamespace: "default",
		MonitorName:      monitor.Name,
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	processed, _, err := reconciler.processNextQueuedMonitorRun(ctx, monitor, "orka-agents", "orka")
	if err != nil {
		t.Fatalf("processNextQueuedMonitorRun() error = %v, want the interruption recorded", err)
	}
	if ctx.Err() == nil {
		t.Fatal("run context was not cancelled; the test did not exercise the interruption")
	}
	if processed == nil || processed.ID != "cancelled-run" || processed.Phase != repositoryMonitorRunPhaseFailed {
		t.Fatalf("processed run = %#v, want cancelled-run failed", processed)
	}
	run, err := monitorStore.GetMonitorRun(context.Background(), "default", "cancelled-run")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseFailed || run.CompletedAt == nil ||
		!strings.HasPrefix(run.Error, repositoryMonitorInterruptedRunError+": ") || !strings.Contains(run.Error, "context canceled") {
		t.Fatalf("run = %#v, want failed as interrupted by context cancellation", run)
	}
	event, metadata := interruptedRunEvent(t, monitorStore, monitor.Name, "cancelled-run")
	if event.Summary != run.Error || metadata[eventReasonField] != "interrupted_run" {
		t.Fatalf("event = %#v metadata %#v, want interrupted_run event with the run error", event, metadata)
	}
	if reconciler.monitorRunActive(repositoryMonitorRunKey("default", monitor.Name, "cancelled-run")) {
		t.Fatal("run is still registered as executing in this process")
	}
}

func TestRepositoryMonitorRecordInterruptedRunLeavesTerminalRunUnchanged(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reconciler, monitorStore, monitor := newInterruptedRunTestReconciler(t, "terminal", heldControllerEpochManager(3), "")
	completedAt := time.Now().Add(-time.Second).Round(time.Second)
	if err := monitorStore.CreateMonitorRun(context.Background(), &store.MonitorRun{
		ID:               "done-run",
		MonitorNamespace: "default",
		MonitorName:      monitor.Name,
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseSucceeded,
		StartedAt:        completedAt.Add(-time.Minute),
		CompletedAt:      &completedAt,
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if recorded := reconciler.recordInterruptedMonitorRun(ctx, monitor, "done-run", context.Canceled); recorded != nil {
		t.Fatalf("recordInterruptedMonitorRun() = %#v, want nil for a run that already completed", recorded)
	}
	run, err := monitorStore.GetMonitorRun(context.Background(), "default", "done-run")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseSucceeded || run.Error != "" {
		t.Fatalf("run = %#v, want succeeded run unchanged", run)
	}
}
