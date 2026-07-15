//nolint:goconst
package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

const (
	repositoryMonitorTestDefaultBranch  = "main"
	repositoryMonitorTestRepoURL        = "https://github.com/orka-agents/orka"
	repositoryMonitorTestReviewerSecret = "reviewer-credentials"
	repositoryMonitorTestHeadSHA        = "sha1"
)

func TestRepositoryMonitorReconcileRecordsMetadataAndStatus(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orka",
			Namespace: "default",
			UID:       "uid-1",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Branch:  repositoryMonitorTestDefaultBranch,
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "orka"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	record, err := monitorStore.GetRepositoryMonitor(ctx, "default", "orka")
	if err != nil {
		t.Fatalf("GetRepositoryMonitor() error = %v", err)
	}
	if record.Owner != "orka-agents" || record.Repository != "orka" || record.Branch != repositoryMonitorTestDefaultBranch {
		t.Fatalf("record = %#v, want parsed repo metadata", record)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "orka"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseReady {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseReady)
	}
	if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != "MetadataRecorded" {
		t.Fatalf("conditions = %#v, want MetadataRecorded", current.Status.Conditions)
	}
}

func TestRepositoryMonitorReconcileSkipsNoOpIdleStatusPatch(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "idle",
			Namespace: "default",
			UID:       "uid-idle",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	countingClient := &statusPatchCountingClient{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
			WithObjects(repositoryMonitorControllerObjects(monitor)...).
			Build(),
	}
	reconciler := &RepositoryMonitorReconciler{Client: countingClient, Scheme: scheme, Store: monitorStore}
	req := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "idle"}}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("first Reconcile() error = %v", err)
	}
	if countingClient.statusPatchCount != 1 {
		t.Fatalf("statusPatchCount after first reconcile = %d, want 1", countingClient.statusPatchCount)
	}

	if _, err := reconciler.Reconcile(ctx, req); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if countingClient.statusPatchCount != 1 {
		t.Fatalf("statusPatchCount after second reconcile = %d, want no additional status patch", countingClient.statusPatchCount)
	}
}

func TestRepositoryMonitorReconcileQueuesDueScheduledRun(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "scheduled",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL:  "https://github.com/orka-agents/orka",
			Branch:   repositoryMonitorTestDefaultBranch,
			Schedule: "* * * * *",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "scheduled"}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want positive", result.RequeueAfter)
	}

	runs, _, err := monitorStore.ListMonitorRuns(ctx, storeMonitorRunFilter("default", "scheduled"))
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].Trigger != "schedule" || runs[0].Phase != repositoryMonitorRunPhaseQueued {
		t.Fatalf("runs = %#v, want one queued schedule run", runs)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "scheduled"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.LastRunID != runs[0].ID {
		t.Fatalf("LastRunID = %q, want %q", current.Status.LastRunID, runs[0].ID)
	}
}

func TestRepositoryMonitorReconcileDoesNotQueueScheduledRunWhenActiveRunExists(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:              "scheduled",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-2 * time.Hour)),
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL:  "https://github.com/orka-agents/orka",
			Branch:   repositoryMonitorTestDefaultBranch,
			Schedule: "* * * * *",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "active-run",
		MonitorNamespace: "default",
		MonitorName:      "scheduled",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseRunning,
		StartedAt:        time.Now().Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "scheduled"}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want positive", result.RequeueAfter)
	}

	runs, _, err := monitorStore.ListMonitorRuns(ctx, storeMonitorRunFilter("default", "scheduled"))
	if err != nil {
		t.Fatalf("ListMonitorRuns() error = %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "active-run" {
		t.Fatalf("runs = %#v, want only existing active run", runs)
	}
}

func TestRepositoryMonitorReconcileProcessesQueuedRunBeforeInvalidSchedule(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithoutAuth(t, `[
		{"number":1,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1"},"head":{"ref":"feature","sha":"sha1"},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid-schedule-manual",
			Namespace: "default",
			UID:       "uid-invalid-schedule-manual",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL:  "https://github.com/orka-agents/orka",
			Schedule: "not a schedule",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "manual-run",
		MonitorNamespace: "default",
		MonitorName:      "invalid-schedule-manual",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "invalid-schedule-manual"}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want positive", result.RequeueAfter)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "manual-run")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseSucceeded || run.CompletedAt == nil {
		t.Fatalf("run = %#v, want queued manual run processed before invalid schedule status", run)
	}

	for _, phase := range []string{repositoryMonitorRunPhaseQueued, repositoryMonitorRunPhaseRunning} {
		activeRuns, _, err := monitorStore.ListMonitorRuns(ctx, store.MonitorRunFilter{
			Namespace:   "default",
			MonitorName: "invalid-schedule-manual",
			Phase:       phase,
			Limit:       1,
		})
		if err != nil {
			t.Fatalf("ListMonitorRuns(%s) error = %v", phase, err)
		}
		if len(activeRuns) != 0 {
			t.Fatalf("%s runs = %#v, want none", phase, activeRuns)
		}
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "invalid-schedule-manual"}}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "invalid-schedule-manual"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseError)
	}
	if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != "InvalidSchedule" {
		t.Fatalf("conditions = %#v, want InvalidSchedule", current.Status.Conditions)
	}
}

func TestRepositoryMonitorReconcilePreservesLatestFailedRunStatus(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "failed-idle",
			Namespace: "default",
			UID:       "uid-failed-idle",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
		Status: corev1alpha1.RepositoryMonitorStatus{Phase: repositoryMonitorPhaseReady},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()

	completedAt := time.Now().Add(-1 * time.Minute)
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "failed-run",
		MonitorNamespace: "default",
		MonitorName:      "failed-idle",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseFailed,
		StartedAt:        completedAt.Add(-1 * time.Minute),
		CompletedAt:      &completedAt,
		Error:            "inventory unavailable",
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "failed-idle"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "failed-idle"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseError)
	}
	if current.Status.LastRunID != "failed-run" {
		t.Fatalf("LastRunID = %q, want failed-run", current.Status.LastRunID)
	}
	if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != "RunFailed" {
		t.Fatalf("conditions = %#v, want RunFailed", current.Status.Conditions)
	}
}

func TestRepositoryMonitorReconcileReplaysLatestSuccessfulRunStatus(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "succeeded-idle",
			Namespace: "default",
			UID:       "uid-succeeded-idle",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
		Status: corev1alpha1.RepositoryMonitorStatus{Phase: repositoryMonitorPhasePending},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()

	completedAt := time.Now().Add(-1 * time.Minute).Round(time.Second)
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "succeeded-run",
		MonitorNamespace: "default",
		MonitorName:      "succeeded-idle",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseSucceeded,
		StartedAt:        completedAt.Add(-1 * time.Minute),
		CompletedAt:      &completedAt,
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "succeeded-idle",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		State:            repositoryMonitorItemStateOpen,
		LastVerdict:      repositoryMonitorRunPhaseQueued,
	}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "succeeded-idle"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "succeeded-idle"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseReady {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseReady)
	}
	if current.Status.LastRunID != "succeeded-run" {
		t.Fatalf("LastRunID = %q, want succeeded-run", current.Status.LastRunID)
	}
	if current.Status.LastRunTime == nil || !current.Status.LastRunTime.Time.Equal(completedAt) {
		t.Fatalf("LastRunTime = %#v, want %s", current.Status.LastRunTime, completedAt)
	}
	if current.Status.LastSuccessfulRunTime == nil || !current.Status.LastSuccessfulRunTime.Time.Equal(completedAt) {
		t.Fatalf("LastSuccessfulRunTime = %#v, want %s", current.Status.LastSuccessfulRunTime, completedAt)
	}
	if current.Status.OpenPullRequests != 1 || current.Status.PendingReviews != 1 {
		t.Fatalf("status counts = %#v, want open=1 pending=1", current.Status)
	}
	if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != "RunSucceeded" {
		t.Fatalf("conditions = %#v, want RunSucceeded", current.Status.Conditions)
	}
}

func TestRepositoryMonitorReconcileRecoversStaleRunningRun(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "stale-running",
			Namespace: "default",
			UID:       "uid-stale-running",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "stale-run",
		MonitorNamespace: "default",
		MonitorName:      "stale-running",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseRunning,
		StartedAt:        time.Now().Add(-repositoryMonitorRunningRunTimeout - time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "stale-running"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "stale-run")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseFailed || run.CompletedAt == nil || !strings.Contains(run.Error, "did not complete") {
		t.Fatalf("run = %#v, want stale running run marked failed", run)
	}
	activeRuns, _, err := monitorStore.ListMonitorRuns(ctx, store.MonitorRunFilter{
		Namespace:   "default",
		MonitorName: "stale-running",
		Phase:       repositoryMonitorRunPhaseRunning,
		Limit:       1,
	})
	if err != nil {
		t.Fatalf("ListMonitorRuns(running) error = %v", err)
	}
	if len(activeRuns) != 0 {
		t.Fatalf("running runs = %#v, want none", activeRuns)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "stale-running"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError || current.Status.LastRunID != "stale-run" {
		t.Fatalf("status = %#v, want Error with stale-run as last run", current.Status)
	}
}

func TestRepositoryMonitorReconcileKeepsFreshRunningRunActive(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fresh-running",
			Namespace: "default",
			UID:       "uid-fresh-running",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "fresh-run",
		MonitorNamespace: "default",
		MonitorName:      "fresh-running",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseRunning,
		StartedAt:        time.Now().Add(-time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "fresh-running"}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want positive", result.RequeueAfter)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "fresh-run")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseRunning || run.CompletedAt != nil || run.Error != "" {
		t.Fatalf("run = %#v, want fresh running run left active", run)
	}
}

func TestRepositoryMonitorReconcileMarksRunFailedWhenStartEventWriteFails(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "event-failure",
			Namespace: "default",
			UID:       "uid-event-failure",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-event-failure",
		MonitorNamespace: "default",
		MonitorName:      "event-failure",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{
		Client: cl,
		Scheme: scheme,
		Store: failingMonitorEventStore{
			RepositoryMonitorStore: monitorStore,
			failEventType:          "run_started",
		},
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "event-failure"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-event-failure")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseFailed || run.CompletedAt == nil || !strings.Contains(run.Error, "audit event unavailable") {
		t.Fatalf("run = %#v, want failed run with audit event error", run)
	}
	activeRuns, _, err := monitorStore.ListMonitorRuns(ctx, store.MonitorRunFilter{
		Namespace:   "default",
		MonitorName: "event-failure",
		Phase:       repositoryMonitorRunPhaseRunning,
		Limit:       1,
	})
	if err != nil {
		t.Fatalf("ListMonitorRuns(running) error = %v", err)
	}
	if len(activeRuns) != 0 {
		t.Fatalf("running runs = %#v, want none", activeRuns)
	}
	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "event-failure"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseError)
	}
}

func TestRepositoryMonitorReconcileProcessesQueuedPRInventoryRun(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServer(t)
	t.Cleanup(server.Close)

	maxPerRun := int32(1)
	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "inventory",
			Namespace: "default",
			UID:       "uid-inventory",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Targets: corev1alpha1.RepositoryMonitorTargets{
				PullRequests: corev1alpha1.RepositoryMonitorPullRequestTarget{MaxPerRun: &maxPerRun},
			},
			GitSecretRef: &corev1.LocalObjectReference{Name: "github-token"},
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
			Policy: corev1alpha1.RepositoryMonitorPolicySpec{
				PauseLabels: []string{"orka:human-review"},
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
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-1",
		MonitorNamespace: "default",
		MonitorName:      "inventory",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace:    "default",
		MonitorName:         "inventory",
		Kind:                repositoryMonitorPullRequestKind,
		Number:              5,
		LastReviewedHeadSHA: "sha5",
		LastVerdict:         repositoryMonitorVerdictSkipped,
		SkipReason:          "blocked_label",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(existing) error = %v", err)
	}
	if err := monitorStore.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "review-record-pr-5-sha5",
		MonitorNamespace: "default",
		MonitorName:      "inventory",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           5,
		HeadSHA:          "sha5",
		Verdict:          repositoryMonitorReviewVerdictNeedsChanges,
	}); err != nil {
		t.Fatalf("CreateReviewRecord(existing) error = %v", err)
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "inventory"}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want positive", result.RequeueAfter)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-1")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseSucceeded || run.SelectedCount != 1 || run.CreatedTaskCount != 1 || run.SkippedCount != 4 {
		t.Fatalf("run = %#v, want succeeded with 1 selected, 1 created task, and 4 skipped", run)
	}

	assertRepositoryMonitorInventoryItems(t, ctx, monitorStore)
	assertRepositoryMonitorInventoryEvents(t, ctx, monitorStore)
	assertRepositoryMonitorReviewTask(t, ctx, cl, monitorStore)

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "inventory"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.OpenPullRequests != 5 || current.Status.PendingReviews != 1 || current.Status.BlockedItems != 4 {
		t.Fatalf("status counts = %#v, want open=5 pending=1 blocked=4", current.Status)
	}
	if current.Status.LastSuccessfulRunTime == nil {
		t.Fatal("LastSuccessfulRunTime is nil, want completed successful run time")
	}
}

func TestRepositoryMonitorReviewedHeadFreshHonorsStaleReviewTTL(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	ttl := metav1.Duration{Duration: time.Minute}
	monitor := &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: "ttl-review", Namespace: "default"},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			Review: corev1alpha1.RepositoryMonitorReviewSpec{StaleReviewTTL: &ttl},
		},
	}
	item := &store.MonitorItem{
		MonitorNamespace:    "default",
		MonitorName:         "ttl-review",
		Kind:                repositoryMonitorPullRequestKind,
		Number:              1,
		HeadSHA:             "sha1",
		LastReviewedHeadSHA: "sha1",
		UpdatedAt:           time.Now(),
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}

	fresh, err := reconciler.repositoryMonitorReviewedHeadFresh(ctx, monitor, item, "sha1")
	if err != nil {
		t.Fatalf("repositoryMonitorReviewedHeadFresh(no records) error = %v", err)
	}
	if fresh {
		t.Fatal("fresh = true, want reviewed head without an immutable review record to expire when staleReviewTTL is set")
	}

	if err := monitorStore.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "old-review-record",
		MonitorNamespace: "default",
		MonitorName:      "ttl-review",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		HeadSHA:          "sha1",
		Verdict:          repositoryMonitorReviewVerdictPassed,
		CreatedAt:        time.Now().Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateReviewRecord(old) error = %v", err)
	}
	fresh, err = reconciler.repositoryMonitorReviewedHeadFresh(ctx, monitor, item, "sha1")
	if err != nil {
		t.Fatalf("repositoryMonitorReviewedHeadFresh(old) error = %v", err)
	}
	if fresh {
		t.Fatal("fresh = true, want old review record to expire past staleReviewTTL")
	}

	if err := monitorStore.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "fresh-skipped-record",
		MonitorNamespace: "default",
		MonitorName:      "ttl-review",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		HeadSHA:          "sha1",
		Verdict:          repositoryMonitorVerdictSkipped,
		CreatedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("CreateReviewRecord(skipped) error = %v", err)
	}
	fresh, err = reconciler.repositoryMonitorReviewedHeadFresh(ctx, monitor, item, "sha1")
	if err != nil {
		t.Fatalf("repositoryMonitorReviewedHeadFresh(skipped) error = %v", err)
	}
	if fresh {
		t.Fatal("fresh = true, want skipped retry record not to refresh stale review TTL")
	}

	if err := monitorStore.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "fresh-review-record",
		MonitorNamespace: "default",
		MonitorName:      "ttl-review",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		HeadSHA:          "sha1",
		Verdict:          repositoryMonitorReviewVerdictPassed,
		CreatedAt:        time.Now(),
	}); err != nil {
		t.Fatalf("CreateReviewRecord(fresh) error = %v", err)
	}
	fresh, err = reconciler.repositoryMonitorReviewedHeadFresh(ctx, monitor, item, "sha1")
	if err != nil {
		t.Fatalf("repositoryMonitorReviewedHeadFresh(fresh) error = %v", err)
	}
	if !fresh {
		t.Fatal("fresh = false, want newest review record inside staleReviewTTL to stay fresh")
	}
}

func TestRepositoryMonitorReconcileExpiredReviewedHeadRespectsMaxPerRun(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":1,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"ready","sha":"sha1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]},
		{"number":2,"title":"Expired review","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base2","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"expired","sha":"sha2","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("ttl-capacity")
	maxPerRun := int32(1)
	ttl := metav1.Duration{Duration: time.Minute}
	monitor.Spec.Targets.PullRequests.MaxPerRun = &maxPerRun
	monitor.Spec.Review.StaleReviewTTL = &ttl
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace:    "default",
		MonitorName:         "ttl-capacity",
		Kind:                repositoryMonitorPullRequestKind,
		Number:              2,
		State:               repositoryMonitorItemStateOpen,
		HeadSHA:             "sha2",
		LastReviewedHeadSHA: "sha2",
		LastVerdict:         repositoryMonitorReviewVerdictPassed,
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(reviewed) error = %v", err)
	}
	if err := monitorStore.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "expired-review-record",
		MonitorNamespace: "default",
		MonitorName:      "ttl-capacity",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           2,
		HeadSHA:          "sha2",
		Verdict:          repositoryMonitorReviewVerdictPassed,
		CreatedAt:        time.Now().Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateReviewRecord(expired) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-ttl-capacity",
		MonitorNamespace: "default",
		MonitorName:      "ttl-capacity",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ttl-capacity"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-ttl-capacity")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.SelectedCount != 1 || run.CreatedTaskCount != 1 || run.SkippedCount != 1 {
		t.Fatalf("run = %#v, want one selected task and one over-limit stale reviewed item", run)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "ttl-capacity", repositoryMonitorPullRequestKind, "2")
	if err != nil {
		t.Fatalf("GetMonitorItem(expired) error = %v", err)
	}
	if item.SkipReason != repositoryMonitorSkipReasonOverLimit || item.LastVerdict != repositoryMonitorVerdictSkipped {
		t.Fatalf("item = %#v, want expired reviewed item skipped over_limit after capacity is consumed", item)
	}
}

func TestRepositoryMonitorReconcileSkipsPendingReviewWithoutConsumingCapacity(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithoutAuth(t, `[
		{"number":1,"title":"Pending","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1"},"head":{"ref":"pending","sha":"sha1"},"labels":[]},
		{"number":2,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base2"},"head":{"ref":"ready","sha":"sha2"},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	maxPerRun := int32(1)
	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pending-review",
			Namespace: "default",
			UID:       "uid-pending-review",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Targets: corev1alpha1.RepositoryMonitorTargets{
				PullRequests: corev1alpha1.RepositoryMonitorPullRequestTarget{MaxPerRun: &maxPerRun},
			},
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	pendingReviewTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pending-review-task",
			Namespace: "default",
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, pendingReviewTask)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "pending-review",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          "sha1",
		LastVerdict:      repositoryMonitorRunPhaseQueued,
		LastReviewID:     "pending-review-task",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(pending) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-pending-review",
		MonitorNamespace: "default",
		MonitorName:      "pending-review",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pending-review"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-pending-review")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseSucceeded || run.SelectedCount != 1 || run.SkippedCount != 1 {
		t.Fatalf("run = %#v, want one pending skip and one new selection", run)
	}

	pending, err := monitorStore.GetMonitorItem(ctx, "default", "pending-review", repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatalf("GetMonitorItem(pending) error = %v", err)
	}
	if pending.LastVerdict != repositoryMonitorRunPhaseQueued || pending.SkipReason != repositoryMonitorSkipReasonPending {
		t.Fatalf("pending item = %#v, want queued review_pending", pending)
	}
	selected, err := monitorStore.GetMonitorItem(ctx, "default", "pending-review", repositoryMonitorPullRequestKind, "2")
	if err != nil {
		t.Fatalf("GetMonitorItem(selected) error = %v", err)
	}
	if selected.LastVerdict != repositoryMonitorRunPhaseQueued || selected.SkipReason != "" {
		t.Fatalf("selected item = %#v, want newly queued review", selected)
	}
}

func TestRepositoryMonitorReconcileCreatesTaskForQueuedItemMissingBackingTask(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithoutAuth(t, `[
		{"number":1,"title":"Previously queued","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1"},"head":{"ref":"queued","sha":"sha1"},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "queued-without-task",
			Namespace: "default",
			UID:       "uid-queued-without-task",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "queued-without-task",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          "sha1",
		LastVerdict:      repositoryMonitorRunPhaseQueued,
		LastReviewID:     "missing-review-task",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(queued) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-queued-without-task",
		MonitorNamespace: "default",
		MonitorName:      "queued-without-task",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "queued-without-task"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-queued-without-task")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.SelectedCount != 1 || run.CreatedTaskCount != 1 || run.SkippedCount != 0 {
		t.Fatalf("run = %#v, want selected item to get a backing task", run)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "queued-without-task", repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.LastReviewID == "" || item.LastReviewID == "missing-review-task" || item.SkipReason != "" {
		t.Fatalf("item = %#v, want fresh review task link without skip reason", item)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: item.LastReviewID}, &task); err != nil {
		t.Fatalf("Get review task %q error = %v", item.LastReviewID, err)
	}
}

func TestRepositoryMonitorReconcileFailsClosedOnReviewTaskNameCollision(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithoutAuth(t, `[
		{"number":1,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature","sha":"sha1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "collision",
			Namespace: "default",
			UID:       "uid-collision",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	run := &store.MonitorRun{
		ID:               "run-collision",
		MonitorNamespace: "default",
		MonitorName:      "collision",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}
	pr := repositoryMonitorPullRequest{
		Number:      1,
		HeadSHA:     "sha1",
		HeadRepo:    "orka-agents/orka",
		HeadRepoURL: "https://github.com/orka-agents/orka.git",
	}
	collidingTaskName := repositoryMonitorReviewTaskName(monitor, run, pr)
	collidingTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      collidingTaskName,
			Namespace: "default",
			Labels: map[string]string{
				labels.LabelManaged:           "true",
				labels.LabelCreatedBy:         "repository-monitor",
				labels.LabelRepositoryMonitor: labels.SelectorValue("collision"),
				labels.LabelMonitorRun:        labels.SelectorValue("run-collision"),
				labels.LabelGitHubRepository:  labels.SelectorValue("orka-agents/orka"),
				labels.LabelGitHubTarget:      labels.SelectorValue(repositoryMonitorPullRequestKind),
				labels.LabelGitHubNumber:      labels.SelectorValue("1"),
			},
			Annotations: map[string]string{
				labels.AnnotationRepositoryMonitorName:  "collision",
				labels.AnnotationMonitorRunID:           "run-collision",
				labels.AnnotationMonitorItemKind:        repositoryMonitorPullRequestKind,
				labels.AnnotationMonitorItemNumber:      "1",
				labels.AnnotationMonitorHeadSHA:         "sha1",
				labels.AnnotationGitHubRepository:       "orka-agents/orka",
				labels.AnnotationAgentReadOnly:          "true",
				labels.AnnotationWorkspaceInitContainer: "true",
			},
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(monitor, corev1alpha1.GroupVersion.WithKind("RepositoryMonitor")),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:     corev1alpha1.TaskTypeAgent,
			AgentRef: &corev1alpha1.AgentReference{Name: "spoofed-reviewer"},
			Prompt:   "spoofed review result",
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, collidingTask)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.CreateMonitorRun(ctx, run); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "collision"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	gotRun, err := monitorStore.GetMonitorRun(ctx, "default", "run-collision")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if gotRun.Phase != repositoryMonitorRunPhaseFailed || !strings.Contains(gotRun.Error, "spec does not match") {
		t.Fatalf("run = %#v, want failed run from unsafe review task collision", gotRun)
	}
}

func TestRepositoryMonitorReviewTaskReuseAllowsDefaultedTaskScheduleFields(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "defaulted-task",
			Namespace: "default",
			UID:       "uid-defaulted-task",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	run := &store.MonitorRun{
		ID:               "run-defaulted-task",
		MonitorNamespace: "default",
		MonitorName:      "defaulted-task",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}
	pr := repositoryMonitorPullRequest{
		Number:      1,
		BaseBranch:  repositoryMonitorTestDefaultBranch,
		HeadSHA:     "sha1",
		HeadRepo:    "orka-agents/orka",
		HeadRepoURL: "https://github.com/orka-agents/orka.git",
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client: cl,
		Scheme: scheme,
		Store:  setupControllerSQLiteStore(t),
	}

	taskName, created, err := reconciler.createRepositoryMonitorReviewTask(ctx, monitor, run, "orka-agents", "orka", pr)
	if err != nil {
		t.Fatalf("createRepositoryMonitorReviewTask() error = %v", err)
	}
	if !created {
		t.Fatal("createRepositoryMonitorReviewTask() created = false, want true")
	}

	var existing corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: taskName}, &existing); err != nil {
		t.Fatalf("Get review task() error = %v", err)
	}
	expected := existing.DeepCopy()
	startingDeadlineSeconds := int64(100)
	successfulRunsHistoryLimit := int32(3)
	failedRunsHistoryLimit := int32(1)
	existing.Spec.ConcurrencyPolicy = corev1alpha1.ForbidConcurrent
	existing.Spec.StartingDeadlineSeconds = &startingDeadlineSeconds
	existing.Spec.SuccessfulRunsHistoryLimit = &successfulRunsHistoryLimit
	existing.Spec.FailedRunsHistoryLimit = &failedRunsHistoryLimit

	if err := validateRepositoryMonitorReviewTaskMatchesExpected(&existing, expected, monitor, run, "orka-agents/orka", pr); err != nil {
		t.Fatalf("validateRepositoryMonitorReviewTaskMatchesExpected() error = %v", err)
	}
}

