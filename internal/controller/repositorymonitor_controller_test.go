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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/workerenv"
)

const (
	repositoryMonitorTestDefaultBranch  = "main"
	repositoryMonitorTestRepoURL        = "https://github.com/sozercan/orka"
	repositoryMonitorTestReviewerSecret = "reviewer-credentials"
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
			RepoURL: "https://github.com/sozercan/orka",
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
	if record.Owner != "sozercan" || record.Repository != "orka" || record.Branch != repositoryMonitorTestDefaultBranch {
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
			RepoURL: "https://github.com/sozercan/orka",
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
			RepoURL:  "https://github.com/sozercan/orka",
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
			RepoURL:  "https://github.com/sozercan/orka",
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
			RepoURL:  "https://github.com/sozercan/orka",
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
			RepoURL: "https://github.com/sozercan/orka",
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
			RepoURL: "https://github.com/sozercan/orka",
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
			RepoURL: "https://github.com/sozercan/orka",
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
			RepoURL: "https://github.com/sozercan/orka",
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
			RepoURL: "https://github.com/sozercan/orka",
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
			RepoURL: "https://github.com/sozercan/orka",
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
		{"number":1,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"ready","sha":"sha1","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]},
		{"number":2,"title":"Expired review","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base2","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"expired","sha":"sha2","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]}
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
			RepoURL: "https://github.com/sozercan/orka",
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
			RepoURL: "https://github.com/sozercan/orka",
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
		{"number":1,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"feature","sha":"sha1","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]}
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
			RepoURL: "https://github.com/sozercan/orka",
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
		HeadRepo:    "sozercan/orka",
		HeadRepoURL: "https://github.com/sozercan/orka.git",
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
				labels.LabelGitHubRepository:  labels.SelectorValue("sozercan/orka"),
				labels.LabelGitHubTarget:      labels.SelectorValue(repositoryMonitorPullRequestKind),
				labels.LabelGitHubNumber:      labels.SelectorValue("1"),
			},
			Annotations: map[string]string{
				labels.AnnotationRepositoryMonitorName:  "collision",
				labels.AnnotationMonitorRunID:           "run-collision",
				labels.AnnotationMonitorItemKind:        repositoryMonitorPullRequestKind,
				labels.AnnotationMonitorItemNumber:      "1",
				labels.AnnotationMonitorHeadSHA:         "sha1",
				labels.AnnotationGitHubRepository:       "sozercan/orka",
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
			RepoURL: "https://github.com/sozercan/orka",
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
		HeadRepo:    "sozercan/orka",
		HeadRepoURL: "https://github.com/sozercan/orka.git",
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

	taskName, created, err := reconciler.createRepositoryMonitorReviewTask(ctx, monitor, run, "sozercan", "orka", pr)
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

	if err := validateRepositoryMonitorReviewTaskMatchesExpected(&existing, expected, monitor, run, "sozercan/orka", pr); err != nil {
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
			RepoURL: "https://github.com/sozercan/orka",
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
		{"number":7,"title":"Fork PR","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"forker"},"base":{"ref":"main","sha":"base7","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"feature","sha":"fork-sha","repo":{"full_name":"forker/orka","clone_url":"https://github.com/forker/orka.git"}},"labels":[]}
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
		{"number":9,"title":"Same repo SSH monitor","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base9","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"feature","sha":"ssh-head-sha","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("ssh-same-repo")
	monitor.Spec.RepoURL = "git@github.com:sozercan/orka.git"
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
		{"number":8,"title":"Missing head repo","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"forker"},"base":{"ref":"main","sha":"base8","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"feature","sha":"unknown-head-sha"},"labels":[]}
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
			RepoURL: "https://github.com/sozercan/orka",
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
		{Number: 7, State: repositoryMonitorItemStateOpen, LastVerdict: repositoryMonitorReviewVerdictPassed},
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
	if counts.openPullRequests != 8 || counts.pendingReviews != 1 || counts.blockedItems != 6 {
		t.Fatalf("counts = %#v, want open=8 pending=1 blocked=6", counts)
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
			RepoURL: "https://github.com/sozercan/orka",
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
		TargetKind:       "issue",
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
			RepoURL: "https://github.com/sozercan/orka",
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
		TargetKind:       "issue",
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
		{"number":1,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"feature","sha":"sha1","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]},
		{"number":2,"title":"Draft","state":"open","draft":true,"mergeable_state":"unknown","user":{"login":"bob"},"base":{"ref":"main","sha":"base2","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"draft","sha":"sha2","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]},
		{"number":3,"title":"Blocked","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"cara"},"base":{"ref":"main","sha":"base3","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"blocked","sha":"sha3","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[{"name":"orka:human-review"}]},
		{"number":4,"title":"Over limit","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"dev"},"base":{"ref":"main","sha":"base4","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"second","sha":"sha4","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]},
		{"number":5,"title":"Already reviewed","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"erin"},"base":{"ref":"main","sha":"base5","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"reviewed","sha":"sha5","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]},
		{"number":6,"title":"Backport","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"frank"},"base":{"ref":"release","sha":"base6","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"backport","sha":"sha6","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]}
	]`)
}

func newRepositoryMonitorPullRequestInventoryServerWithBody(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return newRepositoryMonitorPullRequestInventoryServerWithAuth(t, body, "Bearer test-token")
}

func newRepositoryMonitorPullRequestInventoryServerWithoutAuth(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return newRepositoryMonitorPullRequestInventoryServerWithAuth(t, body, "")
}

func newRepositoryMonitorSinglePullRequestServerWithBody(t *testing.T, number int64, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wantPath := fmt.Sprintf("/repos/sozercan/orka/pulls/%d", number)
		if r.URL.Path != wantPath {
			t.Fatalf("request path = %q, want single pull request path %q", r.URL.Path, wantPath)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("Authorization header = %q, want %q", got, "Bearer test-token")
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
}

func newRepositoryMonitorPullRequestInventoryServerWithAuth(t *testing.T, body, wantAuth string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/sozercan/orka/pulls" {
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

func repositoryMonitorInventoryTestObjects(name string) (*corev1alpha1.RepositoryMonitor, *corev1.Secret) {
	monitor := &corev1alpha1.RepositoryMonitor{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor"},
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID("uid-" + name),
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL:      "https://github.com/sozercan/orka",
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
			RepoURL: "https://github.com/sozercan/orka",
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
				labels.AnnotationGitHubRepository:      "sozercan/orka",
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

	payload := map[string]any{
		"schemaVersion": repositoryMonitorReviewSchemaVersion,
		"repo":          "sozercan/orka",
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
	if task.Spec.AgentRuntime.Workspace.GitRepo != repositoryMonitorTestRepoURL || task.Spec.AgentRuntime.Workspace.Ref != "sha1" || task.Spec.AgentRuntime.Workspace.PRBaseBranch != "main" {
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
			RepoURL: "https://token@github.com/sozercan/orka",
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
			RepoURL: "https://github.com/sozercan/orka",
			Targets: corev1alpha1.RepositoryMonitorTargets{
				PullRequests: corev1alpha1.RepositoryMonitorPullRequestTarget{Enabled: &pullRequestsEnabled},
				Issues:       corev1alpha1.RepositoryMonitorIssueTarget{Enabled: true},
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

func TestRepositoryMonitorReconcileRejectsRequireGreenCIWithoutPersistingMetadata(t *testing.T) {
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
			Name:      "require-green-ci",
			Namespace: "default",
		},
		Spec: corev1alpha1.RepositoryMonitorSpec{
			RepoURL: "https://github.com/sozercan/orka",
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
			Review: corev1alpha1.RepositoryMonitorReviewSpec{RequireGreenCI: true},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore}

	if _, err := reconciler.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "default", Name: "require-green-ci"}}); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if _, err := monitorStore.GetRepositoryMonitor(ctx, "default", "require-green-ci"); err != store.ErrNotFound {
		t.Fatalf("GetRepositoryMonitor() error = %v, want ErrNotFound", err)
	}
	var current corev1alpha1.RepositoryMonitor
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "default", Name: "require-green-ci"}, &current); err != nil {
		t.Fatalf("Get monitor() error = %v", err)
	}
	if current.Status.Phase != repositoryMonitorPhaseError {
		t.Fatalf("phase = %q, want %q", current.Status.Phase, repositoryMonitorPhaseError)
	}
	if len(current.Status.Conditions) != 1 || current.Status.Conditions[0].Reason != "UnsupportedTarget" {
		t.Fatalf("conditions = %#v, want UnsupportedTarget", current.Status.Conditions)
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
			RepoURL: "https://github.com/sozercan/orka",
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
				repositoryMonitorControllerTestAgent("no-secret", "default", corev1alpha1.AgentRuntimeClaude, ""),
			},
			reason: repositoryMonitorReasonReviewerCredentialsInvalid,
		},
		{
			name:     "secret without auth key",
			reviewer: "bad-secret-reviewer",
			objects: []crclient.Object{
				repositoryMonitorControllerTestAgent("bad-secret-reviewer", "default", corev1alpha1.AgentRuntimeClaude, "bad-reviewer-secret"),
				repositoryMonitorControllerTestSecret("bad-reviewer-secret", "default", map[string][]byte{
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
					RepoURL: "https://github.com/sozercan/orka",
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
			RepoURL:      "https://github.com/sozercan/orka",
			GitSecretRef: &corev1.LocalObjectReference{Name: "bad-git-secret"},
			Agents: corev1alpha1.RepositoryMonitorAgents{
				Reviewer: &corev1alpha1.AgentReference{Name: "reviewer"},
			},
		},
	}
	secret := repositoryMonitorControllerTestSecret("bad-git-secret", "default", map[string][]byte{
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
			RepoURL: "https://github.com/sozercan/orka",
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
		repositoryMonitorControllerTestAgent("reviewer", "default", corev1alpha1.AgentRuntimeClaude, repositoryMonitorTestReviewerSecret),
		repositoryMonitorControllerTestSecret(repositoryMonitorTestReviewerSecret, "default", map[string][]byte{
			workerenv.AnthropicAPIKey: []byte("anthropic-key"),
		}),
	}
	return append(defaults, objects...)
}

func repositoryMonitorControllerTestAgent(name, namespace string, runtimeType corev1alpha1.AgentRuntimeType, secretName string) *corev1alpha1.Agent {
	agent := &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: corev1alpha1.AgentSpec{
			Runtime: &corev1alpha1.AgentCLIRuntime{Type: runtimeType},
		},
	}
	if secretName != "" {
		agent.Spec.SecretRef = &corev1.LocalObjectReference{Name: secretName}
	}
	return agent
}

func repositoryMonitorControllerTestSecret(name, namespace string, data map[string][]byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Data:       data,
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
