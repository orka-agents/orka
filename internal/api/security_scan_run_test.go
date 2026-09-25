package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	"github.com/orka-agents/orka/internal/store"
)

func TestCreateManualSecurityScanReplacesStaleRunIdentity(t *testing.T) {
	provider := newTestOIDCProvider(t)
	config := testContextTokenConfig(t, provider, "")
	for _, tt := range []struct {
		name           string
		uid            string
		generation     int64
		clearStatus    bool
		invalidBinding string
		failed         bool
	}{
		{name: "edited", uid: "current-uid", generation: 1},
		{name: "failed run with active sibling", uid: "current-uid", generation: 1, failed: true},
		{name: "recreated", uid: "previous-uid", generation: 2},
		{name: "legacy"},
		{name: "status cleared during admission", uid: "current-uid", generation: 1, clearStatus: true},
		{name: "missing binding", uid: "current-uid", generation: 2, invalidBinding: "missing"},
		{name: "foreign binding", uid: "current-uid", generation: 2, invalidBinding: "foreign"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "identity-scan", Namespace: "demo", UID: "current-uid", Generation: 2},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: securityTestRepoURL, AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
				Status: corev1alpha1.RepositoryScanStatus{LastScanID: "scan_old", LastProcessedCommit: "old-base"},
			}
			oldOwner := scan.DeepCopy()
			if tt.uid != "" {
				oldOwner.UID = types.UID(tt.uid)
			}
			oldTask := &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{
					Name: "old-mapper", Namespace: scan.Namespace,
					Labels: map[string]string{
						labels.LabelSecurityTarget: scan.Name, labels.LabelSecurityScanID: "scan_old", labels.LabelSecurityStage: security.StageMapper,
					},
					OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(oldOwner, corev1alpha1.GroupVersion.WithKind("RepositoryScan"))},
				},
				Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning},
			}
			app, handlers := setupSecurityHandlersWithAuthzFixture(t, config, ContextTokenAuthorizationModeEnforce, scan, oldTask, securityRuntimeTestAgent(scan.Spec.AnalysisAgentRef.Name))
			if tt.clearStatus {
				base, ok := handlers.client.(client.WithWatch)
				require.True(t, ok)
				handlers.client = interceptor.NewClient(base, interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
						if err := c.Create(ctx, object, opts...); err != nil {
							return err
						}
						if _, ok := object.(*corev1alpha1.Task); !ok {
							return nil
						}
						current := &corev1alpha1.RepositoryScan{}
						if err := c.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
							return err
						}
						current.Status.LastScanID = ""
						current.Status.LastScanTaskName = ""
						return c.Status().Update(ctx, current)
					},
				})
			}
			old := &store.ScanRun{
				ID: "scan_old", Namespace: scan.Namespace, RepositoryScan: scan.Name,
				RepositoryScanUID: tt.uid, RepositoryScanGeneration: tt.generation, Phase: "running",
			}
			if tt.failed {
				old.Phase = "failed"
			}
			if tt.invalidBinding == "foreign" {
				old.RepositoryScan = "other-scan"
			}
			if tt.invalidBinding != "missing" {
				require.NoError(t, handlers.securityStore.CreateScanRun(ctx, old))
			}
			token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite})
			request := httptest.NewRequest(http.MethodPost, "/security/repositories/identity-scan/scans?namespace=demo", nil)
			request.Header.Set(TransactionTokenHeaderName, token)
			response, err := app.Test(request)
			require.NoError(t, err)
			require.Equal(t, http.StatusConflict, response.StatusCode)
			require.NoError(t, response.Body.Close())
			if tt.invalidBinding == "" {
				pending, err := handlers.securityStore.GetScanRun(ctx, scan.Namespace, old.ID)
				require.NoError(t, err)
				require.True(t, pending.CancellationPending)
			}
			request = httptest.NewRequest(http.MethodPost, "/security/repositories/identity-scan/scans?namespace=demo", nil)
			request.Header.Set(TransactionTokenHeaderName, token)
			response, err = app.Test(request)
			require.NoError(t, err)
			t.Cleanup(func() { _ = response.Body.Close() })
			require.Equal(t, http.StatusCreated, response.StatusCode)
			var run store.ScanRun
			require.NoError(t, json.NewDecoder(response.Body).Decode(&run))
			require.True(t, security.ScanRunMatchesRepositoryScan(&run, scan))
			require.NotEqual(t, "scan_old", run.ID)
			require.Empty(t, run.BaseCommit)
			oldRun, err := handlers.securityStore.GetScanRun(ctx, scan.Namespace, "scan_old")
			if tt.invalidBinding == "missing" {
				require.ErrorIs(t, err, store.ErrNotFound)
			} else {
				require.NoError(t, err)
				wantPhase := "failed"
				if tt.invalidBinding == "foreign" {
					wantPhase = "running"
				}
				require.Equal(t, wantPhase, oldRun.Phase)
				require.Equal(t, tt.uid, oldRun.RepositoryScanUID)
			}
			oldTaskErr := handlers.client.Get(ctx, client.ObjectKeyFromObject(oldTask), &corev1alpha1.Task{})
			require.True(t, apierrors.IsNotFound(oldTaskErr), "obsolete owned pipeline Task must be cancelled")
			current := &corev1alpha1.RepositoryScan{}
			require.NoError(t, handlers.client.Get(ctx, client.ObjectKeyFromObject(scan), current))
			require.Equal(t, run.ID, current.Status.LastScanID)
			require.Empty(t, current.Status.LastProcessedCommit)
			require.Contains(t, current.Finalizers, security.RepositoryScanRunFinalizer)
			task := &corev1alpha1.Task{}
			require.NoError(t, handlers.client.Get(ctx, client.ObjectKey{Namespace: scan.Namespace, Name: run.TaskName}, task))
			require.True(t, metav1.IsControlledBy(task, scan))
			require.Contains(t, task.Spec.Prompt, `"scanId":"`+run.ID+`"`)
		})
	}
}