func TestRepositoryMonitorReconcileKeepsSucceededBackingTaskPendingWithoutTypedResult(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	const terminalReviewHeadSHA = "sha1"
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithoutAuth(t, `[
		{"number":1,"title":"Completed task","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1"},"head":{"ref":"queued","sha":"sha1"},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "queued-terminal-task",
			Namespace: "default",
			UID:       "uid-queued-terminal-task",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	completedReviewTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "completed-review-task", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status:     corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, completedReviewTask)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "queued-terminal-task",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          terminalReviewHeadSHA,
		LastVerdict:      repositoryMonitorRunPhaseQueued,
		LastReviewID:     "completed-review-task",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(queued) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-queued-terminal-task",
		MonitorNamespace: "default",
		MonitorName:      "queued-terminal-task",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "queued-terminal-task"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-queued-terminal-task")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.SelectedCount != 0 || run.CreatedTaskCount != 0 || run.SkippedCount != 1 {
		t.Fatalf("run = %#v, want succeeded task to stay pending without typed result ingest", run)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "queued-terminal-task", repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.LastReviewID != "completed-review-task" || item.LastReviewedHeadSHA != "" || item.LastVerdict != repositoryMonitorRunPhaseQueued || item.SkipReason != repositoryMonitorSkipReasonPending {
		t.Fatalf("item = %#v, want succeeded task to remain pending until typed result ingest", item)
	}
}

func TestRepositoryMonitorReconcileIngestsTypedReviewResult(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	const reviewHeadSHA = "sha1"
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := repositoryMonitorReviewIngestTestMonitor("review-ingest")
	task := repositoryMonitorReviewIngestTestTask("completed-review-task", "review-ingest", 1, reviewHeadSHA)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:      cl,
		Scheme:      scheme,
		Store:       monitorStore,
		ResultStore: monitorStore,
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "review-ingest",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          reviewHeadSHA,
		LastVerdict:      repositoryMonitorRunPhaseQueued,
		LastReviewID:     "completed-review-task",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(queued) error = %v", err)
	}
	if err := monitorStore.SaveResult(ctx, "default", "completed-review-task", repositoryMonitorReviewResultEnvelope(t, 1, reviewHeadSHA, repositoryMonitorReviewVerdictNeedsChanges)); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "review-ingest"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	records, _, err := monitorStore.ListReviewRecords(ctx, store.ReviewRecordFilter{Namespace: "default", MonitorName: "review-ingest", Number: 1, Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewRecords() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v, want one typed review record", records)
	}
	record := records[0]
	if record.TaskName != "completed-review-task" || record.HeadSHA != reviewHeadSHA || record.Verdict != repositoryMonitorReviewVerdictNeedsChanges || !record.Repairable || record.SecurityStatus != "clear" {
		t.Fatalf("record = %#v, want typed exact-head review result", record)
	}
	var findings []repositoryMonitorReviewFinding
	if err := json.Unmarshal([]byte(record.FindingsJSON), &findings); err != nil {
		t.Fatalf("FindingsJSON invalid: %v", err)
	}
	if len(findings) != 1 || findings[0].Priority != "P1" {
		t.Fatalf("findings = %#v, want one P1 finding", findings)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "review-ingest", repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.LastReviewID != record.ID || item.LastReviewedHeadSHA != reviewHeadSHA || item.LastVerdict != repositoryMonitorReviewVerdictNeedsChanges || item.SkipReason != "" {
		t.Fatalf("item = %#v, want ingested review result applied", item)
	}
	events, _, err := monitorStore.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: "default", MonitorName: "review-ingest", EventType: "review_result_ingested", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorEvents() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %#v, want one ingest event", events)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "review-ingest"}}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	records, _, err = monitorStore.ListReviewRecords(ctx, store.ReviewRecordFilter{Namespace: "default", MonitorName: "review-ingest", Number: 1, Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewRecords(second) error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records after second reconcile = %#v, want idempotent ingest", records)
	}
}

func TestRepositoryMonitorReconcileSkippedReviewResultDoesNotMarkHeadFresh(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	const reviewHeadSHA = "sha1"
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := repositoryMonitorReviewIngestTestMonitor("review-skipped")
	task := repositoryMonitorReviewIngestTestTask("skipped-review-task", "review-skipped", 1, reviewHeadSHA)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:      cl,
		Scheme:      scheme,
		Store:       monitorStore,
		ResultStore: monitorStore,
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "review-skipped",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          reviewHeadSHA,
		LastVerdict:      repositoryMonitorRunPhaseQueued,
		LastReviewID:     "skipped-review-task",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(queued) error = %v", err)
	}
	if err := monitorStore.SaveResult(ctx, "default", "skipped-review-task", repositoryMonitorReviewResultEnvelope(t, 1, reviewHeadSHA, repositoryMonitorVerdictSkipped)); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "review-skipped"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	item, err := monitorStore.GetMonitorItem(ctx, "default", "review-skipped", repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.LastReviewID != repositoryMonitorReviewRecordID(task) || item.LastReviewedHeadSHA != "" || item.LastVerdict != repositoryMonitorVerdictSkipped || item.SkipReason != repositoryMonitorVerdictSkipped {
		t.Fatalf("item = %#v, want skipped review result applied without marking head fresh", item)
	}
}

func TestRepositoryMonitorReviewPublishDisabledSkipsWithoutGitHubCall(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	const reviewHeadSHA = "sha1"
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	calledGitHub := false
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		calledGitHub = true
		t.Fatal("GitHub should not be called when publishing is disabled")
	}))
	t.Cleanup(server.Close)

	monitor := repositoryMonitorReviewIngestTestMonitor("publish-disabled")
	task := repositoryMonitorReviewIngestTestTask("publish-disabled-task", "publish-disabled", 1, reviewHeadSHA)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
	seedRepositoryMonitorQueuedReview(t, ctx, monitorStore, "publish-disabled", "publish-disabled-task", repositoryMonitorReviewResultEnvelope(t, 1, reviewHeadSHA, repositoryMonitorReviewVerdictNeedsChanges))

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "publish-disabled"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if calledGitHub {
		t.Fatal("GitHub was called despite disabled publishing")
	}
	publishRecords, _, err := monitorStore.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{Namespace: "default", MonitorName: "publish-disabled", Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewPublishRecords() error = %v", err)
	}
	if len(publishRecords) != 1 || publishRecords[0].Phase != repositoryMonitorPublishPhaseSkipped || publishRecords[0].SkipReason != repositoryMonitorPublishSkipDisabled {
		t.Fatalf("publishRecords = %#v, want one publish_disabled skip", publishRecords)
	}
}

func TestRepositoryMonitorReviewPublishPostsCommentReviewWithInlineFindings(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	const reviewHeadSHA = "sha1"
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	publishServer := newRepositoryMonitorPublishTestServer(t, repositoryMonitorPublishTestServerConfig{
		HeadSHA:     reviewHeadSHA,
		ReviewsBody: `[{"body":"<!-- orka:repo-monitor namespace=default name=publish-inline pr=1 head=sha1 run=fake review=fake -->"}]`,
		FilesBody: `[
			{"filename":"main.go","patch":"@@ -9,0 +10,3 @@\n+line10\n+line11\n+line12"}
		]`,
	})
	t.Cleanup(publishServer.Close)

	maxComments := int32(1)
	postNeedsChanges := true
	monitor := repositoryMonitorReviewIngestTestMonitor("publish-inline")
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: "github-token"}
	monitor.Spec.Review.Publish = corev1alpha1.RepositoryMonitorReviewPublishSpec{
		Enabled:          true,
		Mode:             repositoryMonitorPublishModeSummaryWithInlineFindings,
		Event:            repositoryMonitorPublishEventComment,
		PostNeedsChanges: &postNeedsChanges,
		SameHeadPolicy:   repositoryMonitorPublishSameHeadPolicySkip,
		Inline: corev1alpha1.RepositoryMonitorReviewPublishInlineSpec{
			Enabled:     true,
			MinPriority: repositoryMonitorPublishDefaultInlineMinPriority,
			MaxComments: &maxComments,
		},
	}
	task := repositoryMonitorReviewIngestTestTask("publish-inline-task", "publish-inline", 1, reviewHeadSHA)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "default"}, Data: map[string][]byte{"token": []byte("test-token")}}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: publishServer.URL}
	result := repositoryMonitorReviewResultEnvelopeWith(t, 1, reviewHeadSHA, repositoryMonitorReviewVerdictNeedsChanges, func(payload map[string]any) {
		payload["summary"] = "Summary mentions @team but should not notify.\n/merge\n<!-- fake marker -->"
		payload["findings"] = []map[string]any{
			{"priority": "P1", "confidence": "high", "file": "main.go", "line": 10, "title": "Notify @team", "body": "This pings @alice if unsanitized.", "recommendation": "Handle safely."},
			{"priority": "P2", "confidence": "high", "file": "main.go", "line": 11, "title": "Second inline but capped", "body": "Would map but max comments is one.", "recommendation": "Keep summary fallback."},
			{"priority": "P3", "confidence": "high", "file": "main.go", "line": 12, "title": "Low priority", "body": "P3 should be summary-only.", "recommendation": "No inline."},
			{"priority": "P1", "confidence": "high", "file": "main.go", "line": 99, "title": "Unmapped", "body": "Line is not in the patch.", "recommendation": "Summarize only."},
		}
	})
	seedRepositoryMonitorQueuedReview(t, ctx, monitorStore, "publish-inline", "publish-inline-task", result)

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "publish-inline"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if publishServer.PostCount != 1 {
		t.Fatalf("post count = %d, want 1", publishServer.PostCount)
	}
	posted := publishServer.PostedReview
	if posted.CommitID != reviewHeadSHA || posted.Event != repositoryMonitorPublishEventComment {
		t.Fatalf("posted review = %#v, want COMMENT for exact head", posted)
	}
	if len(posted.Comments) != 1 || posted.Comments[0].Path != "main.go" || posted.Comments[0].Line != 10 || posted.Comments[0].Side != "RIGHT" {
		t.Fatalf("posted comments = %#v, want one RIGHT-side line 10 comment", posted.Comments)
	}
	if strings.Contains(posted.Body, "@team") || strings.Contains(posted.Body, "@alice") || strings.Contains(posted.Comments[0].Body, "@alice") {
		t.Fatalf("posted review was not mention-neutralized: body=%q comment=%q", posted.Body, posted.Comments[0].Body)
	}
	if strings.Contains(posted.Body, "\n/merge") || strings.Contains(posted.Body, "<!-- fake marker -->") {
		t.Fatalf("posted review did not neutralize active command/comment text: %q", posted.Body)
	}
	for _, want := range []string{"Unmapped", "P3", "<!-- orka:repo-monitor namespace=default name=publish-inline pr=1 head=sha1"} {
		if !strings.Contains(posted.Body, want) {
			t.Fatalf("posted body missing %q: %s", want, posted.Body)
		}
	}
	publishRecords, _, err := monitorStore.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{Namespace: "default", MonitorName: "publish-inline", Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewPublishRecords() error = %v", err)
	}
	if len(publishRecords) != 1 || publishRecords[0].Phase != repositoryMonitorPublishPhaseSucceeded || publishRecords[0].GitHubReviewID != "123" || publishRecords[0].InlineCommentCount != 1 || !strings.HasPrefix(publishRecords[0].BodyDigest, "sha256:") {
		t.Fatalf("publishRecords = %#v, want succeeded GitHub review record", publishRecords)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "publish-inline", repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.LastPublishPhase != repositoryMonitorPublishPhaseSucceeded || item.LastPublishURL == "" {
		t.Fatalf("item = %#v, want succeeded publish status", item)
	}
}

func TestRepositoryMonitorReviewPublishSafetySkips(t *testing.T) {
	tests := []struct {
		name              string
		verdict           string
		mutateMonitor     func(*corev1alpha1.RepositoryMonitor)
		mutatePayload     func(map[string]any)
		serverConfig      repositoryMonitorPublishTestServerConfig
		seedDuplicate     bool
		seedStartedMarker bool
		wantReason        string
		wantPosts         int
	}{
		{
			name:       "missing git secret",
			verdict:    repositoryMonitorReviewVerdictNeedsChanges,
			wantReason: repositoryMonitorPublishSkipMissingGitSecret,
		},
		{
			name:    "head changed",
			verdict: repositoryMonitorReviewVerdictNeedsChanges,
			mutateMonitor: func(m *corev1alpha1.RepositoryMonitor) {
				m.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: "github-token"}
			},
			serverConfig: repositoryMonitorPublishTestServerConfig{HeadSHA: "new-sha"},
			wantReason:   repositoryMonitorPublishSkipHeadSHAChanged,
		},
		{
			name:    "closed pr",
			verdict: repositoryMonitorReviewVerdictNeedsChanges,
			mutateMonitor: func(m *corev1alpha1.RepositoryMonitor) {
				m.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: "github-token"}
			},
			serverConfig: repositoryMonitorPublishTestServerConfig{State: "closed"},
			wantReason:   repositoryMonitorPublishSkipPRClosed,
		},
		{
			name:    "blocked label",
			verdict: repositoryMonitorReviewVerdictNeedsChanges,
			mutateMonitor: func(m *corev1alpha1.RepositoryMonitor) {
				m.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: "github-token"}
				m.Spec.Policy.ProtectedLabels = []string{"do-not-touch"}
			},
			serverConfig: repositoryMonitorPublishTestServerConfig{Labels: []string{"do-not-touch"}},
			wantReason:   repositoryMonitorPublishSkipBlockedLabel,
		},
		{
			name:    "duplicate same head",
			verdict: repositoryMonitorReviewVerdictNeedsChanges,
			mutateMonitor: func(m *corev1alpha1.RepositoryMonitor) {
				m.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: "github-token"}
			},
			seedDuplicate: true,
			wantReason:    repositoryMonitorPublishSkipDuplicateSameHead,
		},
		{
			name:    "trusted started marker duplicate",
			verdict: repositoryMonitorReviewVerdictNeedsChanges,
			mutateMonitor: func(m *corev1alpha1.RepositoryMonitor) {
				m.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: "github-token"}
			},
			seedStartedMarker: true,
			wantReason:        repositoryMonitorPublishSkipDuplicateSameHead,
		},
		{
			name:       "passed default not posted",
			verdict:    repositoryMonitorReviewVerdictPassed,
			wantReason: repositoryMonitorPublishSkipVerdictNotConfigured,
		},
		{
			name:       "security sensitive default not public",
			verdict:    repositoryMonitorReviewVerdictSecuritySensitive,
			wantReason: repositoryMonitorPublishSkipSecuritySensitiveNotPublic,
			mutatePayload: func(payload map[string]any) {
				payload["security"] = map[string]any{"status": "security_sensitive", "notes": "sensitive"}
			},
		},
		{
			name:       "security status sensitive with needs changes",
			verdict:    repositoryMonitorReviewVerdictNeedsChanges,
			wantReason: repositoryMonitorPublishSkipSecuritySensitiveNotPublic,
			mutatePayload: func(payload map[string]any) {
				payload["security"] = map[string]any{"status": "security_sensitive", "notes": "sensitive"}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			monitorStore := setupControllerSQLiteStore(t)
			const reviewHeadSHA = "sha1"
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme() error = %v", err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatalf("corev1 AddToScheme() error = %v", err)
			}
			monitorName := "publish-skip-" + repositoryMonitorBoundedDNSName(strings.ReplaceAll(tt.name, " ", "-"), 40)
			serverConfig := tt.serverConfig
			if serverConfig.HeadSHA == "" {
				serverConfig.HeadSHA = reviewHeadSHA
			}
			if tt.seedStartedMarker {
				serverConfig.ReviewsBody = fmt.Sprintf(`[{"body":"<!-- orka:repo-monitor namespace=default name=%s pr=1 head=sha1 run=run-crashed review=review-crashed publish=reserved-publish -->"}]`, monitorName)
			}
			publishServer := newRepositoryMonitorPublishTestServer(t, serverConfig)
			t.Cleanup(publishServer.Close)

			monitor := repositoryMonitorReviewIngestTestMonitor(monitorName)
			monitor.Spec.Review.Publish.Enabled = true
			monitor.Spec.Review.Publish.Event = repositoryMonitorPublishEventComment
			if tt.mutateMonitor != nil {
				tt.mutateMonitor(monitor)
			}
			task := repositoryMonitorReviewIngestTestTask(monitorName+"-task", monitorName, 1, reviewHeadSHA)
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "default"}, Data: map[string][]byte{"token": []byte("test-token")}}
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
				WithObjects(repositoryMonitorControllerObjects(monitor, task, secret)...).
				Build()
			reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: publishServer.URL}
			result := repositoryMonitorReviewResultEnvelopeWith(t, 1, reviewHeadSHA, tt.verdict, tt.mutatePayload)
			seedRepositoryMonitorQueuedReview(t, ctx, monitorStore, monitorName, task.Name, result)
			if tt.seedDuplicate || tt.seedStartedMarker {
				phase := repositoryMonitorPublishPhaseSucceeded
				id := "already-published"
				if tt.seedStartedMarker {
					phase = repositoryMonitorPublishPhaseStarted
					id = "reserved-publish"
				}
				if err := monitorStore.CreateReviewPublishRecord(ctx, &store.ReviewPublishRecord{
					ID:               id,
					MonitorNamespace: "default",
					MonitorName:      monitorName,
					ItemKind:         repositoryMonitorPullRequestKind,
					ItemNumber:       1,
					HeadSHA:          reviewHeadSHA,
					Phase:            phase,
					Event:            repositoryMonitorPublishEventComment,
				}); err != nil {
					t.Fatalf("CreateReviewPublishRecord(duplicate) error = %v", err)
				}
			}

			if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitorName}}); err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if publishServer.PostCount != tt.wantPosts {
				t.Fatalf("post count = %d, want %d", publishServer.PostCount, tt.wantPosts)
			}
			publishRecords, _, err := monitorStore.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{Namespace: "default", MonitorName: monitorName, Phase: repositoryMonitorPublishPhaseSkipped, Limit: 10})
			if err != nil {
				t.Fatalf("ListReviewPublishRecords() error = %v", err)
			}
			if len(publishRecords) != 1 || publishRecords[0].SkipReason != tt.wantReason {
				t.Fatalf("publishRecords = %#v, want skip reason %q", publishRecords, tt.wantReason)
			}
		})
	}
}

func TestRepositoryMonitorReviewPublishRetriesReviewRecordWithoutTerminalPublish(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	const reviewHeadSHA = "sha1"
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	publishServer := newRepositoryMonitorPublishTestServer(t, repositoryMonitorPublishTestServerConfig{HeadSHA: reviewHeadSHA})
	t.Cleanup(publishServer.Close)

	postNeedsChanges := true
	monitor := repositoryMonitorReviewIngestTestMonitor("publish-pending")
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: "github-token"}
	monitor.Spec.Review.Publish = corev1alpha1.RepositoryMonitorReviewPublishSpec{Enabled: true, Event: repositoryMonitorPublishEventComment, PostNeedsChanges: &postNeedsChanges}
	task := repositoryMonitorReviewIngestTestTask("publish-pending-task", "publish-pending", 1, reviewHeadSHA)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "default"}, Data: map[string][]byte{"token": []byte("test-token")}}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: publishServer.URL}
	if err := monitorStore.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "review-existing",
		MonitorNamespace: "default",
		MonitorName:      "publish-pending",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		HeadSHA:          reviewHeadSHA,
		TaskName:         task.Name,
		TaskNamespace:    task.Namespace,
		Verdict:          repositoryMonitorReviewVerdictNeedsChanges,
		Confidence:       repositoryMonitorReviewConfidenceHigh,
		SecurityStatus:   "clear",
		FindingsJSON:     "[]",
		Summary:          "Review already ingested before publish completed.",
	}); err != nil {
		t.Fatalf("CreateReviewRecord() error = %v", err)
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace:    "default",
		MonitorName:         "publish-pending",
		Kind:                repositoryMonitorPullRequestKind,
		Number:              1,
		State:               repositoryMonitorItemStateOpen,
		HeadSHA:             reviewHeadSHA,
		LastVerdict:         repositoryMonitorReviewVerdictNeedsChanges,
		LastReviewID:        "review-existing",
		LastReviewedHeadSHA: reviewHeadSHA,
	}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "publish-pending"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if publishServer.PostCount != 1 {
		t.Fatalf("post count = %d, want pending review record to publish once", publishServer.PostCount)
	}
}

func TestRepositoryMonitorReviewPublishRetriesRecoverableSkippedRecord(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	const reviewHeadSHA = "sha1"
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	publishServer := newRepositoryMonitorPublishTestServer(t, repositoryMonitorPublishTestServerConfig{HeadSHA: reviewHeadSHA})
	t.Cleanup(publishServer.Close)

	postNeedsChanges := true
	monitor := repositoryMonitorReviewIngestTestMonitor("publish-recoverable-skip")
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: "github-token"}
	monitor.Spec.Review.Publish = corev1alpha1.RepositoryMonitorReviewPublishSpec{Enabled: true, Event: repositoryMonitorPublishEventComment, PostNeedsChanges: &postNeedsChanges}
	task := repositoryMonitorReviewIngestTestTask("publish-recoverable-skip-task", "publish-recoverable-skip", 1, reviewHeadSHA)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "default"}, Data: map[string][]byte{"token": []byte("test-token")}}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: publishServer.URL}
	if err := monitorStore.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "review-recoverable-skip",
		MonitorNamespace: "default",
		MonitorName:      "publish-recoverable-skip",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		HeadSHA:          reviewHeadSHA,
		TaskName:         task.Name,
		TaskNamespace:    task.Namespace,
		Verdict:          repositoryMonitorReviewVerdictNeedsChanges,
		Confidence:       repositoryMonitorReviewConfidenceHigh,
		SecurityStatus:   "clear",
		FindingsJSON:     "[]",
		Summary:          "Review had a recoverable publish skip before credentials existed.",
	}); err != nil {
		t.Fatalf("CreateReviewRecord() error = %v", err)
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace:    "default",
		MonitorName:         "publish-recoverable-skip",
		Kind:                repositoryMonitorPullRequestKind,
		Number:              1,
		State:               repositoryMonitorItemStateOpen,
		HeadSHA:             reviewHeadSHA,
		LastVerdict:         repositoryMonitorReviewVerdictNeedsChanges,
		LastReviewID:        "review-recoverable-skip",
		LastReviewedHeadSHA: reviewHeadSHA,
	}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	skippedAt := time.Now().Add(-10 * time.Minute)
	if err := monitorStore.CreateReviewPublishRecord(ctx, &store.ReviewPublishRecord{
		ID:               "recoverable-skip",
		MonitorNamespace: "default",
		MonitorName:      "publish-recoverable-skip",
		ItemKind:         repositoryMonitorPullRequestKind,
		ItemNumber:       1,
		HeadSHA:          reviewHeadSHA,
		ReviewTaskName:   task.Name,
		ReviewRecordID:   "review-recoverable-skip",
		Phase:            repositoryMonitorPublishPhaseSkipped,
		Event:            repositoryMonitorPublishEventComment,
		SkipReason:       repositoryMonitorPublishSkipMissingGitSecret,
		CreatedAt:        skippedAt,
		UpdatedAt:        skippedAt,
	}); err != nil {
		t.Fatalf("CreateReviewPublishRecord(recoverable skip) error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "publish-recoverable-skip"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if publishServer.PostCount != 1 {
		t.Fatalf("post count = %d, want recovered skipped publish to retry once", publishServer.PostCount)
	}
}

func TestRepositoryMonitorReviewPublishWaitsBeforeRetryingRecentRecoverableSkip(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	const reviewHeadSHA = "sha1"
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	publishServer := newRepositoryMonitorPublishTestServer(t, repositoryMonitorPublishTestServerConfig{HeadSHA: reviewHeadSHA})
	t.Cleanup(publishServer.Close)

	postNeedsChanges := true
	monitor := repositoryMonitorReviewIngestTestMonitor("publish-recent-skip")
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: "github-token"}
	monitor.Spec.Review.Publish = corev1alpha1.RepositoryMonitorReviewPublishSpec{Enabled: true, Event: repositoryMonitorPublishEventComment, PostNeedsChanges: &postNeedsChanges}
	task := repositoryMonitorReviewIngestTestTask("publish-recent-skip-task", "publish-recent-skip", 1, reviewHeadSHA)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "default"}, Data: map[string][]byte{"token": []byte("test-token")}}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: publishServer.URL}
	if err := monitorStore.CreateReviewRecord(ctx, &store.ReviewRecord{
		ID:               "review-recent-skip",
		MonitorNamespace: "default",
		MonitorName:      "publish-recent-skip",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		HeadSHA:          reviewHeadSHA,
		TaskName:         task.Name,
		TaskNamespace:    task.Namespace,
		Verdict:          repositoryMonitorReviewVerdictNeedsChanges,
		Confidence:       repositoryMonitorReviewConfidenceHigh,
		SecurityStatus:   "clear",
		FindingsJSON:     "[]",
		Summary:          "Review had a recent recoverable publish skip.",
	}); err != nil {
		t.Fatalf("CreateReviewRecord() error = %v", err)
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace:    "default",
		MonitorName:         "publish-recent-skip",
		Kind:                repositoryMonitorPullRequestKind,
		Number:              1,
		State:               repositoryMonitorItemStateOpen,
		HeadSHA:             reviewHeadSHA,
		LastVerdict:         repositoryMonitorReviewVerdictNeedsChanges,
		LastReviewID:        "review-recent-skip",
		LastReviewedHeadSHA: reviewHeadSHA,
	}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	now := time.Now()
	if err := monitorStore.CreateReviewPublishRecord(ctx, &store.ReviewPublishRecord{
		ID:               "recent-recoverable-skip",
		MonitorNamespace: "default",
		MonitorName:      "publish-recent-skip",
		ItemKind:         repositoryMonitorPullRequestKind,
		ItemNumber:       1,
		HeadSHA:          reviewHeadSHA,
		ReviewTaskName:   task.Name,
		ReviewRecordID:   "review-recent-skip",
		Phase:            repositoryMonitorPublishPhaseSkipped,
		Event:            repositoryMonitorPublishEventComment,
		SkipReason:       repositoryMonitorPublishSkipMissingGitSecret,
		CreatedAt:        now,
		UpdatedAt:        now,
	}); err != nil {
		t.Fatalf("CreateReviewPublishRecord(recent skip) error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "publish-recent-skip"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if publishServer.PostCount != 0 {
		t.Fatalf("post count = %d, want recent recoverable skip to stay in cooldown", publishServer.PostCount)
	}
}

func TestRepositoryMonitorReviewPublishGitHubPermissionFailureCreatesFailedRecord(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	const reviewHeadSHA = "sha1"
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	publishServer := newRepositoryMonitorPublishTestServer(t, repositoryMonitorPublishTestServerConfig{HeadSHA: reviewHeadSHA, PostStatus: http.StatusForbidden, PostBody: `{"message":"forbidden"}`})
	t.Cleanup(publishServer.Close)

	postNeedsChanges := true
	monitor := repositoryMonitorReviewIngestTestMonitor("publish-forbidden")
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: "github-token"}
	monitor.Spec.Review.Publish = corev1alpha1.RepositoryMonitorReviewPublishSpec{Enabled: true, Event: repositoryMonitorPublishEventComment, PostNeedsChanges: &postNeedsChanges}
	task := repositoryMonitorReviewIngestTestTask("publish-forbidden-task", "publish-forbidden", 1, reviewHeadSHA)
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: "default"}, Data: map[string][]byte{"token": []byte("test-token")}}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: publishServer.URL}
	seedRepositoryMonitorQueuedReview(t, ctx, monitorStore, "publish-forbidden", "publish-forbidden-task", repositoryMonitorReviewResultEnvelope(t, 1, reviewHeadSHA, repositoryMonitorReviewVerdictNeedsChanges))

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "publish-forbidden"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "publish-forbidden"}}); err != nil {
		t.Fatalf("second Reconcile() error = %v", err)
	}
	if publishServer.PostCount != 1 {
		t.Fatalf("post count after two reconciles = %d, want no retry storm", publishServer.PostCount)
	}
	publishRecords, _, err := monitorStore.ListReviewPublishRecords(ctx, store.ReviewPublishRecordFilter{Namespace: "default", MonitorName: "publish-forbidden", Phase: repositoryMonitorPublishPhaseFailed, Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewPublishRecords() error = %v", err)
	}
	if len(publishRecords) != 1 || publishRecords[0].SkipReason != repositoryMonitorPublishFailureGitHubPermissionDenied || !strings.Contains(publishRecords[0].Error, "403") {
		t.Fatalf("publishRecords = %#v, want one github_permission_denied failure", publishRecords)
	}
}

func TestRepositoryMonitorReconcileRetriesTransientReviewResultReadError(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	const reviewHeadSHA = "sha1"
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := repositoryMonitorReviewIngestTestMonitor("review-transient-result-error")
	task := repositoryMonitorReviewIngestTestTask("transient-review-task", "review-transient-result-error", 1, reviewHeadSHA)
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task)...).
		Build()
	readErr := errors.New("sqlite busy")
	reconciler := &RepositoryMonitorReconciler{
		Client:      cl,
		Scheme:      scheme,
		Store:       monitorStore,
		ResultStore: transientGetResultErrorStore{ResultStore: monitorStore, err: readErr},
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "review-transient-result-error",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          reviewHeadSHA,
		LastVerdict:      repositoryMonitorRunPhaseQueued,
		LastReviewID:     "transient-review-task",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(queued) error = %v", err)
	}

	_, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "review-transient-result-error"}})
	if err == nil {
		t.Fatal("Reconcile() error = nil, want transient result-store error")
	}
	if !strings.Contains(err.Error(), readErr.Error()) {
		t.Fatalf("Reconcile() error = %q, want %q", err.Error(), readErr.Error())
	}

	records, _, err := monitorStore.ListReviewRecords(ctx, store.ReviewRecordFilter{Namespace: "default", MonitorName: "review-transient-result-error", Number: 1, Limit: 10})
	if err != nil {
		t.Fatalf("ListReviewRecords() error = %v", err)
	}
	if len(records) != 0 {
		t.Fatalf("records = %#v, want no immutable rejection for transient result-store error", records)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "review-transient-result-error", repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.LastVerdict != repositoryMonitorRunPhaseQueued || item.LastReviewID != "transient-review-task" || item.LastReviewedHeadSHA != "" {
		t.Fatalf("item = %#v, want queued review preserved for retry", item)
	}
}

func TestRepositoryMonitorReconcileRejectsMalformedReviewResult(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := repositoryMonitorReviewIngestTestMonitor("review-malformed")
	task := repositoryMonitorReviewIngestTestTask("malformed-review-task", "review-malformed", 2, "sha2")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "review-malformed",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           2,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          "sha2",
		LastVerdict:      repositoryMonitorRunPhaseQueued,
		LastReviewID:     "malformed-review-task",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(queued) error = %v", err)
	}
	if err := monitorStore.SaveResult(ctx, "default", "malformed-review-task", []byte("not a review result")); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "review-malformed"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	record, item := assertRepositoryMonitorRejectedReview(t, ctx, monitorStore, "review-malformed", "2")
	if record.Verdict != repositoryMonitorReviewVerdictFailed || item.LastVerdict != repositoryMonitorReviewVerdictFailed || item.SkipReason != repositoryMonitorReviewSkipReasonMalformed {
		t.Fatalf("record = %#v item = %#v, want malformed review failure", record, item)
	}
}

func TestRepositoryMonitorReconcileRejectsStaleReviewResult(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := repositoryMonitorReviewIngestTestMonitor("review-stale")
	task := repositoryMonitorReviewIngestTestTask("stale-review-task", "review-stale", 3, "oldsha")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "review-stale",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           3,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          "newsha",
		LastVerdict:      repositoryMonitorRunPhaseQueued,
		LastReviewID:     "stale-review-task",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(queued) error = %v", err)
	}
	if err := monitorStore.SaveResult(ctx, "default", "stale-review-task", repositoryMonitorReviewResultEnvelope(t, 3, "oldsha", repositoryMonitorReviewVerdictPassed)); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "review-stale"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	record, item := assertRepositoryMonitorRejectedReview(t, ctx, monitorStore, "review-stale", "3")
	if record.Verdict != repositoryMonitorReviewVerdictStale || record.HeadSHA != "oldsha" || item.LastVerdict != repositoryMonitorReviewVerdictStale || item.LastReviewedHeadSHA != "" || item.SkipReason != repositoryMonitorReviewSkipReasonStaleHead {
		t.Fatalf("record = %#v item = %#v, want stale review rejection", record, item)
	}
}

func TestRepositoryMonitorReconcileRejectsReviewTaskBindingMismatch(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := repositoryMonitorReviewIngestTestMonitor("review-task-mismatch")
	task := repositoryMonitorReviewIngestTestTask("mismatched-review-task", "review-task-mismatch", 99, "sha4")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, task)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "review-task-mismatch",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           4,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          "sha4",
		LastVerdict:      repositoryMonitorRunPhaseQueued,
		LastReviewID:     "mismatched-review-task",
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(queued) error = %v", err)
	}
	if err := monitorStore.SaveResult(ctx, "default", "mismatched-review-task", repositoryMonitorReviewResultEnvelope(t, 4, "sha4", repositoryMonitorReviewVerdictPassed)); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "review-task-mismatch"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	record, item := assertRepositoryMonitorRejectedReview(t, ctx, monitorStore, "review-task-mismatch", "4")
	if record.Verdict != repositoryMonitorReviewVerdictFailed || item.LastVerdict != repositoryMonitorReviewVerdictFailed || item.SkipReason != repositoryMonitorReviewSkipReasonTaskMismatch {
		t.Fatalf("record = %#v item = %#v, want task binding mismatch rejection", record, item)
	}
	if !strings.Contains(record.Summary, `item number "99", want "4"`) {
		t.Fatalf("record summary = %q, want item number mismatch detail", record.Summary)
	}
}

func TestRepositoryMonitorReconcileForkPullRequestTaskUsesHeadRepoWithoutBaseSecret(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":7,"title":"Fork PR","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"forker"},"base":{"ref":"main","sha":"base7","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature","sha":"fork-sha","repo":{"full_name":"forker/orka","clone_url":"https://github.com/forker/orka.git"}},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("fork-pr")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-fork-pr",
		MonitorNamespace: "default",
		MonitorName:      "fork-pr",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "fork-pr"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	item, err := monitorStore.GetMonitorItem(ctx, "default", "fork-pr", repositoryMonitorPullRequestKind, "7")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: item.LastReviewID}, &task); err != nil {
		t.Fatalf("Get review task %q error = %v", item.LastReviewID, err)
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		t.Fatalf("task AgentRuntime.Workspace is nil")
	}
	if task.Spec.AgentRuntime.Workspace.GitRepo != "https://github.com/forker/orka.git" || task.Spec.AgentRuntime.Workspace.Ref != "fork-sha" {
		t.Fatalf("workspace = %#v, want fork repo at exact fork head", task.Spec.AgentRuntime.Workspace)
	}
	if task.Spec.AgentRuntime.Workspace.GitSecretRef != nil {
		t.Fatalf("workspace GitSecretRef = %#v, want nil for fork PR review", task.Spec.AgentRuntime.Workspace.GitSecretRef)
	}
	if !strings.Contains(task.Spec.Prompt, `"headRepo": "forker/orka"`) {
		t.Fatalf("prompt does not include fork head repo:\n%s", task.Spec.Prompt)
	}
}

func TestRepositoryMonitorReconcileSameRepoSSHMonitorUsesHTTPSCloneURL(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":9,"title":"Same repo SSH monitor","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base9","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature","sha":"ssh-head-sha","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("ssh-same-repo")
	monitor.Spec.RepoURL = "git@github.com:orka-agents/orka.git"
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-ssh-same-repo",
		MonitorNamespace: "default",
		MonitorName:      "ssh-same-repo",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "ssh-same-repo"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	item, err := monitorStore.GetMonitorItem(ctx, "default", "ssh-same-repo", repositoryMonitorPullRequestKind, "9")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: item.LastReviewID}, &task); err != nil {
		t.Fatalf("Get review task %q error = %v", item.LastReviewID, err)
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		t.Fatalf("task AgentRuntime.Workspace is nil")
	}
	if task.Spec.AgentRuntime.Workspace.GitRepo != repositoryMonitorTestRepoURL || task.Spec.AgentRuntime.Workspace.Ref != "ssh-head-sha" {
		t.Fatalf("workspace = %#v, want HTTPS monitored repo at exact head", task.Spec.AgentRuntime.Workspace)
	}
	if task.Spec.AgentRuntime.Workspace.GitSecretRef == nil || task.Spec.AgentRuntime.Workspace.GitSecretRef.Name != "github-token" {
		t.Fatalf("workspace GitSecretRef = %#v, want github-token for same-repo review", task.Spec.AgentRuntime.Workspace.GitSecretRef)
	}
}

func TestRepositoryMonitorReconcileMissingHeadRepoDoesNotAttachBaseSecret(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":8,"title":"Missing head repo","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"forker"},"base":{"ref":"main","sha":"base8","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature","sha":"unknown-head-sha"},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("missing-head-repo")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-missing-head-repo",
		MonitorNamespace: "default",
		MonitorName:      "missing-head-repo",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing-head-repo"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	item, err := monitorStore.GetMonitorItem(ctx, "default", "missing-head-repo", repositoryMonitorPullRequestKind, "8")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: item.LastReviewID}, &task); err != nil {
		t.Fatalf("Get review task %q error = %v", item.LastReviewID, err)
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		t.Fatalf("task AgentRuntime.Workspace is nil")
	}
	if task.Spec.AgentRuntime.Workspace.GitRepo != repositoryMonitorTestRepoURL || task.Spec.AgentRuntime.Workspace.Ref != "unknown-head-sha" {
		t.Fatalf("workspace = %#v, want monitored repo at exact head", task.Spec.AgentRuntime.Workspace)
	}
	if task.Spec.AgentRuntime.Workspace.GitSecretRef != nil {
		t.Fatalf("workspace GitSecretRef = %#v, want nil when head repo is unverified", task.Spec.AgentRuntime.Workspace.GitSecretRef)
	}
}

func TestRepositoryMonitorReconcileProcessesPublicInventoryWithoutGitSecret(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithoutAuth(t, `[
		{"number":1,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1"},"head":{"ref":"feature","sha":"sha1"},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "public-inventory",
			Namespace: "default",
			UID:       "uid-public-inventory",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-public",
		MonitorNamespace: "default",
		MonitorName:      "public-inventory",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "public-inventory"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-public")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseSucceeded || run.SelectedCount != 1 || run.CompletedAt == nil {
		t.Fatalf("run = %#v, want public inventory run to succeed", run)
	}
}

func TestRepositoryMonitorReconcileProcessesQueuedRunWhenSuspended(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":1,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1"},"head":{"ref":"feature","sha":"sha1"},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("suspended-manual")
	suspend := true
	monitor.Spec.Suspend = &suspend
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-suspended-manual",
		MonitorNamespace: "default",
		MonitorName:      "suspended-manual",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "suspended-manual"}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter <= 0 {
		t.Fatalf("RequeueAfter = %v, want positive", result.RequeueAfter)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-suspended-manual")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseSucceeded || run.CompletedAt == nil {
		t.Fatalf("run = %#v, want completed successful run", run)
	}

	activeRuns, _, err := monitorStore.ListMonitorRuns(ctx, store.MonitorRunFilter{
		Namespace:   "default",
		MonitorName: "suspended-manual",
		Phase:       repositoryMonitorRunPhaseQueued,
		Limit:       1,
	})
	if err != nil {
		t.Fatalf("ListMonitorRuns(queued) error = %v", err)
	}
	if len(activeRuns) != 0 {
		t.Fatalf("queued runs = %#v, want none", activeRuns)
	}
}

