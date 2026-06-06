package controller

import (
	"context"
	"encoding/json"
	"errors"
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
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
)

const repositoryMonitorTestDefaultBranch = "main"

func TestRepositoryMonitorReconcileRecordsMetadataAndStatus(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
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
		WithObjects(monitor).
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
			WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor, secret).
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
	}); err != nil {
		t.Fatalf("UpsertMonitorItem(existing) error = %v", err)
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
	if run.Phase != repositoryMonitorRunPhaseSucceeded || run.SelectedCount != 1 || run.SkippedCount != 4 {
		t.Fatalf("run = %#v, want succeeded with 1 selected and 4 skipped", run)
	}

	assertRepositoryMonitorInventoryItems(t, ctx, monitorStore)
	assertRepositoryMonitorInventoryEvents(t, ctx, monitorStore)

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

func TestRepositoryMonitorReconcileSkipsPendingReviewWithoutConsumingCapacity(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
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
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(monitor).
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

func TestRepositoryMonitorReconcileProcessesPublicInventoryWithoutGitSecret(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
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
		WithObjects(monitor).
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
		WithObjects(monitor, secret).
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
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":2,"title":"Targeted","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base2"},"head":{"ref":"targeted","sha":"sha2"},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("targeted-status")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(monitor, secret).
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

func TestRepositoryMonitorReconcileStaleExactEventDoesNotRewriteCurrentPullRequest(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("core AddToScheme() error = %v", err)
	}

	server := newRepositoryMonitorPullRequestInventoryServerWithBody(t, `[
		{"number":2,"title":"Current head","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"bob"},"base":{"ref":"main","sha":"base2"},"head":{"ref":"targeted","sha":"new-sha"},"labels":[]}
	]`)
	t.Cleanup(server.Close)

	monitor, secret := repositoryMonitorInventoryTestObjects("stale-exact-event")
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(monitor, secret).
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
		WithObjects(monitor, secret).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		{"number":1,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base1"},"head":{"ref":"feature","sha":"sha1"},"labels":[]},
		{"number":2,"title":"Draft","state":"open","draft":true,"mergeable_state":"unknown","user":{"login":"bob"},"base":{"ref":"main","sha":"base2"},"head":{"ref":"draft","sha":"sha2"},"labels":[]},
		{"number":3,"title":"Blocked","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"cara"},"base":{"ref":"main","sha":"base3"},"head":{"ref":"blocked","sha":"sha3"},"labels":[{"name":"orka:human-review"}]},
		{"number":4,"title":"Over limit","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"dev"},"base":{"ref":"main","sha":"base4"},"head":{"ref":"second","sha":"sha4"},"labels":[]},
		{"number":5,"title":"Already reviewed","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"erin"},"base":{"ref":"main","sha":"base5"},"head":{"ref":"reviewed","sha":"sha5"},"labels":[]},
		{"number":6,"title":"Backport","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"frank"},"base":{"ref":"release","sha":"base6"},"head":{"ref":"backport","sha":"sha6"},"labels":[]}
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

func assertRepositoryMonitorInventoryItems(t *testing.T, ctx context.Context, monitorStore store.RepositoryMonitorStore) {
	t.Helper()
	wantReasons := map[string]string{
		"1": "",
		"2": "draft",
		"3": "blocked_label",
		"4": "over_limit",
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
	if eventCounts["item_selected"] != 1 || eventCounts["item_skipped"] != 4 || eventCounts["run_succeeded"] != 1 {
		data, _ := json.Marshal(events)
		t.Fatalf("events = %s, want selected/skipped/run_succeeded counts", data)
	}
}

func TestRepositoryMonitorReconcileRejectsInvalidRepoURLWithoutPersistingMetadata(t *testing.T) {
	ctx := context.Background()
	monitorStore := setupControllerSQLiteStore(t)
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme() error = %v", err)
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
		WithObjects(monitor).
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