func TestUpdateRepositoryScanRunStatusRejectsNewerBinding(t *testing.T) {
	ctx := context.Background()
	current := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: "demo", UID: "scan-uid", Generation: 1},
		Status:     corev1alpha1.RepositoryScanStatus{LastScanID: "scan_newer"},
	}
	_, handlers := setupSecurityHandlersWithAuthzFixture(t, ContextTokenConfig{}, ContextTokenAuthorizationModeEnforce, current)
	stale := current.DeepCopy()
	stale.Status.LastScanID = "scan_old"
	err := handlers.updateRepositoryScanRunStatus(ctx, stale, "scan_attempt", "task", false)
	require.ErrorContains(t, err, "a newer scan run already owns repository scan status")
	after := &corev1alpha1.RepositoryScan{}
	require.NoError(t, handlers.client.Get(ctx, client.ObjectKeyFromObject(current), after))
	require.Equal(t, current.Status, after.Status)
}

func TestCreateManualSecurityScanUsesLiveStatusAfterFinalizerPatch(t *testing.T) {
	provider := newTestOIDCProvider(t)
	config := testContextTokenConfig(t, provider, "")
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: "demo", UID: "scan-uid", Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: securityTestRepoURL, AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, config, ContextTokenAuthorizationModeEnforce, scan, securityRuntimeTestAgent(scan.Spec.AnalysisAgentRef.Name))
	base, ok := handlers.client.(client.WithWatch)
	require.True(t, ok)
	stale := &corev1alpha1.RepositoryScan{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), stale))
	handlers.apiReader = base
	handlers.client = interceptor.NewClient(base, interceptor.Funcs{
		Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if current, ok := object.(*corev1alpha1.RepositoryScan); ok {
				*current = *stale.DeepCopy()
				return nil
			}
			return c.Get(ctx, key, object, opts...)
		},
	})
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite})
	request := httptest.NewRequest(http.MethodPost, "/security/repositories/scan/scans?namespace=demo", nil)
	request.Header.Set(TransactionTokenHeaderName, token)
	response, err := app.Test(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusCreated, response.StatusCode)
	var run store.ScanRun
	require.NoError(t, json.NewDecoder(response.Body).Decode(&run))
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
	require.Contains(t, current.Finalizers, security.RepositoryScanRunFinalizer)
	require.Equal(t, run.ID, current.Status.LastScanID)
	require.Equal(t, run.TaskName, current.Status.LastScanTaskName)
	require.NotEqual(t, stale.ResourceVersion, current.ResourceVersion)
}