func TestRepositoryMonitorReconcileTargetedRunPreservesRepositoryWideStatusCounts(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorSinglePullRequestServerWithBody(t, 2, `{"number":2,"title":"Targeted","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base2"},"head":{"ref":"targeted","sha":"sha2"},"labels":[]}`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("targeted-status")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	for _, item := range []store.MonitorItem{
		{MonitorNamespace: "default", MonitorName: "targeted-status", Kind: repositoryMonitorPullRequestKind, Number: 1, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorRunPhaseQueued},
		{MonitorNamespace: "default", MonitorName: "targeted-status", Kind: repositoryMonitorPullRequestKind, Number: 2, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorRunPhaseQueued},
		{MonitorNamespace: "default", MonitorName: "targeted-status", Kind: repositoryMonitorPullRequestKind, Number: 3, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorVerdictSkipped, SkipReason: "blocked_label"},
		{MonitorNamespace: "default", MonitorName: "targeted-status", Kind: repositoryMonitorPullRequestKind, Number: 4, State: repositoryMonitorItemStateOutOfScope, LastVerdict: repositoryMonitorVerdictSkipped, SkipReason: repositoryMonitorSkipReasonMissing},
	} {
		if err := monitorStore.UpsertMonitorItem(ctx, &item); err != nil {
			t.Fatalf("UpsertMonitorItem(%d) error = %v", item.Number, err)
		}
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-targeted",
		MonitorNamespace: "default",
		MonitorName:      "targeted-status",
		Trigger:          "manual",
		TargetKind:       repositoryMonitorPullRequestKind,
		TargetNumber:     2,
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "targeted-status"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-targeted")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.SelectedCount != 1 || run.SkippedCount != 0 {
		t.Fatalf("run counts = selected=%d skipped=%d, want selected=1 skipped=0", run.SelectedCount, run.SkippedCount)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "targeted-status"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.OpenPullRequests != 3 || current.Status.PendingReviews != 2 || current.Status.BlockedItems != 1 {
		t.Fatalf("status counts = %#v, want aggregate open=3 pending=2 blocked=1", current.Status)
	}
	retired, err := monitorStore.GetMonitorItem(ctx, "default", "targeted-status", repositoryMonitorPullRequestKind, "4")
	if err != nil {
		t.Fatalf("GetMonitorItem(retired) error = %v", err)
	}
	if retired.State != repositoryMonitorItemStateOutOfScope {
		t.Fatalf("retired item state = %q, want unchanged out_of_scope", retired.State)
	}
}

func TestRepositoryMonitorStatusCountsIncludesTerminalBlockedReviewVerdicts(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: "review-status", Namespace: defaultNS},
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	items := []store.MonitorItem{
		{Number: 1, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorRunPhaseQueued},
		{Number: 2, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorVerdictSkipped},
		{Number: 3, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorReviewVerdictFailed},
		{Number: 4, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorReviewVerdictStale},
		{Number: 5, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorReviewVerdictNeedsHuman},
		{Number: 6, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorReviewVerdictSecuritySensitive},
		{Number: 7, State: repositoryMonitorItemStateOpen, HeadSHA: "head-7", LastReviewedHeadSHA: "head-7", LastVerdict: repositoryMonitorReviewVerdictPassed, RepairState: repositoryMonitorRepairPhaseSucceeded},
		{Number: 8, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorReviewVerdictNeedsChanges},
		{Number: 9, State: repositoryMonitorItemStateOutOfScope, LastVerdict: repositoryMonitorReviewVerdictFailed},
	}
	for i := range items {
		item := items[i]
		item.MonitorNamespace = defaultNS
		item.MonitorName = "review-status"
		item.Kind = repositoryMonitorPullRequestKind
		if err := monitorStore.UpsertMonitorItem(ctx, &item); err != nil {
			t.Fatalf("UpsertMonitorItem(%d) error = %v", item.Number, err)
		}
	}

	counts, err := reconciler.repositoryMonitorStatusCounts(ctx, monitor)
	if err != nil {
		t.Fatalf("repositoryMonitorStatusCounts() error = %v", err)
	}
	if counts.openPullRequests != 8 || counts.pendingReviews != 1 || counts.blockedItems != 6 || counts.mergeReadyItems != 1 {
		t.Fatalf("counts = %#v, want open=8 pending=1 blocked=6 mergeReady=1", counts)
	}
}

func TestRepositoryMonitorReconcileStaleExactEventDoesNotRewriteCurrentPullRequest(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorSinglePullRequestServerWithBody(t, 2, `{"number":2,"title":"Current head","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base2"},"head":{"ref":"targeted","sha":"new-sha"},"labels":[]}`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("stale-exact-event")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "stale-exact-event",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           2,
		Title:            "Current head",
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          "new-sha",
		LastVerdict:      repositoryMonitorRunPhaseQueued,
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(current) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-stale-exact-event",
		MonitorNamespace: "default",
		MonitorName:      "stale-exact-event",
		Trigger:          "event",
		TargetKind:       repositoryMonitorPullRequestKind,
		TargetNumber:     2,
		TargetSHA:        "old-sha",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "stale-exact-event"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-stale-exact-event")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseSucceeded || run.SelectedCount != 0 || run.SkippedCount != 0 {
		t.Fatalf("run = %#v, want stale exact event to complete without selecting or skipping current head", run)
	}

	item, err := monitorStore.GetMonitorItem(ctx, "default", "stale-exact-event", repositoryMonitorPullRequestKind, "2")
	if err != nil {
		t.Fatalf("GetMonitorItem(current) error = %v", err)
	}
	if item.HeadSHA != "new-sha" || item.LastVerdict != repositoryMonitorRunPhaseQueued || item.SkipReason != "" {
		t.Fatalf("item = %#v, want current head left queued without stale skip reason", item)
	}

	events, _, err := monitorStore.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: "default", MonitorName: "stale-exact-event", RunID: "run-stale-exact-event", EventType: "item_skipped", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorEvents(item_skipped) error = %v", err)
	}
	if len(events) != 0 {
		t.Fatalf("item_skipped events = %#v, want no stale skip event", events)
	}
}

func TestRepositoryMonitorStaleHeadCommandBlocksWorkAction(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := newRepositoryMonitorSinglePullRequestServerWithBody(t, 2, `{"number":2,"title":"Current head","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base2"},"head":{"ref":"targeted","sha":"new-sha"},"labels":[]}`)
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("stale-head-command")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "2", Number: 2, State: repositoryMonitorItemStateOpen, HeadSHA: "new-sha", LastVerdict: repositoryMonitorRunPhaseQueued}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	command := &store.CommandEvent{ID: "cmd-stale-review", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 2, Intent: "review", HeadSHA: "old-sha", Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, "review")
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 2, TargetSHA: "old-sha", DesiredAction: "review", Status: repositoryMonitorWorkActionStatusQueued, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	run := &store.MonitorRun{ID: "run-stale-command", CommandEventID: command.ID, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 2, TargetSHA: "old-sha"}
	selected, created, skipped, err := reconciler.processPullRequestInventoryRun(ctx, monitor, run, "orka-agents", "orka")
	if err != nil || selected != 0 || created != 0 || skipped != 1 {
		t.Fatalf("processPullRequestInventoryRun() = selected=%d created=%d skipped=%d err=%v", selected, created, skipped, err)
	}
	action, err := monitorStore.GetWorkAction(ctx, monitor.Namespace, actionID)
	if err != nil {
		t.Fatalf("GetWorkAction() error = %v", err)
	}
	if action.Status != repositoryMonitorWorkActionStatusBlocked || action.BlockedReason != "stale_head_sha" || action.CompletedAt == nil {
		t.Fatalf("work action = %#v, want terminal stale_head_sha block", action)
	}
	item, err := monitorStore.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "2")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.HeadSHA != "new-sha" || item.SkipReason != "" {
		t.Fatalf("current item was rewritten by stale command: %#v", item)
	}
}

func TestRepositoryMonitorStopCommandBypassesStaleHeadAndCancelsTask(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "stale-stop", Namespace: defaultNS}}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "active-review", Namespace: defaultNS, Labels: map[string]string{
		labels.LabelRepositoryMonitor: labels.SelectorValue(monitor.Name),
		labels.LabelGitHubTarget:      labels.SelectorValue(repositoryMonitorPullRequestKind),
		labels.LabelGitHubNumber:      labels.SelectorValue("42"),
	}}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.Task{}).WithObjects(task).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "42", Number: 42, State: repositoryMonitorItemStateOpen, HeadSHA: "new-head"}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	command := &store.CommandEvent{ID: "cmd-stop-stale", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 42, Intent: repositoryMonitorCommandIntentStop, HeadSHA: "old-head", Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentStop)
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, DesiredAction: repositoryMonitorCommandIntentStop, Status: repositoryMonitorWorkActionStatusQueued, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	run := &store.MonitorRun{ID: "run-stop-stale", MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 42, TargetSHA: "old-head"}
	selected, created, skipped, err := reconciler.processPullRequestInventoryRun(ctx, monitor, run, "orka-agents", "orka")
	if err != nil || selected != 1 || created != 0 || skipped != 0 {
		t.Fatalf("processPullRequestInventoryRun() = selected=%d created=%d skipped=%d err=%v", selected, created, skipped, err)
	}
	var currentTask corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: defaultNS, Name: task.Name}, &currentTask); err != nil {
		t.Fatalf("Get task error = %v", err)
	}
	if currentTask.Status.Phase != corev1alpha1.TaskPhaseCancelled {
		t.Fatalf("task phase = %q, want Cancelled", currentTask.Status.Phase)
	}
	item, err := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorPullRequestKind, "42")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.HeadSHA != "new-head" || item.SkipReason != "stopped_by_command" {
		t.Fatalf("stopped item = %#v, want current head preserved", item)
	}
	action, err := monitorStore.GetWorkAction(ctx, defaultNS, actionID)
	if err != nil {
		t.Fatalf("GetWorkAction() error = %v", err)
	}
	if action.Status != repositoryMonitorWorkActionStatusSucceeded {
		t.Fatalf("stop action = %#v, want succeeded", action)
	}
}

func TestRepositoryMonitorResumeCommandRequiresLiveUnblockedTarget(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := newRepositoryMonitorSinglePullRequestServerWithBody(t, 42, `{"number":42,"title":"Paused","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base42"},"head":{"ref":"targeted","sha":"head42"},"labels":[{"name":"hold"}]}`)
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("resume-live-validation")
	monitor.Spec.Policy.PauseLabels = []string{"hold"}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "42", Number: 42, State: repositoryMonitorItemStateOpen, HeadSHA: "head42", RepairState: repositoryMonitorRepairPhaseFailed, SkipReason: repositoryMonitorIssueSkipStoppedByCommand}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	command := &store.CommandEvent{ID: "cmd-resume-live", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 42, Intent: repositoryMonitorCommandIntentResume, HeadSHA: "head42", Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	run := &store.MonitorRun{ID: "run-resume-live", MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 42, TargetSHA: "head42"}
	selected, created, skipped, err := reconciler.processPullRequestInventoryRun(ctx, monitor, run, "orka-agents", "orka")
	if err != nil || selected != 1 || created != 0 || skipped != 0 {
		t.Fatalf("processPullRequestInventoryRun() selected=%d created=%d skipped=%d err=%v", selected, created, skipped, err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorPullRequestKind, "42")
	if err != nil || item.RepairState != repositoryMonitorRepairPhaseFailed || item.SkipReason != repositoryMonitorIssueSkipStoppedByCommand {
		t.Fatalf("blocked resume changed stopped item: %#v err=%v", item, err)
	}
	action, err := monitorStore.GetWorkAction(ctx, defaultNS, store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentResume))
	if err != nil || action.Status != repositoryMonitorWorkActionStatusBlocked || action.BlockedReason != repositoryMonitorSkipReasonBlockedLabel {
		t.Fatalf("resume action = %#v err=%v, want blocked label", action, err)
	}
}

func TestRepositoryMonitorUnknownStopTargetDoesNotCreateOpenInventoryItem(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/repos/orka-agents/orka/pulls/404" {
			t.Fatalf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("unknown-stop-target")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	command := &store.CommandEvent{ID: "cmd-stop-unknown", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 404, Intent: repositoryMonitorCommandIntentStop, Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	run := &store.MonitorRun{ID: "run-stop-unknown", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 404}
	if _, _, _, err := reconciler.processPullRequestInventoryRun(ctx, monitor, run, "orka-agents", "orka"); err == nil {
		t.Fatal("processPullRequestInventoryRun() error = nil, want target lookup failure")
	}
	if item, err := monitorStore.GetMonitorItem(ctx, monitor.Namespace, monitor.Name, repositoryMonitorPullRequestKind, "404"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown target item = %#v err=%v, want no inventory item", item, err)
	}
	counts, err := reconciler.repositoryMonitorStatusCounts(ctx, monitor)
	if err != nil {
		t.Fatalf("repositoryMonitorStatusCounts() error = %v", err)
	}
	if counts.openPullRequests != 0 {
		t.Fatalf("open pull requests = %d, want 0", counts.openPullRequests)
	}
}

func TestRepositoryMonitorPullRequestShortcutsIgnoreIssueCommands(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "cross-target-command", Namespace: defaultNS}}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "42", Number: 42, State: repositoryMonitorItemStateOpen, HeadSHA: "pr-head"}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	command := &store.CommandEvent{ID: "cmd-stop-issue-42", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 42, Intent: repositoryMonitorCommandIntentStop, Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	issueRun := &store.MonitorRun{ID: "run-stop-issue-42", MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, TargetKind: repositoryMonitorIssueKind, TargetNumber: 42}
	if handled, created, err := reconciler.processTargetedPullRequestControlCommand(ctx, monitor, issueRun, "orka-agents", "orka"); err != nil || handled || created != 0 {
		t.Fatalf("issue run pull-request shortcut handled=%v created=%d err=%v", handled, created, err)
	}
	if reason := repositoryMonitorTargetPullRequestCommandBlockReason(nil, "main", issueRun); reason != "" {
		t.Fatalf("issue run pull-request block reason = %q, want empty", reason)
	}
	malformedRun := *issueRun
	malformedRun.TargetKind = repositoryMonitorPullRequestKind
	if handled, created, err := reconciler.processTargetedPullRequestControlCommand(ctx, monitor, &malformedRun, "orka-agents", "orka"); err != nil || handled || created != 0 {
		t.Fatalf("issue command pull-request shortcut handled=%v created=%d err=%v", handled, created, err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorPullRequestKind, "42")
	if err != nil || item.SkipReason != "" || item.HeadSHA != "pr-head" {
		t.Fatalf("pull request item changed by issue command: %#v err=%v", item, err)
	}
}

func TestRepositoryMonitorReconcileFullInventoryRetiresMissingPullRequests(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":1,"title":"Still open","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1"},"head":{"ref":"feature","sha":"sha1"},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("retire-missing")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{
		Client:           cl,
		Scheme:           scheme,
		Store:            monitorStore,
		GitHubAPIBaseURL: server.URL,
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "retire-missing",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           7,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          "old-sha",
		LastVerdict:      repositoryMonitorRunPhaseQueued,
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(stale) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-full",
		MonitorNamespace: "default",
		MonitorName:      "retire-missing",
		Trigger:          "manual",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "retire-missing"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	retired, err := monitorStore.GetMonitorItem(ctx, "default", "retire-missing", repositoryMonitorPullRequestKind, "7")
	if err != nil {
		t.Fatalf("GetMonitorItem(retired) error = %v", err)
	}
	if retired.State != repositoryMonitorItemStateOutOfScope || retired.LastVerdict != repositoryMonitorVerdictSkipped || retired.SkipReason != repositoryMonitorSkipReasonMissing {
		t.Fatalf("retired item = %#v, want out_of_scope skipped with missing reason", retired)
	}

	events, _, err := monitorStore.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: "default", MonitorName: "retire-missing", RunID: "run-full", EventType: "item_retired", Limit: 10})
	if err != nil {
		t.Fatalf("ListMonitorEvents(item_retired) error = %v", err)
	}
	if len(events) != 1 || events[0].ItemNumber != 7 {
		t.Fatalf("retired events = %#v, want one item_retired event for PR 7", events)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "retire-missing"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.OpenPullRequests != 1 || current.Status.PendingReviews != 1 || current.Status.BlockedItems != 0 {
		t.Fatalf("status counts = %#v, want only current open inventory counted", current.Status)
	}
}

func TestRepositoryMonitorReconcileFailsUnsupportedRunTargetKind(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unsupported-run",
			Namespace: "default",
			UID:       "uid-unsupported-run",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "run-unsupported",
		MonitorNamespace: "default",
		MonitorName:      "unsupported-run",
		Trigger:          "manual",
		TargetKind:       "commit",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        time.Now().Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "unsupported-run"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	run, err := monitorStore.GetMonitorRun(ctx, "default", "run-unsupported")
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseFailed || !strings.Contains(run.Error, "targetKind") {
		t.Fatalf("run = %#v, want failed unsupported targetKind", run)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "unsupported-run"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseError)
	}
}

func TestParseRepositoryMonitorPatchRightLinesHandlesPlusPlusContent(t *testing.T) {
	lines := parseRepositoryMonitorPatchRightLines("@@ -1,0 +10,2 @@\n++i\n+next")
	if _, ok := lines[10]; !ok {
		t.Fatalf("lines = %#v, want added ++i line 10 commentable", lines)
	}
	if _, ok := lines[11]; !ok {
		t.Fatalf("lines = %#v, want following added line 11 commentable", lines)
	}
}

func TestRepositoryMonitorItemFromPullRequestClearsPublishStateOnHeadChange(t *testing.T) {
	monitor := repositoryMonitorReviewIngestTestMonitor("publish-head-change")
	existing := &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      "publish-head-change",
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		HeadSHA:          "old-head",
		LastPublishID:    "publish-old",
		LastPublishPhase: repositoryMonitorPublishPhaseSucceeded,
		LastPublishURL:   "https://github.example/review/old",
	}
	item := repositoryMonitorItemFromPullRequest(monitor, repositoryMonitorPullRequest{Number: 1, State: repositoryMonitorItemStateOpen, HeadSHA: "new-head"}, existing)
	if item.LastPublishID != "" || item.LastPublishPhase != "" || item.LastPublishURL != "" {
		t.Fatalf("item = %#v, want publish status cleared for new head", item)
	}

	item = repositoryMonitorItemFromPullRequest(monitor, repositoryMonitorPullRequest{Number: 1, State: repositoryMonitorItemStateOpen, HeadSHA: "old-head"}, existing)
	if item.LastPublishID != "publish-old" || item.LastPublishPhase != repositoryMonitorPublishPhaseSucceeded || item.LastPublishURL == "" {
		t.Fatalf("item = %#v, want publish status preserved for same head", item)
	}
}

