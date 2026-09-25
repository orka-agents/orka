/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const scanProgressScanUID = "scan-1-uid"

func scanProgressTask(name, scanID, stage string, phase corev1alpha1.TaskPhase) *corev1alpha1.Task {
	controller := true
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo", Labels: map[string]string{
			labels.LabelSecurityTarget: "scan-1",
			labels.LabelSecurityScanID: scanID,
			labels.LabelSecurityStage:  stage,
		}, OwnerReferences: []metav1.OwnerReference{{
			APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan", Name: "scan-1", UID: scanProgressScanUID, Controller: &controller,
		}}},
		Spec:   corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: phase},
	}
}

func scanProgressScan() *corev1alpha1.RepositoryScan {
	scan := securityAuthzTestRepositoryScan("scan-1", securityTestRepoURL)
	scan.UID = scanProgressScanUID
	return scan
}

func setupScanProgressHandlers(t *testing.T, objs ...runtime.Object) (*fiber.App, *Handlers) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	handlers := NewHandlers(HandlersConfig{Client: fakeClient, SecurityStore: sqlite.NewStore(db, ":memory:")})
	app := fiber.New()
	app.Get("/security/repositories/:name/scans/:scanID/progress", handlers.GetSecurityScanProgress)
	return app, handlers
}

func TestGetSecurityScanProgressGroupsRunTasksByStage(t *testing.T) {
	scan := scanProgressScan()
	// A Task that merely copies the labels but is not controlled by the
	// RepositoryScan must not be counted or named.
	impostor := scanProgressTask("impostor-review", "run-1", security.StageReview, corev1alpha1.TaskPhaseFailed)
	impostor.OwnerReferences = nil
	app, handlers := setupScanProgressHandlers(t,
		scan,
		impostor,
		scanProgressTask("goof-threat-model", "run-1", security.StageThreatModel, corev1alpha1.TaskPhaseSucceeded),
		scanProgressTask("goof-mapper", "run-1", security.StageMapper, corev1alpha1.TaskPhaseSucceeded),
		scanProgressTask("goof-review-1", "run-1", security.StageReview, corev1alpha1.TaskPhaseSucceeded),
		scanProgressTask("goof-review-2", "run-1", security.StageReview, corev1alpha1.TaskPhaseRunning),
		scanProgressTask("goof-review-3", "run-1", security.StageReview, corev1alpha1.TaskPhaseFailed),
		// An older run's Task must not be counted.
		scanProgressTask("goof-review-old", "run-0", security.StageReview, corev1alpha1.TaskPhaseSucceeded),
	)
	require.NoError(t, handlers.securityStore.CreateScanRun(context.Background(), &store.ScanRun{
		ID: "run-1", Namespace: "demo", RepositoryScan: "scan-1", Mode: "manual", Phase: "running",
		SliceCount: 3, ReviewedSliceCount: 1,
	}))
	require.NoError(t, handlers.securityStore.CreateScanRun(context.Background(), &store.ScanRun{
		ID: "run-other", Namespace: "demo", RepositoryScan: "scan-2", Mode: "manual", Phase: "succeeded",
	}))

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/security/repositories/scan-1/scans/run-1/progress?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body SecurityScanProgressResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Equal(t, "run-1", body.Scan.ID)
	require.False(t, body.Complete)
	require.Len(t, body.Stages, len(security.ScanStageOrder))
	require.Equal(t, security.StageProgress{Stage: security.StageThreatModel, Label: "threat model", Tasks: 1, Succeeded: 1}, body.Stages[0])
	review := body.Stages[2]
	require.Equal(t, 3, review.Tasks)
	require.Equal(t, 1, review.Running)
	require.Equal(t, 1, review.Succeeded)
	require.Equal(t, 1, review.Failed)
	require.Equal(t, []string{"goof-review-3"}, review.FailedTasks)
	require.Equal(t, 0, body.Stages[3].Tasks)

	// A run that belongs to another repository is not visible through this one.
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/security/repositories/scan-1/scans/run-other/progress?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/security/repositories/scan-1/scans/missing/progress?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestGetSecurityScanProgressReportsCompletion(t *testing.T) {
	app, handlers := setupScanProgressHandlers(t, scanProgressScan())
	require.NoError(t, handlers.securityStore.CreateScanRun(context.Background(), &store.ScanRun{
		ID: "run-1", Namespace: "demo", RepositoryScan: "scan-1", Mode: "manual", Phase: "failed",
	}))
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/security/repositories/scan-1/scans/run-1/progress?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var body SecurityScanProgressResponse
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.True(t, body.Complete)
	require.Equal(t, "failed", body.Scan.Phase)
}

func TestHandlers_ListTasks_LabelSelector(t *testing.T) {
	labeled := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "proxy-1", Namespace: "default", Labels: map[string]string{"orka.ai/source": "anthropic-proxy"}},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
	}
	other := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "manual-1", Namespace: "default"},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeContainer},
	}
	handlers, app := setupTestHandlersWithObjects(labeled, other)
	app.Get("/tasks", handlers.ListTasks)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/tasks?namespace=default&labelSelector=orka.ai%2Fsource%3Danthropic-proxy", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var list struct {
		Items []corev1alpha1.Task `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.Len(t, list.Items, 1)
	require.Equal(t, "proxy-1", list.Items[0].Name)

	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/tasks?namespace=default&labelSelector=orka.ai%2Fsource%21%3Danthropic-proxy", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&list))
	require.Len(t, list.Items, 1)
	require.Equal(t, "manual-1", list.Items[0].Name)

	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/tasks?namespace=default&labelSelector=a%3D%3Db%3D%3Dc", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Contains(t, string(body), "invalid labelSelector")
}