func TestCreateManualSecurityScanDoesNotProjectStatusOntoEditedScan(t *testing.T) {
	provider := newTestOIDCProvider(t)
	config := testContextTokenConfig(t, provider, "")
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "edited-scan", Namespace: "demo", UID: "scan-uid", Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: securityTestRepoURL, AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, config, ContextTokenAuthorizationModeEnforce, scan, securityRuntimeTestAgent(scan.Spec.AnalysisAgentRef.Name))
	base, ok := handlers.client.(client.WithWatch)
	require.True(t, ok)
	handlers.client = interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, object, opts...); err != nil {
				return err
			}
			if _, ok := object.(*corev1alpha1.Task); ok {
				current := &corev1alpha1.RepositoryScan{}
				if err := c.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
					return err
				}
				current.Generation++
				current.Spec.SubPath = "edited"
				return c.Update(ctx, current)
			}
			return nil
		},
	})
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite})
	request := httptest.NewRequest(http.MethodPost, "/security/repositories/edited-scan/scans?namespace=demo", nil)
	request.Header.Set(TransactionTokenHeaderName, token)
	response, err := app.Test(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusConflict, response.StatusCode)
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, base.Get(context.Background(), client.ObjectKeyFromObject(scan), current))
	require.Equal(t, int64(2), current.Generation)
	require.Empty(t, current.Status.LastScanID)
	require.Empty(t, current.Status.Phase)
	runs, _, err := handlers.securityStore.ListScanRuns(context.Background(), scan.Namespace, scan.Name, 10, "")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, "pending", runs[0].Phase)
	require.True(t, runs[0].CancellationPending)
	require.True(t, apierrors.IsNotFound(base.Get(context.Background(), client.ObjectKey{Namespace: scan.Namespace, Name: runs[0].TaskName}, &corev1alpha1.Task{})))
	_, err = security.RetireStaleScanRuns(context.Background(), handlers.securityStore, base, base, current)
	require.NoError(t, err)
	retired, err := handlers.securityStore.GetScanRun(context.Background(), scan.Namespace, runs[0].ID)
	require.NoError(t, err)
	require.Equal(t, "failed", retired.Phase)
	require.False(t, retired.CancellationPending)
}