func TestRepositoryMonitorReconcileProcessesOldestQueuedRunFirst(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fifo-runs",
			Namespace: "default",
			UID:       "uid-fifo-runs",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
	now := time.Now()
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "older-run",
		MonitorNamespace: "default",
		MonitorName:      "fifo-runs",
		Trigger:          "manual",
		TargetKind:       "commit",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        now.Add(-2 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun(older) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{
		ID:               "newer-run",
		MonitorNamespace: "default",
		MonitorName:      "fifo-runs",
		Trigger:          "pull_request_event",
		TargetKind:       "commit",
		Phase:            repositoryMonitorRunPhaseQueued,
		StartedAt:        now.Add(-1 * time.Minute),
	}); err != nil {
		t.Fatalf("CreateMonitorRun(newer) error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "fifo-runs"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	older, err := monitorStore.GetMonitorRun(ctx, "default", "older-run")
	if err != nil {
		t.Fatalf("GetMonitorRun(older) error = %v", err)
	}
	if older.Phase != repositoryMonitorRunPhaseFailed || !strings.Contains(older.Error, "targetKind") {
		t.Fatalf("older run = %#v, want oldest queued run processed and failed", older)
	}
	newer, err := monitorStore.GetMonitorRun(ctx, "default", "newer-run")
	if err != nil {
		t.Fatalf("GetMonitorRun(newer) error = %v", err)
	}
	if newer.Phase != repositoryMonitorRunPhaseQueued || newer.CompletedAt != nil {
		t.Fatalf("newer run = %#v, want newer queued run left pending", newer)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "fifo-runs"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.LastRunID != "older-run" {
		t.Fatalf("LastRunID = %q, want older-run", current.Status.LastRunID)
	}
}

func newRepositoryMonitorPullRequestInventoryServer(t *testing.T) *httptest.Server {
	t.Helper()
	return newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":1,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature","sha":"sha1","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]},
		{"number":2,"title":"Draft","state":"open","draft":true,"mergeable_state":"unknown","user":{"login":"bob"},"base":{"ref":"main","sha":"base2","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"draft","sha":"sha2","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]},
		{"number":3,"title":"Blocked","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"cara"},"base":{"ref":"main","sha":"base3","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"blocked","sha":"sha3","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[{"name":"orka:human-review"}]},
		{"number":4,"title":"Over limit","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"dev"},"base":{"ref":"main","sha":"base4","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"second","sha":"sha4","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]},
		{"number":5,"title":"Already reviewed","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"erin"},"base":{"ref":"main","sha":"base5","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"reviewed","sha":"sha5","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]},
		{"number":6,"title":"Backport","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"frank"},"base":{"ref":"release","sha":"base6","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"backport","sha":"sha6","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}
	]`)
}

func newRepositoryMonitorPullRequestInventoryServerWithBody(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return newRepositoryMonitorPullRequestInventoryServerWithAuth(t, body, repositoryMonitorTestBearerHeader())
}

func newRepositoryMonitorPullRequestInventoryServerWithoutAuth(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return newRepositoryMonitorPullRequestInventoryServerWithAuth(t, body, "")
}

func newRepositoryMonitorSinglePullRequestServerWithBody(t *testing.T, number int64, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := fmt.Sprintf("/repos/orka-agents/orka/pulls/%d", number)
		if r.URL.Path != wantPath {
			t.Fatalf("request path = %q, want single pull request path %q", r.URL.Path, wantPath)
		}
		if got := r.Header.Get("Authorization"); got != repositoryMonitorTestBearerHeader() {
			t.Fatalf("Authorization header = %q, want %q", got, repositoryMonitorTestBearerHeader())
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func newRepositoryMonitorPullRequestInventoryServerWithAuth(t *testing.T, body, wantAuth string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/orka-agents/orka/pulls" {
			t.Fatalf("request path = %q, want pull inventory path", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != wantAuth {
			t.Fatalf("Authorization header = %q, want %q", got, wantAuth)
		}
		if got := r.URL.Query().Get("state"); got != "open" {
			t.Fatalf("state query = %q, want open", got)
		}
		if got := r.URL.Query().Get("base"); got != repositoryMonitorTestDefaultBranch {
			t.Fatalf("base query = %q, want main", got)
		}
		if got := r.URL.Query().Get("per_page"); got != "50" {
			t.Fatalf("per_page query = %q, want 50", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func repositoryMonitorTestBearerHeader() string {
	return strings.Join([]string{"Bearer", "test" + "-" + "token"}, " ")
}

func repositoryMonitorInventoryTestObjects(name string) (*corev1alpha1.RepositoryMonitor, *corev1.Secret) {
	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL:      "https://github.com/orka-agents/orka",
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
	return monitor, secret
}

func repositoryMonitorReviewIngestTestMonitor(name string) *corev1alpha1.RepositoryMonitor {
	return &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
}

func repositoryMonitorReviewIngestTestTask(name, monitorName string, prNumber int64, headSHA string) *corev1alpha1.Task {
	controller := true
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(),
				Kind:       "RepositoryMonitor",
				Name:       monitorName,
				UID:        types.UID("uid-" + monitorName),
				Controller: &controller,
			}},
			Annotations: map[string]string{
				labels.AnnotationRepositoryMonitorName: monitorName,
				labels.AnnotationMonitorItemKind:       repositoryMonitorPullRequestKind,
				labels.AnnotationMonitorItemNumber:     strconv.FormatInt(prNumber, 10),
				labels.AnnotationMonitorHeadSHA:        headSHA,
				labels.AnnotationGitHubRepository:      "orka-agents/orka",
			},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{
			Phase:     corev1alpha1.TaskPhaseSucceeded,
			ResultRef: &corev1alpha1.ResultReference{Available: true},
		},
	}
}

type transientGetResultErrorStore struct {
	store.ResultStore
	err error
}

func (s transientGetResultErrorStore) GetResult(_ context.Context, _, _ string) ([]byte, error) {
	return nil, s.err
}

func repositoryMonitorReviewResultEnvelope(t *testing.T, prNumber int64, headSHA, verdict string) []byte {
	t.Helper()
	return repositoryMonitorReviewResultEnvelopeWith(t, prNumber, headSHA, verdict, nil)
}

func repositoryMonitorReviewResultEnvelopeWith(t *testing.T, prNumber int64, headSHA, verdict string, mutate func(map[string]any)) []byte {
	t.Helper()

	payload := map[string]any{
		"schemaVersion": repositoryMonitorReviewSchemaVersion,
		"repo":          "orka-agents/orka",
		"prNumber":      prNumber,
		"headSHA":       headSHA,
		"verdict":       verdict,
		"confidence":    repositoryMonitorReviewConfidenceHigh,
		"repairable":    true,
		"summary":       "Review found one issue.",
		"findings": []map[string]any{
			{
				"priority":       "P1",
				"confidence":     repositoryMonitorReviewConfidenceHigh,
				"file":           "internal/example.go",
				"line":           42,
				"title":          "Bug",
				"body":           "This can fail.",
				"recommendation": "Handle the failure.",
			},
		},
		"security": map[string]any{
			"status": "clear",
			"notes":  "",
		},
		"tests": map[string]any{
			"status":   "not_run",
			"evidence": "",
		},
		"suggestedComment": "Please fix the failing path.",
	}
	if mutate != nil {
		mutate(payload)
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal review payload: %v", err)
	}
	envelopeJSON, err := json.Marshal(map[string]any{
		"version": 1,
		"summary": string(payloadJSON),
	})
	if err != nil {
		t.Fatalf("marshal review envelope: %v", err)
	}
	return envelopeJSON
}

type repositoryMonitorPublishTestServerConfig struct {
	State       string
	HeadSHA     string
	BaseBranch  string
	Labels      []string
	ReviewsBody string
	FilesBody   string
	PostStatus  int
	PostBody    string
}

type repositoryMonitorPublishTestServer struct {
	*httptest.Server
	PostCount    int
	PostedReview repositoryMonitorPullRequestReviewRequest
}

func newRepositoryMonitorPublishTestServer(t *testing.T, cfg repositoryMonitorPublishTestServerConfig) *repositoryMonitorPublishTestServer {
	t.Helper()
	if cfg.State == "" {
		cfg.State = "open"
	}
	if cfg.HeadSHA == "" {
		cfg.HeadSHA = repositoryMonitorTestHeadSHA
	}
	if cfg.BaseBranch == "" {
		cfg.BaseBranch = repositoryMonitorTestDefaultBranch
	}
	if cfg.ReviewsBody == "" {
		cfg.ReviewsBody = `[]`
	}
	if cfg.FilesBody == "" {
		cfg.FilesBody = `[]`
	}
	if cfg.PostStatus == 0 {
		cfg.PostStatus = http.StatusCreated
	}
	if cfg.PostBody == "" {
		cfg.PostBody = `{"id":123,"html_url":"https://github.com/orka-agents/orka/pull/1#pullrequestreview-123"}`
	}
	testServer := &repositoryMonitorPublishTestServer{}
	testServer.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != repositoryMonitorTestBearerHeader() {
			t.Fatalf("Authorization header = %q, want %q", got, repositoryMonitorTestBearerHeader())
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/pulls/1":
			_, _ = w.Write([]byte(repositoryMonitorPublishPullBody(cfg)))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/pulls/1/reviews":
			_, _ = w.Write([]byte(cfg.ReviewsBody))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/pulls/1/files":
			_, _ = w.Write([]byte(cfg.FilesBody))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/orka-agents/orka/pulls/1/reviews":
			testServer.PostCount++
			if err := json.NewDecoder(r.Body).Decode(&testServer.PostedReview); err != nil {
				t.Fatalf("decode posted review: %v", err)
			}
			w.WriteHeader(cfg.PostStatus)
			_, _ = w.Write([]byte(cfg.PostBody))
		default:
			t.Fatalf("unexpected GitHub request %s %s", r.Method, r.URL.String())
		}
	}))
	return testServer
}

func repositoryMonitorPublishPullBody(cfg repositoryMonitorPublishTestServerConfig) string {
	labelItems := make([]map[string]string, 0, len(cfg.Labels))
	for _, label := range cfg.Labels {
		labelItems = append(labelItems, map[string]string{"name": label})
	}
	body := map[string]any{
		"number":          1,
		"title":           "Publish me",
		"state":           cfg.State,
		"draft":           false,
		"mergeable_state": "clean",
		"user":            map[string]any{"login": "alice"},
		"base": map[string]any{
			"ref": cfg.BaseBranch,
			"sha": "base1",
			"repo": map[string]any{
				"full_name": "orka-agents/orka",
				"clone_url": "https://github.com/orka-agents/orka.git",
			},
		},
		"head": map[string]any{
			"ref": "feature",
			"sha": cfg.HeadSHA,
			"repo": map[string]any{
				"full_name": "orka-agents/orka",
				"clone_url": "https://github.com/orka-agents/orka.git",
			},
		},
		"labels": labelItems,
	}
	data, _ := json.Marshal(body)
	return string(data)
}

func seedRepositoryMonitorQueuedReview(t *testing.T, ctx context.Context, monitorStore store.RepositoryMonitorStore, monitorName, taskName string, result []byte) {
	t.Helper()
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{
		MonitorNamespace: "default",
		MonitorName:      monitorName,
		Kind:             repositoryMonitorPullRequestKind,
		Number:           1,
		State:            repositoryMonitorItemStateOpen,
		HeadSHA:          repositoryMonitorTestHeadSHA,
		LastVerdict:      repositoryMonitorRunPhaseQueued,
		LastReviewID:     taskName,
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(queued) error = %v", err)
	}
	resultStore, ok := monitorStore.(store.ResultStore)
	if !ok {
		t.Fatalf("monitorStore does not implement ResultStore")
	}
	if err := resultStore.SaveResult(ctx, "default", taskName, result); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}
}

func assertRepositoryMonitorRejectedReview(t *testing.T, ctx context.Context, monitorStore store.RepositoryMonitorStore, monitorName, itemKey string) (*store.ReviewRecord, *store.MonitorItem) {
	t.Helper()

	item, err := monitorStore.GetMonitorItem(ctx, "default", monitorName, repositoryMonitorPullRequestKind, itemKey)
	if err != nil {
		t.Fatalf("GetMonitorItem(%s) error = %v", itemKey, err)
	}
	records, _, err := monitorStore.ListReviewRecords(ctx, store.ReviewRecordFilter{
		Namespace:   "default",
		MonitorName: monitorName,
		Number:      item.Number,
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("ListReviewRecords() error = %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("records = %#v, want one rejected review record", records)
	}
	events, _, err := monitorStore.ListMonitorEvents(ctx, store.MonitorEventFilter{
		Namespace:   "default",
		MonitorName: monitorName,
		EventType:   "review_result_rejected",
		Limit:       10,
	})
	if err != nil {
		t.Fatalf("ListMonitorEvents() error = %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events = %#v, want one rejection event", events)
	}
	return &records[0], item
}

func assertRepositoryMonitorInventoryItems(t *testing.T, ctx context.Context, monitorStore store.RepositoryMonitorStore) {
	t.Helper()
	wantReasons := map[string]string{
		"1": "",
		"2": "draft",
		"3": "blocked_label",
		"4": repositoryMonitorSkipReasonOverLimit,
		"5": "already_reviewed",
	}
	for itemKey, wantReason := range wantReasons {
		item, err := monitorStore.GetMonitorItem(ctx, "default", "inventory", repositoryMonitorPullRequestKind, itemKey)
		if err != nil {
			t.Fatalf("GetMonitorItem(%s) error = %v", itemKey, err)
		}
		if item.SkipReason != wantReason {
			t.Fatalf("item %s skipReason = %q, want %q", itemKey, item.SkipReason, wantReason)
		}
		if wantReason == "" && item.LastVerdict != repositoryMonitorRunPhaseQueued {
			t.Fatalf("item %s verdict = %q, want queued", itemKey, item.LastVerdict)
		}
		if itemKey == "1" && item.LastReviewID == "" {
			t.Fatalf("item %s LastReviewID is empty, want review task name", itemKey)
		}
		if itemKey == "5" {
			if item.LastVerdict != repositoryMonitorReviewVerdictNeedsChanges {
				t.Fatalf("item %s verdict = %q, want previous review verdict %q", itemKey, item.LastVerdict, repositoryMonitorReviewVerdictNeedsChanges)
			}
			continue
		}
		if wantReason != "" && item.LastVerdict != "skipped" {
			t.Fatalf("item %s verdict = %q, want skipped", itemKey, item.LastVerdict)
		}
	}
	if _, err := monitorStore.GetMonitorItem(ctx, "default", "inventory", repositoryMonitorPullRequestKind, "6"); err != store.ErrNotFound {
		t.Fatalf("GetMonitorItem(release branch) error = %v, want ErrNotFound", err)
	}
}

func assertRepositoryMonitorInventoryEvents(t *testing.T, ctx context.Context, monitorStore store.RepositoryMonitorStore) {
	t.Helper()
	events, _, err := monitorStore.ListMonitorEvents(ctx, store.MonitorEventFilter{Namespace: "default", MonitorName: "inventory", RunID: "run-1", Limit: 20})
	if err != nil {
		t.Fatalf("ListMonitorEvents() error = %v", err)
	}
	eventCounts := map[string]int{}
	for _, event := range events {
		eventCounts[event.EventType]++
	}
	if eventCounts["review_task_created"] != 1 || eventCounts["item_selected"] != 1 || eventCounts["item_skipped"] != 4 || eventCounts["run_succeeded"] != 1 {
		data, _ := json.Marshal(events)
		t.Fatalf("events = %s, want review_task_created/selected/skipped/run_succeeded counts", data)
	}
}

func assertRepositoryMonitorReviewTask(t *testing.T, ctx context.Context, cl crclient.Client, monitorStore store.RepositoryMonitorStore) {
	t.Helper()
	item, err := monitorStore.GetMonitorItem(ctx, "default", "inventory", repositoryMonitorPullRequestKind, "1")
	if err != nil {
		t.Fatalf("GetMonitorItem(selected) error = %v", err)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: item.LastReviewID}, &task); err != nil {
		t.Fatalf("Get review task %q error = %v", item.LastReviewID, err)
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAgent {
		t.Fatalf("task type = %q, want agent", task.Spec.Type)
	}
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "reviewer" {
		t.Fatalf("task AgentRef = %#v, want reviewer", task.Spec.AgentRef)
	}
	if task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil {
		t.Fatalf("task AgentRuntime.Workspace is nil")
	}
	if task.Spec.AgentRuntime.Workspace.GitRepo != repositoryMonitorTestRepoURL || task.Spec.AgentRuntime.Workspace.Ref != "sha1" || task.Spec.AgentRuntime.Workspace.PRBaseBranch != repositoryMonitorTestDefaultBranch {
		t.Fatalf("workspace = %#v, want repo with exact PR head sha1", task.Spec.AgentRuntime.Workspace)
	}
	if task.Spec.AgentRuntime.Workspace.GitSecretRef == nil || task.Spec.AgentRuntime.Workspace.GitSecretRef.Name != "github-token" {
		t.Fatalf("workspace GitSecretRef = %#v, want github-token", task.Spec.AgentRuntime.Workspace.GitSecretRef)
	}
	if got := repositoryMonitorTaskEnvValue(task.Spec.Env, workerenv.PRBaseRepo); got != repositoryMonitorTestRepoURL {
		t.Fatalf("%s = %q, want %s", workerenv.PRBaseRepo, got, repositoryMonitorTestRepoURL)
	}
	if got := repositoryMonitorTaskEnvValue(task.Spec.Env, workerenv.PRBaseSHA); got != "base1" {
		t.Fatalf("%s = %q, want base1", workerenv.PRBaseSHA, got)
	}
	if got := repositoryMonitorTaskEnvValue(task.Spec.Env, workerenv.ResultStdout); got != scheduledRunLabelValue {
		t.Fatalf("%s = %q, want "+scheduledRunLabelValue+" so review JSON is preserved", workerenv.ResultStdout, got)
	}
	if task.Labels[labels.LabelRepositoryMonitor] != labels.SelectorValue("inventory") || task.Labels[labels.LabelMonitorRun] != labels.SelectorValue("run-1") {
		t.Fatalf("task labels = %#v, want monitor and run labels", task.Labels)
	}
	if task.Annotations[labels.AnnotationMonitorHeadSHA] != "sha1" || task.Annotations[labels.AnnotationMonitorItemNumber] != "1" {
		t.Fatalf("task annotations = %#v, want PR number and exact head", task.Annotations)
	}
	if !strings.Contains(task.Spec.Prompt, `"schemaVersion": "orka.prReview.input.v1"`) || !strings.Contains(task.Spec.Prompt, `"headSHA": "sha1"`) || !strings.Contains(task.Spec.Prompt, `"schemaVersion": "orka.prReview.v1"`) {
		t.Fatalf("task prompt does not include expected review input/output contracts:\n%s", task.Spec.Prompt)
	}
	if !strings.Contains(task.Spec.Prompt, "/workspace/.git/orka/pr-review.diff") {
		t.Fatalf("task prompt does not include generated PR diff context path:\n%s", task.Spec.Prompt)
	}
}

func repositoryMonitorTaskEnvValue(envVars []corev1.EnvVar, name string) string {
	for _, envVar := range envVars {
		if envVar.Name == name {
			return envVar.Value
		}
	}
	return ""
}

func TestRepositoryMonitorReconcileRejectsInvalidRepoURLWithoutPersistingMetadata(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "invalid",
			Namespace: "default",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://token@github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "invalid"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if _, err := monitorStore.GetRepositoryMonitor(ctx, "default", "invalid"); err != store.ErrNotFound {
		t.Fatalf("GetRepositoryMonitor() error = %v, want ErrNotFound", err)
	}
	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "invalid"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseError)
	}
	if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != "InvalidRepositoryURL" {
		t.Fatalf("conditions = %#v, want InvalidRepositoryURL", current.Status.Conditions)
	}
}

func TestRepositoryMonitorReconcileRejectsUnsupportedTargetWithoutPersistingMetadata(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	pullRequestsEnabled := false
	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unsupported-target",
			Namespace: "default",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Targets: corev1alpha1.RepositoryMonitorTargets{
				PullRequests: corev1alpha1.RepositoryMonitorPullRequestTarget{Enabled: &pullRequestsEnabled},
				Commits:      corev1alpha1.RepositoryMonitorCommitTarget{Enabled: true},
			},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "unsupported-target"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if _, err := monitorStore.GetRepositoryMonitor(ctx, "default", "unsupported-target"); err != store.ErrNotFound {
		t.Fatalf("GetRepositoryMonitor() error = %v, want ErrNotFound", err)
	}
	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "unsupported-target"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseError)
	}
	if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != "UnsupportedTarget" {
		t.Fatalf("conditions = %#v, want UnsupportedTarget", current.Status.Conditions)
	}
}

func TestRepositoryMonitorReconcileAllowsRequireGreenCI(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor, secret := repositoryMonitorInventoryTestObjects("require-green-ci")
	monitor.Spec.Review.RequireGreenCI = true
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "require-green-ci"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, err := monitorStore.GetRepositoryMonitor(ctx, "default", "require-green-ci"); err != nil {
		t.Fatalf("GetRepositoryMonitor() error = %v", err)
	}
}

func TestRepositoryMonitorReconcileRejectsMissingReviewerWithoutPersistingMetadata(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "missing-reviewer",
			Namespace: "default",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "missing-reviewer"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if _, err := monitorStore.GetRepositoryMonitor(ctx, "default", "missing-reviewer"); err != store.ErrNotFound {
		t.Fatalf("GetRepositoryMonitor() error = %v, want ErrNotFound", err)
	}
	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "missing-reviewer"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseError)
	}
	if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != "MissingReviewerAgent" {
		t.Fatalf("conditions = %#v, want MissingReviewerAgent", current.Status.Conditions)
	}
}

func TestRepositoryMonitorReconcileRejectsInvalidReviewerAgentWithoutPersistingMetadata(t *testing.T) {
	tests := []struct {
		name     string
		reviewer string
		objects  []crclient.Object
		reason   string
	}{
		{
			name:     "missing agent",
			reviewer: "missing-reviewer",
			reason:   "ReviewerAgentNotFound",
		},
		{
			name:     "agent without runtime",
			reviewer: "no-runtime",
			objects: []crclient.Object{
				&corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "no-runtime", Namespace: "default"}},
			},
			reason: "UnsupportedReviewerAgent",
		},
		{
			name:     "agent without secretRef",
			reviewer: "no-secret",
			objects: []crclient.Object{
				repositoryMonitorControllerTestAgent("no-secret", corev1alpha1.AgentRuntimeClaude, ""),
			},
			reason: repositoryMonitorReasonReviewerCredentialsInvalid,
		},
		{
			name:     "secret without auth key",
			reviewer: "bad-secret-reviewer",
			objects: []crclient.Object{
				repositoryMonitorControllerTestAgent("bad-secret-reviewer", corev1alpha1.AgentRuntimeClaude, "bad-reviewer-secret"),
				repositoryMonitorControllerTestSecret("bad-reviewer-secret", map[string][]byte{
					workerenv.AnthropicBaseURL: []byte("https://anthropic.example"),
				}),
			},
			reason: repositoryMonitorReasonReviewerCredentialsInvalid,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			monitorStore := setupControllerSQLiteStore(t)
			scheme := runtime.NewScheme()
			if err := corev1alpha1.AddToScheme(scheme); err != nil {
				t.Fatalf("AddToScheme() error = %v", err)
			}
			if err := corev1.AddToScheme(scheme); err != nil {
				t.Fatalf("corev1 AddToScheme() error = %v", err)
			}

			monitor := &corev1alpha1.RepositoryMonitor{
				TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
				ObjectMeta: metav1.ObjectMeta{
					Name:      "invalid-reviewer-" + repositoryMonitorBoundedDNSName(tt.name, 24),
					Namespace: "default",
				},
				Spec: corev1alpha1.RepositoryMonitorSpec{
					RepoURL: "https://github.com/orka-agents/orka",
					Agents: corev1alpha1.RepositoryMonitorAgents{
						Reviewer: &corev1alpha1.AgentReference{Name: tt.reviewer},
					},
				},
			}
			objects := repositoryMonitorControllerObjects(append([]crclient.Object{monitor}, tt.objects...)...)
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
				WithObjects(objects...).
				Build()
			reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

			result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}})
			if err != nil {
				t.Fatalf("Reconcile() error = %v", err)
			}
			if result.RequeueAfter != repositoryMonitorValidationRetry {
				t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, repositoryMonitorValidationRetry)
			}

			if _, err := monitorStore.GetRepositoryMonitor(ctx, "default", monitor.Name); err != store.ErrNotFound {
				t.Fatalf("GetRepositoryMonitor() error = %v, want ErrNotFound", err)
			}
			var current corev1alpha1.RepositoryMonitor
			if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: monitor.Name}, &current); err != nil {
				t.Fatalf("Get monitor() error = %v", err)
			}
			if current.Status.Phase != repositoryMonitorPhaseError {
				t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseError)
			}
			if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != tt.reason {
				t.Fatalf("conditions = %#v, want %s", current.Status.Conditions, tt.reason)
			}
		})
	}
}

func TestRepositoryMonitorReconcileRejectsInvalidGitSecretWithoutPersistingMetadata(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "bad-git-secret",
			Namespace: "default",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL:      "https://github.com/orka-agents/orka",
			GitSecretRef: &corev1.LocalObjectReference{Name: "bad-git-secret"},
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	secret := repositoryMonitorControllerTestSecret("bad-git-secret", map[string][]byte{
		"username": []byte("octocat"),
	})
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	result, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "bad-git-secret"}})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if result.RequeueAfter != repositoryMonitorValidationRetry {
		t.Fatalf("RequeueAfter = %v, want %v", result.RequeueAfter, repositoryMonitorValidationRetry)
	}

	if _, err := monitorStore.GetRepositoryMonitor(ctx, "default", "bad-git-secret"); err != store.ErrNotFound {
		t.Fatalf("GetRepositoryMonitor() error = %v, want ErrNotFound", err)
	}
	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "bad-git-secret"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseError)
	}
	if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != repositoryMonitorReasonGitSecretInvalid {
		t.Fatalf("conditions = %#v, want %s", current.Status.Conditions, repositoryMonitorReasonGitSecretInvalid)
	}
}

func TestReadRepositoryMonitorGitHubResponseRejectsOversizedBody(t *testing.T) {
	if _, err := readRepositoryMonitorGitHubResponse(strings.NewReader("abcdef"), 5); err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("readRepositoryMonitorGitHubResponse() error = %v, want exceeded error", err)
	}
}

func TestRepositoryMonitorReconcileUnsuspendSetsReady(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "unsuspended",
			Namespace: "default",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/orka-agents/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
		Status: corev1alpha1.RepositoryMonitorStatus{Phase: repositoryMonitorPhaseSuspended},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "unsuspended"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "unsuspended"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseReady {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseReady)
	}
}

func repositoryMonitorControllerObjects(objects ...crclient.Object) []crclient.Object {
	defaults := []crclient.Object{
		repositoryMonitorControllerTestAgent("reviewer", corev1alpha1.AgentRuntimeClaude, repositoryMonitorTestReviewerSecret),
		repositoryMonitorControllerTestAgent("triager", corev1alpha1.AgentRuntimeClaude, repositoryMonitorTestReviewerSecret),
		repositoryMonitorControllerTestAgent("researcher", corev1alpha1.AgentRuntimeClaude, repositoryMonitorTestReviewerSecret),
		repositoryMonitorControllerTestAgent("planner", corev1alpha1.AgentRuntimeClaude, repositoryMonitorTestReviewerSecret),
		repositoryMonitorControllerTestAgent("implementer", corev1alpha1.AgentRuntimeCodex, "implementer-credentials"),
		repositoryMonitorControllerTestAgent("repairer", corev1alpha1.AgentRuntimeCodex, "implementer-credentials"),
		repositoryMonitorControllerTestSecret("implementer-credentials", map[string][]byte{
			workerenv.OpenAIAPIKey: []byte("x"),
		}),
		repositoryMonitorControllerTestSecret(repositoryMonitorTestReviewerSecret, map[string][]byte{
			workerenv.AnthropicAPIKey: []byte("anthropic-key"),
		}),
	}
	return append(defaults, objects...)
}

func repositoryMonitorControllerTestAgent(name string, runtimeType corev1alpha1.AgentRuntimeType, secretName string) *corev1alpha1.Agent {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: runtimeType},
		},
	}
	if secretName != "" {
		agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: secretName}
	}
	return agent
}

func repositoryMonitorControllerTestSecret(name string, data map[string][]byte) *corev1.Secret {
	immutable := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", UID: types.UID("uid-" + name)},
		Data:       data,
		Immutable:  &immutable,
	}
}

func storeMonitorRunFilter(namespace, monitorName string) store.MonitorRunFilter {
	return store.MonitorRunFilter{Namespace: namespace, MonitorName: monitorName, Limit: 10}
}

type failingMonitorEventStore struct {
	store.RepositoryMonitorStore
	failEventType string
}

func (s failingMonitorEventStore) CreateMonitorEvent(ctx context.Context, event *store.MonitorEvent) error {
	if event != nil && event.EventType == s.failEventType {
		return errors.New("audit event unavailable")
	}
	return s.RepositoryMonitorStore.CreateMonitorEvent(ctx, event)
}

type statusPatchCountingClient struct {
	crclient.Client
	statusPatchCount int
}

func (c *statusPatchCountingClient) Status() crclient.SubResourceWriter {
	return &statusPatchCountingWriter{
		SubResourceWriter: c.Client.Status(),
		statusPatchCount:  &c.statusPatchCount,
	}
}

type statusPatchCountingWriter struct {
	crclient.SubResourceWriter
	statusPatchCount *int
}

func (w *statusPatchCountingWriter) Patch(ctx context.Context, obj crclient.Object, patch crclient.Patch, opts ...crclient.SubResourcePatchOption) error {
	*w.statusPatchCount++
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

func TestRepositoryMonitorIssueContentDigestIgnoresOrkaLabels(t *testing.T) {
	base := repositoryMonitorIssue{Number: 12, Title: "Bug", Body: "Fix me", Labels: []string{"bug", "orka:plan", "orka-state:planning"}}
	withOrkaNoise := base
	withOrkaNoise.Labels = []string{"orka:implement", "bug", "orka-state:ready"}
	if got, want := repositoryMonitorIssueContentDigest(withOrkaNoise), repositoryMonitorIssueContentDigest(base); got != want {
		t.Fatalf("digest changed for Orka-owned labels: got %s want %s", got, want)
	}
	customCommand := base
	customCommand.Labels = []string{"bug", "bot:plan"}
	if got, want := repositoryMonitorIssueContentDigest(customCommand, "bot:plan"), repositoryMonitorIssueContentDigest(base, "bot:plan"); got != want {
		t.Fatalf("digest changed for configured command label: got %s want %s", got, want)
	}
	changed := base
	changed.Labels = []string{"bug", "priority:p1"}
	if got, want := repositoryMonitorIssueContentDigest(changed), repositoryMonitorIssueContentDigest(base); got == want {
		t.Fatalf("digest did not change for human-controlled label: %s", got)
	}
}

func TestRepositoryMonitorMutationResultAcceptsPushEnvelope(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "mutation-envelope", Namespace: "default"}}
	item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "77", Number: 77, SnapshotDigest: "sha256:issue77"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "mutation-task", Namespace: "default", Annotations: map[string]string{repositoryMonitorIssueAnnotationCommandID: "cmd-mutation"}}}
	raw := []byte(`{"version":1,"summary":"pushed","baseSHA":"base","headSHA":"head","pushBranch":"orka/issue-77"}`)

	record := repositoryMonitorActionRecordFromTask(monitor, item, task, repositoryMonitorIssueActionMutateToPR, raw)
	if record.Verdict != repositoryMonitorIssueVerdictSuccess {
		t.Fatalf("mutation verdict = %q, want %q; summary=%q", record.Verdict, repositoryMonitorIssueVerdictSuccess, record.Summary)
	}
	if record.Summary != "pushed" {
		t.Fatalf("summary = %q, want pushed", record.Summary)
	}
}

func TestRepositoryMonitorWorkActionReasonFields(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "reason-fields", Namespace: "default", UID: types.UID("uid-reason-fields")}}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	run := &store.MonitorRun{ID: "run-reason", MonitorNamespace: "default", MonitorName: monitor.Name}
	command := &store.CommandEvent{ID: "cmd-reason", MonitorNamespace: "default", MonitorName: monitor.Name, Intent: "stop", IdempotencyKey: "idem-reason"}

	if err := reconciler.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorIssueKind, 77, "", "sha256:reason", repositoryMonitorCommandIntentStop, repositoryMonitorWorkActionStatusSucceeded, "stopped", "", "stopped_by_command"); err != nil {
		t.Fatalf("record succeeded action error = %v", err)
	}
	action, err := monitorStore.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentStop))
	if err != nil {
		t.Fatalf("GetWorkAction(succeeded) error = %v", err)
	}
	if action.BlockedReason != "" || action.Error != "" {
		t.Fatalf("succeeded action BlockedReason=%q Error=%q, want both empty", action.BlockedReason, action.Error)
	}

	if err := reconciler.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorIssueKind, 77, "", "sha256:reason", repositoryMonitorCommandIntentStop, repositoryMonitorWorkActionStatusBlocked, "blocked", "", "guarded"); err != nil {
		t.Fatalf("record blocked action error = %v", err)
	}
	action, err = monitorStore.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentStop))
	if err != nil {
		t.Fatalf("GetWorkAction(blocked) error = %v", err)
	}
	if action.BlockedReason != "guarded" || action.Error != "" {
		t.Fatalf("blocked action BlockedReason=%q Error=%q, want blocked reason only", action.BlockedReason, action.Error)
	}

	if err := reconciler.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorIssueKind, 77, "", "sha256:reason", repositoryMonitorCommandIntentStop, repositoryMonitorWorkActionStatusFailed, "failed", "", "boom"); err != nil {
		t.Fatalf("record failed action error = %v", err)
	}
	action, err = monitorStore.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentStop))
	if err != nil {
		t.Fatalf("GetWorkAction(failed) error = %v", err)
	}
	if action.BlockedReason != "" || action.Error != "boom" {
		t.Fatalf("failed action BlockedReason=%q Error=%q, want error only", action.BlockedReason, action.Error)
	}
	if action.CompletedAt == nil {
		t.Fatal("failed action CompletedAt is nil, want terminal timestamp")
	}

	if err := reconciler.recordRepositoryMonitorWorkActionState(ctx, monitor, run, command, repositoryMonitorIssueKind, 77, "", "sha256:reason", repositoryMonitorCommandIntentStop, repositoryMonitorWorkActionStatusRunning, "retrying", "", ""); err != nil {
		t.Fatalf("record retrying action error = %v", err)
	}
	action, err = monitorStore.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentStop))
	if err != nil {
		t.Fatalf("GetWorkAction(retrying) error = %v", err)
	}
	if action.Status != repositoryMonitorWorkActionStatusRunning || action.CompletedAt != nil || action.BlockedReason != "" || action.Error != "" {
		t.Fatalf("retrying action = %#v, want non-terminal running state", action)
	}
}

func TestRepositoryMonitorDecomposeActionReusesCommandWorkAction(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "decompose-action", Namespace: defaultNS}}
	command := &store.CommandEvent{ID: "cmd-decompose", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Intent: "decompose"}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, "decompose")
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, DesiredAction: "decompose", Status: repositoryMonitorWorkActionStatusQueued, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	if err := reconciler.recordRepositoryMonitorWorkActionState(ctx, monitor, nil, command, repositoryMonitorIssueKind, 77, "", "sha256:decompose", repositoryMonitorIssueActionDecompose, repositoryMonitorWorkActionStatusRunning, repositoryMonitorIssuePhasePlanQueued, "decompose-task", ""); err != nil {
		t.Fatalf("recordRepositoryMonitorWorkActionState() error = %v", err)
	}
	actions, _, err := monitorStore.ListWorkActions(ctx, store.WorkActionFilter{Namespace: defaultNS, MonitorName: monitor.Name, Limit: 10})
	if err != nil {
		t.Fatalf("ListWorkActions() error = %v", err)
	}
	if len(actions) != 1 || actions[0].ID != actionID || actions[0].DesiredAction != "decompose" || actions[0].Status != repositoryMonitorWorkActionStatusRunning {
		t.Fatalf("decompose actions = %#v, want one updated command action", actions)
	}
}

func TestRepositoryMonitorIssueActionTaskResultModes(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("issue-result-mode")
	monitor.Spec.Agents.Planner = &corev1alpha1.AgentReference{Name: "planner"}
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
	item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "77", Number: 77, Title: "Metrics", State: "open", SnapshotDigest: "sha256:issue77"}
	run := &store.MonitorRun{ID: "run-result-mode", MonitorNamespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: 77}
	command := &store.CommandEvent{ID: "cmd-result-mode", MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 77, Intent: "plan", Status: "accepted"}

	planTaskName, queued, err := reconciler.createRepositoryMonitorIssueActionTask(ctx, monitor, run, command, item, "orka-agents", "orka", repositoryMonitorIssueActionPlan, repositoryMonitorIssuePhasePlanQueued, monitor.Spec.Agents.Planner)
	if err != nil || !queued {
		t.Fatalf("create plan task queued=%v err=%v", queued, err)
	}
	var planTask corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: planTaskName}, &planTask); err != nil {
		t.Fatalf("Get plan task error = %v", err)
	}
	if !taskEnvHasValue(planTask.Spec.Env, workerenv.ResultStdout, scheduledRunLabelValue) {
		t.Fatalf("plan task env = %#v, want %s=true", planTask.Spec.Env, workerenv.ResultStdout)
	}
	if planTask.Annotations[labels.AnnotationAgentReadOnly] != scheduledRunLabelValue || planTask.Annotations[labels.AnnotationWorkspaceInitContainer] != scheduledRunLabelValue {
		t.Fatalf("plan task annotations = %#v, want hardened read-only workspace", planTask.Annotations)
	}

	implementationTaskName, queued, err := reconciler.createRepositoryMonitorIssueActionTask(ctx, monitor, run, command, item, "orka-agents", "orka", repositoryMonitorIssueActionImplementation, repositoryMonitorIssuePhaseImplementationQueued, monitor.Spec.Agents.Implementer)
	if err != nil || !queued {
		t.Fatalf("create implementation task queued=%v err=%v", queued, err)
	}
	var implementationTask corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: implementationTaskName}, &implementationTask); err != nil {
		t.Fatalf("Get implementation task error = %v", err)
	}
	if taskEnvHasValue(implementationTask.Spec.Env, workerenv.ResultStdout, scheduledRunLabelValue) {
		t.Fatalf("implementation task env = %#v, did not want %s=true because workspace finalization must capture patch diffs", implementationTask.Spec.Env, workerenv.ResultStdout)
	}
	if implementationTask.Annotations[labels.AnnotationAgentReadOnly] != "" || implementationTask.Annotations[labels.AnnotationWorkspaceInitContainer] != scheduledRunLabelValue || implementationTask.Annotations[labels.AnnotationAgentRuntimeAuthOnly] != scheduledRunLabelValue {
		t.Fatalf("implementation task annotations = %#v, want workspace init and runtime-only credentials without read-only tools", implementationTask.Annotations)
	}
	if implementationTask.Spec.SecretRef == nil || implementationTask.Spec.SecretRef.Name == "implementer-credentials" || implementationTask.Annotations[repositoryMonitorIssueAnnotationRuntimeAuthFields] != workerenv.OpenAIAPIKey {
		t.Fatalf("implementation runtime credential binding = spec=%#v annotations=%#v", implementationTask.Spec.SecretRef, implementationTask.Annotations)
	}
	var runtimeSnapshot corev1.Secret
	if err := cl.Get(ctx, types.NamespacedName{Namespace: defaultNS, Name: implementationTask.Spec.SecretRef.Name}, &runtimeSnapshot); err != nil {
		t.Fatalf("Get runtime auth snapshot error = %v", err)
	}
	if runtimeSnapshot.Immutable == nil || !*runtimeSnapshot.Immutable || string(runtimeSnapshot.Data[workerenv.OpenAIAPIKey]) != "x" || !metav1.IsControlledBy(&runtimeSnapshot, monitor) {
		t.Fatalf("runtime auth snapshot = %#v, want immutable monitor-owned scoped copy", runtimeSnapshot)
	}
	if err := cl.Delete(ctx, &implementationTask); err != nil {
		t.Fatalf("Delete implementation task error = %v", err)
	}
	if err := reconciler.cleanupRepositoryMonitorOrphanedRuntimeAuthSnapshots(ctx, monitor); err != nil {
		t.Fatalf("cleanupRepositoryMonitorOrphanedRuntimeAuthSnapshots() error = %v", err)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: defaultNS, Name: runtimeSnapshot.Name}, &corev1.Secret{}); err == nil || crclient.IgnoreNotFound(err) != nil {
		t.Fatalf("runtime auth snapshot cleanup error = %v, want deleted snapshot", err)
	}
}

func TestRepositoryMonitorImplementationTaskCreationFailureCleansRuntimeAuthSnapshot(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("implementation-create-cleanup")
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, ok := obj.(*corev1alpha1.Task); ok {
					return errors.New("permanent task creation failure")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
	item := &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "77", Number: 77, SnapshotDigest: "sha256:create-cleanup"}
	run := &store.MonitorRun{ID: "run-create-cleanup"}
	command := &store.CommandEvent{ID: "cmd-create-cleanup", Intent: repositoryMonitorCommandIntentImplement}
	if _, created, err := reconciler.createRepositoryMonitorIssueActionTask(ctx, monitor, run, command, item, "orka-agents", "orka", repositoryMonitorIssueActionImplementation, repositoryMonitorIssuePhaseImplementationQueued, monitor.Spec.Agents.Implementer); err == nil || created {
		t.Fatalf("createRepositoryMonitorIssueActionTask() created=%v err=%v, want permanent failure", created, err)
	}
	var snapshots corev1.SecretList
	if err := cl.List(ctx, &snapshots, crclient.InNamespace(defaultNS), crclient.MatchingLabels{
		labels.LabelCreatedBy:         "repository-monitor",
		labels.LabelRepositoryMonitor: labels.SelectorValue(monitor.Name),
	}); err != nil {
		t.Fatalf("List snapshots error = %v", err)
	}
	if len(snapshots.Items) != 0 {
		t.Fatalf("runtime auth snapshots = %#v, want cleanup after task creation failure", snapshots.Items)
	}
}

func TestRepositoryMonitorImplementationTaskAmbiguousCreatePreservesSnapshot(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	monitor, secret := repositoryMonitorInventoryTestObjects("implementation-ambiguous-create")
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, ok := obj.(*corev1alpha1.Task); ok {
					if err := c.Create(ctx, obj, opts...); err != nil {
						return err
					}
					return errors.New("timeout after task persisted")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
	item := &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "78", Number: 78, SnapshotDigest: "sha256:ambiguous-create"}
	run := &store.MonitorRun{ID: "run-ambiguous-create"}
	command := &store.CommandEvent{ID: "cmd-ambiguous-create", Intent: repositoryMonitorCommandIntentImplement}
	taskName, created, err := reconciler.createRepositoryMonitorIssueActionTask(ctx, monitor, run, command, item, "orka-agents", "orka", repositoryMonitorIssueActionImplementation, repositoryMonitorIssuePhaseImplementationQueued, monitor.Spec.Agents.Implementer)
	if err != nil || created || taskName == "" {
		t.Fatalf("createRepositoryMonitorIssueActionTask() task=%q created=%v err=%v", taskName, created, err)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: defaultNS, Name: taskName}, &task); err != nil {
		t.Fatalf("Get persisted task error = %v", err)
	}
	if task.Spec.SecretRef == nil {
		t.Fatal("persisted task has no runtime auth snapshot reference")
	}
	var snapshot corev1.Secret
	if err := cl.Get(ctx, types.NamespacedName{Namespace: defaultNS, Name: task.Spec.SecretRef.Name}, &snapshot); err != nil {
		t.Fatalf("Get preserved snapshot error = %v", err)
	}
	var source corev1.Secret
	if err := cl.Get(ctx, types.NamespacedName{Namespace: defaultNS, Name: "implementer-credentials"}, &source); err != nil {
		t.Fatalf("Get source runtime config error = %v", err)
	}
	source.Immutable = nil
	source.Data[workerenv.OpenAIAPIKey] = []byte("rotated-value")
	if err := cl.Update(ctx, &source); err != nil {
		t.Fatalf("Update source runtime config error = %v", err)
	}
	recoveredName, recoveredCreated, err := reconciler.createRepositoryMonitorIssueActionTask(ctx, monitor, run, command, item, "orka-agents", "orka", repositoryMonitorIssueActionImplementation, repositoryMonitorIssuePhaseImplementationQueued, monitor.Spec.Agents.Implementer)
	if err != nil || recoveredCreated || recoveredName != taskName {
		t.Fatalf("retry after source rotation task=%q created=%v err=%v", recoveredName, recoveredCreated, err)
	}
}

func TestRepositoryMonitorRejectsMismatchedPreexistingRuntimeAuthSnapshot(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	monitor := &corev1alpha1.RepositoryMonitor{TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"}, ObjectMeta: metav1.ObjectMeta{Name: "snapshot-spoof", Namespace: defaultNS, UID: "uid-snapshot-spoof"}}
	ref := &corev1alpha1.AgentReference{Name: "implementer"}
	agent := repositoryMonitorControllerTestAgent("implementer", corev1alpha1.AgentRuntimeCodex, "source-runtime")
	source := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "source-runtime", Namespace: defaultNS, UID: "uid-source-runtime"}, Data: map[string][]byte{workerenv.OpenAIAPIKey: []byte("expected-value")}}
	taskName := "impl-spoofed-snapshot"
	snapshotName := repositoryMonitorBoundedDNSName(taskName+"-runtime-auth", 63)
	controller := true
	immutable := true
	spoofed := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Name: snapshotName, Namespace: defaultNS,
		Labels:          map[string]string{labels.LabelManaged: scheduledRunLabelValue, labels.LabelCreatedBy: "repository-monitor", labels.LabelRepositoryMonitor: labels.SelectorValue(monitor.Name)},
		Annotations:     map[string]string{repositoryMonitorIssueAnnotationRuntimeAuthTask: taskName, repositoryMonitorIssueAnnotationRuntimeAuthSourceUID: string(source.UID)},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor", Name: monitor.Name, UID: monitor.UID, Controller: &controller}},
	}, Data: map[string][]byte{workerenv.OpenAIAPIKey: []byte("attacker-value"), workerenv.OpenAIBaseURL: []byte("https://example.invalid/v1")}, Immutable: &immutable}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(monitor, agent, source, spoofed).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme}
	if _, err := reconciler.repositoryMonitorImplementationRuntimeCredentialBinding(ctx, monitor, taskName, ref); !errors.Is(err, errRepositoryMonitorRuntimeAuthBindingInvalid) {
		t.Fatalf("binding error = %v, want mismatched snapshot rejection", err)
	}
}

func taskEnvHasValue(env []corev1.EnvVar, name, value string) bool {
	for _, entry := range env {
		if entry.Name == name && entry.Value == value {
			return true
		}
	}
	return false
}

func TestRepositoryMonitorReconcileInventoriesIssues(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/orka-agents/orka/issues" {
			t.Fatalf("request path = %q, want issue inventory path", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != repositoryMonitorTestBearerHeader() {
			t.Fatalf("Authorization header = %q, want bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number":11,"title":"Open issue","body":"Implement this","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/orka-agents/orka/issues/11","user":{"login":"alice"},"labels":[{"name":"bug"},{"name":"orka:plan"}]},
			{"number":12,"title":"PR-shaped issue","body":"","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/orka-agents/orka/pull/12","user":{"login":"bob"},"pull_request":{"html_url":"https://github.com/orka-agents/orka/pull/12"},"labels":[]}
		]`))
	}))
	t.Cleanup(server.Close)

	pullRequestsEnabled := false
	monitor, secret := repositoryMonitorInventoryTestObjects("issue-inventory")
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-issues", MonitorNamespace: "default", MonitorName: "issue-inventory", Trigger: "manual", Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "issue-inventory"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "issue-inventory", repositoryMonitorIssueKind, "11")
	if err != nil {
		t.Fatalf("GetMonitorItem(issue) error = %v", err)
	}
	if item.SnapshotDigest == "" || item.WorkflowPhase != repositoryMonitorIssuePhaseDiscovered || item.State != repositoryMonitorItemStateOpen {
		t.Fatalf("issue item = %#v, want digest/discovered/open", item)
	}
	if _, err := monitorStore.GetMonitorItem(ctx, "default", "issue-inventory", repositoryMonitorIssueKind, "12"); err != store.ErrNotFound {
		t.Fatalf("PR-shaped issue item error = %v, want ErrNotFound", err)
	}
	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "issue-inventory"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.OpenIssues != 1 {
		t.Fatalf("OpenIssues = %d, want 1", current.Status.OpenIssues)
	}
}

func TestRepositoryMonitorIssueTriageTaskQueuesAndIngestsActionRecord(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/orka-agents/orka/issues/22" {
			t.Fatalf("request path = %q, want targeted issue path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":22,"title":"Needs triage","body":"Classify me","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/orka-agents/orka/issues/22","user":{"login":"alice"},"labels":[{"name":"bug"}]}`))
	}))
	t.Cleanup(server.Close)
	pullRequestsEnabled := false
	monitor, secret := repositoryMonitorInventoryTestObjects("issue-triage")
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Triager = &corev1alpha1.AgentReference{Name: "triager"}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
	processedAt := time.Now()
	command := &store.CommandEvent{ID: "cmd-triage-22", MonitorNamespace: "default", MonitorName: "issue-triage", Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: 22, Intent: "triage", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-triage-22", MonitorNamespace: "default", MonitorName: "issue-triage", Trigger: "github_label_command", TargetKind: repositoryMonitorIssueKind, TargetNumber: 22, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "issue-triage"}}); err != nil {
		t.Fatalf("Reconcile(queue) error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "issue-triage", repositoryMonitorIssueKind, "22")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhaseTriageQueued || item.LastActionTaskName == "" {
		t.Fatalf("item after queue = %#v, want triage queued with task", item)
	}
	result := fmt.Appendf(nil, `{"schemaVersion":"orka.issueTriage.v1","repo":"orka-agents/orka","issueNumber":22,"snapshotDigest":%q,"verdict":"actionable","confidence":"high","category":"bug","priority":"P2","recommendedLane":"research_then_plan","risk":"medium","summary":"Ready to research."}`, item.SnapshotDigest)
	if err := monitorStore.SaveResult(ctx, "default", item.LastActionTaskName, result); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: item.LastActionTaskName}, &task); err != nil {
		t.Fatalf("Get task() error = %v", err)
	}
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	if err := cl.Status().Update(ctx, &task); err != nil {
		t.Fatalf("Status().Update(task) error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "issue-triage"}}); err != nil {
		t.Fatalf("Reconcile(ingest) error = %v", err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", "issue-triage", repositoryMonitorIssueKind, "22")
	if err != nil {
		t.Fatalf("GetMonitorItem(after ingest) error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhaseTriaged || item.LastActionID == "" || item.LastVerdict != "actionable" {
		t.Fatalf("item after ingest = %#v, want triaged actionable", item)
	}
	records, _, err := monitorStore.ListActionRecords(ctx, store.ActionRecordFilter{Namespace: "default", MonitorName: "issue-triage", Kind: repositoryMonitorIssueKind, Number: 22, ActionKind: repositoryMonitorIssueActionTriage, Limit: 10})
	if err != nil {
		t.Fatalf("ListActionRecords() error = %v", err)
	}
	if len(records) != 1 || records[0].Summary != "Ready to research." {
		t.Fatalf("records = %#v, want one triage record", records)
	}
}

//nolint:gocyclo // End-to-end monitor workflow test intentionally exercises the full issue-to-PR path.
func TestRepositoryMonitorIssueImplementToPRFakeGitHubE2E(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	createdPR := false
	createdStatusComment := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/issues/77":
			_, _ = w.Write([]byte(`{"number":77,"title":"Add fake health","body":"Please add /healthz.","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/orka-agents/orka/issues/77","user":{"login":"alice"},"labels":[{"name":"bug"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/pulls":
			_, _ = w.Write([]byte(`[]`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/orka-agents/orka/issues/77/comments":
			createdStatusComment = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode issue status comment body: %v", err)
			}
			if !strings.Contains(fmt.Sprint(body["body"]), "Orka Issue Status") {
				t.Fatalf("status comment body = %#v, want Orka Issue Status", body)
			}
			_, _ = w.Write([]byte(`{"id":7701,"html_url":"https://github.com/orka-agents/orka/issues/77#issuecomment-7701"}`))
		case r.Method == http.MethodPatch && r.URL.Path == "/repos/orka-agents/orka/issues/comments/7701":
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode status comment update body: %v", err)
			}
			if !strings.Contains(fmt.Sprint(body["body"]), "Linked PR: #177") {
				t.Fatalf("updated status comment body = %#v, want linked PR", body)
			}
			_, _ = w.Write([]byte(`{"id":7701,"html_url":"https://github.com/orka-agents/orka/issues/77#issuecomment-7701"}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/orka-agents/orka/pulls":
			createdPR = true
			var body map[string]any
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Fatalf("decode create PR body: %v", err)
			}
			if body["base"] != repositoryMonitorTestDefaultBranch || body["head"] == "" {
				t.Fatalf("create PR body = %#v, want base main and non-empty head", body)
			}
			_, _ = w.Write([]byte(`{"number":177,"html_url":"https://github.com/orka-agents/orka/pull/177"}`))
		default:
			t.Fatalf("unexpected GitHub request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	t.Cleanup(server.Close)

	pullRequestsEnabled := false
	requireApprovedPlan := false
	monitor, secret := repositoryMonitorInventoryTestObjects("issue-impl-e2e")
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	monitor.Spec.IssueWorkflow.Implementation.RequireApprovedPlan = &requireApprovedPlan
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, ArtifactStore: monitorStore, GitHubAPIBaseURL: server.URL}

	processedAt := time.Now()
	command := &store.CommandEvent{ID: "cmd-implement-77", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: 77, Intent: "implement", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-implement-77", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorIssueKind, TargetNumber: 77, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(queue implementation) error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "77")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhaseImplementationQueued || item.LastActionTaskName == "" {
		t.Fatalf("item after implementation queue = %#v, want implementation queued with task", item)
	}
	jobs, _, err := monitorStore.ListImplementationJobs(ctx, store.ImplementationJobFilter{Namespace: "default", MonitorName: monitor.Name, IssueNumber: 77, Limit: 10})
	if err != nil {
		t.Fatalf("ListImplementationJobs() error = %v", err)
	}
	if len(jobs) != 1 || jobs[0].TaskName != item.LastActionTaskName || jobs[0].Phase != repositoryMonitorIssuePhaseImplementationQueued {
		t.Fatalf("implementation jobs = %#v, want queued implementation job", jobs)
	}

	implTaskName := item.LastActionTaskName
	implementationDiff := "diff --git a/internal/health.go b/internal/health.go\n--- a/internal/health.go\n+++ b/internal/health.go\n@@ -1 +1 @@\n-old\n+new\n"
	agentResult, _ := json.Marshal(map[string]any{
		"schemaVersion":  "orka.issueImplementation.v1",
		"repo":           "orka-agents/orka",
		"issueNumber":    77,
		"snapshotDigest": item.SnapshotDigest,
		"status":         "patch_ready",
		"summary":        "Implemented fake health endpoint.",
	})
	implBytes, _ := common.FormatStructuredResult(&common.StructuredResult{
		Summary: string(agentResult),
		BaseSHA: "base77",
		HeadSHA: "base77",
		Diff:    implementationDiff,
		Files:   []string{"internal/health.go"},
	})
	if err := monitorStore.SaveResult(ctx, "default", implTaskName, implBytes); err != nil {
		t.Fatalf("SaveResult(implementation) error = %v", err)
	}
	markRepositoryMonitorTestTaskSucceeded(t, ctx, cl, implTaskName)

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(implementation ingest) error = %v", err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "77")
	if err != nil {
		t.Fatalf("GetMonitorItem(after implementation ingest) error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhaseMutationQueued || item.LastActionTaskName == "" || item.LastActionTaskName == implTaskName {
		t.Fatalf("item after mutation queue = %#v, want mutation queued with new task", item)
	}
	mutationTaskName := item.LastActionTaskName
	var mutationTask corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: mutationTaskName}, &mutationTask); err != nil {
		t.Fatalf("Get mutation task() error = %v", err)
	}
	branch := repositoryMonitorIssueTaskPushBranch(&mutationTask)
	if branch == "" {
		t.Fatalf("mutation task workspace = %#v, want push branch", mutationTask.Spec.AgentRuntime)
	}

	if mutationTask.Spec.Type != corev1alpha1.TaskTypeContainer || mutationTask.Spec.AgentRef != nil || mutationTask.Spec.Image != "" || len(mutationTask.Spec.Command) != 1 || mutationTask.Spec.Command[0] != scheduledRunLabelValue {
		t.Fatalf("mutation task spec = %#v, want deterministic trusted general-worker container", mutationTask.Spec)
	}
	if mutationTask.Spec.Workspace == nil || mutationTask.Spec.AgentRuntime != nil || mutationTask.Spec.PriorTaskRef == nil || mutationTask.Spec.PriorTaskRef.Name != implTaskName {
		t.Fatalf("mutation task workspace/prior task = %#v, want validated implementation replay", mutationTask.Spec)
	}
	if !taskEnvHasValue(mutationTask.Spec.Env, workerenv.PriorTaskDiffSHA256, repositoryMonitorIssuePatchDigest(implementationDiff)) {
		t.Fatalf("mutation task env = %#v, want validated diff digest", mutationTask.Spec.Env)
	}
	mutationBytes, _ := common.FormatStructuredResult(&common.StructuredResult{
		Summary:    "Pushed validated implementation branch.",
		BaseSHA:    "base77",
		HeadSHA:    "head77",
		Diff:       implementationDiff,
		Files:      []string{"internal/health.go"},
		PushBranch: branch,
	})
	if err := monitorStore.SaveResult(ctx, "default", mutationTaskName, mutationBytes); err != nil {
		t.Fatalf("SaveResult(mutation) error = %v", err)
	}
	markRepositoryMonitorTestTaskSucceeded(t, ctx, cl, mutationTaskName)

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(mutation ingest) error = %v", err)
	}
	if !createdPR {
		t.Fatal("fake GitHub create PR endpoint was not called")
	}
	if !createdStatusComment {
		t.Fatal("fake GitHub issue status comment endpoint was not called")
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "77")
	if err != nil {
		t.Fatalf("GetMonitorItem(after PR) error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhasePROpened || item.LinkedPRNumber != 177 || item.StatusCommentID != "7701" {
		t.Fatalf("item after PR = %#v, want pr_opened linked to 177 with status comment", item)
	}
	mutations, _, err := monitorStore.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{Namespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: 77, Limit: 10})
	if err != nil {
		t.Fatalf("ListGitHubMutationRecords() error = %v", err)
	}
	if !repositoryMonitorTestHasMutation(mutations, "push_branch") || !repositoryMonitorTestHasMutation(mutations, "create_pr") {
		t.Fatalf("mutations = %#v, want push_branch and create_pr", mutations)
	}
	jobs, _, err = monitorStore.ListImplementationJobs(ctx, store.ImplementationJobFilter{Namespace: "default", MonitorName: monitor.Name, IssueNumber: 77, Limit: 10})
	if err != nil {
		t.Fatalf("ListImplementationJobs(final) error = %v", err)
	}
	if len(jobs) != 1 || jobs[0].Phase != repositoryMonitorIssuePhasePROpened || jobs[0].PRNumber != 177 || jobs[0].PatchArtifactID == "" {
		t.Fatalf("implementation job final = %#v, want pr_opened with PR and patch artifact", jobs)
	}
	artifacts, err := monitorStore.ListArtifacts(ctx, "default", implTaskName)
	if err != nil {
		t.Fatalf("ListArtifacts() error = %v", err)
	}
	foundPatchSummary := false
	for _, artifact := range artifacts {
		if strings.HasSuffix(artifact.Filename, ".json") {
			foundPatchSummary = true
		}
	}
	if !foundPatchSummary {
		t.Fatalf("artifacts = %#v, want patch summary artifact", artifacts)
	}
}

func TestRepositoryMonitorIssueApprovePlanWithoutCurrentPlanBlocksWorkAction(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/issues/44":
			_, _ = w.Write([]byte(`{"number":44,"title":"Add tray app","body":"Build Windows tray app.","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/orka-agents/orka/issues/44","user":{"login":"alice"},"labels":[]}`))
		default:
			t.Fatalf("unexpected GitHub request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	t.Cleanup(server.Close)

	pullRequestsEnabled := false
	monitor, secret := repositoryMonitorInventoryTestObjects("issue-approve-no-plan")
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, ArtifactStore: monitorStore, GitHubAPIBaseURL: server.URL}

	processedAt := time.Now()
	command := &store.CommandEvent{ID: "cmd-approve-44", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: 44, Intent: "approve_plan", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-approve-44", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorIssueKind, TargetNumber: 44, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(approve without plan) error = %v", err)
	}
	actions, _, err := monitorStore.ListWorkActions(ctx, store.WorkActionFilter{Namespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: 44, DesiredAction: "approve", Limit: 10})
	if err != nil {
		t.Fatalf("ListWorkActions(approve) error = %v", err)
	}
	if len(actions) != 1 || actions[0].Status != repositoryMonitorWorkActionStatusBlocked || actions[0].BlockedReason != "no_current_plan_to_approve" || actions[0].CompletedAt == nil {
		t.Fatalf("approve work actions = %#v, want terminal blocked no_current_plan_to_approve", actions)
	}
}

func TestRepositoryMonitorImplementationBudgetIgnoresFailedActionsButCountsMutations(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	maxActive := int32(1)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "vekil-ui", Namespace: "default"}}
	monitor.Spec.IssueWorkflow.Implementation.MaxActive = &maxActive
	item := &store.MonitorItem{Number: 44, SnapshotDigest: "sha256:test"}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}

	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: "wa-blocked", MonitorNamespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: 44, DesiredAction: "implement", Status: repositoryMonitorWorkActionStatusBlocked, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction(blocked) error = %v", err)
	}
	if err := monitorStore.CreateImplementationJob(ctx, &store.ImplementationJob{ID: "impl-blocked", MonitorNamespace: "default", MonitorName: monitor.Name, IssueNumber: 44, SnapshotDigest: item.SnapshotDigest, Phase: repositoryMonitorIssuePhaseImplementationQueued, WorkActionID: "wa-blocked", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateImplementationJob(blocked) error = %v", err)
	}
	if reason, err := reconciler.issueImplementationBudgetBlockReason(ctx, monitor, item, ""); err != nil || reason != "" {
		t.Fatalf("blocked job reason=%q err=%v, want no active budget block", reason, err)
	}

	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: "wa-succeeded", MonitorNamespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: 45, DesiredAction: "implement", Status: repositoryMonitorWorkActionStatusSucceeded, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction(succeeded) error = %v", err)
	}
	if err := monitorStore.CreateImplementationJob(ctx, &store.ImplementationJob{ID: "impl-mutating", MonitorNamespace: "default", MonitorName: monitor.Name, IssueNumber: 45, SnapshotDigest: "sha256:other", Phase: repositoryMonitorIssuePhaseMutationQueued, WorkActionID: "wa-succeeded", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateImplementationJob(mutating) error = %v", err)
	}
	if reason, err := reconciler.issueImplementationBudgetBlockReason(ctx, monitor, item, ""); err != nil || reason != "implementation_active_budget_exhausted" {
		t.Fatalf("mutating job reason=%q err=%v, want implementation_active_budget_exhausted", reason, err)
	}
	currentTaskName := "current-implementation-task"
	if err := monitorStore.CreateImplementationJob(ctx, &store.ImplementationJob{ID: repositoryMonitorImplementationJobID(currentTaskName), MonitorNamespace: "default", MonitorName: monitor.Name, IssueNumber: item.Number, SnapshotDigest: item.SnapshotDigest, Phase: repositoryMonitorIssuePhaseImplementationQueued, TaskName: currentTaskName, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateImplementationJob(current) error = %v", err)
	}
	if reason, err := reconciler.issueImplementationBudgetBlockReason(ctx, monitor, item, currentTaskName); err != nil || reason != "" {
		t.Fatalf("existing current job reason=%q err=%v, want retry reuse", reason, err)
	}
}

func TestRepositoryMonitorIssueImplementationRecordBindsFinalizedDiffIdentity(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "vekil-ui", Namespace: "default"}}
	item := &store.MonitorItem{Number: 44, SnapshotDigest: "sha256:test"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "implementation-task", Namespace: "default", Annotations: map[string]string{repositoryMonitorIssueAnnotationCommandID: "cmd-impl"}}}
	agentJSON := `{"schemaVersion":"orka.issueImplementation.v1","status":"patch_ready","summary":"Implemented Windows tray support."}`
	raw, err := common.FormatStructuredResult(&common.StructuredResult{
		Summary: agentJSON,
		BaseSHA: "base",
		HeadSHA: "base",
		Diff:    "diff --git a/README.md b/README.md\n--- a/README.md\n+++ b/README.md\n@@ -1 +1 @@\n-old\n+new\n",
		Files:   []string{"README.md"},
	})
	if err != nil {
		t.Fatalf("FormatStructuredResult() error = %v", err)
	}

	record := repositoryMonitorActionRecordFromTask(monitor, item, task, repositoryMonitorIssueActionImplementation, raw)
	if record.Verdict != "patch_ready" {
		t.Fatalf("record verdict=%q, want patch_ready", record.Verdict)
	}
	if record.Summary != "Implemented Windows tray support." {
		t.Fatalf("record summary=%q, want agent summary", record.Summary)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(record.PayloadJSON), &body); err != nil {
		t.Fatalf("PayloadJSON invalid: %v", err)
	}
	if got := numberField(body, "issueNumber"); got != 44 {
		t.Fatalf("issueNumber=%d, want 44", got)
	}
	if got := stringField(body, "snapshotDigest"); got != "sha256:test" {
		t.Fatalf("snapshotDigest=%q, want sha256:test", got)
	}
	if got := stringField(body, "status"); got != "patch_ready" {
		t.Fatalf("status=%q, want patch_ready", got)
	}
	if got := stringField(body, "diff"); got == "" {
		t.Fatal("finalized diff was not preserved")
	}
}

func TestRepositoryMonitorIssueImplementationRecordRejectsMalformedSuccess(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "malformed-impl", Namespace: "default"}}
	item := &store.MonitorItem{Number: 44, SnapshotDigest: "sha256:test"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "implementation-task", Namespace: "default", Annotations: map[string]string{repositoryMonitorIssueAnnotationCommandID: "cmd-impl"}}}
	raw, err := common.FormatStructuredResult(&common.StructuredResult{
		Summary: "I changed the files but did not return the required JSON.",
		Diff:    "diff --git a/README.md b/README.md\n",
		Files:   []string{"README.md"},
	})
	if err != nil {
		t.Fatalf("FormatStructuredResult() error = %v", err)
	}
	record := repositoryMonitorActionRecordFromTask(monitor, item, task, repositoryMonitorIssueActionImplementation, raw)
	if record.Verdict != repositoryMonitorReviewVerdictFailed {
		t.Fatalf("record verdict=%q, want failed for missing status", record.Verdict)
	}
}

func TestRepositoryMonitorIssueImplementationRecordPreservesBlockedFinalizedStatus(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "blocked-impl", Namespace: "default"}}
	item := &store.MonitorItem{Number: 44, SnapshotDigest: "sha256:test"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "implementation-task", Namespace: "default", Annotations: map[string]string{repositoryMonitorIssueAnnotationCommandID: "cmd-impl"}}}
	raw, err := common.FormatStructuredResult(&common.StructuredResult{Summary: `{"schemaVersion":"orka.issueImplementation.v1","status":"blocked","summary":"Needs decomposition."}`})
	if err != nil {
		t.Fatalf("FormatStructuredResult() error = %v", err)
	}
	record := repositoryMonitorActionRecordFromTask(monitor, item, task, repositoryMonitorIssueActionImplementation, raw)
	if record.Verdict != repositoryMonitorIssuePhaseBlocked || record.Summary != "Needs decomposition." {
		t.Fatalf("record = %#v, want blocked model status and summary", record)
	}
}

func TestRepositoryMonitorIssueActionRecordExtractsFencedJSON(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "vekil-ui", Namespace: "default"}}
	item := &store.MonitorItem{Number: 44, SnapshotDigest: "sha256:test"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "plan-task", Namespace: "default", Annotations: map[string]string{repositoryMonitorIssueAnnotationCommandID: "cmd-plan"}}}
	raw := []byte("Based on my analysis:\n\n```json\n{\n  \"schemaVersion\": \"orka.issuePlan.v1\",\n  \"repo\": \"sozercan/vekil\",\n  \"issueNumber\": 44,\n  \"snapshotDigest\": \"sha256:test\",\n  \"status\": \"ready\",\n  \"summary\": \"Plan ready\",\n  \"acceptanceCriteria\": [],\n  \"steps\": [],\n  \"validationCommands\": [],\n  \"allowedFiles\": [],\n  \"risk\": \"medium\",\n  \"categories\": [],\n  \"requiresHumanApproval\": true\n}\n```\n")

	record := repositoryMonitorActionRecordFromTask(monitor, item, task, repositoryMonitorIssueActionPlan, raw)
	if record.Verdict != "ready" || record.Summary != "Plan ready" {
		t.Fatalf("record verdict=%q summary=%q, want ready Plan ready", record.Verdict, record.Summary)
	}
	if !strings.HasPrefix(strings.TrimSpace(record.PayloadJSON), "{") || strings.Contains(record.PayloadJSON, "```") {
		t.Fatalf("PayloadJSON = %q, want extracted raw JSON object", record.PayloadJSON)
	}
	var body map[string]any
	if err := json.Unmarshal([]byte(record.PayloadJSON), &body); err != nil {
		t.Fatalf("PayloadJSON is not valid JSON: %v", err)
	}
}

func TestRepositoryMonitorIssueImplementContinuesAfterAutoApprovedPlan(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/issues/44":
			_, _ = w.Write([]byte(`{"number":44,"title":"Add tray app","body":"Build Windows tray app.","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/orka-agents/orka/issues/44","user":{"login":"alice"},"labels":[]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/orka-agents/orka/issues/44/comments":
			_, _ = w.Write([]byte(`{"id":4401,"html_url":"https://github.com/orka-agents/orka/issues/44#issuecomment-4401"}`))
		default:
			t.Fatalf("unexpected GitHub request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	t.Cleanup(server.Close)

	pullRequestsEnabled := false
	requireApprovedPlan := true
	monitor, secret := repositoryMonitorInventoryTestObjects("issue-implement-plan-handoff")
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Planner = &corev1alpha1.AgentReference{Name: "planner"}
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	monitor.Spec.IssueWorkflow.Implementation.RequireApprovedPlan = &requireApprovedPlan
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, ArtifactStore: monitorStore, GitHubAPIBaseURL: server.URL}

	processedAt := time.Now()
	command := &store.CommandEvent{ID: "cmd-implement-44", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: 44, Intent: "implement", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: store.RepositoryMonitorWorkActionID(command.ID, "implement"), MonitorNamespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: 44, DesiredAction: "implement", Status: repositoryMonitorWorkActionStatusQueued, CreatedAt: processedAt}); err != nil {
		t.Fatalf("CreateWorkAction(implement) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-implement-44", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorIssueKind, TargetNumber: 44, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(queue prerequisite plan) error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "44")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhasePlanQueued || item.LastActionKind != repositoryMonitorIssueActionPlan || item.LastActionTaskName == "" {
		t.Fatalf("item after implement handoff = %#v, want plan queued", item)
	}
	planTaskName := item.LastActionTaskName
	implementAction, err := monitorStore.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, "implement"))
	if err != nil {
		t.Fatalf("GetWorkAction(implement) error = %v", err)
	}
	if implementAction.Status != repositoryMonitorWorkActionStatusRunning || implementAction.Phase != repositoryMonitorIssuePhasePlanQueued {
		t.Fatalf("implement action = %#v, want running prerequisite plan", implementAction)
	}

	planResult := fmt.Sprintf(`{"schemaVersion":"orka.issuePlan.v1","repo":"orka-agents/orka","issueNumber":44,"snapshotDigest":%q,"status":"ready","summary":"Small plan","acceptanceCriteria":[],"steps":["edit"],"validationCommands":[],"allowedFiles":["README.md"],"risk":"low","categories":[],"requiresHumanApproval":false}`, item.SnapshotDigest)
	if err := monitorStore.SaveResult(ctx, "default", planTaskName, []byte(planResult)); err != nil {
		t.Fatalf("SaveResult(plan) error = %v", err)
	}
	markRepositoryMonitorTestTaskSucceeded(t, ctx, cl, planTaskName)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(continue implementation) error = %v", err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "44")
	if err != nil {
		t.Fatalf("GetMonitorItem(after plan) error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhaseImplementationQueued || item.LastActionKind != repositoryMonitorIssueActionImplementation || item.LastActionTaskName == "" || item.LastActionTaskName == planTaskName {
		t.Fatalf("item after auto-approved plan = %#v, want implementation queued", item)
	}
	implementAction, err = monitorStore.GetWorkAction(ctx, "default", store.RepositoryMonitorWorkActionID(command.ID, "implement"))
	if err != nil {
		t.Fatalf("GetWorkAction(implement after plan) error = %v", err)
	}
	if implementAction.Status != repositoryMonitorWorkActionStatusRunning || implementAction.TaskName != item.LastActionTaskName {
		t.Fatalf("implement action after plan = %#v, want running implementation", implementAction)
	}
}

func markRepositoryMonitorTestTaskSucceeded(t *testing.T, ctx context.Context, cl crclient.Client, name string) {
	t.Helper()
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: name}, &task); err != nil {
		t.Fatalf("Get task %q: %v", name, err)
	}
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	if err := cl.Status().Update(ctx, &task); err != nil {
		t.Fatalf("Status().Update(%q) error = %v", name, err)
	}
}

func repositoryMonitorTestHasMutation(records []store.GitHubMutationRecord, operation string) bool {
	for _, record := range records {
		if record.Operation == operation && record.Status == "succeeded" {
			return true
		}
	}
	return false
}

func TestRepositoryMonitorPullRequestFixCommandQueuesRepairTask(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := newRepositoryMonitorSinglePullRequestServerWithBody(t, 31, `{"number":31,"title":"Fix me","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base31","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature-fix","sha":"head31","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}`)
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("pr-fix")
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	monitor.Spec.Repair.Enabled = true
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
	processedAt := time.Now()
	command := &store.CommandEvent{ID: "cmd-fix-31", MonitorNamespace: "default", MonitorName: "pr-fix", Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 31, Intent: "fix", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-fix-31", MonitorNamespace: "default", MonitorName: "pr-fix", Trigger: "github_label_command", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 31, TargetSHA: "head31", CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pr-fix"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "pr-fix", repositoryMonitorPullRequestKind, "31")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.RepairState != repositoryMonitorRepairPhaseQueued {
		t.Fatalf("item = %#v, want queued repair", item)
	}
	jobs, _, err := monitorStore.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: "default", MonitorName: "pr-fix", PRNumber: 31, Limit: 10})
	if err != nil {
		t.Fatalf("ListRepairJobs() error = %v", err)
	}
	if len(jobs) != 1 || jobs[0].TaskName == "" {
		t.Fatalf("jobs = %#v, want one repair job with task", jobs)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: jobs[0].TaskName}, &task); err != nil {
		t.Fatalf("Get repair task() error = %v", err)
	}
	if task.Spec.AgentRef == nil || task.Spec.AgentRef.Name != "repairer" || task.Spec.AgentRuntime == nil || task.Spec.AgentRuntime.Workspace == nil || task.Spec.AgentRuntime.Workspace.PushBranch != "feature-fix" {
		t.Fatalf("repair task spec = %#v, want repairer push to feature branch", task.Spec)
	}
}

func TestRepositoryMonitorRepairTaskCreationRetryRestoresQueuedJob(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	monitor, _ := repositoryMonitorInventoryTestObjects("repair-create-retry")
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	failTaskCreate := true
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(monitor).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c crclient.WithWatch, obj crclient.Object, opts ...crclient.CreateOption) error {
				if _, ok := obj.(*corev1alpha1.Task); ok && failTaskCreate {
					failTaskCreate = false
					return errors.New("transient task create failure")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}
	run := &store.MonitorRun{ID: "run-repair-retry"}
	command := &store.CommandEvent{ID: "cmd-repair-retry", Intent: "fix", Source: "api"}
	pr := repositoryMonitorPullRequest{Number: 31, HeadSHA: "head31", BaseSHA: "base31", HeadBranch: "feature-fix", BaseBranch: "main"}
	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "31", Number: 31, HeadSHA: pr.HeadSHA}

	if created, err := reconciler.createRepositoryMonitorRepairTask(ctx, monitor, run, command, "orka-agents", "orka", pr, item, 1, 1); err == nil || created != 0 {
		t.Fatalf("first create result = (%d, %v), want transient failure", created, err)
	}
	jobID := "repair-" + repositoryMonitorShortHash(command.ID)
	job, err := monitorStore.GetRepairJob(ctx, monitor.Namespace, jobID)
	if err != nil {
		t.Fatalf("GetRepairJob(after failure) error = %v", err)
	}
	if job.Phase != repositoryMonitorRepairPhaseQueued || job.LastError != repositoryMonitorRepairTaskCreateError || job.CompletedAt != nil {
		t.Fatalf("job after transient failure = %#v, want retryable queued state", job)
	}

	if created, err := reconciler.createRepositoryMonitorRepairTask(ctx, monitor, run, command, "orka-agents", "orka", pr, item, 1, 1); err != nil || created != 1 {
		t.Fatalf("retry create result = (%d, %v), want created", created, err)
	}
	job, err = monitorStore.GetRepairJob(ctx, monitor.Namespace, jobID)
	if err != nil {
		t.Fatalf("GetRepairJob(after retry) error = %v", err)
	}
	if job.Phase != repositoryMonitorRepairPhaseQueued || job.LastError != "" || job.CompletedAt != nil {
		t.Fatalf("job after successful retry = %#v, want clean queued state", job)
	}
	var task corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: monitor.Namespace, Name: job.TaskName}, &task); err != nil {
		t.Fatalf("Get repair task after retry error = %v", err)
	}
}

//nolint:gocyclo // End-to-end monitor workflow test intentionally exercises stop/resume and late task safety.
func TestRepositoryMonitorIssueStopPreventsLateImplementationMutation(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/orka-agents/orka/issues/79" {
			t.Fatalf("request path = %q, want targeted issue path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":79,"title":"Stop me","body":"Do not continue after stop.","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/orka-agents/orka/issues/79","user":{"login":"alice"},"labels":[{"name":"bug"}]}`))
	}))
	t.Cleanup(server.Close)

	pullRequestsEnabled := false
	requireApprovedPlan := false
	monitor, secret := repositoryMonitorInventoryTestObjects("issue-stop-e2e")
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	monitor.Spec.IssueWorkflow.Implementation.RequireApprovedPlan = &requireApprovedPlan
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, ArtifactStore: monitorStore, GitHubAPIBaseURL: server.URL}

	processedAt := time.Now()
	implementCommand := &store.CommandEvent{ID: "cmd-implement-79", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: 79, Intent: "implement", Command: "implement", CommentID: "implement-79", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, implementCommand); err != nil {
		t.Fatalf("CreateCommandEvent(implement) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-implement-79", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorIssueKind, TargetNumber: 79, CommandEventID: implementCommand.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun(implement) error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(queue implementation) error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "79")
	if err != nil {
		t.Fatalf("GetMonitorItem(queued) error = %v", err)
	}
	implTaskName := item.LastActionTaskName
	if item.WorkflowPhase != repositoryMonitorIssuePhaseImplementationQueued || implTaskName == "" {
		t.Fatalf("item after implementation queue = %#v, want queued implementation", item)
	}

	stopCommand := &store.CommandEvent{ID: "cmd-stop-79", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: 79, Intent: repositoryMonitorCommandIntentStop, Command: repositoryMonitorCommandIntentStop, CommentID: "stop-79", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, stopCommand); err != nil {
		t.Fatalf("CreateCommandEvent(stop) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-stop-79", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorIssueKind, TargetNumber: 79, CommandEventID: stopCommand.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun(stop) error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(stop) error = %v", err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "79")
	if err != nil {
		t.Fatalf("GetMonitorItem(stopped) error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhaseBlocked || item.SkipReason != repositoryMonitorIssueSkipStoppedByCommand {
		t.Fatalf("item after stop = %#v, want blocked stopped_by_command", item)
	}
	workActions, _, err := monitorStore.ListWorkActions(ctx, store.WorkActionFilter{Namespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: 79, DesiredAction: "implement", Limit: 10})
	if err != nil {
		t.Fatalf("ListWorkActions() error = %v", err)
	}
	if len(workActions) != 1 || workActions[0].Status != repositoryMonitorWorkActionStatusCancelled {
		t.Fatalf("work actions = %#v, want cancelled implementation action", workActions)
	}
	implementationJobs, _, err := monitorStore.ListImplementationJobs(ctx, store.ImplementationJobFilter{Namespace: "default", MonitorName: monitor.Name, IssueNumber: 79, Limit: 10})
	if err != nil {
		t.Fatalf("ListImplementationJobs() error = %v", err)
	}
	if len(implementationJobs) != 1 || implementationJobs[0].Phase != repositoryMonitorWorkActionStatusCancelled || implementationJobs[0].CompletedAt == nil {
		t.Fatalf("implementation jobs = %#v, want cancelled terminal job", implementationJobs)
	}
	var stoppedTask corev1alpha1.Task
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: implTaskName}, &stoppedTask); err != nil {
		t.Fatalf("Get stopped implementation task error = %v", err)
	}
	if stoppedTask.Status.Phase != corev1alpha1.TaskPhaseCancelled {
		t.Fatalf("implementation task phase = %s, want Cancelled", stoppedTask.Status.Phase)
	}

	lateResult := map[string]any{
		"version":        1,
		"schemaVersion":  "orka.issueImplementation.v1",
		"repo":           "orka-agents/orka",
		"issueNumber":    79,
		"snapshotDigest": item.SnapshotDigest,
		"status":         "patch_ready",
		"summary":        "Late result should not mutate.",
		"baseSHA":        "base79",
		"diff":           "diff --git a/internal/late.go b/internal/late.go\n--- a/internal/late.go\n+++ b/internal/late.go\n@@ -1 +1 @@\n-old\n+new\n",
		"files":          []string{"internal/late.go"},
	}
	lateBytes, _ := json.Marshal(lateResult)
	if err := monitorStore.SaveResult(ctx, "default", implTaskName, lateBytes); err != nil {
		t.Fatalf("SaveResult(late implementation) error = %v", err)
	}
	markRepositoryMonitorTestTaskSucceeded(t, ctx, cl, implTaskName)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(late implementation) error = %v", err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "79")
	if err != nil {
		t.Fatalf("GetMonitorItem(after late result) error = %v", err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhaseBlocked || item.SkipReason != repositoryMonitorIssueSkipStoppedByCommand {
		t.Fatalf("item after late result = %#v, want still stopped", item)
	}
	var tasks corev1alpha1.TaskList
	if err := cl.List(ctx, &tasks, crclient.InNamespace("default")); err != nil {
		t.Fatalf("List tasks error = %v", err)
	}
	for _, task := range tasks.Items {
		if task.Name != implTaskName && strings.HasPrefix(task.Name, "monmutate-") {
			t.Fatalf("unexpected mutation task after stopped implementation: %s", task.Name)
		}
	}
	mutations, _, err := monitorStore.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{Namespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorIssueKind, TargetNumber: 79, Limit: 10})
	if err != nil {
		t.Fatalf("ListGitHubMutationRecords() error = %v", err)
	}
	if len(mutations) != 0 {
		t.Fatalf("mutations = %#v, want no GitHub mutation after stop", mutations)
	}

	resumeCommand := &store.CommandEvent{ID: "cmd-resume-79", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorIssueKind, Number: 79, Intent: repositoryMonitorCommandIntentResume, Command: repositoryMonitorCommandIntentResume, CommentID: "resume-79", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, resumeCommand); err != nil {
		t.Fatalf("CreateCommandEvent(resume) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-resume-79", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorIssueKind, TargetNumber: 79, CommandEventID: resumeCommand.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun(resume) error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(resume) error = %v", err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "79")
	if err != nil {
		t.Fatalf("GetMonitorItem(resumed) error = %v", err)
	}
	if item.WorkflowPhase == repositoryMonitorIssuePhaseBlocked || item.SkipReason != "" {
		t.Fatalf("item after resume = %#v, want unblocked", item)
	}
}

//nolint:gocyclo // End-to-end monitor workflow test intentionally exercises review, repair, readiness, and automerge.
func TestRepositoryMonitorPRReviewRepairReadinessAutomergeFakeGitHubE2E(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}

	merged := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/pulls/88":
			_, _ = w.Write([]byte(`{"number":88,"title":"Repair me","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base88","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature-repair","sha":"head88-fixed","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/pulls":
			_, _ = w.Write([]byte(`[{"number":88,"title":"Repair me","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base88","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature-repair","sha":"head88-fixed","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}]`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/commits/head88-fixed/check-runs":
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/commits/head88-fixed/status":
			_, _ = w.Write([]byte(`{"state":"success","statuses":[{"context":"legacy","state":"success"}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/orka-agents/orka/pulls/88/merge":
			merged = true
			_, _ = w.Write([]byte(`{"sha":"merged88"}`))
		default:
			t.Fatalf("unexpected GitHub request %s %s?%s", r.Method, r.URL.Path, r.URL.RawQuery)
		}
	}))
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("pr-review-repair-e2e")
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	monitor.Spec.Repair.Enabled = true
	monitor.Spec.Automerge.Enabled = true
	globalGate := false
	monitor.Spec.Automerge.RequireGlobalMergeGate = &globalGate
	monitor.Spec.Automerge.AllowedMergeMethods = []string{"squash"}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}, &corev1alpha1.Task{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}

	processedAt := time.Now()
	reviewCommand := &store.CommandEvent{ID: "cmd-review-88", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 88, Intent: "review", Command: "review", CommentID: "review-88", HeadSHA: "head88-fixed", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, reviewCommand); err != nil {
		t.Fatalf("CreateCommandEvent(review) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-review-88", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 88, TargetSHA: "head88-fixed", CommandEventID: reviewCommand.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun(review) error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(queue review) error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorPullRequestKind, "88")
	if err != nil {
		t.Fatalf("GetMonitorItem(review queued) error = %v", err)
	}
	if item.LastVerdict != repositoryMonitorRunPhaseQueued || item.LastReviewID == "" {
		t.Fatalf("item after review queue = %#v, want queued review", item)
	}
	reviewTaskName := item.LastReviewID
	if err := monitorStore.SaveResult(ctx, "default", reviewTaskName, repositoryMonitorReviewResultEnvelope(t, 88, "head88-fixed", repositoryMonitorReviewVerdictNeedsChanges)); err != nil {
		t.Fatalf("SaveResult(review) error = %v", err)
	}
	markRepositoryMonitorTestTaskSucceeded(t, ctx, cl, reviewTaskName)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(ingest review) error = %v", err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorPullRequestKind, "88")
	if err != nil {
		t.Fatalf("GetMonitorItem(review ingested) error = %v", err)
	}
	if item.LastVerdict != repositoryMonitorReviewVerdictNeedsChanges || item.LastReviewedHeadSHA != "head88-fixed" {
		t.Fatalf("item after review ingest = %#v, want needs_changes on head88-fixed", item)
	}

	fixCommand := &store.CommandEvent{ID: "cmd-fix-88", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 88, Intent: "fix", Command: "fix", CommentID: "fix-88", HeadSHA: "head88-fixed", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, fixCommand); err != nil {
		t.Fatalf("CreateCommandEvent(fix) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-fix-88", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 88, TargetSHA: "head88-fixed", CommandEventID: fixCommand.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun(fix) error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(queue repair) error = %v", err)
	}
	repairs, _, err := monitorStore.ListRepairJobs(ctx, store.RepairJobFilter{Namespace: "default", MonitorName: monitor.Name, PRNumber: 88, Limit: 10})
	if err != nil {
		t.Fatalf("ListRepairJobs() error = %v", err)
	}
	if len(repairs) != 1 || repairs[0].TaskName == "" || repairs[0].Phase != repositoryMonitorRepairPhaseQueued {
		t.Fatalf("repairs = %#v, want queued repair", repairs)
	}
	repairTaskName := repairs[0].TaskName
	repairResult, _ := common.FormatStructuredResult(&common.StructuredResult{Summary: "fixed review finding", HeadSHA: "head88-fixed", PushBranch: "feature-repair", Files: []string{"internal/example.go"}})
	if err := monitorStore.SaveResult(ctx, "default", repairTaskName, repairResult); err != nil {
		t.Fatalf("SaveResult(repair) error = %v", err)
	}
	markRepositoryMonitorTestTaskSucceeded(t, ctx, cl, repairTaskName)
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(ingest repair) error = %v", err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorPullRequestKind, "88")
	if err != nil {
		t.Fatalf("GetMonitorItem(repair ingested) error = %v", err)
	}
	if item.RepairState != repositoryMonitorRepairPhaseSucceeded || item.LastVerdict != "" || item.LastReviewedHeadSHA != "" {
		t.Fatalf("item after repair = %#v, want repair succeeded and review stale", item)
	}
	mutations, _, err := monitorStore.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{Namespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 88, Operation: "push_branch", Limit: 10})
	if err != nil {
		t.Fatalf("ListGitHubMutationRecords(push) error = %v", err)
	}
	if !repositoryMonitorTestHasMutation(mutations, "push_branch") {
		t.Fatalf("mutations = %#v, want repair push mutation", mutations)
	}

	passedReviewTask := repositoryMonitorReviewIngestTestTask("review-task-88-fixed", monitor.Name, 88, "head88-fixed")
	if err := cl.Create(ctx, passedReviewTask); err != nil {
		t.Fatalf("Create passed review task error = %v", err)
	}
	if err := monitorStore.SaveResult(ctx, "default", passedReviewTask.Name, repositoryMonitorReviewResultEnvelope(t, 88, "head88-fixed", repositoryMonitorReviewVerdictPassed)); err != nil {
		t.Fatalf("SaveResult(passed review) error = %v", err)
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "88", Number: 88, State: repositoryMonitorItemStateOpen, HeadSHA: "head88-fixed", BaseBranch: "main", LastVerdict: repositoryMonitorRunPhaseQueued, LastReviewID: passedReviewTask.Name, RepairState: repositoryMonitorRepairPhaseSucceeded}); err != nil {
		t.Fatalf("UpsertMonitorItem(passed review pending) error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(ingest passed review) error = %v", err)
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorPullRequestKind, "88")
	if err != nil {
		t.Fatalf("GetMonitorItem(passed review) error = %v", err)
	}
	if item.LastVerdict != repositoryMonitorReviewVerdictPassed || item.LastReviewedHeadSHA != "head88-fixed" || item.AutomergeState != repositoryMonitorAutomergeStateMergeReady {
		t.Fatalf("item after passed review = %#v, want merge_ready", item)
	}

	automergeCommand := &store.CommandEvent{ID: "cmd-automerge-88", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 88, Intent: repositoryMonitorCommandIntentAutomerge, Command: repositoryMonitorCommandIntentAutomerge, CommentID: "automerge-88", Permission: "maintain", HeadSHA: "head88-fixed", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, automergeCommand); err != nil {
		t.Fatalf("CreateCommandEvent(automerge) error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-automerge-88", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 88, TargetSHA: "head88-fixed", CommandEventID: automergeCommand.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun(automerge) error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile(automerge) error = %v", err)
	}
	if !merged {
		t.Fatal("fake GitHub merge endpoint was not called")
	}
	item, err = monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorPullRequestKind, "88")
	if err != nil {
		t.Fatalf("GetMonitorItem(merged) error = %v", err)
	}
	if item.AutomergeState != repositoryMonitorAutomergeStateMerged || item.State != "merged" {
		t.Fatalf("item after automerge = %#v, want merged", item)
	}
	mergeMutations, _, err := monitorStore.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{Namespace: "default", MonitorName: monitor.Name, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 88, Operation: "merge_pr", Limit: 10})
	if err != nil {
		t.Fatalf("ListGitHubMutationRecords(merge) error = %v", err)
	}
	if !repositoryMonitorTestHasMutation(mergeMutations, "merge_pr") {
		t.Fatalf("merge mutations = %#v, want merge_pr", mergeMutations)
	}
}

func TestRepositoryMonitorIssueWorkflowPolicyHelpers(t *testing.T) {
	triageEnabled := false
	implEnabled := false
	monitor := &corev1alpha1.RepositoryMonitor{Spec: corev1alpha1.RepositoryMonitorSpec{IssueWorkflow: corev1alpha1.RepositoryMonitorIssueWorkflowSpec{
		Triage:         corev1alpha1.RepositoryMonitorIssueWorkflowPhaseSpec{Enabled: &triageEnabled},
		Implementation: corev1alpha1.RepositoryMonitorIssueImplementationSpec{Enabled: &implEnabled},
		Planning:       corev1alpha1.RepositoryMonitorIssuePlanningSpec{RequireHumanApprovalFor: []string{"high", "database-migration"}},
	}}}
	if repositoryMonitorIssuePhaseEnabled(monitor, repositoryMonitorIssueActionTriage) {
		t.Fatal("triage phase enabled despite explicit false")
	}
	if repositoryMonitorIssuePhaseEnabled(monitor, repositoryMonitorIssueActionImplementation) {
		t.Fatal("implementation phase enabled despite explicit false")
	}
	if !repositoryMonitorPlanRiskRequiresApproval(monitor, `{"risk":"high","requiresHumanApproval":false}`) {
		t.Fatal("high risk plan did not require approval")
	}
	if !repositoryMonitorPlanRiskRequiresApproval(monitor, `{"risk":"medium","categories":["database-migration"],"requiresHumanApproval":false}`) {
		t.Fatal("configured database-migration category did not require approval")
	}
	if !repositoryMonitorPlanRiskRequiresApproval(monitor, `{"risk":"low","requiresHumanApproval":false}`) {
		t.Fatal("legacy plan without categories did not fail closed for configured category policy")
	}
	if repositoryMonitorPlanReadyVerdict("") || repositoryMonitorPlanReadyVerdict(repositoryMonitorReviewVerdictStale) {
		t.Fatal("empty or stale plan verdict considered ready")
	}
	if !repositoryMonitorPlanReadyVerdict("ready") {
		t.Fatal("ready plan verdict not considered ready")
	}
	if repositoryMonitorIssueInventoryBlockCanClear("stopped_by_command") {
		t.Fatal("stopped issue block can be cleared by inventory")
	}
	if !repositoryMonitorIssueInventoryBlockCanClear(repositoryMonitorSkipReasonOverLimit) {
		t.Fatal("transient over-limit block cannot be cleared by inventory")
	}
	if repositoryMonitorImplementationReadyVerdict("blocked") || repositoryMonitorImplementationReadyVerdict("needs_human") {
		t.Fatal("blocked implementation verdict considered ready")
	}
	if !repositoryMonitorImplementationReadyVerdict("patch_ready") {
		t.Fatal("patch_ready implementation verdict not considered ready")
	}
	maxFiles := int32(3)
	monitor.Spec.IssueWorkflow.Implementation.MaxChangedFiles = &maxFiles
	monitor.Spec.IssueWorkflow.Implementation.AllowedPaths = []string{"internal/**", "docs/*.md"}
	if got := repositoryMonitorImplementationMaxChangedFiles(monitor); got != 3 {
		t.Fatalf("max changed files = %d, want 3", got)
	}
	if !repositoryMonitorImplementationPathAllowed(monitor, "internal/controller/x.go") {
		t.Fatal("internal path should be allowed")
	}
	if !repositoryMonitorImplementationPathAllowed(monitor, "docs/guide.md") {
		t.Fatal("docs markdown path should be allowed")
	}
	if repositoryMonitorImplementationPathAllowed(monitor, "website/docs/guide.md") {
		t.Fatal("website docs path should not be allowed by docs/*.md")
	}
}

func TestRepositoryMonitorPullRequestAutomergeCommandMergesWhenGatesPass(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	merged := false
	sawStartedAudit := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != repositoryMonitorTestBearerHeader() {
			t.Fatalf("Authorization header = %q, want %q", got, repositoryMonitorTestBearerHeader())
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/pulls/41":
			_, _ = w.Write([]byte(`{"number":41,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base41","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"ready","sha":"head41","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/commits/head41/check-runs":
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/commits/head41/status":
			_, _ = w.Write([]byte(`{"state":"success","statuses":[{"context":"legacy","state":"success"}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/orka-agents/orka/pulls/41/merge":
			mutations, _, err := monitorStore.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{Namespace: "default", MonitorName: "pr-automerge", Operation: "merge_pr", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 41, Limit: 10})
			if err != nil || len(mutations) != 1 || mutations[0].Status != "started" {
				t.Fatalf("pre-merge mutation audit = %#v err=%v, want one started record", mutations, err)
			}
			sawStartedAudit = true
			merged = true
			_, _ = w.Write([]byte(`{"sha":"merged-sha"}`))
		default:
			t.Fatalf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("pr-automerge")
	globalGate := false
	monitor.Spec.Automerge.Enabled = true
	monitor.Spec.Automerge.RequireGlobalMergeGate = &globalGate
	monitor.Spec.Automerge.AllowedMergeMethods = []string{"squash"}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: "default", MonitorName: "pr-automerge", Kind: repositoryMonitorPullRequestKind, ItemKey: "41", Number: 41, State: repositoryMonitorItemStateOpen, HeadSHA: "head41", LastVerdict: repositoryMonitorReviewVerdictPassed, LastReviewedHeadSHA: "head41"}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	processedAt := time.Now()
	command := &store.CommandEvent{ID: "cmd-automerge-41", MonitorNamespace: "default", MonitorName: "pr-automerge", Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 41, Intent: "automerge", Permission: "maintain", HeadSHA: "head41", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-automerge-41", MonitorNamespace: "default", MonitorName: "pr-automerge", Trigger: "github_label_command", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 41, TargetSHA: "head41", CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "pr-automerge"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !merged {
		t.Fatal("merge endpoint was not called")
	}
	if !sawStartedAudit {
		t.Fatal("merge endpoint was called before the started mutation audit was durable")
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", "pr-automerge", repositoryMonitorPullRequestKind, "41")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.AutomergeState != repositoryMonitorAutomergeStateMerged {
		t.Fatalf("item = %#v, want automerge merged", item)
	}
	mutations, _, err := monitorStore.ListGitHubMutationRecords(ctx, store.GitHubMutationRecordFilter{Namespace: "default", MonitorName: monitor.Name, Operation: "merge_pr", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 41, Limit: 10})
	if err != nil {
		t.Fatalf("ListGitHubMutationRecords() error = %v", err)
	}
	if len(mutations) != 1 || mutations[0].Status != "succeeded" || mutations[0].ExternalID != "merged-sha" {
		t.Fatalf("merge mutation outcome = %#v, want one succeeded record", mutations)
	}
}

func TestRepositoryMonitorAutomergeRecoversMergedStartedAttempt(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := newRepositoryMonitorSinglePullRequestServerWithBody(t, 41, `{"number":41,"title":"Merged","state":"closed","merged":true,"merge_commit_sha":"merged-sha","draft":false,"mergeable_state":"unknown","user":{"login":"alice"},"base":{"ref":"main","sha":"base41","repo":{"full_name":"orka-agents/orka"}},"head":{"ref":"ready","sha":"head41","repo":{"full_name":"orka-agents/orka"}},"labels":[]}`)
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("automerge-recovery")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	command := &store.CommandEvent{ID: "cmd-automerge-recovery", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 41, Intent: repositoryMonitorCommandIntentAutomerge, HeadSHA: "head41", Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	run := &store.MonitorRun{ID: "run-automerge-recovery", MonitorNamespace: defaultNS, MonitorName: monitor.Name, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 41, TargetSHA: "head41", CommandEventID: command.ID}
	mutationID := "ghmut-" + repositoryMonitorShortHash(command.ID+"-merge")
	if err := monitorStore.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: mutationID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, RunID: run.ID, CommandEventID: command.ID, Operation: "merge_pr", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 41, TargetSHA: "head41", Status: repositoryMonitorAutomergeStateStarted, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateGitHubMutationRecord() error = %v", err)
	}
	selected, created, skipped, err := reconciler.processPullRequestInventoryRun(ctx, monitor, run, "orka-agents", "orka")
	if err != nil || selected != 1 || created != 0 || skipped != 0 {
		t.Fatalf("processPullRequestInventoryRun() = selected=%d created=%d skipped=%d err=%v", selected, created, skipped, err)
	}
	mutation, err := monitorStore.GetGitHubMutationRecord(ctx, defaultNS, mutationID)
	if err != nil {
		t.Fatalf("GetGitHubMutationRecord() error = %v", err)
	}
	if mutation.Status != repositoryMonitorRunPhaseSucceeded || mutation.ExternalID != "merged-sha" {
		t.Fatalf("recovered mutation = %#v, want succeeded merged-sha", mutation)
	}
	item, err := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorPullRequestKind, "41")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.State != "merged" || item.AutomergeState != repositoryMonitorAutomergeStateMerged {
		t.Fatalf("recovered item = %#v, want merged", item)
	}
}

func TestRepositoryMonitorAutomergeRecoveryRejectsDifferentHead(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "automerge-stale-recovery", Namespace: defaultNS}}
	command := &store.CommandEvent{ID: "cmd-automerge-stale", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 41, Intent: repositoryMonitorCommandIntentAutomerge, HeadSHA: "head-a", Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	mutationID := "ghmut-" + repositoryMonitorShortHash(command.ID+"-merge")
	if err := monitorStore.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: mutationID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, Operation: "merge_pr", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 41, TargetSHA: "head-a", Status: repositoryMonitorAutomergeStateStarted, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateGitHubMutationRecord() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	handled, err := reconciler.reconcileRepositoryMonitorCompletedAutomerge(ctx, monitor, &store.MonitorRun{ID: "run-stale", CommandEventID: command.ID, TargetNumber: 41, TargetSHA: "head-a"}, []repositoryMonitorPullRequest{{Number: 41, State: "closed", HeadSHA: "head-b", Merged: true, MergeCommitSHA: "merge-b"}})
	if err != nil || handled {
		t.Fatalf("reconcileRepositoryMonitorCompletedAutomerge() handled=%v err=%v, want stale-head rejection", handled, err)
	}
	mutation, err := monitorStore.GetGitHubMutationRecord(ctx, defaultNS, mutationID)
	if err != nil {
		t.Fatalf("GetGitHubMutationRecord() error = %v", err)
	}
	if mutation.Status != repositoryMonitorAutomergeStateStarted || mutation.ExternalID != "" {
		t.Fatalf("stale-head mutation was changed: %#v", mutation)
	}
}

func TestRepositoryMonitorAutomergeTransientMergeErrorPropagates(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/commits/head41/check-runs":
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/commits/head41/status":
			_, _ = w.Write([]byte(`{"state":"success","statuses":[]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/orka-agents/orka/pulls/41/merge":
			w.WriteHeader(http.StatusBadGateway)
			_, _ = w.Write([]byte(`{"message":"upstream unavailable"}`))
		default:
			t.Fatalf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("automerge-transient")
	globalGate := false
	monitor.Spec.Automerge.Enabled = true
	monitor.Spec.Automerge.RequireGlobalMergeGate = &globalGate
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	command := &store.CommandEvent{ID: "cmd-automerge-transient", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Intent: repositoryMonitorCommandIntentAutomerge, Permission: "maintain", HeadSHA: "head41"}
	pr := repositoryMonitorPullRequest{Number: 41, State: repositoryMonitorItemStateOpen, HeadSHA: "head41", BaseSHA: "base41", BaseBranch: "main", HeadBranch: "ready", HeadRepo: "orka-agents/orka", MergeableState: "clean"}
	item := &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "41", Number: 41, State: repositoryMonitorItemStateOpen, HeadSHA: "head41", LastVerdict: repositoryMonitorReviewVerdictPassed, LastReviewedHeadSHA: "head41"}
	handled, err := reconciler.tryProcessPullRequestAutomergeCommand(ctx, monitor, &store.MonitorRun{ID: "run-automerge-transient"}, command, "orka-agents", "orka", pr, item)
	if !handled || err == nil {
		t.Fatalf("tryProcessPullRequestAutomergeCommand() handled=%v err=%v, want propagated transient error", handled, err)
	}
	var ghErr *repositoryMonitorGitHubAPIError
	if !errors.As(err, &ghErr) || ghErr.StatusCode != http.StatusBadGateway {
		t.Fatalf("automerge error = %v, want GitHub 502", err)
	}
	mutationID := "ghmut-" + repositoryMonitorShortHash(command.ID+"-merge")
	mutation, getErr := monitorStore.GetGitHubMutationRecord(ctx, defaultNS, mutationID)
	if getErr != nil || mutation.Status != repositoryMonitorAutomergeStatePending {
		t.Fatalf("retryable mutation = %#v err=%v, want pending", mutation, getErr)
	}
	storedItem, getErr := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorPullRequestKind, "41")
	if getErr != nil || storedItem.AutomergeState != repositoryMonitorAutomergeStatePending || storedItem.SkipReason != repositoryMonitorRunRetryScheduled {
		t.Fatalf("retryable item = %#v err=%v, want pending retry_scheduled", storedItem, getErr)
	}
	action, getErr := monitorStore.GetWorkAction(ctx, defaultNS, store.RepositoryMonitorWorkActionID(command.ID, store.RepositoryMonitorDesiredActionForActionKind(repositoryMonitorActionAutomerge)))
	if getErr != nil || action.Status != repositoryMonitorWorkActionStatusRunning || action.CompletedAt != nil {
		t.Fatalf("retryable work action = %#v err=%v, want running without completion", action, getErr)
	}
}

func repositoryMonitorPatchValidationReason(t *testing.T, reconciler *RepositoryMonitorReconciler, ctx context.Context, monitor *corev1alpha1.RepositoryMonitor, item *store.MonitorItem, record *store.ActionRecord, task *corev1alpha1.Task, result *common.StructuredResult) string {
	t.Helper()
	reason, err := reconciler.validateAndSaveIssuePatchArtifacts(ctx, monitor, item, record, task, result)
	if err != nil {
		t.Fatalf("validateAndSaveIssuePatchArtifacts() error = %v", err)
	}
	return reason
}

func TestRepositoryMonitorAutomergePermanentGateFinalizesExistingMutation(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "automerge-gate-terminal", Namespace: defaultNS}}
	command := &store.CommandEvent{ID: "cmd-automerge-gate-terminal", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Intent: repositoryMonitorCommandIntentAutomerge, HeadSHA: "head41"}
	item := &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "41", Number: 41, State: repositoryMonitorItemStateOpen, HeadSHA: command.HeadSHA, AutomergeState: repositoryMonitorAutomergeStatePending}
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	mutationID := "ghmut-" + repositoryMonitorShortHash(command.ID+"-merge")
	if err := monitorStore.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: mutationID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, Operation: "merge_pr", TargetKind: repositoryMonitorPullRequestKind, TargetNumber: item.Number, TargetSHA: command.HeadSHA, Status: repositoryMonitorAutomergeStatePending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateGitHubMutationRecord() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	pr := repositoryMonitorPullRequest{Number: item.Number, State: repositoryMonitorItemStateOpen, HeadSHA: command.HeadSHA}
	handled, err := reconciler.tryProcessPullRequestAutomergeCommand(ctx, monitor, &store.MonitorRun{ID: "run-automerge-gate-terminal"}, command, "orka-agents", "orka", pr, item)
	if err != nil || !handled {
		t.Fatalf("tryProcessPullRequestAutomergeCommand() handled=%v err=%v", handled, err)
	}
	mutation, err := monitorStore.GetGitHubMutationRecord(ctx, defaultNS, mutationID)
	if err != nil || mutation.Status != repositoryMonitorRunPhaseFailed || mutation.Error != repositoryMonitorAutomergeReasonDisabled {
		t.Fatalf("gate-terminal mutation = %#v err=%v", mutation, err)
	}
	action, err := monitorStore.GetWorkAction(ctx, defaultNS, store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentAutomerge))
	if err != nil || action.Status != repositoryMonitorWorkActionStatusBlocked || action.BlockedReason != repositoryMonitorAutomergeReasonDisabled {
		t.Fatalf("gate-terminal action = %#v err=%v", action, err)
	}
}

func TestRepositoryMonitorAutomergeAmbiguousMergeErrorRemainsRetryable(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/commits/head41/check-runs":
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/commits/head41/status":
			_, _ = w.Write([]byte(`{"state":"success","statuses":[]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/orka-agents/orka/pulls/41/merge":
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = w.Write([]byte(`{"message":"head cannot be merged"}`))
		default:
			t.Fatalf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("automerge-permanent")
	globalGate := false
	monitor.Spec.Automerge.Enabled = true
	monitor.Spec.Automerge.RequireGlobalMergeGate = &globalGate
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	command := &store.CommandEvent{ID: "cmd-automerge-permanent", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Intent: repositoryMonitorCommandIntentAutomerge, Permission: "maintain", HeadSHA: "head41"}
	pr := repositoryMonitorPullRequest{Number: 41, State: repositoryMonitorItemStateOpen, HeadSHA: "head41", MergeableState: "clean"}
	item := &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "41", Number: 41, State: repositoryMonitorItemStateOpen, HeadSHA: "head41", LastVerdict: repositoryMonitorReviewVerdictPassed, LastReviewedHeadSHA: "head41"}
	handled, err := reconciler.tryProcessPullRequestAutomergeCommand(ctx, monitor, &store.MonitorRun{ID: "run-automerge-permanent"}, command, "orka-agents", "orka", pr, item)
	if !handled || err == nil {
		t.Fatalf("tryProcessPullRequestAutomergeCommand() handled=%v err=%v, want permanent error", handled, err)
	}
	action, getErr := monitorStore.GetWorkAction(ctx, defaultNS, store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentAutomerge))
	if getErr != nil || action.Status != repositoryMonitorWorkActionStatusRunning || action.Error != "" || action.CompletedAt != nil {
		t.Fatalf("retryable automerge action = %#v err=%v", action, getErr)
	}
}

func TestRepositoryMonitorIssuePatchValidationArtifacts(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := repositoryMonitorReviewIngestTestMonitor("patch-validation")
	monitor.Spec.Owner = "sozercan"
	monitor.Spec.Repository = "orka"
	item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 55, SnapshotDigest: "sha256:test"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "impl-task", Namespace: "default"}}
	record := &store.ActionRecord{ID: "act-impl", CommandEventID: "cmd-impl"}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore, ArtifactStore: monitorStore}
	missingFiles := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/internal/x.go b/internal/x.go\n"}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, missingFiles); got != "patch_file_list_missing" {
		t.Fatalf("missing file list reason = %q, want patch_file_list_missing", got)
	}
	denied := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n", Files: []string{".github/workflows/ci.yml"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, denied); got != "patch_path_denied" {
		t.Fatalf("denied patch reason = %q, want patch_path_denied", got)
	}
	manifestMismatch := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/docs/safe.md b/.github/workflows/pwn.yml\n--- a/docs/safe.md\n+++ b/.github/workflows/pwn.yml\n", Files: []string{"docs/safe.md"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, manifestMismatch); got != "patch_path_manifest_mismatch" {
		t.Fatalf("forged patch manifest reason = %q, want patch_path_manifest_mismatch", got)
	}
	headerLikeHunk := &common.StructuredResult{
		BaseSHA: "base",
		Diff:    "diff --git a/schema.sql b/schema.sql\n--- a/schema.sql\n+++ b/schema.sql\n@@ -1 +1 @@\n--- initialize schema\n+-- initialize schema safely\n",
		Files:   []string{"schema.sql"},
	}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, &store.ActionRecord{ID: "act-header-like", CommandEventID: "cmd-header-like"}, &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "impl-header-like", Namespace: "default"}}, headerLikeHunk); got != "" {
		t.Fatalf("header-like hunk patch reason = %q, want empty", got)
	}
	missingGitHeader := &common.StructuredResult{BaseSHA: "base", Diff: "--- a/.github/workflows/pwn.yml\n+++ b/.github/workflows/pwn.yml\n@@ -1 +1 @@\n-old\n+new\n", Files: []string{"docs/safe.md"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, missingGitHeader); got != "patch_path_invalid" {
		t.Fatalf("non-git patch reason = %q, want patch_path_invalid", got)
	}
	prefixBypass := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git \"x/.github/workflows/pwn.yml\" \"x/.github/workflows/pwn.yml\"\n--- \"x/.github/workflows/pwn.yml\"\n+++ \"x/.github/workflows/pwn.yml\"\n@@ -1 +1 @@\n-old\n+new\n", Files: []string{"x/.github/workflows/pwn.yml"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, prefixBypass); got != "patch_path_invalid" {
		t.Fatalf("noncanonical prefix reason = %q, want patch_path_invalid", got)
	}
	mixedPatch := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/docs/safe.md b/docs/safe.md\n--- a/docs/safe.md\n+++ b/docs/safe.md\n@@ -1 +1 @@\n-old\n+new\n--- a/.github/workflows/pwn.yml\n+++ b/.github/workflows/pwn.yml\n@@ -1 +1 @@\n-old\n+new\n", Files: []string{"docs/safe.md"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, mixedPatch); got != "patch_path_invalid" {
		t.Fatalf("mixed patch reason = %q, want patch_path_invalid", got)
	}
	leadingMixedPatch := &common.StructuredResult{BaseSHA: "base", Diff: "--- a/.github/workflows/pwn.yml\n+++ b/.github/workflows/pwn.yml\n@@ -1 +1 @@\n-old\n+new\ndiff --git a/docs/safe.md b/docs/safe.md\n--- a/docs/safe.md\n+++ b/docs/safe.md\n@@ -1 +1 @@\n-old\n+new\n", Files: []string{"docs/safe.md"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, leadingMixedPatch); got != "patch_path_invalid" {
		t.Fatalf("leading mixed patch reason = %q, want patch_path_invalid", got)
	}
	dotPath := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/./.github/workflows/pwn.yml b/./.github/workflows/pwn.yml\n--- a/./.github/workflows/pwn.yml\n+++ b/./.github/workflows/pwn.yml\n@@ -1 +1 @@\n-old\n+new\n", Files: []string{"./.github/workflows/pwn.yml"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, dotPath); got != "patch_path_invalid" {
		t.Fatalf("dot path reason = %q, want patch_path_invalid", got)
	}
	credentialMarker := "github" + "_pat_" + strings.Repeat("a", 24)
	secretPatch := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/config.txt b/config.txt\n--- a/config.txt\n+++ b/config.txt\n@@ -0,0 +1 @@\n+++ " + credentialMarker + "\n", Files: []string{"config.txt"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, secretPatch); got != "patch_secret_scan_failed" {
		t.Fatalf("secret patch reason = %q, want patch_secret_scan_failed", got)
	}
	modeMismatch := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/.github/workflows/pwn.yml b/.github/workflows/pwn.yml\nold mode 100644\nnew mode 100755\n", Files: []string{"docs/safe.md"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, modeMismatch); got != "patch_path_manifest_mismatch" {
		t.Fatalf("mode-only manifest mismatch reason = %q, want patch_path_manifest_mismatch", got)
	}
	renameDenied := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/docs/pwn.yml b/.github/workflows/pwn.yml\nsimilarity index 100%\nrename from docs/pwn.yml\nrename to .github/workflows/pwn.yml\n", Files: []string{"docs/pwn.yml", ".github/workflows/pwn.yml"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, renameDenied); got != "patch_path_denied" {
		t.Fatalf("rename destination reason = %q, want patch_path_denied", got)
	}
	valid := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/internal/x.go b/internal/x.go\n--- a/internal/x.go\n+++ b/internal/x.go\n@@ -1 +1 @@\n-old\n+new\n", Files: []string{"internal/x.go"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, valid); got != "" {
		t.Fatalf("valid patch reason = %q, want empty", got)
	}
	if _, _, err := monitorStore.GetArtifact(ctx, "default", "impl-task", repositoryMonitorIssuePatchDiffArtifact(55, "act-impl")); err != nil {
		t.Fatalf("diff artifact missing: %v", err)
	}
	if _, _, err := monitorStore.GetArtifact(ctx, "default", "impl-task", repositoryMonitorIssuePatchSummaryArtifact(55, "act-impl")); err != nil {
		t.Fatalf("summary artifact missing: %v", err)
	}
}

func TestRepositoryMonitorIssuePatchRejectsInjectedRuntimeCredential(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	opaqueValue := strings.Repeat("z", 24) + "-opaque"
	runtimeConfig := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "implementation-runtime-config", Namespace: defaultNS}, Data: map[string][]byte{workerenv.OpenAIAPIKey: []byte(opaqueValue)}}
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "opaque-implementer", Namespace: defaultNS}, Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}, SecretRef: &corev1.LocalObjectReference{Name: runtimeConfig.Name}}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, runtimeConfig).Build()
	monitor := repositoryMonitorReviewIngestTestMonitor("opaque-patch-credential")
	item := &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 55, SnapshotDigest: "sha256:test"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "impl-opaque", Namespace: defaultNS, Annotations: map[string]string{labels.AnnotationAgentRuntimeAuthOnly: scheduledRunLabelValue}}, Spec: corev1alpha1.TaskSpec{AgentRef: &corev1alpha1.AgentReference{Name: agent.Name}}}
	record := &store.ActionRecord{ID: "act-opaque", CommandEventID: "cmd-opaque"}
	reconciler := &RepositoryMonitorReconciler{Client: cl, Store: monitorStore, ArtifactStore: monitorStore}
	result := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/config.txt b/config.txt\n--- a/config.txt\n+++ b/config.txt\n@@ -0,0 +1 @@\n+" + opaqueValue + "\n", Files: []string{"config.txt"}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, result); got != "patch_secret_scan_failed" {
		t.Fatalf("opaque credential patch reason = %q, want patch_secret_scan_failed", got)
	}
	pathResult := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/" + opaqueValue + " b/" + opaqueValue + "\nnew file mode 100644\n", Files: []string{opaqueValue}}
	if got := repositoryMonitorPatchValidationReason(t, reconciler, ctx, monitor, item, record, task, pathResult); got != "patch_secret_scan_failed" {
		t.Fatalf("opaque credential path reason = %q, want patch_secret_scan_failed", got)
	}
}

func TestRepositoryMonitorIssueImplementationResultRedactsRuntimeCredentialBeforePersistence(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	immutable := true
	credential := strings.Repeat("q", 24) + "-opaque"
	runtimeConfig := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pinned-runtime", Namespace: defaultNS, UID: "uid-pinned-runtime"}, Data: map[string][]byte{workerenv.OpenAIAPIKey: []byte(credential)}, Immutable: &immutable}
	gitSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "github-token", Namespace: defaultNS}, Data: map[string][]byte{repositoryMonitorTokenKey: []byte("test-token")}}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/orka-agents/orka/issues/55/comments" {
			t.Fatalf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		}
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode status comment: %v", err)
		}
		if strings.Contains(payload["body"], credential) || !strings.Contains(payload["body"], "could not be safely accepted") {
			t.Fatalf("status comment was not safely redacted: %q", payload["body"])
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":5501,"html_url":"https://github.com/orka-agents/orka/issues/55#issuecomment-5501"}`))
	}))
	t.Cleanup(server.Close)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(runtimeConfig, gitSecret).Build()
	monitor := repositoryMonitorReviewIngestTestMonitor("implementation-result-redaction")
	monitor.Spec.GitSecretRef = &corev1.LocalObjectReference{Name: gitSecret.Name}
	item := &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "55", Number: 55, SnapshotDigest: "sha256:redact", WorkflowPhase: repositoryMonitorIssuePhaseImplementing, LastActionKind: repositoryMonitorIssueActionImplementation, LastActionTaskName: "impl-redact"}
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "impl-redact", Namespace: defaultNS, Annotations: map[string]string{
		repositoryMonitorIssueAnnotationActionKind:             repositoryMonitorIssueActionImplementation,
		repositoryMonitorIssueAnnotationCommandID:              "cmd-redact",
		labels.AnnotationAgentRuntimeAuthOnly:                  scheduledRunLabelValue,
		repositoryMonitorIssueAnnotationRuntimeAgentGeneration: "0",
		repositoryMonitorIssueAnnotationRuntimeAuthUID:         "uid-pinned-runtime",
		repositoryMonitorIssueAnnotationRuntimeAuthFields:      workerenv.OpenAIAPIKey,
	}}, Spec: corev1alpha1.TaskSpec{SecretRef: &corev1alpha1.SecretReference{Name: runtimeConfig.Name}}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseSucceeded}}
	summary, _ := json.Marshal(map[string]any{"schemaVersion": "orka.issueImplementation.v1", "status": "patch_ready", "summary": "leaked " + credential})
	raw, err := common.FormatStructuredResult(&common.StructuredResult{Summary: string(summary), BaseSHA: "base", Diff: "diff --git a/internal/x.go b/internal/x.go\n--- a/internal/x.go\n+++ b/internal/x.go\n@@ -1 +1 @@\n-old\n+" + credential + "\n", Files: []string{"internal/x.go"}})
	if err != nil {
		t.Fatalf("FormatStructuredResult() error = %v", err)
	}
	if err := monitorStore.SaveResult(ctx, task.Namespace, task.Name, raw); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Client: cl, Store: monitorStore, ResultStore: monitorStore, ArtifactStore: monitorStore, GitHubAPIBaseURL: server.URL}
	handled, err := reconciler.ingestCompletedRepositoryMonitorIssueTask(ctx, monitor, item, task)
	if err != nil || !handled {
		t.Fatalf("ingestCompletedRepositoryMonitorIssueTask() handled=%v err=%v", handled, err)
	}
	record, err := monitorStore.GetActionRecord(ctx, defaultNS, repositoryMonitorIssueActionRecordID(task))
	if err != nil {
		t.Fatalf("GetActionRecord() error = %v", err)
	}
	if strings.Contains(record.PayloadJSON, credential) || strings.Contains(record.Summary, credential) {
		t.Fatalf("credential persisted in action record: %#v", record)
	}
	durableResult, err := monitorStore.GetResult(ctx, task.Namespace, task.Name)
	if err != nil {
		t.Fatalf("GetResult() error = %v", err)
	}
	if strings.Contains(string(durableResult), credential) || !strings.Contains(string(durableResult), "could not be safely accepted") {
		t.Fatalf("durable result was not safely replaced: %q", durableResult)
	}
	if record.Verdict != repositoryMonitorPatchSensitiveContentReason || record.Summary != "Implementation result was blocked because it could not be safely accepted." {
		t.Fatalf("sanitized record = %#v", record)
	}
	storedItem, err := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorIssueKind, "55")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if storedItem.WorkflowPhase != repositoryMonitorIssuePhaseBlocked || storedItem.SkipReason != repositoryMonitorPatchSensitiveContentReason {
		t.Fatalf("stored item = %#v, want sensitive-result block", storedItem)
	}
}

