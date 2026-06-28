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
	"github.com/sozercan/orka/workers/common"
)

const (
	repositoryMonitorTestDefaultBranch  = "main"
	repositoryMonitorTestRepoURL        = "https://github.com/sozercan/orka"
	repositoryMonitorTestReviewerSecret = "reviewer-credentials"
	repositoryMonitorTestHeadSHA        = "sha1"
)

func repositoryMonitorTestBearerHeader() string {
	return strings.Join([]string{"Bearer", "test" + "-" + "token"}, " ")
}

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
	return newRepositoryMonitorPullRequestInventoryServerWithAuth(t, body, repositoryMonitorTestBearerHeader())
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
	return repositoryMonitorReviewResultEnvelopeWith(t, prNumber, headSHA, verdict, nil)
}

func repositoryMonitorReviewResultEnvelopeWith(t *testing.T, prNumber int64, headSHA, verdict string, mutate func(map[string]any)) []byte {
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
		cfg.PostBody = `{"id":123,"html_url":"https://github.com/sozercan/orka/pull/1#pullrequestreview-123"}`
	}
	testServer := &repositoryMonitorPublishTestServer{}
	testServer.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != repositoryMonitorTestBearerHeader() {
			t.Fatalf("Authorization header = %q, want %q", got, repositoryMonitorTestBearerHeader())
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sozercan/orka/pulls/1":
			_, _ = w.Write([]byte(repositoryMonitorPublishPullBody(cfg)))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sozercan/orka/pulls/1/reviews":
			_, _ = w.Write([]byte(cfg.ReviewsBody))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sozercan/orka/pulls/1/files":
			_, _ = w.Write([]byte(cfg.FilesBody))
		case r.Method == http.MethodPost && r.URL.Path == "/repos/sozercan/orka/pulls/1/reviews":
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
				"full_name": "sozercan/orka",
				"clone_url": "https://github.com/sozercan/orka.git",
			},
		},
		"head": map[string]any{
			"ref": "feature",
			"sha": cfg.HeadSHA,
			"repo": map[string]any{
				"full_name": "sozercan/orka",
				"clone_url": "https://github.com/sozercan/orka.git",
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
		if r.URL.Path != "/repos/sozercan/orka/issues" {
			t.Fatalf("request path = %q, want issue inventory path", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != repositoryMonitorTestBearerHeader() {
			t.Fatalf("Authorization header = %q, want bearer token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[
			{"number":11,"title":"Open issue","body":"Implement this","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/sozercan/orka/issues/11","user":{"login":"alice"},"labels":[{"name":"bug"},{"name":"orka:plan"}]},
			{"number":12,"title":"PR-shaped issue","body":"","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/sozercan/orka/pull/12","user":{"login":"bob"},"pull_request":{"html_url":"https://github.com/sozercan/orka/pull/12"},"labels":[]}
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
		if r.URL.Path != "/repos/sozercan/orka/issues/22" {
			t.Fatalf("request path = %q, want targeted issue path", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"number":22,"title":"Needs triage","body":"Classify me","state":"open","updated_at":"2026-06-01T00:00:00Z","html_url":"https://github.com/sozercan/orka/issues/22","user":{"login":"alice"},"labels":[{"name":"bug"}]}`))
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
	command := &store.CommandEvent{ID: "cmd-triage-22", MonitorNamespace: "default", MonitorName: "issue-triage", Repo: "sozercan/orka", Kind: repositoryMonitorIssueKind, Number: 22, Intent: "triage", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
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
	result := fmt.Appendf(nil, `{"schemaVersion":"orka.issueTriage.v1","repo":"sozercan/orka","issueNumber":22,"snapshotDigest":%q,"verdict":"actionable","confidence":"high","category":"bug","priority":"P2","recommendedLane":"research_then_plan","risk":"medium","summary":"Ready to research."}`, item.SnapshotDigest)
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
	server := newRepositoryMonitorSinglePullRequestServerWithBody(t, 31, `{"number":31,"title":"Fix me","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base31","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"feature-fix","sha":"head31","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]}`)
	t.Cleanup(server.Close)
	monitor, secret := repositoryMonitorInventoryTestObjects("pr-fix")
	monitor.Spec.Agents.Repairer = &corev1alpha1.AgentReference{Name: "repairer"}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(repositoryMonitorControllerObjects(monitor, secret)...).
		Build()
	reconciler := &RepositoryMonitorReconciler{Client: cl, Scheme: scheme, Store: monitorStore, ResultStore: monitorStore, GitHubAPIBaseURL: server.URL}
	processedAt := time.Now()
	command := &store.CommandEvent{ID: "cmd-fix-31", MonitorNamespace: "default", MonitorName: "pr-fix", Repo: "sozercan/orka", Kind: repositoryMonitorPullRequestKind, Number: 31, Intent: "fix", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
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
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != repositoryMonitorTestBearerHeader() {
			t.Fatalf("Authorization header = %q, want %q", got, repositoryMonitorTestBearerHeader())
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sozercan/orka/pulls/41":
			_, _ = w.Write([]byte(`{"number":41,"title":"Ready","state":"open","draft":false,"mergeable_state":"clean","user":{"login":"alice"},"base":{"ref":"main","sha":"base41","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"head":{"ref":"ready","sha":"head41","repo":{"full_name":"sozercan/orka","clone_url":"https://github.com/sozercan/orka.git"}},"labels":[]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sozercan/orka/commits/head41/check-runs":
			_, _ = w.Write([]byte(`{"total_count":1,"check_runs":[{"name":"test","status":"completed","conclusion":"success"}]}`))
		case r.Method == http.MethodGet && r.URL.Path == "/repos/sozercan/orka/commits/head41/status":
			_, _ = w.Write([]byte(`{"state":"success","statuses":[{"context":"legacy","state":"success"}]}`))
		case r.Method == http.MethodPut && r.URL.Path == "/repos/sozercan/orka/pulls/41/merge":
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
	command := &store.CommandEvent{ID: "cmd-automerge-41", MonitorNamespace: "default", MonitorName: "pr-automerge", Repo: "sozercan/orka", Kind: repositoryMonitorPullRequestKind, Number: 41, Intent: "automerge", Permission: "maintain", HeadSHA: "head41", Status: "accepted", CreatedAt: processedAt, ProcessedAt: &processedAt}
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
	item, err := monitorStore.GetMonitorItem(ctx, "default", "pr-automerge", repositoryMonitorPullRequestKind, "41")
	if err != nil {
		t.Fatalf("GetMonitorItem() error = %v", err)
	}
	if item.AutomergeState != repositoryMonitorAutomergeStateMerged {
		t.Fatalf("item = %#v, want automerge merged", item)
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
	denied := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/.github/workflows/ci.yml b/.github/workflows/ci.yml\n", Files: []string{".github/workflows/ci.yml"}}
	if got := reconciler.validateAndSaveIssuePatchArtifacts(ctx, monitor, item, record, task, denied); got != "patch_path_denied" {
		t.Fatalf("denied patch reason = %q, want patch_path_denied", got)
	}
	valid := &common.StructuredResult{BaseSHA: "base", Diff: "diff --git a/internal/x.go b/internal/x.go\n--- a/internal/x.go\n+++ b/internal/x.go\n@@ -1 +1 @@\n-old\n+new\n", Files: []string{"internal/x.go"}}
	if got := reconciler.validateAndSaveIssuePatchArtifacts(ctx, monitor, item, record, task, valid); got != "" {
		t.Fatalf("valid patch reason = %q, want empty", got)
	}
	if _, _, err := monitorStore.GetArtifact(ctx, "default", "impl-task", repositoryMonitorIssuePatchDiffArtifact(55, "act-impl")); err != nil {
		t.Fatalf("diff artifact missing: %v", err)
	}
	if _, _, err := monitorStore.GetArtifact(ctx, "default", "impl-task", repositoryMonitorIssuePatchSummaryArtifact(55, "act-impl")); err != nil {
		t.Fatalf("summary artifact missing: %v", err)
	}
}