func TestCreateManualSecurityScanCleansUpUnconfirmedTaskAdmission(t *testing.T) {
	provider := newTestOIDCProvider(t)
	config := testContextTokenConfig(t, provider, "")
	for _, failure := range []string{"lost create response", "failed ownership read"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: "demo", UID: "scan-uid", Generation: 1},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: securityTestRepoURL, AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
			}
			app, handlers := setupSecurityHandlersWithAuthzFixture(t, config, ContextTokenAuthorizationModeEnforce, scan, securityRuntimeTestAgent(scan.Spec.AnalysisAgentRef.Name))
			base, ok := handlers.client.(client.WithWatch)
			require.True(t, ok)
			admissionErr := fmt.Errorf("injected admission failure")
			created := false
			handlers.client = interceptor.NewClient(base, interceptor.Funcs{
				Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
					if task, ok := object.(*corev1alpha1.Task); ok {
						task.Finalizers = []string{"test.orka.ai/hold-cleanup"}
					}
					if err := c.Create(ctx, object, opts...); err != nil {
						return err
					}
					created = true
					if failure == "lost create response" {
						return admissionErr
					}
					return nil
				},
			})
			handlers.apiReader = interceptor.NewClient(base, interceptor.Funcs{
				Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
					if _, ok := object.(*corev1alpha1.RepositoryScan); ok && created && failure == "failed ownership read" {
						return admissionErr
					}
					return c.Get(ctx, key, object, opts...)
				},
			})
			token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite})
			request := httptest.NewRequest(http.MethodPost, "/security/repositories/scan/scans?namespace=demo", nil)
			request.Header.Set(TransactionTokenHeaderName, token)
			response, err := app.Test(request)
			require.NoError(t, err)
			t.Cleanup(func() { _ = response.Body.Close() })
			require.Equal(t, http.StatusInternalServerError, response.StatusCode)
			runs, _, err := handlers.securityStore.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
			require.NoError(t, err)
			require.Len(t, runs, 1)
			require.True(t, runs[0].CancellationPending)
			task := &corev1alpha1.Task{}
			require.NoError(t, base.Get(ctx, client.ObjectKey{Namespace: scan.Namespace, Name: runs[0].TaskName}, task))
			require.False(t, task.DeletionTimestamp.IsZero())
			_, err = security.RetireStaleScanRuns(ctx, handlers.securityStore, base, base, scan)
			require.ErrorIs(t, err, security.ErrScanRunCancellationPending)
			task.Finalizers = nil
			require.NoError(t, base.Update(ctx, task))
			_, err = security.RetireStaleScanRuns(ctx, handlers.securityStore, base, base, scan)
			require.NoError(t, err)
			retired, err := handlers.securityStore.GetScanRun(ctx, scan.Namespace, runs[0].ID)
			require.NoError(t, err)
			require.Equal(t, "failed", retired.Phase)
			require.False(t, retired.CancellationPending)
		})
	}
}

func TestCreateManualSecurityScanRollsBackChangedStatusBinding(t *testing.T) {
	provider := newTestOIDCProvider(t)
	config := testContextTokenConfig(t, provider, "")
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: "demo", UID: "scan-uid", Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: securityTestRepoURL, AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, config, ContextTokenAuthorizationModeEnforce, scan, securityRuntimeTestAgent(scan.Spec.AnalysisAgentRef.Name))
	completed := &store.ScanRun{
		ID: "scan_completed", Namespace: scan.Namespace, RepositoryScan: scan.Name,
		RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation, Phase: "succeeded",
	}
	require.NoError(t, handlers.securityStore.CreateScanRun(ctx, completed))
	base, ok := handlers.client.(client.WithWatch)
	require.True(t, ok)
	handlers.client = interceptor.NewClient(base, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, object client.Object, opts ...client.CreateOption) error {
			if err := c.Create(ctx, object, opts...); err != nil {
				return err
			}
			if _, ok := object.(*corev1alpha1.Task); !ok {
				return nil
			}
			current := &corev1alpha1.RepositoryScan{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
				return err
			}
			current.Status.LastScanID = completed.ID
			return c.Status().Update(ctx, current)
		},
	})
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite})
	request := httptest.NewRequest(http.MethodPost, "/security/repositories/scan/scans?namespace=demo", nil)
	request.Header.Set(TransactionTokenHeaderName, token)
	response, err := app.Test(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusConflict, response.StatusCode)
	_, err = security.RetireStaleScanRuns(ctx, handlers.securityStore, base, base, scan)
	require.NoError(t, err)
	runs, _, err := handlers.securityStore.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
	require.NoError(t, err)
	require.Len(t, runs, 2)
	for _, run := range runs {
		if run.ID == completed.ID {
			require.Equal(t, "succeeded", run.Phase)
			continue
		}
		require.Equal(t, "failed", run.Phase)
		require.NotNil(t, run.CompletedAt)
		require.True(t, apierrors.IsNotFound(base.Get(ctx, client.ObjectKey{Namespace: scan.Namespace, Name: run.TaskName}, &corev1alpha1.Task{})))
	}
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, base.Get(ctx, client.ObjectKeyFromObject(scan), current))
	require.Equal(t, completed.ID, current.Status.LastScanID)
}