func TestRepositoryMonitorStoredImplementationRecordIsRedactedBeforeApply(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	immutable := true
	credential := strings.Repeat("r", 24) + "-stored"
	runtimeConfig := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "stored-runtime", Namespace: defaultNS, UID: "uid-stored-runtime"}, Data: map[string][]byte{workerenv.OpenAIAPIKey: []byte(credential)}, Immutable: &immutable}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(runtimeConfig).Build()
	monitor := repositoryMonitorReviewIngestTestMonitor("stored-implementation-redaction")
	item := &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "56", Number: 56, SnapshotDigest: "sha256:stored"}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "impl-stored", Namespace: defaultNS, Annotations: map[string]string{
		repositoryMonitorIssueAnnotationActionKind:             repositoryMonitorIssueActionImplementation,
		repositoryMonitorIssueAnnotationCommandID:              "cmd-stored",
		labels.AnnotationAgentRuntimeAuthOnly:                  scheduledRunLabelValue,
		repositoryMonitorIssueAnnotationRuntimeAgentGeneration: "0",
		repositoryMonitorIssueAnnotationRuntimeAuthUID:         "uid-stored-runtime",
		repositoryMonitorIssueAnnotationRuntimeAuthFields:      workerenv.OpenAIAPIKey,
	}}, Spec: corev1alpha1.TaskSpec{SecretRef: &corev1alpha1.SecretReference{Name: runtimeConfig.Name}}}
	raw, _ := json.Marshal(map[string]any{
		"schemaVersion": "orka.issueImplementation.v1", "issueNumber": item.Number,
		"snapshotDigest": item.SnapshotDigest, "status": "patch_ready", "summary": "stored " + credential,
	})
	record := repositoryMonitorActionRecordFromTask(monitor, item, task, repositoryMonitorIssueActionImplementation, raw)
	if err := monitorStore.CreateActionRecord(ctx, record); err != nil {
		t.Fatalf("CreateActionRecord() error = %v", err)
	}
	if err := monitorStore.SaveResult(ctx, task.Namespace, task.Name, raw); err != nil {
		t.Fatalf("SaveResult() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Client: cl, Store: monitorStore, ResultStore: monitorStore}
	redacted, err := reconciler.sanitizeRepositoryMonitorStoredImplementationRecord(ctx, monitor, item, task, record)
	if err != nil {
		t.Fatalf("sanitizeRepositoryMonitorStoredImplementationRecord() error = %v", err)
	}
	if strings.Contains(redacted.PayloadJSON, credential) || strings.Contains(redacted.Summary, credential) || redacted.Verdict != repositoryMonitorPatchSensitiveContentReason {
		t.Fatalf("redacted record = %#v", redacted)
	}
	stored, err := monitorStore.GetActionRecord(ctx, defaultNS, record.ID)
	if err != nil || strings.Contains(stored.PayloadJSON, credential) || stored.Verdict != repositoryMonitorPatchSensitiveContentReason {
		t.Fatalf("stored action record = %#v err=%v", stored, err)
	}
	result, err := monitorStore.GetResult(ctx, task.Namespace, task.Name)
	if err != nil || strings.Contains(string(result), credential) {
		t.Fatalf("stored result = %q err=%v", result, err)
	}
}

func TestRepositoryMonitorIssuePatchScansPinnedCredentialWithoutCurrentAgent(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	immutable := true
	credential := strings.Repeat("p", 24) + "-pinned"
	runtimeConfig := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "pinned-runtime", Namespace: defaultNS, UID: "uid-pinned-runtime"}, Data: map[string][]byte{workerenv.OpenAIAPIKey: []byte(credential)}, Immutable: &immutable}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(runtimeConfig).Build()
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "impl-pinned", Namespace: defaultNS, Annotations: map[string]string{
		labels.AnnotationAgentRuntimeAuthOnly:                  scheduledRunLabelValue,
		repositoryMonitorIssueAnnotationActionKind:             repositoryMonitorIssueActionImplementation,
		repositoryMonitorIssueAnnotationRuntimeAgentGeneration: "0",
		repositoryMonitorIssueAnnotationRuntimeAuthUID:         "uid-pinned-runtime",
		repositoryMonitorIssueAnnotationRuntimeAuthFields:      workerenv.OpenAIAPIKey,
	}}, Spec: corev1alpha1.TaskSpec{AgentRef: &corev1alpha1.AgentReference{Name: "removed-agent"}, SecretRef: &corev1alpha1.SecretReference{Name: runtimeConfig.Name}}}
	reconciler := &RepositoryMonitorReconciler{Client: cl}
	contains, err := reconciler.repositoryMonitorPatchContainsRuntimeCredential(ctx, task, "safe prefix "+credential+" safe suffix")
	if err != nil || !contains {
		t.Fatalf("repositoryMonitorPatchContainsRuntimeCredential() contains=%v err=%v, want pinned credential match", contains, err)
	}
}

func TestRepositoryMonitorIssuePatchCredentialLookupErrorRemainsRetryable(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	lookupErr := errors.New("transient agent lookup failure")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, c crclient.WithWatch, key crclient.ObjectKey, obj crclient.Object, opts ...crclient.GetOption) error {
				if _, ok := obj.(*corev1alpha1.Agent); ok {
					return lookupErr
				}
				return c.Get(ctx, key, obj, opts...)
			},
		}).
		Build()
	monitor := repositoryMonitorReviewIngestTestMonitor("patch-credential-lookup-retry")
	item := &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "55", Number: 55, SnapshotDigest: "sha256:test", WorkflowPhase: repositoryMonitorIssuePhaseImplementing, LastActionKind: repositoryMonitorIssueActionImplementation, LastActionTaskName: "impl-retry"}
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	if err := monitorStore.CreateImplementationJob(ctx, &store.ImplementationJob{ID: repositoryMonitorImplementationJobID("impl-retry"), MonitorNamespace: defaultNS, MonitorName: monitor.Name, IssueNumber: item.Number, SnapshotDigest: item.SnapshotDigest, Phase: repositoryMonitorIssuePhaseImplementationQueued, TaskName: "impl-retry", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateImplementationJob() error = %v", err)
	}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "impl-retry", Namespace: defaultNS, Annotations: map[string]string{labels.AnnotationAgentRuntimeAuthOnly: scheduledRunLabelValue}}, Spec: corev1alpha1.TaskSpec{AgentRef: &corev1alpha1.AgentReference{Name: "implementer"}}}
	diff := "diff --git a/internal/x.go b/internal/x.go\n--- a/internal/x.go\n+++ b/internal/x.go\n@@ -1 +1 @@\n-old\n+new\n"
	payload, err := common.FormatStructuredResult(&common.StructuredResult{Summary: `{"schemaVersion":"orka.issueImplementation.v1","status":"patch_ready","summary":"safe patch"}`, BaseSHA: "base", Diff: diff, Files: []string{"internal/x.go"}})
	if err != nil {
		t.Fatalf("FormatStructuredResult() error = %v", err)
	}
	record := &store.ActionRecord{ID: "act-lookup-retry", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: item.Number, ActionKind: repositoryMonitorIssueActionImplementation, SnapshotDigest: item.SnapshotDigest, TaskName: task.Name, CommandEventID: "cmd-lookup-retry", Verdict: "patch_ready", PayloadJSON: string(payload), CreatedAt: time.Now()}
	reconciler := &RepositoryMonitorReconciler{Client: cl, Store: monitorStore, ArtifactStore: monitorStore}
	handled, err := reconciler.applyIssueActionRecord(ctx, monitor, item, record, task)
	if handled || !errors.Is(err, lookupErr) {
		t.Fatalf("applyIssueActionRecord() handled=%v err=%v, want retryable lookup error", handled, err)
	}
	storedItem, err := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorIssueKind, "55")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if storedItem.WorkflowPhase != repositoryMonitorIssuePhaseImplementing || storedItem.SkipReason != "" {
		t.Fatalf("stored item = %#v, want implementation state preserved for retry", storedItem)
	}
	job, err := monitorStore.GetImplementationJob(ctx, defaultNS, repositoryMonitorImplementationJobID(task.Name))
	if err != nil {
		t.Fatalf("GetImplementationJob() error = %v", err)
	}
	if job.Phase != repositoryMonitorIssuePhaseImplementationQueued || job.CompletedAt != nil || job.Error != "" {
		t.Fatalf("implementation job = %#v, want non-terminal retryable state", job)
	}
	if action, err := monitorStore.GetWorkAction(ctx, defaultNS, store.RepositoryMonitorWorkActionID(record.CommandEventID, store.RepositoryMonitorDesiredActionForActionKind(repositoryMonitorIssueActionImplementation))); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("implementation work action = %#v err=%v, want no terminal action", action, err)
	}
}