func TestCreateManualSecurityScanRollsBackExhaustedStatusConflicts(t *testing.T) {
	provider := newTestOIDCProvider(t)
	config := testContextTokenConfig(t, provider, "")
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: "demo", UID: "scan-uid", Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: securityTestRepoURL, AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, config, ContextTokenAuthorizationModeEnforce, scan, securityRuntimeTestAgent(scan.Spec.AnalysisAgentRef.Name))
	base, ok := handlers.client.(client.WithWatch)
	require.True(t, ok)
	patches := 0
	handlers.client = interceptor.NewClient(base, interceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, c client.Client, subresource string, object client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
			if _, ok := object.(*corev1alpha1.RepositoryScan); ok && subresource == "status" {
				patches++
				return apierrors.NewConflict(corev1alpha1.GroupVersion.WithResource("repositoryscans").GroupResource(), object.GetName(), fmt.Errorf("concurrent status writer"))
			}
			return c.SubResource(subresource).Patch(ctx, object, patch, opts...)
		},
	})
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite})
	request := httptest.NewRequest(http.MethodPost, "/security/repositories/scan/scans?namespace=demo", nil)
	request.Header.Set(TransactionTokenHeaderName, token)
	response, err := app.Test(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusConflict, response.StatusCode)
	require.Greater(t, patches, 1, "exercise retry exhaustion")
	runs, _, err := handlers.securityStore.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, "pending", runs[0].Phase)
	require.True(t, runs[0].CancellationPending)
	require.True(t, apierrors.IsNotFound(base.Get(ctx, client.ObjectKey{Namespace: scan.Namespace, Name: runs[0].TaskName}, &corev1alpha1.Task{})))
	active, err := handlers.securityStore.ListActiveScanRuns(ctx, scan.Namespace, scan.Name)
	require.NoError(t, err)
	require.Len(t, active, 1, "rollback reserves the run until a later empty read")
	_, err = security.RetireStaleScanRuns(ctx, handlers.securityStore, base, base, scan)
	require.NoError(t, err)
	active, err = handlers.securityStore.ListActiveScanRuns(ctx, scan.Namespace, scan.Name)
	require.NoError(t, err)
	require.Empty(t, active)
}

func TestCreateManualSecurityScanReturnsConflictWhenRetirementIsFenced(t *testing.T) {
	provider := newTestOIDCProvider(t)
	config := testContextTokenConfig(t, provider, "")
	ctx := context.Background()
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan", Namespace: "demo", UID: "scan-uid", Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: securityTestRepoURL, AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, config, ContextTokenAuthorizationModeEnforce, scan, securityRuntimeTestAgent(scan.Spec.AnalysisAgentRef.Name))
	base, ok := handlers.client.(client.WithWatch)
	require.True(t, ok)
	handlers.client = interceptor.NewClient(base, interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, opts ...client.PatchOption) error {
			if err := c.Patch(ctx, object, patch, opts...); err != nil {
				return err
			}
			if _, ok := object.(*corev1alpha1.RepositoryScan); !ok {
				return nil
			}
			current := &corev1alpha1.RepositoryScan{}
			if err := c.Get(ctx, client.ObjectKeyFromObject(scan), current); err != nil {
				return err
			}
			current.Generation++
			current.Spec.SubPath = "edited"
			if err := c.Update(ctx, current); err != nil {
				return err
			}
			return handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
				ID: "scan_newer", Namespace: scan.Namespace, RepositoryScan: scan.Name,
				RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: current.Generation, Phase: "pending",
			})
		},
	})
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite})
	request := httptest.NewRequest(http.MethodPost, "/security/repositories/scan/scans?namespace=demo", nil)
	request.Header.Set(TransactionTokenHeaderName, token)
	response, err := app.Test(request)
	require.NoError(t, err)
	t.Cleanup(func() { _ = response.Body.Close() })
	require.Equal(t, http.StatusConflict, response.StatusCode)
	run, err := handlers.securityStore.GetScanRun(ctx, scan.Namespace, "scan_newer")
	require.NoError(t, err)
	require.Equal(t, "pending", run.Phase)
	var tasks corev1alpha1.TaskList
	require.NoError(t, base.List(ctx, &tasks, client.InNamespace(scan.Namespace)))
	require.Empty(t, tasks.Items)
}