func TestRepositoryMonitorRepairPolicyBlocksDisabledAndBudgets(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "repair-policy", Namespace: "default"}}
	pr := repositoryMonitorPullRequest{Number: 31, HeadSHA: "head31", HeadRepo: "orka-agents/orka"}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}

	if reason, _, _, err := reconciler.repositoryMonitorRepairPolicy(ctx, monitor, "orka-agents/orka", pr, ""); err != nil || reason != "repair_disabled" {
		t.Fatalf("disabled repair reason=%q err=%v", reason, err)
	}
	monitor.Spec.Repair.Enabled = true
	if reason, _, _, err := reconciler.repositoryMonitorRepairPolicy(ctx, monitor, "orka-agents/orka", pr, ""); err != nil || reason != "missing_repairer_agent" {
		t.Fatalf("missing repairer reason=%q err=%v", reason, err)
	}
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	forkPR := pr
	forkPR.HeadRepo = "contributor/fork"
	if reason, _, _, err := reconciler.repositoryMonitorRepairPolicy(ctx, monitor, "orka-agents/orka", forkPR, ""); err != nil || reason != "fork_pr_repair_not_writable" {
		t.Fatalf("fork repair reason=%q err=%v", reason, err)
	}
	maxPR := int32(1)
	monitor.Spec.Repair.MaxRepairsPerPR = &maxPR
	if err := monitorStore.CreateRepairJob(ctx, &store.RepairJob{ID: "existing-repair", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", PRNumber: 31, HeadSHA: "old-head", Phase: repositoryMonitorRepairPhaseFailed, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateRepairJob() error = %v", err)
	}
	if reason, _, _, err := reconciler.repositoryMonitorRepairPolicy(ctx, monitor, "orka-agents/orka", pr, ""); err != nil || reason != "repair_pr_budget_exhausted" {
		t.Fatalf("PR budget reason=%q err=%v", reason, err)
	}
	monitor.Spec.Repair.MaxRepairsPerPR = nil
	maxHead := int32(1)
	monitor.Spec.Repair.MaxRepairsPerHead = &maxHead
	if err := monitorStore.CreateRepairJob(ctx, &store.RepairJob{ID: "existing-head-repair", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", PRNumber: 31, HeadSHA: "head31", Phase: repositoryMonitorRepairPhaseFailed, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateRepairJob(head) error = %v", err)
	}
	if reason, _, _, err := reconciler.repositoryMonitorRepairPolicy(ctx, monitor, "orka-agents/orka", pr, ""); err != nil || reason != "repair_head_budget_exhausted" {
		t.Fatalf("head budget reason=%q err=%v", reason, err)
	}

	orphanMonitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "repair-orphan", Namespace: "default"}}
	orphanMonitor.Spec.Repair.Enabled = true
	orphanMonitor.Spec.Repair.MaxRepairsPerPR = &maxPR
	orphanMonitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	if err := monitorStore.CreateRepairJob(ctx, &store.RepairJob{ID: "orphan-repair", MonitorNamespace: "default", MonitorName: orphanMonitor.Name, Repo: "orka-agents/orka", PRNumber: 31, HeadSHA: "head31", Phase: repositoryMonitorRepairPhaseQueued, TaskName: "missing-task", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateRepairJob(orphan) error = %v", err)
	}
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	presentTask := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "present-task", Namespace: "default"}}
	orphanReconciler := &RepositoryMonitorReconciler{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(presentTask).Build(), Store: monitorStore}
	if reason, countPR, countHead, err := orphanReconciler.repositoryMonitorRepairPolicy(ctx, orphanMonitor, "orka-agents/orka", pr, "replacement-repair"); err != nil || reason != "" || countPR != 0 || countHead != 0 {
		t.Fatalf("orphan queued job consumed repair budget: reason=%q countPR=%d countHead=%d err=%v", reason, countPR, countHead, err)
	}
	if err := monitorStore.CreateRepairJob(ctx, &store.RepairJob{ID: "live-repair", MonitorNamespace: "default", MonitorName: orphanMonitor.Name, Repo: "orka-agents/orka", PRNumber: 31, HeadSHA: "head31", Phase: repositoryMonitorRepairPhaseQueued, TaskName: presentTask.Name, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateRepairJob(live) error = %v", err)
	}
	if reason, countPR, countHead, err := orphanReconciler.repositoryMonitorRepairPolicy(ctx, orphanMonitor, "orka-agents/orka", pr, "replacement-repair"); err != nil || reason != "repair_pr_budget_exhausted" || countPR != 1 || countHead != 1 {
		t.Fatalf("live queued job budget result: reason=%q countPR=%d countHead=%d err=%v", reason, countPR, countHead, err)
	}
}

func TestRepositoryMonitorIssueStatusCommentNeutralizesActiveText(t *testing.T) {
	got := sanitizeRepositoryMonitorPublicCommentText("summary\n/landpr\n<!-- hidden command -->\n@maintainer")
	if strings.Contains(got, "\n/landpr") || strings.Contains(got, "<!--") || strings.Contains(got, "@maintainer") {
		t.Fatalf("issue status comment preserved active text: %q", got)
	}
}

func TestRepositoryMonitorIssueInventoryPreservesStatusCommentIdentity(t *testing.T) {
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "m", Namespace: "default"}}
	issue := repositoryMonitorIssue{Number: 44, Title: "x", State: "open"}
	existing := &store.MonitorItem{StatusCommentID: "123", StatusCommentURL: "https://example.test/comment/123"}
	got := repositoryMonitorItemFromIssue(monitor, issue, existing)
	if got.StatusCommentID != existing.StatusCommentID || got.StatusCommentURL != existing.StatusCommentURL {
		t.Fatalf("status comment identity lost during inventory refresh: %#v", got)
	}
}

func TestRepositoryMonitorStateTransitionValidation(t *testing.T) {
	if !repositoryMonitorIssuePhaseTransitionAllowed(repositoryMonitorIssuePhasePlanReady, repositoryMonitorIssuePhaseApprovalRequired) {
		t.Fatal("plan_ready should transition to approval_required")
	}
	if repositoryMonitorIssuePhaseTransitionAllowed(repositoryMonitorIssuePhaseApproved, repositoryMonitorIssuePhaseResearchQueued) {
		t.Fatal("approved should not transition back to research_queued without a target change")
	}
	if !repositoryMonitorPRPhaseTransitionAllowed("reviewed_needs_changes", "repair_queued") {
		t.Fatal("reviewed_needs_changes should transition to repair_queued")
	}
	if repositoryMonitorPRPhaseTransitionAllowed(repositoryMonitorAutomergeStateMergeReady, "repairing") {
		t.Fatal("merge_ready should not jump directly to repairing")
	}
}

func TestRepositoryMonitorRunFailureState(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{name: "github rate limited", err: &repositoryMonitorGitHubAPIError{Operation: "issues", StatusCode: http.StatusForbidden, Body: "secondary rate limit"}, want: "github_rate_limited"},
		{name: "github too many requests", err: &repositoryMonitorGitHubAPIError{Operation: "issues", StatusCode: http.StatusTooManyRequests, Body: "slow down"}, want: "github_rate_limited"},
		{name: "github request timeout", err: &repositoryMonitorGitHubAPIError{Operation: "issues", StatusCode: http.StatusRequestTimeout, Body: "timed out"}, want: repositoryMonitorRunRetryScheduled},
		{name: "github transient", err: &repositoryMonitorGitHubAPIError{Operation: "issues", StatusCode: http.StatusBadGateway, Body: "bad gateway"}, want: "retry_scheduled"},
		{name: "github conflict", err: &repositoryMonitorGitHubAPIError{Operation: "merge", StatusCode: http.StatusConflict, Body: "conflict"}, want: repositoryMonitorRunRetryScheduled},
		{name: "github unprocessable", err: &repositoryMonitorGitHubAPIError{Operation: "merge", StatusCode: http.StatusUnprocessableEntity, Body: "retry"}, want: repositoryMonitorRunRetryScheduled},
		{name: "github not found", err: &repositoryMonitorGitHubAPIError{Operation: "issues", StatusCode: http.StatusNotFound, Body: "missing"}, want: repositoryMonitorRunFailurePermanent},
		{name: "transport timeout", err: fmt.Errorf("dial tcp timeout"), want: "retry_scheduled"},
		{name: "cluster capacity", err: fmt.Errorf("cluster capacity exhausted"), want: "cluster_capacity_blocked"},
		{name: "llm", err: fmt.Errorf("llm_rate_limited by provider"), want: "llm_rate_limited"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := repositoryMonitorRunFailureState(tt.err); got != tt.want {
				t.Fatalf("state = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRepositoryMonitorFailedRunPreservesTerminalActionAndApprovalMapping(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "terminal-action", Namespace: defaultNS}}
	command := store.CommandEvent{ID: "cmd-terminal", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 12, Intent: repositoryMonitorCommandIntentApprovePlan}
	actionKind := repositoryMonitorCommandActionKind(command.Intent)
	if actionKind != repositoryMonitorIssueActionApprove {
		t.Fatalf("approve_plan action kind = %q, want %q", actionKind, repositoryMonitorIssueActionApprove)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, "approve")
	completedAt := time.Now()
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, DesiredAction: "approve", Status: repositoryMonitorWorkActionStatusSucceeded, CompletedAt: &completedAt, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	if err := reconciler.terminalizeRepositoryMonitorFailedCommand(ctx, monitor, command, &store.MonitorRun{ID: "run-terminal"}, "late run failure"); err != nil {
		t.Fatalf("terminalizeRepositoryMonitorFailedCommand() error = %v", err)
	}
	action, err := monitorStore.GetWorkAction(ctx, defaultNS, actionID)
	if err != nil {
		t.Fatalf("GetWorkAction() error = %v", err)
	}
	if action.Status != repositoryMonitorWorkActionStatusSucceeded || action.Error != "" {
		t.Fatalf("terminal action was downgraded: %#v", action)
	}
}

func TestRepositoryMonitorPermanentCommandRunTerminalizesWorkAction(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "permanent-command", Namespace: defaultNS}}
	command := store.CommandEvent{ID: "cmd-permanent", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 7, Intent: "review", HeadSHA: "head-7"}
	runID := repositoryMonitorCommandRunIDFromCommand(command.ID)
	completedAt := time.Now()
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: runID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseFailed, StartedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt, Error: "[run_failed] GitHub returned 404"}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, "review")
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, DesiredAction: "review", Status: repositoryMonitorWorkActionStatusQueued, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	handled, reset, err := reconciler.ensureNoExistingCommandRunBlocksQueue(ctx, monitor, command, runID)
	if err != nil || !handled || reset {
		t.Fatalf("ensureNoExistingCommandRunBlocksQueue() = handled=%v reset=%v err=%v", handled, reset, err)
	}
	action, err := monitorStore.GetWorkAction(ctx, defaultNS, actionID)
	if err != nil {
		t.Fatalf("GetWorkAction() error = %v", err)
	}
	if action.Status != repositoryMonitorWorkActionStatusFailed || action.Error == "" || action.CompletedAt == nil {
		t.Fatalf("terminal action = %#v, want failed permanent outcome", action)
	}
}

func TestRepositoryMonitorTransientCommandRunStopsAfterRetryBudget(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "retry-budget", Namespace: defaultNS}}
	command := store.CommandEvent{ID: "cmd-budget", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 8, Intent: "review", HeadSHA: "head-8"}
	runID := repositoryMonitorCommandRunIDFromCommand(command.ID)
	completedAt := time.Now()
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: runID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseFailed, StartedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt, Error: "[retry_scheduled] upstream unavailable"}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, "review")
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, DesiredAction: "review", Status: repositoryMonitorWorkActionStatusQueued, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	for i := range repositoryMonitorCommandMaxRetries {
		if err := monitorStore.CreateMonitorEvent(ctx, &store.MonitorEvent{ID: fmt.Sprintf("evt-retry-%d", i), MonitorNamespace: defaultNS, MonitorName: monitor.Name, RunID: runID, EventType: "run_failed", CreatedAt: time.Now()}); err != nil {
			t.Fatalf("CreateMonitorEvent(%d) error = %v", i, err)
		}
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	handled, reset, err := reconciler.ensureNoExistingCommandRunBlocksQueue(ctx, monitor, command, runID)
	if err != nil || !handled || reset {
		t.Fatalf("ensureNoExistingCommandRunBlocksQueue() = handled=%v reset=%v err=%v", handled, reset, err)
	}
	action, err := monitorStore.GetWorkAction(ctx, defaultNS, actionID)
	if err != nil {
		t.Fatalf("GetWorkAction() error = %v", err)
	}
	if action.Status != repositoryMonitorWorkActionStatusFailed || action.Error != "retry_attempts_exhausted" {
		t.Fatalf("retry-exhausted action = %#v", action)
	}
}

func TestRepositoryMonitorTransientAutomergeRunFinalizesAfterRetryBudget(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "automerge-retry-budget", Namespace: defaultNS}}
	command := store.CommandEvent{ID: "cmd-automerge-budget", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 8, Intent: repositoryMonitorCommandIntentAutomerge, HeadSHA: "head-8"}
	runID := repositoryMonitorCommandRunIDFromCommand(command.ID)
	completedAt := time.Now()
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: runID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseFailed, StartedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt, Error: "[retry_scheduled] upstream unavailable"}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "8", Number: 8, State: repositoryMonitorItemStateOpen, HeadSHA: command.HeadSHA, AutomergeState: repositoryMonitorAutomergeStatePending, SkipReason: repositoryMonitorRunRetryScheduled}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	mutationID := "ghmut-" + repositoryMonitorShortHash(command.ID+"-merge")
	if err := monitorStore.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: mutationID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, Operation: "merge_pr", TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, Status: repositoryMonitorAutomergeStatePending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateGitHubMutationRecord() error = %v", err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentAutomerge)
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, DesiredAction: repositoryMonitorCommandIntentAutomerge, Status: repositoryMonitorWorkActionStatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	for i := range repositoryMonitorCommandMaxRetries {
		if err := monitorStore.CreateMonitorEvent(ctx, &store.MonitorEvent{ID: fmt.Sprintf("evt-automerge-retry-%d", i), MonitorNamespace: defaultNS, MonitorName: monitor.Name, RunID: runID, EventType: "run_failed", CreatedAt: time.Now()}); err != nil {
			t.Fatalf("CreateMonitorEvent(%d) error = %v", i, err)
		}
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	handled, reset, err := reconciler.ensureNoExistingCommandRunBlocksQueue(ctx, monitor, command, runID)
	if err != nil || !handled || reset {
		t.Fatalf("ensureNoExistingCommandRunBlocksQueue() = handled=%v reset=%v err=%v", handled, reset, err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorPullRequestKind, "8")
	if err != nil || item.AutomergeState != repositoryMonitorAutomergeStateFailed || item.SkipReason != "retry_attempts_exhausted" {
		t.Fatalf("retry-exhausted item = %#v err=%v", item, err)
	}
	mutation, err := monitorStore.GetGitHubMutationRecord(ctx, defaultNS, mutationID)
	if err != nil || mutation.Status != repositoryMonitorRunPhaseFailed || mutation.Error != "retry_attempts_exhausted" {
		t.Fatalf("retry-exhausted mutation = %#v err=%v", mutation, err)
	}
	action, err := monitorStore.GetWorkAction(ctx, defaultNS, actionID)
	if err != nil || action.Status != repositoryMonitorWorkActionStatusFailed || action.Error != "retry_attempts_exhausted" {
		t.Fatalf("retry-exhausted action = %#v err=%v", action, err)
	}
}

func TestRepositoryMonitorBlockedAutomergeRetryFinalizesMutationWithoutRewritingNewHead(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "automerge-blocked-retry", Namespace: defaultNS}}
	command := &store.CommandEvent{ID: "cmd-automerge-stale-retry", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 9, Intent: repositoryMonitorCommandIntentAutomerge, HeadSHA: "old-head", Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "9", Number: 9, State: repositoryMonitorItemStateOpen, HeadSHA: "new-head", AutomergeState: repositoryMonitorAutomergeStateMergeReady}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	mutationID := "ghmut-" + repositoryMonitorShortHash(command.ID+"-merge")
	if err := monitorStore.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: mutationID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, Operation: "merge_pr", TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, Status: repositoryMonitorAutomergeStatePending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateGitHubMutationRecord() error = %v", err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentAutomerge)
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, DesiredAction: repositoryMonitorCommandIntentAutomerge, Status: repositoryMonitorWorkActionStatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	run := &store.MonitorRun{ID: "run-automerge-stale-retry", CommandEventID: command.ID, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA}
	if err := reconciler.blockRepositoryMonitorTargetCommand(ctx, monitor, run, repositoryMonitorReviewSkipReasonStaleHead); err != nil {
		t.Fatalf("blockRepositoryMonitorTargetCommand() error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorPullRequestKind, "9")
	if err != nil || item.HeadSHA != "new-head" || item.AutomergeState != repositoryMonitorAutomergeStateMergeReady || item.SkipReason != "" {
		t.Fatalf("new-head item was rewritten: %#v err=%v", item, err)
	}
	mutation, err := monitorStore.GetGitHubMutationRecord(ctx, defaultNS, mutationID)
	if err != nil || mutation.Status != repositoryMonitorRunPhaseFailed || mutation.Error != repositoryMonitorReviewSkipReasonStaleHead {
		t.Fatalf("blocked retry mutation = %#v err=%v", mutation, err)
	}
	action, err := monitorStore.GetWorkAction(ctx, defaultNS, actionID)
	if err != nil || action.Status != repositoryMonitorWorkActionStatusBlocked || action.BlockedReason != repositoryMonitorReviewSkipReasonStaleHead {
		t.Fatalf("blocked retry action = %#v err=%v", action, err)
	}
}

func TestRepositoryMonitorAutomergeTerminalizationPreservesSucceededMutation(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "automerge-preserve-success", Namespace: defaultNS}}
	command := store.CommandEvent{ID: "cmd-automerge-success", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 10, Intent: repositoryMonitorCommandIntentAutomerge, HeadSHA: "head-10"}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "10", Number: 10, State: repositoryMonitorItemStateOpen, HeadSHA: command.HeadSHA, AutomergeState: repositoryMonitorAutomergeStatePending}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	mutationID := "ghmut-" + repositoryMonitorShortHash(command.ID+"-merge")
	if err := monitorStore.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: mutationID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, Operation: "merge_pr", TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, ExternalID: "merge-sha", Status: repositoryMonitorRunPhaseSucceeded, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateGitHubMutationRecord() error = %v", err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentAutomerge)
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, DesiredAction: repositoryMonitorCommandIntentAutomerge, Status: repositoryMonitorWorkActionStatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	if err := reconciler.terminalizeRepositoryMonitorFailedCommand(ctx, monitor, command, &store.MonitorRun{ID: "run-automerge-success"}, "retry_attempts_exhausted"); err != nil {
		t.Fatalf("terminalizeRepositoryMonitorFailedCommand() error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorPullRequestKind, "10")
	if err != nil || item.State != repositoryMonitorAutomergeStateMerged || item.AutomergeState != repositoryMonitorAutomergeStateMerged || item.SkipReason != "" {
		t.Fatalf("merged item was downgraded: %#v err=%v", item, err)
	}
	mutation, err := monitorStore.GetGitHubMutationRecord(ctx, defaultNS, mutationID)
	if err != nil || mutation.Status != repositoryMonitorRunPhaseSucceeded || mutation.ExternalID != "merge-sha" || mutation.Error != "" {
		t.Fatalf("successful mutation was downgraded: %#v err=%v", mutation, err)
	}
	action, err := monitorStore.GetWorkAction(ctx, defaultNS, actionID)
	if err != nil || action.Status != repositoryMonitorWorkActionStatusSucceeded || action.Error != "" || action.CompletedAt == nil {
		t.Fatalf("successful action was not finalized: %#v err=%v", action, err)
	}
}

func TestRepositoryMonitorMergedDifferentHeadDoesNotSucceedStaleAutomerge(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "automerge-stale-merged", Namespace: defaultNS}}
	command := store.CommandEvent{ID: "cmd-automerge-old-head", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 11, Intent: repositoryMonitorCommandIntentAutomerge, HeadSHA: "old-head"}
	if err := monitorStore.UpsertMonitorItem(ctx, &store.MonitorItem{MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, ItemKey: "11", Number: 11, State: repositoryMonitorAutomergeStateMerged, HeadSHA: "new-head", AutomergeState: repositoryMonitorAutomergeStateMerged}); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	mutationID := "ghmut-" + repositoryMonitorShortHash(command.ID+"-merge")
	if err := monitorStore.CreateGitHubMutationRecord(ctx, &store.GitHubMutationRecord{ID: mutationID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, Operation: "merge_pr", TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, Status: repositoryMonitorAutomergeStatePending, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateGitHubMutationRecord() error = %v", err)
	}
	actionID := store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentAutomerge)
	if err := monitorStore.CreateWorkAction(ctx, &store.WorkAction{ID: actionID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, CommandEventID: command.ID, DesiredAction: repositoryMonitorCommandIntentAutomerge, Status: repositoryMonitorWorkActionStatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	if err := reconciler.terminalizeRepositoryMonitorFailedCommand(ctx, monitor, command, &store.MonitorRun{ID: "run-automerge-old-head"}, "retry_attempts_exhausted"); err != nil {
		t.Fatalf("terminalizeRepositoryMonitorFailedCommand() error = %v", err)
	}
	mutation, err := monitorStore.GetGitHubMutationRecord(ctx, defaultNS, mutationID)
	if err != nil || mutation.Status != repositoryMonitorRunPhaseFailed {
		t.Fatalf("stale-head mutation = %#v err=%v, want failed", mutation, err)
	}
	action, err := monitorStore.GetWorkAction(ctx, defaultNS, actionID)
	if err != nil || action.Status != repositoryMonitorWorkActionStatusFailed {
		t.Fatalf("stale-head action = %#v err=%v, want failed", action, err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, defaultNS, monitor.Name, repositoryMonitorPullRequestKind, "11")
	if err != nil || item.HeadSHA != "new-head" || item.AutomergeState != repositoryMonitorAutomergeStateMerged {
		t.Fatalf("new merged item was rewritten: %#v err=%v", item, err)
	}
}

func TestRepositoryMonitorTransientCommandRunRetriesWithBackoff(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "retry-command", Namespace: defaultNS}}
	command := store.CommandEvent{ID: "cmd-retry", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 7, Intent: "review", HeadSHA: "head-7"}
	runID := repositoryMonitorCommandRunIDFromCommand(command.ID)
	completedAt := time.Now()
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: runID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseFailed, StartedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt, Error: "[retry_scheduled] upstream unavailable"}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	handled, reset, err := reconciler.ensureNoExistingCommandRunBlocksQueue(ctx, monitor, command, runID)
	if err != nil || !handled || !reset {
		t.Fatalf("ensureNoExistingCommandRunBlocksQueue() = handled=%v reset=%v err=%v", handled, reset, err)
	}
	run, err := monitorStore.GetMonitorRun(ctx, defaultNS, runID)
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseQueued || run.CompletedAt != nil || run.Error != "" || !run.StartedAt.After(time.Now()) {
		t.Fatalf("retried run = %#v, want delayed queued state", run)
	}
	processed, requeueAfter, err := reconciler.processNextQueuedMonitorRun(ctx, monitor, "orka-agents", "orka")
	if err != nil || processed != nil || requeueAfter <= 0 {
		t.Fatalf("processNextQueuedMonitorRun() = processed=%#v requeueAfter=%s err=%v", processed, requeueAfter, err)
	}
}

func TestRepositoryMonitorRunSignalFailureRemainsRetryable(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "signal-retry", Namespace: defaultNS}}
	command := store.CommandEvent{ID: "cmd-signal", MonitorNamespace: defaultNS, MonitorName: monitor.Name, Kind: repositoryMonitorPullRequestKind, Number: 9, Intent: "review", HeadSHA: "head-9"}
	runID := repositoryMonitorCommandRunIDFromCommand(command.ID)
	completedAt := time.Now()
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: runID, MonitorNamespace: defaultNS, MonitorName: monitor.Name, TargetKind: command.Kind, TargetNumber: command.Number, TargetSHA: command.HeadSHA, CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseFailed, StartedAt: completedAt.Add(-time.Minute), CompletedAt: &completedAt, Error: "failed to signal repository monitor run: conflict"}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	handled, reset, err := reconciler.ensureNoExistingCommandRunBlocksQueue(ctx, monitor, command, runID)
	if err != nil || !handled || !reset {
		t.Fatalf("ensureNoExistingCommandRunBlocksQueue() = handled=%v reset=%v err=%v", handled, reset, err)
	}
	run, err := monitorStore.GetMonitorRun(ctx, defaultNS, runID)
	if err != nil {
		t.Fatalf("GetMonitorRun() error = %v", err)
	}
	if run.Phase != repositoryMonitorRunPhaseQueued || run.CompletedAt != nil || run.Error != "" {
		t.Fatalf("signal retry run = %#v, want queued reset", run)
	}
}

func TestRepositoryMonitorAutomergeMergeableStatePolicy(t *testing.T) {
	if !repositoryMonitorAutomergeMergeableStateCanCheckCI("clean") {
		t.Fatal("clean mergeable state should proceed to CI")
	}
	if !repositoryMonitorAutomergeMergeableStateCanCheckCI("unstable") {
		t.Fatal("unstable mergeable state should proceed to CI classification")
	}
	if repositoryMonitorAutomergeMergeableStateCanCheckCI("dirty") {
		t.Fatal("dirty mergeable state should block before CI")
	}
}

func TestRepositoryMonitorRequireGreenCIGatesReviewQueue(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/pulls/90":
			_, _ = w.Write([]byte(`{"number":90,"title":"Wait for CI","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base90","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"head":{"ref":"feature-ci","sha":"head90","repo":{"full_name":"orka-agents/orka","clone_url":"https://github.com/orka-agents/orka.git"}},"labels":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/orka-agents/orka/commits/head90/check-runs":
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"failure"}]}`))
		default:
			t.Fatalf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("require-green-ci-gate")
	monitor.Spec.Review.RequireGreenCI = true
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
	processedAt := time.Now()
	command := &store.CommandEvent{ID: "cmd-review-90", MonitorNamespace: "default", MonitorName: monitor.Name, Repo: "orka-agents/orka", Kind: repositoryMonitorPullRequestKind, Number: 90, Intent: "review", Command: "review", CommentID: "review-90", HeadSHA: "head90", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	if err := monitorStore.CreateMonitorRun(ctx, &store.MonitorRun{ID: "run-review-90", MonitorNamespace: "default", MonitorName: monitor.Name, Trigger: repositoryMonitorTriggerLabelCommand, TargetKind: repositoryMonitorPullRequestKind, TargetNumber: 90, TargetSHA: "head90", CommandEventID: command.ID, Phase: repositoryMonitorRunPhaseQueued, StartedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("CreateMonitorRun() error = %v", err)
	}
	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: monitor.Name}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	item, err := monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorPullRequestKind, "90")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.LastReviewID != "" || item.SkipReason != "ci_not_green" || item.CIState != "ci_not_green" {
		t.Fatalf("item = %#v, want no review task and ci_not_green", item)
	}
}

func TestRepositoryMonitorValidationRejectsCodexPlanner(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	pullRequestsEnabled := false
	monitor := &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: "issue-monitor", Namespace: "default"},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			Targets: corev1alpha1.RepositoryMonitorTargets{
				PullRequests: corev1alpha1.RepositoryMonitorPullRequestTarget{Enabled: &pullRequestsEnabled},
				Issues:       corev1alpha1.RepositoryMonitorIssueTarget{Enabled: true},
			},
			Agents: corev1alpha1.RepositoryMonitorAgents{Planner: &corev1alpha1.AgentReference{Name: "codex-planner"}},
		},
	}
	planner := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "codex-planner", Namespace: "default"},
		Spec:       corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCodex}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(planner).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl}
	reason, message, err := reconciler.validateRepositoryMonitorIssueReadOnlyAgents(ctx, monitor)
	if err != nil || reason != "UnsupportedPlannerAgent" || !strings.Contains(message, "use claude") {
		t.Fatalf("validation reason=%q message=%q err=%v", reason, message, err)
	}

	planningEnabled := false
	monitor.Spec.IssueWorkflow.Planning.Enabled = &planningEnabled
	reason, message, err = reconciler.validateRepositoryMonitorIssueReadOnlyAgents(ctx, monitor)
	if err != nil || reason != "" || message != "" {
		t.Fatalf("disabled planner validation reason=%q message=%q err=%v", reason, message, err)
	}
}

func TestRepositoryMonitorPlanApprovalContinuesOriginalImplementCommand(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "plan-approval", Namespace: "default"}}
	original := &store.CommandEvent{ID: "cmd-implement", MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 44, Intent: repositoryMonitorCommandIntentImplement, Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, original); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	action := &store.WorkAction{ID: store.RepositoryMonitorWorkActionID(original.ID, "implement"), MonitorNamespace: "default", MonitorName: monitor.Name, CommandEventID: original.ID, DesiredAction: "implement", Status: repositoryMonitorWorkActionStatusRunning, CreatedAt: time.Now()}
	if err := monitorStore.CreateWorkAction(ctx, action); err != nil {
		t.Fatalf("CreateWorkAction() error = %v", err)
	}
	fallback := &store.CommandEvent{ID: "cmd-approve", Intent: "approve_plan"}
	plan := &store.ActionRecord{CommandEventID: original.ID}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	got, err := reconciler.repositoryMonitorImplementationCommandForPlan(ctx, monitor, fallback, plan)
	if err != nil {
		t.Fatalf("repositoryMonitorImplementationCommandForPlan() error = %v", err)
	}
	if got.ID != original.ID || got.Intent != repositoryMonitorCommandIntentImplement {
		t.Fatalf("implementation command = %#v, want original implement command", got)
	}
	if intent := reconciler.repositoryMonitorCommandIntentForID(ctx, monitor, original.ID, "approve_plan"); intent != repositoryMonitorCommandIntentImplement {
		t.Fatalf("resolved intent=%q, want implement", intent)
	}
	action.Phase = repositoryMonitorIssuePhaseImplementationQueued
	action.TaskName = "existing-implementation-task"
	if err := monitorStore.UpdateWorkAction(ctx, action); err != nil {
		t.Fatalf("UpdateWorkAction(started) error = %v", err)
	}
	got, err = reconciler.repositoryMonitorImplementationCommandForPlan(ctx, monitor, fallback, plan)
	if err != nil {
		t.Fatalf("repositoryMonitorImplementationCommandForPlan(started) error = %v", err)
	}
	if got != nil {
		t.Fatalf("started implementation was requeued with command %#v", got)
	}
	action.Phase = repositoryMonitorIssuePhaseApprovalRequired
	action.TaskName = "plan-task"
	action.Status = repositoryMonitorWorkActionStatusCancelled
	if err := monitorStore.UpdateWorkAction(ctx, action); err != nil {
		t.Fatalf("UpdateWorkAction(cancelled) error = %v", err)
	}
	got, err = reconciler.repositoryMonitorImplementationCommandForPlan(ctx, monitor, fallback, plan)
	if err != nil {
		t.Fatalf("repositoryMonitorImplementationCommandForPlan(cancelled) error = %v", err)
	}
	if got.ID != fallback.ID {
		t.Fatalf("cancelled original command was reused: %#v", got)
	}
}

func TestRepositoryMonitorPlanOnlyApprovalStaysApproved(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	implementationEnabled := false
	monitor := &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: "plan-only", Namespace: "default"},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			IssueWorkflow: corev1alpha1.RepositoryMonitorIssueWorkflowSpec{
				Implementation: corev1alpha1.RepositoryMonitorIssueImplementationSpec{Enabled: &implementationEnabled},
			},
		},
	}
	item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "44", Number: 44, State: "open", SnapshotDigest: "sha256:test", WorkflowPhase: repositoryMonitorIssuePhaseApprovalRequired}
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	plan := &store.ActionRecord{ID: "plan-44", MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 44, ActionKind: repositoryMonitorIssueActionPlan, SnapshotDigest: item.SnapshotDigest, CommandEventID: "cmd-plan", Verdict: repositoryMonitorIssueVerdictReady, PayloadJSON: `{"status":"ready","risk":"low","categories":[],"requiresHumanApproval":true}`, CreatedAt: time.Now()}
	if err := monitorStore.CreateActionRecord(ctx, plan); err != nil {
		t.Fatalf("CreateActionRecord() error = %v", err)
	}
	command := &store.CommandEvent{ID: "cmd-approve", MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 44, Intent: "approve_plan", Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	run := &store.MonitorRun{ID: "run-approve", CommandEventID: command.ID}
	reconciler := &RepositoryMonitorReconciler{Store: monitorStore}
	if _, err := reconciler.processIssueCommandRun(ctx, monitor, run, item, "orka-agents", "orka"); err != nil {
		t.Fatalf("processIssueCommandRun() error = %v", err)
	}
	got, err := monitorStore.GetMonitorItem(ctx, "default", monitor.Name, repositoryMonitorIssueKind, "44")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if got.WorkflowPhase != repositoryMonitorIssuePhaseApproved || got.SkipReason != "" {
		t.Fatalf("plan-only item = %#v, want approved", got)
	}
	jobs, _, err := monitorStore.ListImplementationJobs(ctx, store.ImplementationJobFilter{Namespace: "default", MonitorName: monitor.Name, IssueNumber: 44, Limit: 10})
	if err != nil {
		t.Fatalf("ListImplementationJobs() error = %v", err)
	}
	if len(jobs) != 0 {
		t.Fatalf("implementation jobs = %#v, want none", jobs)
	}
}

func TestRepositoryMonitorPlanResultRejectsMalformedCategories(t *testing.T) {
	body := map[string]any{
		"status":                "ready",
		"risk":                  "low",
		"categories":            []any{map[string]any{"name": "security"}},
		"requiresHumanApproval": false,
	}
	if !repositoryMonitorIssueActionMissingRequiredResult(repositoryMonitorIssueActionPlan, body) {
		t.Fatal("malformed category element was accepted")
	}
}

func TestRepositoryMonitorFailedTaskSummaryDoesNotExposeStatusMessage(t *testing.T) {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "plan-task"}, Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseFailed, Message: "mount secret production-token failed"}}
	summary := repositoryMonitorIssueFailedTaskSummary(repositoryMonitorIssueActionPlan, task)
	if strings.Contains(summary, "production-token") || strings.Contains(summary, "mount secret") {
		t.Fatalf("public summary leaked task status message: %q", summary)
	}
}

func TestRepositoryMonitorJSONExtractionBoundsDecodeAttempts(t *testing.T) {
	raw := strings.Repeat("{", repositoryMonitorIssueJSONDecodeAttempts+1) + `{"status":"ready"}`
	if got := repositoryMonitorFirstJSONObject(raw); got != "" {
		t.Fatalf("extracted JSON after decode-attempt limit: %q", got)
	}
}

func TestRepositoryMonitorValidationRejectsCopilotImplementer(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	pullRequestsEnabled := false
	monitor := &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: "issue-monitor", Namespace: "default"},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			Targets: corev1alpha1.RepositoryMonitorTargets{
				PullRequests: corev1alpha1.RepositoryMonitorPullRequestTarget{Enabled: &pullRequestsEnabled},
				Issues:       corev1alpha1.RepositoryMonitorIssueTarget{Enabled: true},
			},
			Agents: corev1alpha1.RepositoryMonitorAgents{Implementer: &corev1alpha1.AgentReference{Name: "copilot-implementer"}},
		},
	}
	implementer := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "copilot-implementer", Namespace: "default"},
		Spec:       corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeCopilot}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(implementer).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl}
	reason, message, err := reconciler.validateRepositoryMonitorImplementerAgent(ctx, monitor)
	if err != nil || reason != repositoryMonitorReasonUnsupportedImplementerAgent || !strings.Contains(message, "cannot use copilot") {
		t.Fatalf("validation reason=%q message=%q err=%v", reason, message, err)
	}
}

func TestRepositoryMonitorValidationRejectsFoundryImplementer(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	pullRequestsEnabled := false
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "foundry-implementer", Namespace: defaultNS}}
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	agent := repositoryMonitorControllerTestAgent("implementer", corev1alpha1.AgentRuntimeClaude, "foundry-runtime")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "foundry-runtime", Namespace: defaultNS}, Data: map[string][]byte{
		workerenv.AnthropicAPIKey: []byte("direct-value"),
		"CLAUDE_CODE_USE_FOUNDRY": []byte("1"),
	}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, secret).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl}
	reason, message, err := reconciler.validateRepositoryMonitorImplementerAgent(ctx, monitor)
	if err != nil || reason != repositoryMonitorReasonImplementerAuthInvalid || !strings.Contains(message, "cannot use Azure AI Foundry") {
		t.Fatalf("validation reason=%q message=%q err=%v", reason, message, err)
	}
}

func TestRepositoryMonitorMutableImplementerCredentialCreatesImmutableTaskSnapshot(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	pullRequestsEnabled := false
	monitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "mutable-implementer", Namespace: defaultNS}}
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	monitor.Spec.Agents.Implementer = &corev1alpha1.AgentReference{Name: "implementer"}
	agent := repositoryMonitorControllerTestAgent("implementer", corev1alpha1.AgentRuntimeCodex, "mutable-runtime")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "mutable-runtime", Namespace: defaultNS}, Data: map[string][]byte{workerenv.OpenAIAPIKey: []byte("credential")}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(agent, secret).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme}
	reason, message, err := reconciler.validateRepositoryMonitorImplementerAgent(ctx, monitor)
	if err != nil || reason != "" || message != "" {
		t.Fatalf("validation reason=%q message=%q err=%v", reason, message, err)
	}
	binding, err := reconciler.repositoryMonitorImplementationRuntimeCredentialBinding(ctx, monitor, "impl-mutable-source", monitor.Spec.Agents.Implementer)
	if err != nil {
		t.Fatalf("repositoryMonitorImplementationRuntimeCredentialBinding() error = %v", err)
	}
	if binding.authRef == nil || binding.authRef.Name == secret.Name {
		t.Fatalf("binding = %#v, want per-task snapshot", binding)
	}
	var snapshot corev1.Secret
	if err := cl.Get(ctx, types.NamespacedName{Namespace: defaultNS, Name: binding.authRef.Name}, &snapshot); err != nil {
		t.Fatalf("Get snapshot error = %v", err)
	}
	if snapshot.Immutable == nil || !*snapshot.Immutable || string(snapshot.Data[workerenv.OpenAIAPIKey]) != "credential" || !metav1.IsControlledBy(&snapshot, monitor) {
		t.Fatalf("snapshot = %#v, want immutable monitor-owned copy", snapshot)
	}
}

func TestRepositoryMonitorValidationRejectsRuntimeRefImplementer(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	pullRequestsEnabled := false
	monitor := &corev1alpha1.RepositoryMonitor{
		ObjectMeta: metav1.ObjectMeta{Name: "issue-monitor", Namespace: "default"},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			Targets: corev1alpha1.RepositoryMonitorTargets{
				PullRequests: corev1alpha1.RepositoryMonitorPullRequestTarget{Enabled: &pullRequestsEnabled},
				Issues:       corev1alpha1.RepositoryMonitorIssueTarget{Enabled: true},
			},
			Agents: corev1alpha1.RepositoryMonitorAgents{Implementer: &corev1alpha1.AgentReference{Name: "custom-implementer"}},
		},
	}
	implementer := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: "custom-implementer", Namespace: "default"},
		Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{
			RuntimeRef: &corev1alpha1.AgentRuntimeReference{Name: "external-runtime"},
		}},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(implementer).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl}
	reason, message, err := reconciler.validateRepositoryMonitorImplementerAgent(ctx, monitor)
	if err != nil || reason != repositoryMonitorReasonUnsupportedImplementerAgent || !strings.Contains(message, "cannot use runtimeRef") {
		t.Fatalf("validation reason=%q message=%q err=%v", reason, message, err)
	}
}

func TestRepositoryMonitorValidationRejectsRuntimeRefReadOnlyAgents(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	externalReviewer := repositoryMonitorControllerTestAgent("external-reviewer", corev1alpha1.AgentRuntimeClaude, repositoryMonitorTestReviewerSecret)
	externalReviewer.Spec.Runtime.RuntimeRef = &corev1alpha1.AgentRuntimeReference{Name: "external-runtime"}
	externalPlanner := repositoryMonitorControllerTestAgent("external-planner", corev1alpha1.AgentRuntimeClaude, repositoryMonitorTestReviewerSecret)
	externalPlanner.Spec.Runtime.RuntimeRef = &corev1alpha1.AgentRuntimeReference{Name: "external-runtime"}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(externalReviewer, externalPlanner).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl}

	reviewerMonitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "review-monitor", Namespace: "default"}}
	reviewerMonitor.Spec.Agents.Reviewer = &corev1alpha1.AgentReference{Name: externalReviewer.Name}
	if reason, message, err := reconciler.validateRepositoryMonitorReviewerAgent(ctx, reviewerMonitor); err != nil || reason != "UnsupportedReviewerAgent" || !strings.Contains(message, "cannot use runtimeRef") {
		t.Fatalf("reviewer validation reason=%q message=%q err=%v", reason, message, err)
	}

	pullRequestsEnabled := false
	plannerMonitor := &corev1alpha1.RepositoryMonitor{ObjectMeta: metav1.ObjectMeta{Name: "plan-monitor", Namespace: "default"}}
	plannerMonitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	plannerMonitor.Spec.Targets.Issues.Enabled = true
	plannerMonitor.Spec.Agents.Planner = &corev1alpha1.AgentReference{Name: externalPlanner.Name}
	if reason, message, err := reconciler.validateRepositoryMonitorIssueReadOnlyAgents(ctx, plannerMonitor); err != nil || reason != "UnsupportedPlannerAgent" || !strings.Contains(message, "cannot use runtimeRef") {
		t.Fatalf("planner validation reason=%q message=%q err=%v", reason, message, err)
	}
}

func TestRepositoryMonitorIssueActionStatusCommentPolicy(t *testing.T) {
	if !repositoryMonitorIssueActionUpdatesStatusComment(repositoryMonitorIssueActionImplementation, repositoryMonitorIssuePhaseBlocked) {
		t.Fatal("blocked implementation should update the issue status comment")
	}
	if repositoryMonitorIssueActionUpdatesStatusComment(repositoryMonitorIssueActionImplementation, repositoryMonitorIssuePhaseMutationQueued) {
		t.Fatal("successful intermediate implementation should wait for mutation status")
	}
	if !repositoryMonitorIssueActionUpdatesStatusComment(repositoryMonitorIssueActionPlan, repositoryMonitorIssuePhaseApproved) {
		t.Fatal("plan completion should update the issue status comment")
	}
}

func TestRepositoryMonitorNeedsHumanPlanStaysApprovalRequired(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/repos/orka-agents/orka/issues/44/comments" {
			t.Fatalf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":4401,"html_url":"https://github.com/orka-agents/orka/issues/44#issuecomment-4401"}`))
	}))
	t.Cleanup(server.Close)

	monitor, gitConfig := repositoryMonitorInventoryTestObjects("needs-human-plan")
	pullRequestsEnabled := false
	monitor.Spec.Targets.PullRequests.Enabled = &pullRequestsEnabled
	monitor.Spec.Targets.Issues.Enabled = true
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gitConfig).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	command := &store.CommandEvent{ID: "cmd-implement-needs-human", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: 44, Intent: repositoryMonitorCommandIntentImplement, Status: "accepted", CreatedAt: time.Now()}
	if err := monitorStore.CreateCommandEvent(ctx, command); err != nil {
		t.Fatalf("CreateCommandEvent() error = %v", err)
	}
	item := &store.MonitorItem{MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "44", Number: 44, State: repositoryMonitorItemStateOpen, SnapshotDigest: "sha256:test", WorkflowPhase: repositoryMonitorIssuePhasePlanning, LastCommandIntent: command.Intent}
	record := &store.ActionRecord{ID: "act-needs-human-plan", MonitorNamespace: monitor.Namespace, MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, Number: item.Number, ActionKind: repositoryMonitorIssueActionPlan, SnapshotDigest: item.SnapshotDigest, TaskName: "plan-needs-human", CommandEventID: command.ID, Verdict: repositoryMonitorReviewVerdictNeedsHuman, Summary: "Plan requires approval.", PayloadJSON: `{"schemaVersion":"orka.issuePlan.v1","status":"needs_human","risk":"high","categories":["security"],"requiresHumanApproval":true,"summary":"Plan requires approval."}`, CreatedAt: time.Now()}
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: record.TaskName, Namespace: monitor.Namespace, Annotations: map[string]string{labels.AnnotationMonitorRunID: "run-needs-human"}}}

	handled, err := reconciler.applyIssueActionRecord(ctx, monitor, item, record, task)
	if err != nil || !handled {
		t.Fatalf("applyIssueActionRecord() handled=%v err=%v", handled, err)
	}
	if item.WorkflowPhase != repositoryMonitorIssuePhaseApprovalRequired || item.SkipReason != "" {
		t.Fatalf("item after needs-human plan = %#v, want approval_required without terminal block", item)
	}
	planAction, err := monitorStore.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, "plan"))
	if err != nil {
		t.Fatalf("GetWorkAction(plan) error = %v", err)
	}
	if planAction.Status != repositoryMonitorWorkActionStatusSucceeded {
		t.Fatalf("plan action = %#v, want succeeded", planAction)
	}
	implementAction, err := monitorStore.GetWorkAction(ctx, monitor.Namespace, store.RepositoryMonitorWorkActionID(command.ID, repositoryMonitorCommandIntentImplement))
	if err != nil {
		t.Fatalf("GetWorkAction(implement) error = %v", err)
	}
	if implementAction.Status != repositoryMonitorWorkActionStatusRunning || implementAction.Phase != repositoryMonitorIssuePhaseApprovalRequired || implementAction.CompletedAt != nil {
		t.Fatalf("implement action = %#v, want running approval prerequisite", implementAction)
	}
}

func TestRepositoryMonitorIssueStatusCommentRecreatesDeletedComment(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("corev1 AddToScheme() error = %v", err)
	}
	monitor, gitSecret := repositoryMonitorInventoryTestObjects("comment-recovery")
	patchCalls := 0
	postCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.Method {
		case http.MethodPatch:
			patchCalls++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found"}`))
		case http.MethodPost:
			postCalls++
			_, _ = w.Write([]byte(`{"id":9001,"html_url":"https://github.com/orka-agents/orka/issues/44#issuecomment-9001"}`))
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	t.Cleanup(server.Close)
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(gitSecret).Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Store: monitorStore, GitHubAPIBaseURL: server.URL}
	item := &store.MonitorItem{MonitorNamespace: "default", MonitorName: monitor.Name, Kind: repositoryMonitorIssueKind, ItemKey: "44", Number: 44, State: "open", SnapshotDigest: "sha256:test", WorkflowPhase: repositoryMonitorIssuePhaseApprovalRequired, StatusCommentID: "deleted", StatusCommentURL: "https://example.test/deleted"}
	if err := monitorStore.UpsertMonitorItem(ctx, item); err != nil {
		t.Fatalf("UpsertMonitorItem() error = %v", err)
	}
	record := &store.ActionRecord{ID: "plan-44", CommandEventID: "cmd-plan", ActionKind: repositoryMonitorIssueActionPlan, Summary: "Plan ready", PayloadJSON: `{"summary":"Plan ready"}`}
	if err := reconciler.upsertRepositoryMonitorIssueStatusComment(ctx, monitor, item, record); err != nil {
		t.Fatalf("upsertRepositoryMonitorIssueStatusComment() error = %v", err)
	}
	if patchCalls != 1 || postCalls != 1 || item.StatusCommentID != "9001" {
		t.Fatalf("patch=%d post=%d item=%#v, want one recovery create", patchCalls, postCalls, item)
	}
}
