/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

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

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	k8svalidation "k8s.io/apimachinery/pkg/util/validation"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/security"
	securitybundle "github.com/orka-agents/orka/internal/security/bundle"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

const (
	securityTestRepoURL   = "https://github.com/sozercan/actions-test"
	securityTestRepoPRURL = securityTestRepoURL + "/pull/99"
)

type scanRunRecoveryErrorStore struct {
	store.SecurityStore
	store.SecurityRunTaskInputStore
	recoveryErr error
}

func (s *scanRunRecoveryErrorStore) CreateScanRun(context.Context, *store.ScanRun) error {
	return store.ErrConflict
}

func (s *scanRunRecoveryErrorStore) CreateScanRunWithTaskInput(
	context.Context, *store.ScanRun, *store.SecurityRunTaskInput,
) error {
	return store.ErrConflict
}

func (s *scanRunRecoveryErrorStore) ListScanRuns(context.Context, string, string, int, string) ([]store.ScanRun, string, error) {
	return nil, "", s.recoveryErr
}

type delayedScanRunListStore struct {
	store.SecurityStore
	hideNext bool
}

type competingRepositoryReservationStore struct {
	store.SecurityStore
	store.SecurityRunTaskInputStore
	injectedRun *store.ScanRun
}

func (s *competingRepositoryReservationStore) CreateScanRunWithTaskInput(
	ctx context.Context, requested *store.ScanRun, input *store.SecurityRunTaskInput,
) error {
	runUID := "run_8888888888888888888888888888888888888888888888888888888888888888"
	competing := *requested
	competing.ID = security.PublicScanRunID(runUID)
	competing.RunUID = runUID
	competing.TaskName = security.ScanStageTaskNameForRun(
		requested.RepositoryScan, requested.Mode, security.StageThreatModel, "", runUID,
	)
	competing.RequestIdempotencyKey = "req_unrelated_api_reservation"
	competing.IdempotencyKey = competing.RequestIdempotencyKey
	competingInput := *input
	competingInput.RunUID = runUID
	competingInput.ScanRunID = competing.ID
	competingInput.RecordDigest = ""
	competingInput.CreatedAt = time.Time{}
	if err := s.SecurityRunTaskInputStore.CreateScanRunWithTaskInput(ctx, &competing, &competingInput); err != nil {
		return err
	}
	s.injectedRun = &competing
	return fmt.Errorf("%w: injected unrelated repository reservation", store.ErrConflict)
}

func (s *delayedScanRunListStore) ListScanRuns(
	ctx context.Context, namespace, repositoryScan string, limit int, cursor string,
) ([]store.ScanRun, string, error) {
	if s.hideNext {
		s.hideNext = false
		return nil, "", nil
	}
	return s.SecurityStore.ListScanRuns(ctx, namespace, repositoryScan, limit, cursor)
}

func TestSecurityRepositoryActions_ContextTokenAuthorization(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")

	createBody := fmt.Sprintf(`{
		"name":"scan-create",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"analysisAgentRef":{"name":"analysis"}
		}
	}`, securityTestRepoURL)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		scope  string
		want   int
	}{
		{
			name:   "list allowed with security read scope",
			method: http.MethodGet,
			path:   "/security/repositories?namespace=demo",
			scope:  ContextTokenScopeSecurityRead,
			want:   http.StatusOK,
		},
		{
			name:   "list denied without security read scope",
			method: http.MethodGet,
			path:   "/security/repositories?namespace=demo",
			scope:  ContextTokenScopeSecurityWrite,
			want:   http.StatusForbidden,
		},
		{
			name:   "create allowed with security write scope",
			method: http.MethodPost,
			path:   "/security/repositories",
			body:   createBody,
			scope:  ContextTokenScopeSecurityWrite,
			want:   http.StatusCreated,
		},
		{
			name:   "create denied without security write scope",
			method: http.MethodPost,
			path:   "/security/repositories",
			body:   createBody,
			scope:  ContextTokenScopeSecurityRead,
			want:   http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := setupSecurityHandlersWithAuthz(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce)
			token := issueTestContextToken(t, provider, nil, map[string]any{"scope": tt.scope})
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set(TransactionTokenHeaderName, token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, tt.want, resp.StatusCode)
		})
	}
}

func setupSecurityHandlersWithAuthz(t *testing.T, ctxTokenConfig ContextTokenConfig, mode string, objs ...runtime.Object) *fiber.App {
	app, _ := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, mode, objs...)
	return app
}

func setupSecurityHandlersWithAuthzFixture(t *testing.T, ctxTokenConfig ContextTokenConfig, mode string, objs ...runtime.Object) (*fiber.App, *Handlers) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithRuntimeObjects(objs...).
		Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	securityStore := sqlite.NewStore(db, ":memory:")
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{
		Mode: mode,
	})
	require.NoError(t, err)

	handlers := NewHandlers(HandlersConfig{
		Client:                    fakeClient,
		SecurityStore:             securityStore,
		SecurityIntegrityStore:    securityStore,
		SecurityBundleStore:       securityStore,
		ContextTokenAuthorization: authz,
	})

	app := fiber.New()
	app.Use(NewAuthMiddleware(handlers.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Post("/security/repositories", handlers.CreateRepositoryScan)
	app.Get("/security/repositories", handlers.ListRepositoryScans)
	app.Get("/security/repositories/:name", handlers.GetRepositoryScan)
	app.Put("/security/repositories/:name", handlers.UpdateRepositoryScan)
	app.Delete("/security/repositories/:name", handlers.DeleteRepositoryScan)
	app.Get("/security/repositories/:name/threat-model", handlers.GetThreatModel)
	app.Put("/security/repositories/:name/threat-model", handlers.UpdateThreatModel)
	app.Get("/security/repositories/:name/scans", handlers.ListSecurityScanRuns)
	app.Post("/security/repositories/:name/scans", handlers.CreateManualSecurityScan)
	app.Get("/security/repositories/:name/scans/:runID/bundle", handlers.GetSecurityScanBundle)
	app.Get("/security/repositories/:name/scans/:runID/coverage", handlers.GetSecurityScanCoverage)
	app.Get("/security/repositories/:name/findings", handlers.ListSecurityFindings)
	app.Get("/security/repositories/:name/slices", handlers.ListSecurityReviewSlices)
	app.Get("/security/repositories/:name/slices/:sliceID", handlers.GetSecurityReviewSlice)
	app.Get("/security/repositories/:name/dropped-findings", handlers.ListSecurityDroppedFindings)
	app.Get("/security/findings/:id", handlers.GetSecurityFinding)
	app.Post("/security/findings/:id/decisions", handlers.AppendSecurityFindingDecision)
	app.Get("/security/findings/:id/occurrences", handlers.ListSecurityFindingOccurrences)
	app.Get("/security/findings/:id/decisions", handlers.ListSecurityFindingDecisions)
	app.Get("/security/findings/:id/assessments", handlers.ListSecurityFindingAssessments)
	app.Get("/security/findings/:id/patches", handlers.ListSecurityPatchProposals)
	app.Post("/security/findings/:id/dismiss", handlers.DismissSecurityFinding)
	app.Post("/security/findings/:id/reopen", handlers.ReopenSecurityFinding)
	app.Post("/security/findings/:id/validate", handlers.ValidateSecurityFinding)
	app.Post("/security/findings/:id/patch", handlers.GenerateSecurityPatch)
	app.Post("/security/findings/:id/pull-request", handlers.CreateSecurityPullRequest)
	return app, handlers
}

func TestCreateSecurityScanRunHandlesAmbiguousTaskAdmission(t *testing.T) {
	tests := []struct {
		name             string
		createErr        error
		admitTask        bool
		mutateTask       bool
		admitOnRecheck   bool
		verificationErr  bool
		wantError        bool
		wantRunPhase     string
		wantStatusRepair bool
	}{
		{
			name: "admitted matching task repairs status", admitTask: true,
			wantRunPhase: "pending", wantStatusRepair: true,
		},
		{
			name: "ambiguous create followed by immediate absence leaves run repairable", wantError: true,
			wantRunPhase: "pending",
		},
		{
			name: "definitive create rejection leaves run repairable", createErr: apierrors.NewBadRequest("task rejected"),
			wantError: true, wantRunPhase: "pending",
		},
		{
			name:      "definitive rejection recovers a concurrent matching admission",
			createErr: apierrors.NewBadRequest("task rejected"), admitOnRecheck: true,
			wantRunPhase: "pending", wantStatusRepair: true,
		},
		{
			name: "oversized create rejection leaves run repairable", createErr: apierrors.NewRequestEntityTooLargeError("task too large"),
			wantError: true, wantRunPhase: "pending",
		},
		{
			name: "invalid admitted task leaves run repairable", admitTask: true, mutateTask: true, wantError: true,
			wantRunPhase: "pending",
		},
		{
			name: "unknown admission outcome leaves run repairable", verificationErr: true, wantError: true,
			wantRunPhase: "pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			scan := &corev1alpha1.RepositoryScan{
				TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "ambiguous-create", Namespace: "demo", UID: types.UID("ambiguous-create-uid"), Generation: 2,
				},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: "https://github.com/example/repo", Branch: "main",
					AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
			}
			taskCreateAttempted := false
			taskGetCalls := 0
			var taskTemplate *corev1alpha1.Task
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
				WithObjects(scan).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						task, ok := obj.(*corev1alpha1.Task)
						if !ok {
							return c.Create(ctx, obj, opts...)
						}
						taskCreateAttempted = true
						taskTemplate = task.DeepCopy()
						if tt.admitTask {
							persisted := task.DeepCopy()
							if tt.mutateTask {
								persisted.Spec.Prompt += "\nforged"
							}
							require.NoError(t, c.Create(ctx, persisted, opts...))
						}
						if tt.createErr != nil {
							return tt.createErr
						}
						return errors.New("simulated ambiguous task create response")
					},
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if _, ok := obj.(*corev1alpha1.Task); ok && taskCreateAttempted {
							taskGetCalls++
							if tt.admitOnRecheck && taskGetCalls == 2 {
								require.NotNil(t, taskTemplate)
								require.NoError(t, c.Create(ctx, taskTemplate.DeepCopy()))
							}
							if tt.verificationErr {
								return errors.New("simulated task verification outage")
							}
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()

			db, err := sqlite.NewDB(":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			securityStore := sqlite.NewStore(db, ":memory:")
			handlers := NewHandlers(HandlersConfig{Client: cl, SecurityStore: securityStore})
			current := &corev1alpha1.RepositoryScan{}
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(scan), current))

			run, createErr := handlers.createSecurityScanRun(context.Background(), nil, current, "")
			if tt.wantError {
				require.Error(t, createErr)
				require.Nil(t, run)
			} else {
				require.NoError(t, createErr)
				require.NotNil(t, run)
			}
			runs, _, err := securityStore.ListScanRuns(context.Background(), scan.Namespace, scan.Name, 10, "")
			require.NoError(t, err)
			require.Len(t, runs, 1)
			require.Equal(t, tt.wantRunPhase, runs[0].Phase)

			updated := &corev1alpha1.RepositoryScan{}
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(scan), updated))
			if tt.wantStatusRepair {
				require.Equal(t, runs[0].ID, updated.Status.LastScanID)
				require.Equal(t, runs[0].TaskName, updated.Status.LastScanTaskName)
			} else {
				require.Empty(t, updated.Status.LastScanID)
			}
		})
	}
}

func TestSecurityTaskMatchesExpectedIgnoresRequestSpecificProvenance(t *testing.T) {
	controller := true
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{
		Name: "provenance", Namespace: "demo", UID: types.UID("provenance-uid"),
	}}
	expected := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "provenance-task", Namespace: scan.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan", Name: scan.Name,
				UID: scan.UID, Controller: &controller,
			}},
			Labels: map[string]string{
				labels.LabelSecurityScanID: "scan_current", labels.LabelTransactionID: "transaction-a",
				labels.LabelAuthProfile: "profile-a",
			},
			Annotations: map[string]string{
				labels.AnnotationTransactionID: "transaction-a", labels.AnnotationTransactionSubject: "subject-a",
				labels.AnnotationTraceParent: "00-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa-bbbbbbbbbbbbbbbb-01",
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "deterministic prompt",
			RequestedBy: &corev1alpha1.RequestedBy{Subject: "subject-a"},
			Transaction: &corev1alpha1.TaskTransaction{ID: "transaction-a", Subject: "subject-a"},
		},
	}
	existing := expected.DeepCopy()
	existing.Spec.RequestedBy = &corev1alpha1.RequestedBy{Subject: "subject-b"}
	existing.Spec.Transaction = &corev1alpha1.TaskTransaction{ID: "transaction-b", Subject: "subject-b"}
	existing.Labels[labels.LabelTransactionID] = "transaction-b"
	existing.Labels[labels.LabelAuthProfile] = "profile-b"
	existing.Annotations[labels.AnnotationTransactionID] = "transaction-b"
	existing.Annotations[labels.AnnotationTransactionSubject] = "subject-b"
	existing.Annotations[labels.AnnotationTraceParent] = "00-cccccccccccccccccccccccccccccccc-dddddddddddddddd-01"

	existing.Labels["example.com/request-provenance"] = "request-b"
	existing.Annotations["example.com/request-provenance"] = "request-b"
	require.True(t, securityTaskMatchesExpected(existing, expected, scan))

	existing.Spec.Prompt = "different prompt"
	require.False(t, securityTaskMatchesExpected(existing, expected, scan))
	existing.Spec.Prompt = expected.Spec.Prompt
	existing.Labels[labels.LabelSecurityScanID] = "scan_other"
	require.False(t, securityTaskMatchesExpected(existing, expected, scan))
	existing.Labels[labels.LabelSecurityScanID] = expected.Labels[labels.LabelSecurityScanID]
	existing.Labels[labels.LabelSecurityOccurrenceID] = "occurrence_extra"
	require.False(t, securityTaskMatchesExpected(existing, expected, scan))
	delete(existing.Labels, labels.LabelSecurityOccurrenceID)
	existing.Annotations[security.AnnotationValidationBindingVersion] = security.ValidationBindingVersion
	require.False(t, securityTaskMatchesExpected(existing, expected, scan))
}

func TestSecurityTaskMatchesExpectedProtectsHarnessWrapperMetadata(t *testing.T) {
	controller := true
	scan := &corev1alpha1.RepositoryScan{ObjectMeta: metav1.ObjectMeta{
		Name: "harness-binding", Namespace: "demo", UID: types.UID("harness-binding-uid"),
	}}
	baseTask := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name: "harness-binding-task", Namespace: scan.Namespace,
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan", Name: scan.Name,
				UID: scan.UID, Controller: &controller,
			}},
			Labels:      map[string]string{labels.LabelSecurityScanID: "scan_current"},
			Annotations: map[string]string{labels.AnnotationTraceParent: "expected-trace"},
		},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Prompt: "deterministic prompt"},
	}

	keys := []string{
		"orka.ai/harness-wrapper-attempt",
		"orka.ai/harness-wrapper-runtime-session-id",
		"orka.ai/harness-wrapper-turn-id",
		"orka.ai/harness-wrapper-correlation-id",
	}
	for _, key := range keys {
		t.Run(key+"/label", func(t *testing.T) {
			expected := baseTask.DeepCopy()
			existing := baseTask.DeepCopy()
			existing.Labels[key] = "unexpected"
			require.False(t, securityTaskMatchesExpected(existing, expected, scan))

			expected.Labels[key] = "expected"
			existing = expected.DeepCopy()
			existing.Labels[key] = "mismatched"
			require.False(t, securityTaskMatchesExpected(existing, expected, scan))

			existing.Labels[key] = expected.Labels[key]
			existing.Labels[labels.LabelTransactionID] = "request-specific"
			require.True(t, securityTaskMatchesExpected(existing, expected, scan))
		})

		t.Run(key+"/annotation", func(t *testing.T) {
			expected := baseTask.DeepCopy()
			existing := baseTask.DeepCopy()
			existing.Annotations[key] = "unexpected"
			require.False(t, securityTaskMatchesExpected(existing, expected, scan))

			expected.Annotations[key] = "expected"
			existing = expected.DeepCopy()
			existing.Annotations[key] = "mismatched"
			require.False(t, securityTaskMatchesExpected(existing, expected, scan))

			existing.Annotations[key] = expected.Annotations[key]
			existing.Annotations[labels.AnnotationTraceParent] = "request-specific-trace"
			require.True(t, securityTaskMatchesExpected(existing, expected, scan))
		})
	}
}

func TestCreateSecurityScanRunRecoversIdempotentRunAfterUnknownTaskAdmission(t *testing.T) {
	tests := []struct {
		name                    string
		admitFirstTask          bool
		firstCreateErr          error
		firstVerificationOutage bool
		wantTaskCreateCalls     int
	}{
		{name: "recreates absent deterministic task after unknown outcome", firstVerificationOutage: true, wantTaskCreateCalls: 2},
		{name: "verifies already admitted deterministic task", admitFirstTask: true, firstVerificationOutage: true, wantTaskCreateCalls: 1},
		{name: "retries deterministic task after definitive rejection", firstCreateErr: apierrors.NewBadRequest("task rejected"), wantTaskCreateCalls: 2},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			scan := &corev1alpha1.RepositoryScan{
				TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
				ObjectMeta: metav1.ObjectMeta{
					Name: "idempotent-repair", Namespace: "demo", UID: types.UID("idempotent-repair-uid"), Generation: 3,
				},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: "https://github.com/example/repo", Branch: "main",
					AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
			}
			taskCreateCalls := 0
			firstVerificationFailed := false
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
				WithObjects(scan).
				WithInterceptorFuncs(interceptor.Funcs{
					Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
						task, ok := obj.(*corev1alpha1.Task)
						if !ok {
							return c.Create(ctx, obj, opts...)
						}
						taskCreateCalls++
						if taskCreateCalls == 1 {
							if tt.admitFirstTask {
								require.NoError(t, c.Create(ctx, task.DeepCopy(), opts...))
							}
							if tt.firstCreateErr != nil {
								return tt.firstCreateErr
							}
							return errors.New("simulated ambiguous task create response")
						}
						return c.Create(ctx, obj, opts...)
					},
					Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
						if tt.firstVerificationOutage {
							if _, ok := obj.(*corev1alpha1.Task); ok && taskCreateCalls == 1 && !firstVerificationFailed {
								firstVerificationFailed = true
								return errors.New("simulated task verification outage")
							}
						}
						return c.Get(ctx, key, obj, opts...)
					},
				}).Build()

			db, err := sqlite.NewDB(":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			securityStore := sqlite.NewStore(db, ":memory:")
			handlers := NewHandlers(HandlersConfig{Client: cl, SecurityStore: securityStore})
			current := &corev1alpha1.RepositoryScan{}
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(scan), current))

			firstRun, firstErr := handlers.createSecurityScanRun(context.Background(), nil, current, "")
			require.Error(t, firstErr)
			require.Nil(t, firstRun)
			runs, _, err := securityStore.ListScanRuns(context.Background(), scan.Namespace, scan.Name, 10, "")
			require.NoError(t, err)
			require.Len(t, runs, 1)
			require.Equal(t, "pending", runs[0].Phase)
			originalRun := runs[0]

			app := fiber.New()
			app.Post("/security/repositories/:name/scans", handlers.CreateManualSecurityScan)
			req := httptest.NewRequest(http.MethodPost,
				"/security/repositories/"+scan.Name+"/scans?namespace="+scan.Namespace, nil)
			resp, recoveryErr := app.Test(req)
			require.NoError(t, recoveryErr)
			t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
			require.Equal(t, fiber.StatusCreated, resp.StatusCode)
			var recoveredRun store.ScanRun
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&recoveredRun))
			require.Equal(t, originalRun.ID, recoveredRun.ID)
			require.Equal(t, originalRun.RunUID, recoveredRun.RunUID)
			require.Equal(t, tt.wantTaskCreateCalls, taskCreateCalls)

			runs, _, err = securityStore.ListScanRuns(context.Background(), scan.Namespace, scan.Name, 10, "")
			require.NoError(t, err)
			require.Len(t, runs, 1)
			require.Equal(t, originalRun.ID, runs[0].ID)
			require.Equal(t, "pending", runs[0].Phase)

			task := &corev1alpha1.Task{}
			require.NoError(t, cl.Get(context.Background(), types.NamespacedName{
				Namespace: scan.Namespace,
				Name:      originalRun.TaskName,
			}, task))
			require.True(t, metav1.IsControlledBy(task, scan))
			require.Equal(t, originalRun.ID, task.Labels[labels.LabelSecurityScanID])
			require.Equal(t, originalRun.ID, taskEnvValue(task.Spec.Env, security.EnvScanID))

			updated := &corev1alpha1.RepositoryScan{}
			require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(scan), updated))
			require.Equal(t, originalRun.ID, updated.Status.LastScanID)
			require.Equal(t, originalRun.TaskName, updated.Status.LastScanTaskName)
			require.Equal(t, "Scanning", updated.Status.Phase)
		})
	}
}

func TestCreateSecurityScanRunRecoveryUsesImmutableThreatModelInput(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "prompt-snapshot", Namespace: "demo", UID: types.UID("prompt-snapshot-uid"), Generation: 2,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", Branch: "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	createCalls := 0
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if _, ok := obj.(*corev1alpha1.Task); !ok {
					return c.Create(ctx, obj, opts...)
				}
				createCalls++
				if createCalls == 1 {
					return apierrors.NewBadRequest("simulated task rejection")
				}
				return c.Create(ctx, obj, opts...)
			},
		}).
		Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	securityStore := sqlite.NewStore(db, ":memory:")
	require.NoError(t, securityStore.SaveThreatModel(ctx, &store.ThreatModel{
		Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, Content: "original threat model", Source: "user",
	}))
	handlers := NewHandlers(HandlersConfig{Client: cl, SecurityStore: securityStore})
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(scan), current))

	first, firstErr := handlers.createSecurityScanRun(ctx, nil, current, "")
	require.Error(t, firstErr)
	require.Nil(t, first)
	runs, _, err := securityStore.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, "pending", runs[0].Phase)
	input, err := securityStore.GetSecurityRunTaskInput(ctx, scan.Namespace, runs[0].RunUID, security.StageThreatModel)
	require.NoError(t, err)
	require.Equal(t, "original threat model", input.Content)
	require.EqualValues(t, 1, input.SourceVersion)

	require.NoError(t, securityStore.SaveThreatModel(ctx, &store.ThreatModel{
		Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, Content: "replacement threat model", Source: "user",
	}))
	recovered, err := handlers.createSecurityScanRun(ctx, nil, current, "")
	require.NoError(t, err)
	require.Equal(t, runs[0].ID, recovered.ID)
	task := &corev1alpha1.Task{}
	require.NoError(t, cl.Get(ctx, types.NamespacedName{Namespace: scan.Namespace, Name: recovered.TaskName}, task))
	require.Contains(t, task.Spec.Prompt, "original threat model")
	require.NotContains(t, task.Spec.Prompt, "replacement threat model")
}

func TestCreateSecurityScanRunReplaysAfterMapperResolvesHeadCommit(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "resolved-head-replay", Namespace: "demo", UID: types.UID("resolved-head-replay-uid"), Generation: 2,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", Branch: "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan).
		Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	securityStore := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(HandlersConfig{
		Client: cl, SecurityStore: securityStore,
		IntegrityConfig: security.IntegrityConfig{PinnedScanTargetsEnabled: true},
	})
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(scan), current))

	created, err := handlers.createSecurityScanRun(ctx, nil, current, "")
	require.NoError(t, err)
	requestKey := created.RequestIdempotencyKey
	resolvedHead := strings.Repeat("a", 40)
	created.Phase = securityScanRunPhaseRunning
	created.HeadCommit = resolvedHead
	created.ResolvedTargetKey = security.ResolvedTargetKey(
		security.RepositoryTargetID(current), created.BaseCommit, resolvedHead, current.Spec.SubPath, created.PolicyDigest,
	)
	created.TargetReceiptID = "target_receipt_resolved_head"
	require.NoError(t, securityStore.UpdateScanRun(ctx, created))

	app := fiber.New()
	app.Post("/security/repositories/:name/scans", handlers.CreateManualSecurityScan)
	req := httptest.NewRequest(http.MethodPost,
		"/security/repositories/"+scan.Name+"/scans?namespace="+scan.Namespace, nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
	require.Equal(t, fiber.StatusCreated, resp.StatusCode)
	var replayed store.ScanRun
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&replayed))
	require.Equal(t, created.ID, replayed.ID)
	require.Equal(t, requestKey, replayed.RequestIdempotencyKey)
	require.Equal(t, resolvedHead, replayed.HeadCommit)

	runs, _, err := securityStore.ListScanRuns(ctx, scan.Namespace, scan.Name, 10, "")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, created.ID, runs[0].ID)
}

func TestCreateSecurityScanRunRechecksIdempotencyAfterActiveTaskObserved(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "active-recheck", Namespace: "demo", UID: types.UID("active-recheck-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", Branch: "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}, &corev1alpha1.Task{}).
		WithObjects(scan).
		Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	baseStore := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(HandlersConfig{Client: cl, SecurityStore: baseStore})
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(scan), current))

	created, err := handlers.createSecurityScanRun(context.Background(), nil, current, "")
	require.NoError(t, err)
	task := &corev1alpha1.Task{}
	require.NoError(t, cl.Get(context.Background(), types.NamespacedName{Namespace: scan.Namespace, Name: created.TaskName}, task))
	task.Status.Phase = corev1alpha1.TaskPhasePending
	require.NoError(t, cl.Status().Update(context.Background(), task))

	handlers.securityStore = &delayedScanRunListStore{SecurityStore: baseStore, hideNext: true}
	recovered, err := handlers.createSecurityScanRun(context.Background(), nil, current, "")
	require.NoError(t, err)
	require.Equal(t, created.ID, recovered.ID)
	require.Equal(t, created.RunUID, recovered.RunUID)
}

func TestCreateSecurityScanRunReturnsConflictForUnrelatedAtomicReservation(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "atomic-reservation", Namespace: "demo", UID: types.UID("atomic-reservation-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", Branch: "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan).
		Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	baseStore := sqlite.NewStore(db, ":memory:")
	reservationStore := &competingRepositoryReservationStore{
		SecurityStore: baseStore, SecurityRunTaskInputStore: baseStore,
	}
	handlers := NewHandlers(HandlersConfig{
		Client: cl, SecurityStore: reservationStore, SecurityRunTaskInputStore: reservationStore,
	})
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(scan), current))

	run, createErr := handlers.createSecurityScanRun(context.Background(), nil, current, "")
	require.Nil(t, run)
	var fiberErr *fiber.Error
	require.ErrorAs(t, createErr, &fiberErr)
	require.Equal(t, fiber.StatusConflict, fiberErr.Code)
	require.NotNil(t, reservationStore.injectedRun)
	task := &corev1alpha1.Task{}
	err = cl.Get(context.Background(), types.NamespacedName{
		Namespace: scan.Namespace, Name: reservationStore.injectedRun.TaskName,
	}, task)
	require.True(t, apierrors.IsNotFound(err), "losing request created a Task: %v", err)
	runs, _, err := baseStore.ListScanRuns(context.Background(), scan.Namespace, scan.Name, 10, "")
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, reservationStore.injectedRun.ID, runs[0].ID)
}

func TestCreateSecurityScanRunReturnsConflictWhenCompetingRunCannotBeRecovered(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{
		TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryScan"},
		ObjectMeta: metav1.ObjectMeta{
			Name: "recovery-error", Namespace: "demo", UID: types.UID("recovery-error-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: "https://github.com/example/repo", Branch: "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	cl := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
		WithObjects(scan).
		Build()

	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	baseStore := sqlite.NewStore(db, ":memory:")
	recoveryStore := &scanRunRecoveryErrorStore{
		SecurityStore: baseStore, SecurityRunTaskInputStore: baseStore, recoveryErr: store.ErrNotFound,
	}
	handlers := NewHandlers(HandlersConfig{
		Client: cl, SecurityStore: recoveryStore, SecurityRunTaskInputStore: recoveryStore,
	})
	current := &corev1alpha1.RepositoryScan{}
	require.NoError(t, cl.Get(context.Background(), client.ObjectKeyFromObject(scan), current))

	run, createErr := handlers.createSecurityScanRun(context.Background(), nil, current, "")
	require.Nil(t, run)
	var fiberErr *fiber.Error
	require.ErrorAs(t, createErr, &fiberErr)
	require.Equal(t, fiber.StatusConflict, fiberErr.Code)
}

func TestRepairRepositoryScanRunStatusSkipsStaleRuns(t *testing.T) {
	tests := []struct {
		name            string
		expectedUID     types.UID
		expectedGen     int64
		currentUID      types.UID
		currentGen      int64
		oldPhase        string
		oldCompleted    bool
		addNewerRun     bool
		wantStatusID    string
		wantStatusPhase string
	}{
		{
			name: "terminal run", expectedUID: "scan-uid", expectedGen: 2, currentUID: "scan-uid", currentGen: 2,
			oldPhase: "succeeded", oldCompleted: true, wantStatusID: "scan-old", wantStatusPhase: "Ready",
		},
		{
			name: "newer run", expectedUID: "scan-uid", expectedGen: 2, currentUID: "scan-uid", currentGen: 2,
			oldPhase: "pending", addNewerRun: true, wantStatusID: "scan-new", wantStatusPhase: "Scanning",
		},
		{
			name: "repository recreation", expectedUID: "old-scan-uid", expectedGen: 4,
			currentUID: "new-scan-uid", currentGen: 1, oldPhase: "pending",
			wantStatusID: "scan-new", wantStatusPhase: "Ready",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			expectedScan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "status-race", Namespace: "demo", UID: tt.expectedUID, Generation: tt.expectedGen},
			}
			currentScan := expectedScan.DeepCopy()
			currentScan.UID = tt.currentUID
			currentScan.Generation = tt.currentGen
			currentScan.Status.Phase = tt.wantStatusPhase
			currentScan.Status.LastScanID = tt.wantStatusID
			preservedQuality := &corev1alpha1.RepositoryScanQualityStatus{
				SchemaVersion: 1, ObservedRepositoryScanUID: "preserved-uid", ObservedGeneration: currentScan.Generation,
				CoverageStatus: string(store.CoverageStatusPartial),
			}
			currentScan.Status.Quality = preservedQuality.DeepCopy()
			meta.SetStatusCondition(&currentScan.Status.Conditions, metav1.Condition{
				Type: "QualityReady", Status: metav1.ConditionTrue, Reason: "Preserved",
				ObservedGeneration: currentScan.Generation,
			})
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
				WithObjects(currentScan).
				Build()

			db, err := sqlite.NewDB(":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			securityStore := sqlite.NewStore(db, ":memory:")
			started := time.Now().UTC().Add(-time.Minute)
			oldRun := &store.ScanRun{
				ID: "scan-old", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Namespace: "demo", RepositoryScan: expectedScan.Name, RepositoryScanUID: string(expectedScan.UID),
				RepositoryScanGeneration: expectedScan.Generation, TaskName: "status-race-task", Mode: "manual",
				Phase: tt.oldPhase, StartedAt: started, Quality: store.LegacyScanQuality(),
			}
			if tt.oldCompleted {
				completed := started.Add(30 * time.Second)
				oldRun.CompletedAt = &completed
			}
			require.NoError(t, securityStore.CreateScanRun(ctx, oldRun))
			if tt.addNewerRun {
				newer := *oldRun
				newer.ID = "scan-new"
				newer.RunUID = "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
				newer.TaskName = "status-race-new-task"
				newer.StartedAt = started.Add(time.Minute)
				newer.CompletedAt = nil
				newer.Phase = "pending"
				require.NoError(t, securityStore.CreateScanRun(ctx, &newer))
			}

			handlers := NewHandlers(HandlersConfig{Client: cl, SecurityStore: securityStore})
			require.NoError(t, handlers.repairRepositoryScanRunStatus(ctx, expectedScan, oldRun, &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: oldRun.TaskName, Namespace: oldRun.Namespace},
			}))
			updated := &corev1alpha1.RepositoryScan{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(currentScan), updated))
			require.Equal(t, tt.wantStatusID, updated.Status.LastScanID)
			require.Equal(t, tt.wantStatusPhase, updated.Status.Phase)
			require.Equal(t, preservedQuality, updated.Status.Quality)
			qualityReady := meta.FindStatusCondition(updated.Status.Conditions, "QualityReady")
			require.NotNil(t, qualityReady)
			require.Equal(t, "Preserved", qualityReady.Reason)
		})
	}
}

func TestRepairRepositoryScanRunStatusProjectsInitialQualityByGate(t *testing.T) {
	tests := []struct {
		name                 string
		qualityWritesEnabled bool
	}{
		{name: "enabled", qualityWritesEnabled: true},
		{name: "disabled"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{
					Name: "quality-start", Namespace: "demo", UID: types.UID("quality-start-uid"), Generation: 3,
				},
				Spec: corev1alpha1.RepositoryScanSpec{
					ValidationMode: "full", AnalysisIsolationPolicy: "require-hardened",
				},
			}
			scan.Status.Quality = &corev1alpha1.RepositoryScanQualityStatus{
				SchemaVersion: 1, ObservedRepositoryScanUID: "stale-uid", ObservedGeneration: 2,
				CoverageStatus: string(store.CoverageStatusFailed),
			}
			meta.SetStatusCondition(&scan.Status.Conditions, metav1.Condition{
				Type: "QualityReady", Status: metav1.ConditionTrue, Reason: "StaleQuality", ObservedGeneration: 2,
			})
			cl := fake.NewClientBuilder().
				WithScheme(scheme).
				WithStatusSubresource(&corev1alpha1.RepositoryScan{}).
				WithObjects(scan).
				Build()

			db, err := sqlite.NewDB(":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			securityStore := sqlite.NewStore(db, ":memory:")
			run := &store.ScanRun{
				ID: "scan-quality-start", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
				RepositoryScanGeneration: scan.Generation, TaskName: "quality-start-task", Mode: "manual", Phase: "pending",
				StartedAt: time.Now().UTC(), Quality: initialAPIScanQuality(scan, false),
			}
			run.Quality.ReasonCodes = []string{"initial-quality"}
			require.NoError(t, securityStore.CreateScanRun(ctx, run))

			handlers := NewHandlers(HandlersConfig{
				Client: cl, SecurityStore: securityStore,
				IntegrityConfig: security.IntegrityConfig{QualityStateWritesEnabled: tt.qualityWritesEnabled},
			})
			require.NoError(t, handlers.repairRepositoryScanRunStatus(ctx, scan, run, &corev1alpha1.Task{
				ObjectMeta: metav1.ObjectMeta{Name: run.TaskName, Namespace: run.Namespace},
			}))

			updated := &corev1alpha1.RepositoryScan{}
			require.NoError(t, cl.Get(ctx, client.ObjectKeyFromObject(scan), updated))
			require.Equal(t, "Scanning", updated.Status.Phase)
			require.Equal(t, run.ID, updated.Status.LastScanID)
			require.Equal(t, run.TaskName, updated.Status.LastScanTaskName)

			qualityReady := meta.FindStatusCondition(updated.Status.Conditions, "QualityReady")
			if !tt.qualityWritesEnabled {
				require.Nil(t, updated.Status.Quality)
				require.Nil(t, qualityReady)
				return
			}

			require.Equal(t, &corev1alpha1.RepositoryScanQualityStatus{
				SchemaVersion:             int32(run.Quality.SchemaVersion),
				ObservedRepositoryScanUID: run.RepositoryScanUID,
				ObservedGeneration:        run.RepositoryScanGeneration,
				InventoryCoverageStatus:   string(run.Quality.InventoryCoverageStatus),
				CandidateCoverageStatus:   string(run.Quality.CandidateCoverageStatus),
				CoverageStatus:            string(run.Quality.CoverageStatus),
				ValidationScope:           string(run.Quality.ValidationScope),
				ValidationExecution:       string(run.Quality.ValidationExecution),
				AttackPathExecution:       string(run.Quality.AttackPathExecution),
				AnalysisAttestationLevel:  string(run.Quality.AnalysisAttestationLevel),
				TargetVerification:        string(run.Quality.TargetVerification),
				BundleStatus:              string(run.Quality.BundleStatus),
				AuthorizationStatus:       string(run.Quality.AuthorizationStatus),
				IsolationStatus:           string(run.Quality.IsolationStatus),
				ReasonCodes:               []string{"initial-quality"},
			}, updated.Status.Quality)
			require.NotNil(t, qualityReady)
			require.Equal(t, metav1.ConditionUnknown, qualityReady.Status)
			require.Equal(t, "QualityPending", qualityReady.Reason)
			require.Equal(t, scan.Generation, qualityReady.ObservedGeneration)
		})
	}
}

func TestScanRunMatchesIdempotencyKeySupportsCurrentAndLegacyAliases(t *testing.T) {
	const key = "req_test"
	tests := []struct {
		name string
		run  *store.ScanRun
		want bool
	}{
		{name: "current alias", run: &store.ScanRun{RequestIdempotencyKey: key}, want: true},
		{name: "legacy alias", run: &store.ScanRun{IdempotencyKey: key}, want: true},
		{name: "matching aliases", run: &store.ScanRun{RequestIdempotencyKey: key, IdempotencyKey: key}, want: true},
		{name: "conflicting aliases fail closed", run: &store.ScanRun{RequestIdempotencyKey: key, IdempotencyKey: "req_other"}},
		{name: "different alias", run: &store.ScanRun{RequestIdempotencyKey: "req_other"}},
		{name: "nil run", run: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, scanRunMatchesIdempotencyKey(tt.run, key))
		})
	}
}

func taskEnvValue(env []corev1.EnvVar, name string) string {
	for i := range env {
		if env[i].Name == name {
			return env[i].Value
		}
	}
	return ""
}

func TestGenerateSecurityPatch_ContextTokenTransactionContextAuthorization(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	repoURL := securityTestRepoURL

	tests := []struct {
		name string
		tctx map[string]any
		want int
	}{
		{
			name: "matching repo branch and agent allowed",
			tctx: map[string]any{
				"namespace":     "demo",
				"repo":          repoURL,
				"branch":        "main",
				"agent":         "demo/patch",
				"allowedAgents": []any{"demo/patch"},
			},
			want: http.StatusCreated,
		},
		{
			name: "mismatched repo denied",
			tctx: map[string]any{
				"namespace": "demo",
				"repo":      "https://github.com/sozercan/other",
				"branch":    "main",
				"agent":     "demo/patch",
			},
			want: http.StatusForbidden,
		},
		{
			name: "mismatched branch denied",
			tctx: map[string]any{
				"namespace": "demo",
				"repo":      repoURL,
				"branch":    "release",
				"agent":     "demo/patch",
			},
			want: http.StatusForbidden,
		},
		{
			name: "mismatched agent denied",
			tctx: map[string]any{
				"namespace": "demo",
				"repo":      repoURL,
				"branch":    "main",
				"agent":     "demo/analysis",
			},
			want: http.StatusForbidden,
		},
		{
			name: "disallowed allowed agents denied",
			tctx: map[string]any{
				"namespace":     "demo",
				"repo":          repoURL,
				"branch":        "main",
				"allowedAgents": []any{"demo/analysis"},
			},
			want: http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			patchAgent := corev1alpha1.AgentReference{Name: "patch"}
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{
					Name: "scan-1", Namespace: "demo", UID: types.UID("scan-1-uid"), Generation: 1,
				},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL:          repoURL,
					Branch:           "main",
					AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
					PatchAgentRef:    &patchAgent,
				},
			}
			app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan)

			ctx := context.Background()
			require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
				ID:             "finding-1",
				Namespace:      "demo",
				RepositoryScan: "scan-1",
				ScanRunID:      "scan-run-1",
				Fingerprint:    "fp-1",
				Title:          "Command injection",
				Summary:        "Unsanitized user input reaches shell execution.",
				Severity:       "critical",
				Confidence:     "high",
				State:          "validated",
				RootCause:      "Shell command arguments are concatenated directly.",
				Remediation:    "Use argument arrays and validate inputs.",
			}))
			require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
				ID: "scan-run-1", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
				RepositoryScanGeneration: scan.Generation, HeadCommit: strings.Repeat("a", 40),
				TaskName: "scan-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
				Quality: store.LegacyScanQuality(),
			}))

			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx":  tt.tctx,
			})
			req := httptest.NewRequest(http.MethodPost, "/security/findings/finding-1/patch?namespace=demo", nil)
			req.Header.Set(TransactionTokenHeaderName, token)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, tt.want, resp.StatusCode)

			if tt.want != http.StatusCreated {
				return
			}
			var proposal store.PatchProposal
			require.NoError(t, json.NewDecoder(resp.Body).Decode(&proposal))
			require.NotEmpty(t, proposal.TaskName)

			task := &corev1alpha1.Task{}
			require.NoError(t, handlers.client.Get(ctx, clientObjectKey(proposal.TaskName), task))
			require.NotNil(t, task.Spec.AgentRef)
			require.Equal(t, "patch", task.Spec.AgentRef.Name)
			require.NotNil(t, task.Spec.RequestedBy)
			require.Equal(t, testContextTokenSubject, task.Spec.RequestedBy.Subject)
			require.NotNil(t, task.Spec.Transaction)
			require.Equal(t, testContextTokenTransactionID, task.Spec.Transaction.ID)
		})
	}
}

func TestCreateManualSecurityScan_ContextTokenTransactionContextAuthorizationDenials(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	repoURL := securityTestRepoURL

	tests := []struct {
		name    string
		scanRef string
		tctx    map[string]any
	}{
		{
			name: "mismatched repo denied",
			tctx: map[string]any{
				"namespace": "demo",
				"repo":      "https://github.com/sozercan/other",
				"branch":    "main",
				"agent":     "demo/analysis",
			},
		},
		{
			name: "mismatched branch denied",
			tctx: map[string]any{
				"namespace": "demo",
				"repo":      repoURL,
				"branch":    "release",
				"agent":     "demo/analysis",
			},
		},
		{
			name:    "mismatched ref denied",
			scanRef: "refs/tags/allowed",
			tctx: map[string]any{
				"namespace": "demo",
				"repo":      repoURL,
				"branch":    "main",
				"ref":       "refs/tags/disallowed",
				"agent":     "demo/analysis",
			},
		},
		{
			name:    "branch-only token denies ref checkout",
			scanRef: "refs/tags/allowed",
			tctx: map[string]any{
				"namespace": "demo",
				"repo":      repoURL,
				"branch":    "main",
				"agent":     "demo/analysis",
			},
		},
		{
			name: "mismatched agent denied",
			tctx: map[string]any{
				"namespace": "demo",
				"repo":      repoURL,
				"branch":    "main",
				"agent":     "demo/other",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "scan-1",
					Namespace: "demo",
				},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL:          repoURL,
					Branch:           "main",
					Ref:              tt.scanRef,
					AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
			}
			app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan)
			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx":  tt.tctx,
			})
			req := httptest.NewRequest(http.MethodPost, "/security/repositories/scan-1/scans?namespace=demo", nil)
			req.Header.Set(TransactionTokenHeaderName, token)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)

			var tasks corev1alpha1.TaskList
			require.NoError(t, handlers.client.List(context.Background(), &tasks, client.InNamespace("demo")))
			require.Empty(t, tasks.Items)
		})
	}
}

func TestCreateManualSecurityScan_ContextTokenAllowsRefOnlyWorkspaceWithBranchAndRef(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	repoURL := securityTestRepoURL
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scan-1", Namespace: "demo", UID: types.UID("scan-mutations-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          repoURL,
			Ref:              "refs/tags/v1.0.0",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan)
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityWrite,
		"tctx": map[string]any{
			"namespace": "demo",
			"repo":      repoURL,
			"branch":    "release",
			"ref":       "refs/tags/v1.0.0",
			"agent":     "demo/analysis",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/security/repositories/scan-1/scans?namespace=demo", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var run store.ScanRun
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	task := &corev1alpha1.Task{}
	require.NoError(t, handlers.client.Get(context.Background(), clientObjectKey(run.TaskName), task))
	require.NotNil(t, task.Spec.AgentRuntime)
	require.NotNil(t, task.Spec.AgentRuntime.Workspace)
	require.Empty(t, task.Spec.AgentRuntime.Workspace.Branch)
	require.Equal(t, "refs/tags/v1.0.0", task.Spec.AgentRuntime.Workspace.Ref)

	updatedScan := &corev1alpha1.RepositoryScan{}
	require.NoError(t, handlers.client.Get(context.Background(), client.ObjectKey{Namespace: "demo", Name: "scan-1"}, updatedScan))
	require.Equal(t, "Scanning", updatedScan.Status.Phase)
	require.Nil(t, updatedScan.Status.Quality)
	qualityReady := meta.FindStatusCondition(updatedScan.Status.Conditions, "QualityReady")
	require.Nil(t, qualityReady)
}

func TestRepositoryScanMutations_ContextTokenTransactionContextAuthorizationDenials(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	repoURL := securityTestRepoURL
	createBody := fmt.Sprintf(`{
		"name":"scan-create",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"branch":"main",
			"analysisAgentRef":{"name":"analysis"}
		}
	}`, securityTestRepoURL)
	updateBody := fmt.Sprintf(`{
		"spec":{
			"repoURL":%q,
			"branch":"main",
			"analysisAgentRef":{"name":"analysis"}
		}
	}`, securityTestRepoURL)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		objs   []runtime.Object
	}{
		{
			name:   "create repository scan mismatched repo denied",
			method: http.MethodPost,
			path:   "/security/repositories",
			body:   createBody,
		},
		{
			name:   "update repository scan mismatched repo denied",
			method: http.MethodPut,
			path:   "/security/repositories/scan-1?namespace=demo",
			body:   updateBody,
			objs: []runtime.Object{
				&corev1alpha1.RepositoryScan{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "scan-1",
						Namespace: "demo",
					},
					Spec: corev1alpha1.RepositoryScanSpec{
						RepoURL:          repoURL,
						Branch:           "main",
						AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, tt.objs...)
			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx": map[string]any{
					"namespace": "demo",
					"repo":      "https://github.com/sozercan/other",
					"branch":    "main",
					"agent":     "demo/analysis",
				},
			})
			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set(TransactionTokenHeaderName, token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)

			var got corev1alpha1.RepositoryScan
			err = handlers.client.Get(context.Background(), clientObjectKey("scan-create"), &got)
			require.Error(t, err)
		})
	}
}

func TestCreateRepositoryScanPolicyRefs(t *testing.T) {
	ctx := context.Background()
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	policy := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: "demo", Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}},
		Data:       map[string]string{"scan": "Focus on operator repositories.", "fp": "Ignore public docs examples."},
	}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, policy)
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite + " " + ContextTokenScopeConfigMapsRead})
	body := fmt.Sprintf(`{
		"name":"scan-policy-test",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"analysisAgentRef":{"name":"analysis"},
			"customScanInstructionsRef":{"name":"scan-policy","key":"scan"},
			"falsePositivePolicyRef":{"name":"scan-policy","key":"fp"}
		}
	}`, securityTestRepoURL)
	req := httptest.NewRequest(http.MethodPost, "/security/repositories", strings.NewReader(body))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var got corev1alpha1.RepositoryScan
	require.NoError(t, handlers.client.Get(ctx, client.ObjectKey{Namespace: "demo", Name: "scan-policy-test"}, &got))
	require.Equal(t, "scan-policy", got.Spec.CustomScanInstructionsRef.Name)
	require.Equal(t, "fp", got.Spec.FalsePositivePolicyRef.Key)
}

func TestCreateRepositoryScanPolicyRefRequiresConfigMapReadScope(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: "demo", Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}}, Data: map[string]string{"policy": "policy"}}
	app, _ := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, policy)
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite})
	body := fmt.Sprintf(`{
		"name":"scan-policy-test",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"analysisAgentRef":{"name":"analysis"},
			"customScanInstructionsRef":{"name":"scan-policy"}
		}
	}`, securityTestRepoURL)
	req := httptest.NewRequest(http.MethodPost, "/security/repositories", strings.NewReader(body))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestCreateRepositoryScanPolicyRefMissingKeyFails(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: "demo", Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}}, Data: map[string]string{"other": "policy"}}
	app, _ := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, policy)
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite + " " + ContextTokenScopeConfigMapsRead})
	body := fmt.Sprintf(`{
		"name":"scan-policy-test",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"analysisAgentRef":{"name":"analysis"},
			"customScanInstructionsRef":{"name":"scan-policy","key":"missing"}
		}
	}`, securityTestRepoURL)
	req := httptest.NewRequest(http.MethodPost, "/security/repositories", strings.NewReader(body))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCreateRepositoryScanPolicyRefOversizedFails(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: "demo", Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}}, Data: map[string]string{"policy": strings.Repeat("a", security.MaxCustomPolicyBytes+1)}}
	app, _ := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, policy)
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite + " " + ContextTokenScopeConfigMapsRead})
	body := fmt.Sprintf(`{
		"name":"scan-policy-test",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"analysisAgentRef":{"name":"analysis"},
			"customScanInstructionsRef":{"name":"scan-policy"}
		}
	}`, securityTestRepoURL)
	req := httptest.NewRequest(http.MethodPost, "/security/repositories", strings.NewReader(body))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestCreateRepositoryScanPolicyRefOtherNamespaceFails(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	policy := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "scan-policy", Namespace: "other", Labels: map[string]string{security.PolicyConfigMapAllowedLabel: "true"}}, Data: map[string]string{"policy": "policy"}}
	app, _ := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, policy)
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite + " " + ContextTokenScopeConfigMapsRead})
	body := fmt.Sprintf(`{
		"name":"scan-policy-test",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"analysisAgentRef":{"name":"analysis"},
			"customScanInstructionsRef":{"name":"scan-policy"}
		}
	}`, securityTestRepoURL)
	req := httptest.NewRequest(http.MethodPost, "/security/repositories", strings.NewReader(body))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestRepositoryScanMutations_ContextTokenRefAuthorizationDenials(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	repoURL := securityTestRepoURL
	createBody := fmt.Sprintf(`{
		"name":"scan-create",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"branch":"main",
			"ref":"refs/tags/disallowed",
			"analysisAgentRef":{"name":"analysis"}
		}
	}`, securityTestRepoURL)
	updateBody := fmt.Sprintf(`{
		"spec":{
			"repoURL":%q,
			"branch":"main",
			"ref":"refs/tags/disallowed",
			"analysisAgentRef":{"name":"analysis"}
		}
	}`, securityTestRepoURL)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			name:   "create repository scan mismatched ref denied",
			method: http.MethodPost,
			path:   "/security/repositories",
			body:   createBody,
		},
		{
			name:   "update repository scan mismatched ref denied",
			method: http.MethodPut,
			path:   "/security/repositories/scan-1?namespace=demo",
			body:   updateBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := securityAuthzTestRepositoryScan("scan-1", repoURL)
			existing.Spec.Ref = "refs/tags/allowed"
			app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, existing)
			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx": map[string]any{
					"namespace": "demo",
					"repo":      repoURL,
					"branch":    "main",
					"ref":       "refs/tags/allowed",
					"agent":     "demo/analysis",
				},
			})

			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set(TransactionTokenHeaderName, token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)

			var created corev1alpha1.RepositoryScan
			err = handlers.client.Get(context.Background(), clientObjectKey("scan-create"), &created)
			require.Error(t, err)

			var updated corev1alpha1.RepositoryScan
			require.NoError(t, handlers.client.Get(context.Background(), clientObjectKey("scan-1"), &updated))
			require.Equal(t, "refs/tags/allowed", updated.Spec.Ref)
		})
	}
}

func TestRepositoryScanMutations_ContextTokenBranchOnlyDeniesRef(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	repoURL := securityTestRepoURL
	createBody := fmt.Sprintf(`{
		"name":"scan-create",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"branch":"main",
			"ref":"refs/tags/disallowed",
			"analysisAgentRef":{"name":"analysis"}
		}
	}`, securityTestRepoURL)
	updateBody := fmt.Sprintf(`{
		"spec":{
			"repoURL":%q,
			"branch":"main",
			"ref":"refs/tags/disallowed",
			"analysisAgentRef":{"name":"analysis"}
		}
	}`, securityTestRepoURL)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
	}{
		{
			name:   "create repository scan ref denied by branch-only token",
			method: http.MethodPost,
			path:   "/security/repositories",
			body:   createBody,
		},
		{
			name:   "update repository scan ref denied by branch-only token",
			method: http.MethodPut,
			path:   "/security/repositories/scan-1?namespace=demo",
			body:   updateBody,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			existing := securityAuthzTestRepositoryScan("scan-1", repoURL)
			app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, existing)
			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx": map[string]any{
					"namespace": "demo",
					"repo":      repoURL,
					"branch":    "main",
					"agent":     "demo/analysis",
				},
			})

			req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
			req.Header.Set(TransactionTokenHeaderName, token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)

			var created corev1alpha1.RepositoryScan
			err = handlers.client.Get(context.Background(), clientObjectKey("scan-create"), &created)
			require.Error(t, err)

			var updated corev1alpha1.RepositoryScan
			require.NoError(t, handlers.client.Get(context.Background(), clientObjectKey("scan-1"), &updated))
			require.Empty(t, updated.Spec.Ref)
		})
	}
}

func securityAuthzTestRepositoryScan(name, repoURL string) *corev1alpha1.RepositoryScan {
	return &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: "demo",
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          repoURL,
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
}

func securityAuthzTestTctx(repoURL string) map[string]any {
	return map[string]any{
		"namespace": "demo",
		"repo":      repoURL,
		"branch":    "main",
		"agent":     "demo/analysis",
	}
}

func securityAuthzTestBundle(t *testing.T, scan *corev1alpha1.RepositoryScan, generation int64) *store.SecurityScanBundle {
	t.Helper()
	started := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)
	completed := started.Add(time.Minute)
	sealedAt := completed.Add(time.Second)
	publicRunID := "scan-run-api"
	built, err := securitybundle.Build(securitybundle.Input{
		Manifest: securitybundle.ManifestInput{
			SchemaVersion: securitybundle.SchemaVersion,
			Repository:    securitybundle.RepositoryIdentity{Provider: "github", RepositoryID: "target-v1", RepoURL: scan.Spec.RepoURL},
			Target: securitybundle.TargetSnapshot{
				CommitSHA: strings.Repeat("a", 40), TreeDigest: "sha256:" + strings.Repeat("b", 64), TargetID: "target-v1",
				ReceiptID: "target-receipt", ReceiptDigest: "sha256:" + strings.Repeat("c", 64),
			},
			ThreatModel: securitybundle.ThreatModelInput{Version: "1", Content: "# Threat model\n"},
			Quality: securitybundle.QualitySummary{
				InventoryCoverage: "complete", CandidateCoverage: "complete", Coverage: "complete",
				ValidationScope: "all", ValidationExecution: "complete", AttackPathExecution: "complete",
				AnalysisAttestation: "tool-observed", TargetVerification: "verified", Authorization: "verified", Isolation: "hardened",
			},
			Versions:      securitybundle.ComponentVersions{Schema: "security-bundle-v1", Controller: "controller-v1", Additional: map[string]string{}},
			OccurrenceIDs: []string{}, AssessmentIDs: []string{}, StageReceiptIDs: []string{}, EvidenceReceiptIDs: []string{},
			Metadata: map[string]string{
				security.BundleMetadataAuthorizationBranch:         scan.Spec.Branch,
				security.BundleMetadataAuthorizationRef:            scan.Spec.Ref,
				security.BundleMetadataAuthorizationAgentName:      scan.Spec.AnalysisAgentRef.Name,
				security.BundleMetadataAuthorizationAgentNamespace: scan.Namespace,
			},
			Run: securitybundle.RunEnvelope{
				RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PublicRunID: &publicRunID,
				Namespace: scan.Namespace, RepositoryScanName: scan.Name, RepositoryScanUID: string(scan.UID),
				RepositoryScanGeneration: generation, StartedAt: started, CompletedAt: &completed, SealedAt: sealedAt,
			},
		},
		Findings: securitybundle.FindingsInput{SchemaVersion: securitybundle.SchemaVersion, Findings: []securitybundle.Finding{}, Metadata: map[string]string{}},
		Coverage: securitybundle.CoverageInput{
			SchemaVersion: securitybundle.SchemaVersion, InventoryStatus: "complete", CandidateStatus: "complete", CoverageStatus: "complete",
			Inventory: []securitybundle.InventoryCoverageEntry{}, Candidates: []securitybundle.CandidateCoverageEntry{},
			Stages: []securitybundle.StageCoverageEntry{}, Metadata: map[string]string{},
		},
		Evidence: []securitybundle.EvidenceBlobInput{},
	}, securitybundle.DefaultLimits())
	require.NoError(t, err)
	evidence := built.Evidence
	if evidence == nil {
		evidence = []securitybundle.EvidenceBlob{}
	}
	evidenceJSON, err := json.Marshal(evidence)
	require.NoError(t, err)
	return &store.SecurityScanBundle{
		ID: "bundle_api", Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: generation, ScanRunID: publicRunID,
		RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", Version: securitybundle.SchemaVersion,
		ManifestJSON: built.ManifestJSON, FindingsJSON: built.FindingsJSON, CoverageJSON: built.CoverageJSON,
		EvidenceJSON: evidenceJSON, ContentDigest: built.Roots.ContentDigest, RunReceiptDigest: built.Roots.RunReceiptDigest,
		SealedAt: sealedAt,
	}
}

func TestUpdateRepositoryScan_ContextTokenAuthorizesExistingScanBeforeRequestBody(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	existing := securityAuthzTestRepositoryScan("scan-1", "https://github.com/sozercan/other")
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, existing)

	bodyBytes, err := json.Marshal(UpdateRepositoryScanRequest{
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          securityTestRepoURL,
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	})
	require.NoError(t, err)
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityWrite,
		"tctx":  securityAuthzTestTctx(securityTestRepoURL),
	})

	req := httptest.NewRequest(http.MethodPut, "/security/repositories/scan-1?namespace=demo", strings.NewReader(string(bodyBytes)))
	req.Header.Set(TransactionTokenHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	var got corev1alpha1.RepositoryScan
	require.NoError(t, handlers.client.Get(context.Background(), clientObjectKey("scan-1"), &got))
	require.Equal(t, "https://github.com/sozercan/other", got.Spec.RepoURL)
}

func TestListRepositoryScansLatestRunsEnrichment(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityRead})
	scanA := securityAuthzTestRepositoryScan("scan-a", securityTestRepoURL)
	scanA.UID = types.UID("scan-a-uid")
	scanA.Generation = 2
	scanB := securityAuthzTestRepositoryScan("scan-b", "https://github.com/sozercan/other")
	scanB.UID = types.UID("scan-b-uid")
	scanB.Generation = 3
	app, handlers := setupSecurityHandlersWithAuthzFixture(
		t, ctxTokenConfig, ContextTokenAuthorizationModeOff, scanA, scanB,
	)
	now := time.Now().UTC()
	for _, run := range []*store.ScanRun{
		{
			ID: "scan-a-current", Namespace: "demo", RepositoryScan: scanA.Name,
			RepositoryScanUID: string(scanA.UID), RepositoryScanGeneration: scanA.Generation,
			Mode: "initial", Phase: "succeeded", StartedAt: now, Quality: store.LegacyScanQuality(),
		},
		{
			ID: "scan-a-old-incarnation", Namespace: "demo", RepositoryScan: scanA.Name,
			RepositoryScanUID: "old-scan-a-uid", RepositoryScanGeneration: 1,
			Mode: "initial", Phase: "succeeded", StartedAt: now.Add(time.Minute), Quality: store.LegacyScanQuality(),
		},
		{
			ID: "scan-b-current", Namespace: "demo", RepositoryScan: scanB.Name,
			RepositoryScanUID: string(scanB.UID), RepositoryScanGeneration: scanB.Generation,
			Mode: "initial", Phase: "succeeded", StartedAt: now.Add(-time.Minute), Quality: store.LegacyScanQuality(),
		},
	} {
		require.NoError(t, handlers.securityStore.CreateScanRun(context.Background(), run))
	}

	plainReq := httptest.NewRequest(http.MethodGet, "/security/repositories?namespace=demo", nil)
	plainReq.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(plainReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var plain map[string]json.RawMessage
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&plain))
	_, enriched := plain["latestScanRuns"]
	require.False(t, enriched)

	enrichedReq := httptest.NewRequest(
		http.MethodGet, "/security/repositories?namespace=demo&includeLatestRuns=true", nil,
	)
	enrichedReq.Header.Set(TransactionTokenHeaderName, token)
	resp, err = app.Test(enrichedReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var got struct {
		Items          []corev1alpha1.RepositoryScan `json:"items"`
		LatestScanRuns []store.ScanRun               `json:"latestScanRuns"`
		Metadata       ListMeta                      `json:"metadata"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got.Items, 2)
	require.Len(t, got.LatestScanRuns, 2)
	latestByRepository := make(map[string]string, len(got.LatestScanRuns))
	for i := range got.LatestScanRuns {
		latestByRepository[got.LatestScanRuns[i].RepositoryScan] = got.LatestScanRuns[i].ID
	}
	require.Equal(t, "scan-a-current", latestByRepository[scanA.Name])
	require.Equal(t, "scan-b-current", latestByRepository[scanB.Name])
}

func TestListRepositoryScans_ContextTokenFiltersMismatchedScansInEnforceMode(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	matching := securityAuthzTestRepositoryScan("scan-match", securityTestRepoURL)
	matching.UID = types.UID("scan-match-uid")
	matching.Generation = 1
	mismatched := securityAuthzTestRepositoryScan("scan-mismatch", "https://github.com/sozercan/other")
	mismatched.UID = types.UID("scan-mismatch-uid")
	mismatched.Generation = 1
	app, handlers := setupSecurityHandlersWithAuthzFixture(
		t,
		ctxTokenConfig,
		ContextTokenAuthorizationModeEnforce,
		matching,
		mismatched,
	)
	for _, run := range []*store.ScanRun{
		{
			ID: "scan-match-run", Namespace: "demo", RepositoryScan: matching.Name,
			RepositoryScanUID: string(matching.UID), RepositoryScanGeneration: matching.Generation,
			Mode: "initial", Phase: "succeeded", StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
		},
		{
			ID: "scan-mismatch-run", Namespace: "demo", RepositoryScan: mismatched.Name,
			RepositoryScanUID: string(mismatched.UID), RepositoryScanGeneration: mismatched.Generation,
			Mode: "initial", Phase: "succeeded", StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
		},
	} {
		require.NoError(t, handlers.securityStore.CreateScanRun(context.Background(), run))
	}
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead,
		"tctx":  securityAuthzTestTctx(securityTestRepoURL),
	})

	req := httptest.NewRequest(http.MethodGet, "/security/repositories?namespace=demo&includeLatestRuns=true", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var got struct {
		Items          []corev1alpha1.RepositoryScan `json:"items"`
		LatestScanRuns []store.ScanRun               `json:"latestScanRuns"`
		Metadata       ListMeta                      `json:"metadata"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&got))
	require.Len(t, got.Items, 1)
	require.Equal(t, "scan-match", got.Items[0].Name)
	require.Equal(t, securityTestRepoURL, got.Items[0].Spec.RepoURL)
	require.Len(t, got.LatestScanRuns, 1)
	require.Equal(t, "scan-match-run", got.LatestScanRuns[0].ID)
}

func TestRepositoryScanReadDelete_ContextTokenObjectAuthorizationDenials(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")

	tests := []struct {
		name   string
		method string
		scope  string
	}{
		{
			name:   "get repository scan mismatched repo denied",
			method: http.MethodGet,
			scope:  ContextTokenScopeSecurityRead,
		},
		{
			name:   "delete repository scan mismatched repo denied",
			method: http.MethodDelete,
			scope:  ContextTokenScopeSecurityWrite,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, handlers := setupSecurityHandlersWithAuthzFixture(
				t,
				ctxTokenConfig,
				ContextTokenAuthorizationModeEnforce,
				securityAuthzTestRepositoryScan("scan-1", securityTestRepoURL),
			)
			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": tt.scope,
				"tctx":  securityAuthzTestTctx("https://github.com/sozercan/other"),
			})

			req := httptest.NewRequest(tt.method, "/security/repositories/scan-1?namespace=demo", nil)
			req.Header.Set(TransactionTokenHeaderName, token)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)

			var got corev1alpha1.RepositoryScan
			require.NoError(t, handlers.client.Get(context.Background(), clientObjectKey("scan-1"), &got))
		})
	}
}

func TestSecurityRepositoryReadsRejectPriorRepositoryScanIncarnation(t *testing.T) {
	scan := securityAuthzTestRepositoryScan("scan-1", securityTestRepoURL)
	scan.UID = types.UID("repository-scan-current")
	scan.Generation = 2
	_, handlers := setupSecurityHandlersWithAuthzFixture(
		t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff, scan,
	)
	app := fiber.New()
	app.Get("/security/repositories/:name/threat-model", handlers.GetThreatModel)
	app.Put("/security/repositories/:name/threat-model", handlers.UpdateThreatModel)
	app.Get("/security/repositories/:name/slices", handlers.ListSecurityReviewSlices)
	app.Get("/security/repositories/:name/slices/:sliceID", handlers.GetSecurityReviewSlice)
	app.Get("/security/repositories/:name/dropped-findings", handlers.ListSecurityDroppedFindings)
	ctx := context.Background()

	createRun := func(id, uid string, generation int64, fill byte) {
		t.Helper()
		run := &store.ScanRun{
			ID: id, RunUID: "run_" + strings.Repeat(string(fill), 64), Namespace: "demo", RepositoryScan: "scan-1",
			RepositoryScanUID: uid, RepositoryScanGeneration: generation,
			TaskName: id + "-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
			Quality: store.LegacyScanQuality(),
		}
		require.NoError(t, handlers.securityStore.CreateScanRun(ctx, run))
	}
	createRun("scan-old", "repository-scan-old", 1, 'a')
	createRun("scan-current", string(scan.UID), scan.Generation, 'b')

	require.NoError(t, handlers.securityStore.SaveThreatModel(ctx, &store.ThreatModel{
		Namespace: "demo", RepositoryScan: "scan-1", RepositoryScanUID: "repository-scan-old",
		RepositoryScanGeneration: 1, Content: "old threat model", Source: "generated", GeneratedByScan: "scan-old",
	}))
	require.NoError(t, handlers.securityStore.UpsertReviewSlice(ctx, &store.ReviewSlice{
		ID: "slice-old", Namespace: "demo", RepositoryScan: "scan-1", Source: "test", Title: "old",
		Kind: "package", Confidence: "high", Status: "pending", LastScanRunID: "scan-old",
	}))
	require.NoError(t, handlers.securityStore.UpsertReviewSlice(ctx, &store.ReviewSlice{
		ID: "slice-current", Namespace: "demo", RepositoryScan: "scan-1", Source: "test", Title: "current",
		Kind: "package", Confidence: "high", Status: "pending", LastScanRunID: "scan-current",
	}))
	require.NoError(t, handlers.securityStore.CreateDroppedFinding(ctx, &store.DroppedFinding{
		ID: "drop-old", Namespace: "demo", RepositoryScan: "scan-1", ScanRunID: "scan-old", TaskName: "old", Reason: "old",
	}))
	require.NoError(t, handlers.securityStore.CreateDroppedFinding(ctx, &store.DroppedFinding{
		ID: "drop-current", Namespace: "demo", RepositoryScan: "scan-1", ScanRunID: "scan-current", TaskName: "current", Reason: "current",
	}))

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/security/repositories/scan-1/threat-model?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/security/repositories/scan-1/slices?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var slicesResponse struct {
		Items []store.ReviewSlice `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&slicesResponse))
	require.Len(t, slicesResponse.Items, 1)
	require.Equal(t, "slice-current", slicesResponse.Items[0].ID)

	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/security/repositories/scan-1/slices/slice-old?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)

	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/security/repositories/scan-1/dropped-findings?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var droppedResponse struct {
		Items []store.DroppedFinding `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&droppedResponse))
	require.Len(t, droppedResponse.Items, 1)
	require.Equal(t, "drop-current", droppedResponse.Items[0].ID)

	update := httptest.NewRequest(http.MethodPut, "/security/repositories/scan-1/threat-model?namespace=demo", strings.NewReader(`{"content":"current threat model","source":"edited"}`))
	update.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(update)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/security/repositories/scan-1/threat-model?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var model store.ThreatModel
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&model))
	require.Equal(t, string(scan.UID), model.RepositoryScanUID)
	require.Equal(t, scan.Generation, model.RepositoryScanGeneration)
}

func TestThreatModel_ContextTokenRepositoryScanAuthorizationDenials(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")

	tests := []struct {
		name   string
		method string
		scope  string
		body   string
	}{
		{
			name:   "get threat model mismatched repo denied",
			method: http.MethodGet,
			scope:  ContextTokenScopeSecurityRead,
		},
		{
			name:   "update threat model mismatched repo denied",
			method: http.MethodPut,
			scope:  ContextTokenScopeSecurityWrite,
			body:   `{"content":"updated","source":"edited"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, handlers := setupSecurityHandlersWithAuthzFixture(
				t,
				ctxTokenConfig,
				ContextTokenAuthorizationModeEnforce,
				securityAuthzTestRepositoryScan("scan-1", securityTestRepoURL),
			)
			ctx := context.Background()
			require.NoError(t, handlers.securityStore.SaveThreatModel(ctx, &store.ThreatModel{
				Namespace:      "demo",
				RepositoryScan: "scan-1",
				Content:        "model",
				Source:         "generated",
			}))
			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": tt.scope,
				"tctx":  securityAuthzTestTctx("https://github.com/sozercan/other"),
			})

			var body *strings.Reader
			if tt.body != "" {
				body = strings.NewReader(tt.body)
			} else {
				body = strings.NewReader("")
			}
			req := httptest.NewRequest(tt.method, "/security/repositories/scan-1/threat-model?namespace=demo", body)
			req.Header.Set(TransactionTokenHeaderName, token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)

			model, err := handlers.securityStore.GetLatestThreatModel(ctx, "demo", "scan-1")
			require.NoError(t, err)
			require.Equal(t, "model", model.Content)
		})
	}
}

func TestSecurityScanRunAndFindingLists_ContextTokenRepositoryScanAuthorizationDenials(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")

	tests := []struct {
		name string
		path string
		seed func(t *testing.T, handlers *Handlers)
	}{
		{
			name: "list scan runs mismatched repo denied",
			path: "/security/repositories/scan-1/scans?namespace=demo",
			seed: func(t *testing.T, handlers *Handlers) {
				t.Helper()
				require.NoError(t, handlers.securityStore.CreateScanRun(context.Background(), &store.ScanRun{
					ID:             "run-1",
					Namespace:      "demo",
					RepositoryScan: "scan-1",
					Mode:           "manual",
					Phase:          "completed",
				}))
			},
		},
		{
			name: "list findings mismatched repo denied",
			path: "/security/repositories/scan-1/findings?namespace=demo",
			seed: func(t *testing.T, handlers *Handlers) {
				t.Helper()
				require.NoError(t, handlers.securityStore.UpsertFinding(context.Background(), &store.Finding{
					ID:             "finding-1",
					Namespace:      "demo",
					RepositoryScan: "scan-1",
					Fingerprint:    "fp-1",
					Title:          "Command injection",
					Summary:        "Unsanitized user input reaches shell execution.",
					Severity:       "critical",
					Confidence:     "high",
					State:          "open",
				}))
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, handlers := setupSecurityHandlersWithAuthzFixture(
				t,
				ctxTokenConfig,
				ContextTokenAuthorizationModeEnforce,
				securityAuthzTestRepositoryScan("scan-1", securityTestRepoURL),
			)
			tt.seed(t, handlers)
			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityRead,
				"tctx":  securityAuthzTestTctx("https://github.com/sozercan/other"),
			})

			req := httptest.NewRequest(http.MethodGet, tt.path, nil)
			req.Header.Set(TransactionTokenHeaderName, token)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)
		})
	}
}

func TestListSecurityFindingsReturnsEmptyItemsArray(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	app, _ := setupSecurityHandlersWithAuthzFixture(
		t,
		ctxTokenConfig,
		ContextTokenAuthorizationModeEnforce,
		securityAuthzTestRepositoryScan("scan-1", securityTestRepoURL),
	)
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead,
		"tctx":  securityAuthzTestTctx(securityTestRepoURL),
	})

	req := httptest.NewRequest(http.MethodGet, "/security/repositories/scan-1/findings?namespace=demo", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body map[string]json.RawMessage
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.JSONEq(t, "[]", string(body["items"]))
}

func TestGetSecurityFinding_ContextTokenAuthorizesFindingRepositoryScan(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := securityAuthzTestRepositoryScan("scan-1", securityTestRepoURL)
	scan.UID = types.UID("scan-1-uid")
	scan.Generation = 1
	app, handlers := setupSecurityHandlersWithAuthzFixture(
		t,
		ctxTokenConfig,
		ContextTokenAuthorizationModeEnforce,
		scan,
	)
	ctx := context.Background()
	require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
		ID: "scan-run-finding", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, TaskName: "scan-task", Mode: "manual", Phase: "succeeded",
		StartedAt: time.Now().UTC(), Quality: store.LegacyScanQuality(),
	}))
	require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
		ID:             "finding-1",
		Namespace:      "demo",
		RepositoryScan: "scan-1",
		ScanRunID:      "scan-run-finding",
		Fingerprint:    "fp-1",
		Title:          "Command injection",
		Summary:        "Unsanitized user input reaches shell execution.",
		Severity:       "critical",
		Confidence:     "high",
		State:          "open",
	}))
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead,
		"tctx":  securityAuthzTestTctx("https://github.com/sozercan/other"),
	})

	req := httptest.NewRequest(http.MethodGet, "/security/findings/finding-1?namespace=demo", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestListSecurityPatchProposals_ContextTokenUsesPatchAgentAuthorization(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := securityAuthzTestRepositoryScan("scan-1", securityTestRepoURL)
	scan.UID = types.UID("scan-1-uid")
	scan.Generation = 1
	scan.Spec.PatchAgentRef = &corev1alpha1.AgentReference{Name: "patch"}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan)

	ctx := context.Background()
	require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
		ID: "scan-run-1", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, HeadCommit: strings.Repeat("a", 40),
		TaskName: "scan-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
		Quality: store.LegacyScanQuality(),
	}))
	require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
		ID:                  "finding-1",
		Namespace:           "demo",
		RepositoryScan:      "scan-1",
		ScanRunID:           "scan-run-1",
		CurrentOccurrenceID: "occurrence-1",
		Fingerprint:         "fp-1",
		Title:               "Command injection",
		Summary:             "Unsanitized user input reaches shell execution.",
		Severity:            "critical",
		Confidence:          "high",
		State:               "open",
	}))
	require.NoError(t, handlers.securityStore.CreatePatchProposal(ctx, &store.PatchProposal{
		ID:              "proposal-1",
		Namespace:       "demo",
		RepositoryScan:  "scan-1",
		FindingID:       "finding-1",
		OccurrenceID:    "occurrence-1",
		SourceScanRunID: "scan-run-1",
		SourceHeadSHA:   strings.Repeat("a", 40),
		Status:          "ready",
	}))
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead,
		"tctx":  securityAuthzTestTctx(securityTestRepoURL),
	})

	req := httptest.NewRequest(http.MethodGet, "/security/findings/finding-1/patches?namespace=demo", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestListSecurityPatchProposals_ReturnsOnlyCurrentBoundProposals(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := securityAuthzTestRepositoryScan("scan-patch-list", securityTestRepoURL)
	scan.UID = types.UID("scan-patch-list-uid")
	scan.Generation = 2
	scan.Spec.PatchAgentRef = &corev1alpha1.AgentReference{Name: "patch"}
	app, handlers := setupSecurityHandlersWithAuthzFixture(
		t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan,
	)

	ctx := context.Background()
	headSHA := strings.Repeat("d", 40)
	require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
		ID: "scan-run-current", RunUID: "run_dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
		Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, HeadCommit: headSHA,
		TaskName: "scan-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
		Quality: store.LegacyScanQuality(),
	}))
	require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
		ID: "finding-current", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "scan-run-current",
		CurrentOccurrenceID: "occurrence-current", Fingerprint: "fp-current", Title: "Current finding",
		Summary: "current", Severity: "high", Confidence: "high", State: "validated",
	}))
	proposals := []store.PatchProposal{
		{
			ID: "proposal-current", Namespace: "demo", RepositoryScan: scan.Name, FindingID: "finding-current",
			OccurrenceID: "occurrence-current", SourceScanRunID: "scan-run-current", SourceHeadSHA: headSHA,
			Status: "succeeded",
		},
		{
			ID: "proposal-old-occurrence", Namespace: "demo", RepositoryScan: scan.Name, FindingID: "finding-current",
			OccurrenceID: "occurrence-old", SourceScanRunID: "scan-run-current", SourceHeadSHA: headSHA,
			Status: "succeeded",
		},
		{
			ID: "proposal-old-run", Namespace: "demo", RepositoryScan: scan.Name, FindingID: "finding-current",
			OccurrenceID: "occurrence-current", SourceScanRunID: "scan-run-old", SourceHeadSHA: headSHA,
			Status: "succeeded",
		},
		{
			ID: "proposal-old-head", Namespace: "demo", RepositoryScan: scan.Name, FindingID: "finding-current",
			OccurrenceID: "occurrence-current", SourceScanRunID: "scan-run-current", SourceHeadSHA: strings.Repeat("e", 40),
			Status: "succeeded",
		},
		{
			ID: "proposal-legacy", Namespace: "demo", RepositoryScan: scan.Name, FindingID: "finding-current",
			Status: "succeeded",
		},
	}
	for i := range proposals {
		require.NoError(t, handlers.securityStore.CreatePatchProposal(ctx, &proposals[i]))
	}

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead,
		"tctx": map[string]any{
			"namespace": "demo", "repo": securityTestRepoURL, "branch": "main", "agent": "demo/patch",
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/security/findings/finding-current/patches?namespace=demo", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var body struct {
		Items []store.PatchProposal `json:"items"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	require.Len(t, body.Items, 1)
	require.Equal(t, "proposal-current", body.Items[0].ID)
}

func TestListSecurityPatchProposals_RejectsRecreatedRepositoryScan(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := securityAuthzTestRepositoryScan("scan-recreated", securityTestRepoURL)
	scan.UID = types.UID("scan-recreated-new-uid")
	scan.Generation = 1
	scan.Spec.PatchAgentRef = &corev1alpha1.AgentReference{Name: "patch"}
	app, handlers := setupSecurityHandlersWithAuthzFixture(
		t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan,
	)

	ctx := context.Background()
	headSHA := strings.Repeat("f", 40)
	require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
		ID: "scan-run-old", RunUID: "run_eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee",
		Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: "scan-recreated-old-uid",
		RepositoryScanGeneration: scan.Generation, HeadCommit: headSHA,
		TaskName: "scan-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
		Quality: store.LegacyScanQuality(),
	}))
	require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
		ID: "finding-old", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "scan-run-old",
		CurrentOccurrenceID: "occurrence-old", Fingerprint: "fp-old", Title: "Old finding",
		Summary: "old", Severity: "high", Confidence: "high", State: "validated",
	}))
	require.NoError(t, handlers.securityStore.CreatePatchProposal(ctx, &store.PatchProposal{
		ID: "proposal-old", Namespace: "demo", RepositoryScan: scan.Name, FindingID: "finding-old",
		OccurrenceID: "occurrence-old", SourceScanRunID: "scan-run-old", SourceHeadSHA: headSHA,
		Status: "succeeded",
	}))

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead,
		"tctx": map[string]any{
			"namespace": "demo", "repo": securityTestRepoURL, "branch": "main", "agent": "demo/patch",
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/security/findings/finding-old/patches?namespace=demo", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestLegacyFindingStateEndpointsRejectPreIntegrityUnboundRun(t *testing.T) {
	tests := []struct {
		name    string
		path    string
		initial string
	}{
		{name: "dismiss", path: "/security/findings/finding-legacy/dismiss?namespace=demo", initial: "open"},
		{name: "reopen", path: "/security/findings/finding-legacy/reopen?namespace=demo", initial: "dismissed"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := context.Background()
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{
					Name: "legacy-scan", Namespace: "demo", UID: types.UID("legacy-scan-uid"), Generation: 1,
				},
				Spec: corev1alpha1.RepositoryScanSpec{
					RepoURL: securityTestRepoURL, Branch: "main",
					AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
			}
			cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan).Build()
			db, err := sqlite.NewDB(":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			securityStore := sqlite.NewStore(db, ":memory:")
			handlers := NewHandlers(HandlersConfig{
				Client: cl, SecurityStore: securityStore, SecurityIntegrityStore: securityStore,
			})
			require.NoError(t, securityStore.CreateScanRun(ctx, &store.ScanRun{
				ID: "scan-legacy", Namespace: scan.Namespace, RepositoryScan: scan.Name,
				TaskName: "legacy-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
				Quality: store.LegacyScanQuality(),
			}))
			require.NoError(t, securityStore.UpsertFinding(ctx, &store.Finding{
				ID: "finding-legacy", Namespace: scan.Namespace, RepositoryScan: scan.Name, ScanRunID: "scan-legacy",
				Fingerprint: "legacy-fingerprint", Title: "Legacy finding", Summary: "Pre-integrity finding",
				Severity: "high", Confidence: "medium", State: tt.initial,
			}))

			app := fiber.New()
			app.Post("/security/findings/:id/dismiss", handlers.DismissSecurityFinding)
			app.Post("/security/findings/:id/reopen", handlers.ReopenSecurityFinding)
			resp, err := app.Test(httptest.NewRequest(http.MethodPost, tt.path, nil))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
			require.Equal(t, fiber.StatusConflict, resp.StatusCode)
			updated, err := securityStore.GetFinding(ctx, scan.Namespace, "finding-legacy")
			require.NoError(t, err)
			require.Equal(t, tt.initial, updated.State)
		})
	}
}

func TestIntegrityFindingDecisionRejectsPreIntegrityUnboundRun(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "legacy-scan", Namespace: "demo", UID: types.UID("legacy-scan-uid"), Generation: 3,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: securityTestRepoURL, Branch: "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	securityStore := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(HandlersConfig{
		Client: cl, SecurityStore: securityStore, SecurityIntegrityStore: securityStore,
	})
	require.NoError(t, securityStore.CreateScanRun(ctx, &store.ScanRun{
		ID: "scan-legacy", Namespace: scan.Namespace, RepositoryScan: scan.Name,
		TaskName: "legacy-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
		Quality: store.LegacyScanQuality(),
	}))
	require.NoError(t, securityStore.UpsertFinding(ctx, &store.Finding{
		ID: "finding-legacy", Namespace: scan.Namespace, RepositoryScan: scan.Name, ScanRunID: "scan-legacy",
		Fingerprint: "legacy-fingerprint", Title: "Legacy finding", Summary: "Pre-integrity finding",
		Severity: "high", Confidence: "medium", State: "open",
	}))

	app := fiber.New()
	app.Post("/security/findings/:id/decisions", handlers.AppendSecurityFindingDecision)
	body := strings.NewReader(`{"decisionId":"decision-1","scope":"finding","action":"close_wont_fix","expectedDecisionVersion":0}`)
	req := httptest.NewRequest(http.MethodPost, "/security/findings/finding-legacy/decisions?namespace=demo", body)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, resp.Body.Close()) })
	require.Equal(t, fiber.StatusConflict, resp.StatusCode)
}

func TestSecurityFindingMutations_ContextTokenTransactionContextAuthorizationDenials(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scan-1", Namespace: "demo", UID: types.UID("scan-mutations-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          securityTestRepoURL,
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}

	tests := []struct {
		name    string
		path    string
		initial string
	}{
		{
			name:    "dismiss finding mismatched repo denied",
			path:    "/security/findings/finding-1/dismiss?namespace=demo",
			initial: "open",
		},
		{
			name:    "reopen finding mismatched repo denied",
			path:    "/security/findings/finding-1/reopen?namespace=demo",
			initial: "dismissed",
		},
		{
			name:    "validate finding mismatched repo denied",
			path:    "/security/findings/finding-1/validate?namespace=demo",
			initial: "open",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan.DeepCopyObject())
			ctx := context.Background()
			require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
				ID: "scan-run-1", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
				Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
				RepositoryScanGeneration: scan.Generation, HeadCommit: strings.Repeat("a", 40),
				TaskName: "scan-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
				Quality: store.LegacyScanQuality(),
			}))
			require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
				ID:             "finding-1",
				Namespace:      "demo",
				RepositoryScan: "scan-1",
				ScanRunID:      "scan-run-1",
				Fingerprint:    "fp-1",
				Title:          "Command injection",
				Summary:        "Unsanitized user input reaches shell execution.",
				Severity:       "critical",
				Confidence:     "high",
				State:          tt.initial,
			}))

			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx": map[string]any{
					"namespace": "demo",
					"repo":      "https://github.com/sozercan/other",
					"branch":    "main",
					"agent":     "demo/analysis",
				},
			})
			req := httptest.NewRequest(http.MethodPost, tt.path, nil)
			req.Header.Set(TransactionTokenHeaderName, token)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)

			finding, err := handlers.securityStore.GetFinding(ctx, "demo", "finding-1")
			require.NoError(t, err)
			require.Equal(t, tt.initial, finding.State)
			var tasks corev1alpha1.TaskList
			require.NoError(t, handlers.client.List(ctx, &tasks, client.InNamespace("demo")))
			require.Empty(t, tasks.Items)
		})
	}
}

func TestValidateSecurityFinding_RejectsSealedSourceRun(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")

	tests := []struct {
		name         string
		createRun    bool
		bundleStatus store.BundleStatus
		wantStatus   int
		wantTasks    int
		wantFinding  string
	}{
		{
			name:         "sealed source run is rejected",
			createRun:    true,
			bundleStatus: store.BundleStatusSealed,
			wantStatus:   http.StatusConflict,
			wantFinding:  "unvalidated",
		},
		{
			name:         "sealing source run is rejected",
			createRun:    true,
			bundleStatus: store.BundleStatusSealing,
			wantStatus:   http.StatusConflict,
			wantFinding:  "unvalidated",
		},
		{
			name:        "missing source run is rejected",
			wantStatus:  http.StatusConflict,
			wantFinding: "unvalidated",
		},
		{
			name:         "unsealed source run creates validation task",
			createRun:    true,
			bundleStatus: store.BundleStatusNotStarted,
			wantStatus:   http.StatusAccepted,
			wantTasks:    1,
			wantFinding:  "pending",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scan := securityAuthzTestRepositoryScan("scan-1", securityTestRepoURL)
			scan.UID = types.UID("scan-1-uid")
			scan.Generation = 1
			app, handlers := setupSecurityHandlersWithAuthzFixture(
				t,
				ctxTokenConfig,
				ContextTokenAuthorizationModeEnforce,
				scan,
			)
			ctx := context.Background()
			quality := store.LegacyScanQuality()
			quality.BundleStatus = tt.bundleStatus
			if tt.createRun {
				require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
					ID: "scan-run-1", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
					Namespace: "demo", RepositoryScan: "scan-1", RepositoryScanUID: string(scan.UID),
					RepositoryScanGeneration: scan.Generation, HeadCommit: strings.Repeat("a", 40),
					TaskName: "scan-task-1", Mode: "manual", Phase: "succeeded", Quality: quality,
				}))
			}
			require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
				ID:               "finding-1",
				Namespace:        "demo",
				RepositoryScan:   "scan-1",
				ScanRunID:        "scan-run-1",
				Fingerprint:      "fp-1",
				Title:            "Command injection",
				Summary:          "Unsanitized user input reaches shell execution.",
				Severity:         "critical",
				Confidence:       "high",
				ValidationStatus: "unvalidated",
				State:            "open",
			}))

			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx":  securityAuthzTestTctx(securityTestRepoURL),
			})
			req := httptest.NewRequest(http.MethodPost, "/security/findings/finding-1/validate?namespace=demo", nil)
			req.Header.Set(TransactionTokenHeaderName, token)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, tt.wantStatus, resp.StatusCode)

			var tasks corev1alpha1.TaskList
			require.NoError(t, handlers.client.List(ctx, &tasks, client.InNamespace("demo")))
			require.Len(t, tasks.Items, tt.wantTasks)

			finding, err := handlers.securityStore.GetFinding(ctx, "demo", "finding-1")
			require.NoError(t, err)
			require.Equal(t, tt.wantFinding, finding.ValidationStatus)
		})
	}
}

func TestCreateSecurityPullRequest_ContextTokenTransactionContextAuthorizationDenied(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scan-1", Namespace: "demo", UID: types.UID("scan-pr-authz-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          securityTestRepoURL,
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
			PatchAgentRef:    &corev1alpha1.AgentReference{Name: "patch"},
		},
	}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan)
	ctx := context.Background()
	require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
		ID: "scan-run-1", RunUID: "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, HeadCommit: strings.Repeat("b", 40),
		TaskName: "scan-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
		Quality: store.LegacyScanQuality(),
	}))
	require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
		ID:                  "finding-1",
		Namespace:           "demo",
		RepositoryScan:      "scan-1",
		ScanRunID:           "scan-run-1",
		CurrentOccurrenceID: "occurrence-1",
		Fingerprint:         "fp-1",
		Title:               "Command injection",
		Summary:             "Unsanitized user input reaches shell execution.",
		Severity:            "critical",
		Confidence:          "high",
		State:               "validated",
	}))

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityWrite,
		"tctx": map[string]any{
			"namespace": "demo",
			"repo":      "https://github.com/sozercan/other",
			"branch":    "main",
			"agent":     "demo/patch",
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/security/findings/finding-1/pull-request?namespace=demo", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	finding, err := handlers.securityStore.GetFinding(ctx, "demo", "finding-1")
	require.NoError(t, err)
	require.Equal(t, "validated", finding.State)
}

func TestCreateSecurityPullRequest_RejectsUnboundSuccessfulProposal(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")

	tests := []struct {
		name   string
		mutate func(*store.PatchProposal)
	}{
		{
			name: "legacy proposal without bindings",
			mutate: func(proposal *store.PatchProposal) {
				proposal.OccurrenceID = ""
				proposal.SourceScanRunID = ""
				proposal.SourceHeadSHA = ""
			},
		},
		{
			name: "stale occurrence",
			mutate: func(proposal *store.PatchProposal) {
				proposal.OccurrenceID = "occurrence-old"
			},
		},
		{
			name: "stale source run",
			mutate: func(proposal *store.PatchProposal) {
				proposal.SourceScanRunID = "scan-run-old"
			},
		},
		{
			name: "stale source head",
			mutate: func(proposal *store.PatchProposal) {
				proposal.SourceHeadSHA = strings.Repeat("2", 40)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scan := securityAuthzTestRepositoryScan("scan-pr-binding", securityTestRepoURL)
			scan.UID = types.UID("scan-pr-binding-uid")
			scan.Generation = 1
			scan.Spec.PatchAgentRef = &corev1alpha1.AgentReference{Name: "patch"}
			app, handlers := setupSecurityHandlersWithAuthzFixture(
				t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan,
			)

			ctx := context.Background()
			headSHA := strings.Repeat("1", 40)
			require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
				ID: "scan-run-current", RunUID: "run_1111111111111111111111111111111111111111111111111111111111111111",
				Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
				RepositoryScanGeneration: scan.Generation, HeadCommit: headSHA,
				TaskName: "scan-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
				Quality: store.LegacyScanQuality(),
			}))
			require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
				ID: "finding-current", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "scan-run-current",
				CurrentOccurrenceID: "occurrence-current", Fingerprint: "fp-current", Title: "Current finding",
				Summary: "current", Severity: "high", Confidence: "high", State: "validated",
			}))
			proposal := &store.PatchProposal{
				ID: "proposal-current", Namespace: "demo", RepositoryScan: scan.Name, FindingID: "finding-current",
				OccurrenceID: "occurrence-current", SourceScanRunID: "scan-run-current", SourceHeadSHA: headSHA,
				TaskName: "patch-task", Branch: "orka/security/current", Status: "succeeded",
			}
			tt.mutate(proposal)
			require.NoError(t, handlers.securityStore.CreatePatchProposal(ctx, proposal))

			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx": map[string]any{
					"namespace": "demo", "repo": securityTestRepoURL, "branch": "main", "agent": "demo/patch",
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/security/findings/finding-current/pull-request?namespace=demo", nil)
			req.Header.Set(TransactionTokenHeaderName, token)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusConflict, resp.StatusCode)

			finding, err := handlers.securityStore.GetFinding(ctx, "demo", "finding-current")
			require.NoError(t, err)
			require.Equal(t, "validated", finding.State)
			stored, err := handlers.securityStore.ListPatchProposals(ctx, "demo", "finding-current")
			require.NoError(t, err)
			require.Len(t, stored, 1)
			require.Equal(t, "succeeded", stored[0].Status)
		})
	}
}

func TestCreateSecurityPullRequest_RejectsHistoricalFindingAuthorization(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")

	tests := []struct {
		name          string
		runUIDBinding string
		runGeneration int64
		wantStatus    int
	}{
		{
			name:          "RepositoryScan was recreated",
			runUIDBinding: "scan-pr-old-uid",
			runGeneration: 2,
			wantStatus:    http.StatusNotFound,
		},
		{
			name:          "RepositoryScan generation advanced",
			runUIDBinding: "scan-pr-current-uid",
			runGeneration: 1,
			wantStatus:    http.StatusConflict,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scan := securityAuthzTestRepositoryScan("scan-pr-current", securityTestRepoURL)
			scan.UID = types.UID("scan-pr-current-uid")
			scan.Generation = 2
			scan.Spec.PatchAgentRef = &corev1alpha1.AgentReference{Name: "patch"}
			app, handlers := setupSecurityHandlersWithAuthzFixture(
				t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan,
			)

			ctx := context.Background()
			headSHA := strings.Repeat("3", 40)
			require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
				ID: "scan-run-historical", RunUID: "run_3333333333333333333333333333333333333333333333333333333333333333",
				Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: tt.runUIDBinding,
				RepositoryScanGeneration: tt.runGeneration, HeadCommit: headSHA,
				TaskName: "scan-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
				Quality: store.LegacyScanQuality(),
			}))
			require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
				ID: "finding-historical", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "scan-run-historical",
				CurrentOccurrenceID: "occurrence-historical", Fingerprint: "fp-historical", Title: "Historical finding",
				Summary: "historical", Severity: "high", Confidence: "high", State: "validated",
			}))
			require.NoError(t, handlers.securityStore.CreatePatchProposal(ctx, &store.PatchProposal{
				ID: "proposal-historical", Namespace: "demo", RepositoryScan: scan.Name, FindingID: "finding-historical",
				OccurrenceID: "occurrence-historical", SourceScanRunID: "scan-run-historical", SourceHeadSHA: headSHA,
				TaskName: "patch-task", Branch: "orka/security/historical", Status: "succeeded",
			}))

			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx": map[string]any{
					"namespace": "demo", "repo": securityTestRepoURL, "branch": "main", "agent": "demo/patch",
				},
			})
			req := httptest.NewRequest(http.MethodPost, "/security/findings/finding-historical/pull-request?namespace=demo", nil)
			req.Header.Set(TransactionTokenHeaderName, token)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, tt.wantStatus, resp.StatusCode)

			finding, err := handlers.securityStore.GetFinding(ctx, "demo", "finding-historical")
			require.NoError(t, err)
			require.Equal(t, "validated", finding.State)
		})
	}
}

func TestCreateManualSecurityScan_ContextTokenStampsTaskRequesterAndTransaction(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")

	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scan-1", Namespace: "demo", UID: types.UID("scan-1-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          securityTestRepoURL,
			Ref:              "refs/tags/v1.0.0",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan)

	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite + " " + ContextTokenScopeConfigMapsRead})
	req := httptest.NewRequest(http.MethodPost, "/security/repositories/scan-1/scans?namespace=demo", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var run store.ScanRun
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	require.NotEmpty(t, run.TaskName)

	task := &corev1alpha1.Task{}
	require.NoError(t, handlers.client.Get(context.Background(), clientObjectKey(run.TaskName), task))
	require.NotNil(t, task.Spec.RequestedBy)
	require.Equal(t, testContextTokenSubject, task.Spec.RequestedBy.Subject)
	require.NotNil(t, task.Spec.Transaction)
	require.Equal(t, testContextTokenTransactionID, task.Spec.Transaction.ID)
	require.Equal(t, labels.SelectorValue(testContextTokenTransactionID), task.Labels[labels.LabelTransactionID])
	require.Equal(t, testContextTokenTransactionID, task.Annotations[labels.AnnotationTransactionID])
	require.Equal(t, security.StageThreatModel, envValue(task.Spec.Env, security.EnvStage))
	require.Equal(t, "scan-1", envValue(task.Spec.Env, security.EnvRepositoryScanName))
	require.NotNil(t, task.Spec.AgentRuntime)
	require.NotNil(t, task.Spec.AgentRuntime.Workspace)
	require.Empty(t, task.Spec.AgentRuntime.Workspace.Branch)
	require.Equal(t, "refs/tags/v1.0.0", task.Spec.AgentRuntime.Workspace.Ref)
}

func TestCreateSecurityPullRequest_ExistingPR(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			w.WriteHeader(http.StatusUnprocessableEntity)
			fmt.Fprint(w, `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for sozercan:orka/security/fnd-123."}]}`) //nolint:errcheck
		case http.MethodGet:
			require.Equal(t, "sozercan:orka/security/fnd-123", r.URL.Query().Get("head"))
			require.Equal(t, "main", r.URL.Query().Get("base"))
			w.WriteHeader(http.StatusOK)
			fmt.Fprintf(w, `[{"html_url":%q,"number":99}]`, securityTestRepoPRURL) //nolint:errcheck
		default:
			t.Fatalf("unexpected method: %s", r.Method)
		}
	}))
	defer server.Close()

	previousBaseURL := githubPullRequestAPIBaseURL
	githubPullRequestAPIBaseURL = server.URL
	t.Cleanup(func() {
		githubPullRequestAPIBaseURL = previousBaseURL
	})

	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scan-1", Namespace: "demo", UID: types.UID("scan-existing-pr-uid"), Generation: 1,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: securityTestRepoURL,
			Branch:  "main",
			GitSecretRef: &corev1.LocalObjectReference{
				Name: "git-creds",
			},
		},
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "git-creds",
			Namespace: "demo",
		},
		Data: map[string][]byte{
			"token": []byte("test-token"),
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(scan, secret).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	securityStore := sqlite.NewStore(db, ":memory:")

	handlers := NewHandlers(HandlersConfig{
		Client:        fakeClient,
		SecurityStore: securityStore,
	})

	ctx := context.Background()
	require.NoError(t, securityStore.CreateScanRun(ctx, &store.ScanRun{
		ID: "scan-run-1", RunUID: "run_cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
		Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, HeadCommit: strings.Repeat("c", 40),
		TaskName: "scan-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
		Quality: store.LegacyScanQuality(),
	}))
	require.NoError(t, securityStore.UpsertFinding(ctx, &store.Finding{
		ID:                  "finding-1",
		Namespace:           "demo",
		RepositoryScan:      "scan-1",
		ScanRunID:           "scan-run-1",
		CurrentOccurrenceID: "occurrence-1",
		Fingerprint:         "fp-1",
		Title:               "Command injection",
		Summary:             "Unsanitized user input reaches shell execution.",
		Severity:            "critical",
		Confidence:          "high",
		State:               "validated",
		RootCause:           "Shell command arguments are concatenated directly.",
		Remediation:         "Use argument arrays and validate inputs.",
	}))
	require.NoError(t, securityStore.CreatePatchProposal(ctx, &store.PatchProposal{
		ID:              "patch-1",
		Namespace:       "demo",
		RepositoryScan:  "scan-1",
		FindingID:       "finding-1",
		OccurrenceID:    "occurrence-1",
		SourceScanRunID: "scan-run-1",
		SourceHeadSHA:   strings.Repeat("c", 40),
		TaskName:        "patch-task-1",
		Branch:          "orka/security/fnd-123",
		Status:          "succeeded",
		CreatedAt:       time.Now().Add(-time.Minute),
	}))
	require.NoError(t, securityStore.CreatePatchProposal(ctx, &store.PatchProposal{
		ID:              "patch-stale",
		Namespace:       "demo",
		RepositoryScan:  "scan-1",
		FindingID:       "finding-1",
		OccurrenceID:    "occurrence-old",
		SourceScanRunID: "scan-run-1",
		SourceHeadSHA:   strings.Repeat("c", 40),
		TaskName:        "patch-task-stale",
		Branch:          "orka/security/stale",
		Status:          "succeeded",
		CreatedAt:       time.Now(),
	}))

	app := fiber.New()
	app.Post("/security/findings/:id/pull-request", handlers.CreateSecurityPullRequest)

	req := httptest.NewRequest(http.MethodPost, "/security/findings/finding-1/pull-request?namespace=demo", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		PRNumber int    `json:"prNumber"`
		PRURL    string `json:"prURL"`
		Status   string `json:"status"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, 99, result.PRNumber)
	require.Equal(t, securityTestRepoPRURL, result.PRURL)
	require.Equal(t, "existing", result.Status)

	proposals, err := securityStore.ListPatchProposals(ctx, "demo", "finding-1")
	require.NoError(t, err)
	require.Len(t, proposals, 2)
	proposalsByID := make(map[string]store.PatchProposal, len(proposals))
	for i := range proposals {
		proposalsByID[proposals[i].ID] = proposals[i]
	}
	current := proposalsByID["patch-1"]
	require.Equal(t, "pr_opened", current.Status)
	require.Equal(t, securityTestRepoPRURL, current.PRURL)
	require.NotNil(t, current.PRNumber)
	require.Equal(t, 99, *current.PRNumber)
	require.Equal(t, "succeeded", proposalsByID["patch-stale"].Status)

	finding, err := securityStore.GetFinding(ctx, "demo", "finding-1")
	require.NoError(t, err)
	require.Equal(t, "pr_open", finding.State)
	require.Equal(t, securityTestRepoPRURL, finding.PRURL)
	require.NotNil(t, finding.PRNumber)
	require.Equal(t, 99, *finding.PRNumber)
}

func TestCreateSecurityPatchTaskRequiresPushedBranch(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))

	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{Name: "scan-1", Namespace: "demo", UID: types.UID("scan-1-uid"), Generation: 1},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: securityTestRepoURL,
			Ref:     "f00dbabe",
			GitSecretRef: &corev1.LocalObjectReference{
				Name: "git-creds",
			},
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
			PatchAgentRef:    &corev1alpha1.AgentReference{Name: "patch"},
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	securityStore := sqlite.NewStore(db, ":memory:")

	handlers := NewHandlers(HandlersConfig{
		Client:        fakeClient,
		SecurityStore: securityStore,
	})

	finding := &store.Finding{
		ID: "fnd_" + strings.Repeat("a", 64), Namespace: "demo", RepositoryScan: scan.Name,
		ScanRunID: "scan-run-patch", CurrentOccurrenceID: "occ_" + strings.Repeat("b", 64),
		Title: "Command injection", Severity: "high", Confidence: "high",
	}
	headSHA := strings.Repeat("f", 40)
	require.NoError(t, securityStore.CreateScanRun(context.Background(), &store.ScanRun{
		ID: "scan-run-patch", RunUID: "run_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
		Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
		RepositoryScanGeneration: scan.Generation, HeadCommit: headSHA,
		TaskName: "scan-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
		Quality: store.LegacyScanQuality(),
	}))

	proposal, err := handlers.createSecurityPatchTask(context.Background(), nil, scan, finding)
	require.NoError(t, err)
	require.Equal(t, security.ScanStageTaskNameForRun(
		scan.Name, "patch", security.StagePatch, finding.CurrentOccurrenceID,
		"run_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff",
	), proposal.TaskName)
	require.Equal(t, security.PatchProposalIDForOccurrence(
		"run_ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff", finding.CurrentOccurrenceID,
	), proposal.ID)
	require.Regexp(t, `^orka/security/.+-[a-f0-9]{12}$`, proposal.Branch)

	task := &corev1alpha1.Task{}
	require.NoError(t, fakeClient.Get(context.Background(), clientObjectKey(proposal.TaskName), task))
	require.Equal(t, "true", envValue(task.Spec.Env, "ORKA_REQUIRE_PUSH_BRANCH"))
	require.NotNil(t, task.Spec.AgentRuntime)
	require.NotNil(t, task.Spec.AgentRuntime.Workspace)
	require.Equal(t, security.StagePatch, envValue(task.Spec.Env, security.EnvStage))
	require.Equal(t, "scan-1", envValue(task.Spec.Env, security.EnvRepositoryScanName))
	require.Equal(t, finding.ID, envValue(task.Spec.Env, security.EnvFindingID))
	require.Equal(t, finding.CurrentOccurrenceID, envValue(task.Spec.Env, security.EnvOccurrenceID))
	require.Equal(t, labels.SelectorValue(finding.ID), task.Labels[labels.LabelSecurityFindingID])
	require.Equal(t, labels.SelectorValue(finding.CurrentOccurrenceID), task.Labels[labels.LabelSecurityOccurrenceID])
	require.LessOrEqual(t, len(task.Labels[labels.LabelSecurityFindingID]), 63)
	require.LessOrEqual(t, len(task.Labels[labels.LabelSecurityOccurrenceID]), 63)
	require.Empty(t, k8svalidation.IsValidLabelValue(task.Labels[labels.LabelSecurityFindingID]))
	require.Empty(t, k8svalidation.IsValidLabelValue(task.Labels[labels.LabelSecurityOccurrenceID]))
	require.Equal(t, proposal.Branch, envValue(task.Spec.Env, security.EnvPatchBranch))
	require.Empty(t, task.Spec.AgentRuntime.Workspace.Branch)
	require.Equal(t, headSHA, task.Spec.AgentRuntime.Workspace.Ref)
	require.Equal(t, proposal.Branch, task.Spec.AgentRuntime.Workspace.PushBranch)
	require.Equal(t, headSHA, proposal.SourceHeadSHA)
}

func TestCreateSecurityPatchTaskScopesGateOffIdentityToSourceRun(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))

	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name: "scan-gate-off", Namespace: "demo",
			UID: types.UID("scan-gate-off-uid"), Generation: 3,
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          securityTestRepoURL,
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	securityStore := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(HandlersConfig{Client: fakeClient, SecurityStore: securityStore})

	const findingID = "fnd_recurring"
	headSHA := strings.Repeat("a", 40)
	runs := []*store.ScanRun{
		{
			ID: "scan-run-gate-off-1", RunUID: "run_1111111111111111111111111111111111111111111111111111111111111111",
			Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
			RepositoryScanGeneration: scan.Generation, HeadCommit: headSHA,
			TaskName: "source-task-1", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
			Quality: store.LegacyScanQuality(),
		},
		{
			ID: "scan-run-gate-off-2", RunUID: "run_2222222222222222222222222222222222222222222222222222222222222222",
			Namespace: scan.Namespace, RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID),
			RepositoryScanGeneration: scan.Generation, HeadCommit: headSHA,
			TaskName: "source-task-2", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC().Add(time.Second),
			Quality: store.LegacyScanQuality(),
		},
	}
	for _, run := range runs {
		require.NoError(t, securityStore.CreateScanRun(context.Background(), run))
	}

	finding := &store.Finding{
		ID: findingID, Namespace: scan.Namespace, RepositoryScan: scan.Name,
		ScanRunID: runs[0].ID, Title: "Recurring finding", Severity: "high", Confidence: "high",
	}
	first, err := handlers.createSecurityPatchTask(context.Background(), nil, scan, finding)
	require.NoError(t, err)
	require.Empty(t, first.OccurrenceID)
	require.Equal(t, security.ScanStageTaskNameForRun(
		scan.Name, "patch", security.StagePatch, findingID, runs[0].RunUID,
	), first.TaskName)
	require.Equal(t, security.PatchProposalIDForOccurrence(runs[0].RunUID, findingID), first.ID)

	laterFinding := *finding
	laterFinding.ScanRunID = runs[1].ID
	later, err := handlers.createSecurityPatchTask(context.Background(), nil, scan, &laterFinding)
	require.NoError(t, err)
	require.Empty(t, later.OccurrenceID)
	require.Equal(t, security.ScanStageTaskNameForRun(
		scan.Name, "patch", security.StagePatch, findingID, runs[1].RunUID,
	), later.TaskName)
	require.Equal(t, security.PatchProposalIDForOccurrence(runs[1].RunUID, findingID), later.ID)

	require.NotEqual(t, first.TaskName, later.TaskName)
	require.NotEqual(t, first.ID, later.ID)
	require.NotEqual(t, first.Branch, later.Branch)

	proposals, err := securityStore.ListPatchProposals(context.Background(), scan.Namespace, findingID)
	require.NoError(t, err)
	require.Len(t, proposals, 2)

	authorizedLater := &securityFindingAuthorization{finding: &laterFinding, scan: scan, run: runs[1]}
	require.True(t, patchProposalMatchesAuthorizedFinding(later, authorizedLater))
	require.False(t, patchProposalMatchesAuthorizedFinding(first, authorizedLater))
}

func TestCreateSecurityPatchTaskRejectsUnsafeLegacySourceBindings(t *testing.T) {
	tests := []struct {
		name       string
		findingRun string
		run        *store.ScanRun
	}{
		{name: "missing finding run"},
		{name: "missing source run", findingRun: "scan-missing"},
		{name: "legacy run UID", findingRun: "scan-legacy", run: &store.ScanRun{ID: "scan-legacy", HeadCommit: strings.Repeat("a", 40)}},
		{name: "legacy RepositoryScan binding", findingRun: "scan-unbound", run: &store.ScanRun{ID: "scan-unbound", RunUID: "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", HeadCommit: strings.Repeat("b", 40)}},
		{name: "abbreviated source head", findingRun: "scan-short", run: &store.ScanRun{ID: "scan-short", RunUID: "run_cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", HeadCommit: "f00dbabe"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scheme := runtime.NewScheme()
			require.NoError(t, corev1alpha1.AddToScheme(scheme))
			scan := &corev1alpha1.RepositoryScan{
				ObjectMeta: metav1.ObjectMeta{Name: "scan-1", Namespace: "demo", UID: types.UID("scan-1-uid"), Generation: 4},
				Spec:       corev1alpha1.RepositoryScanSpec{RepoURL: securityTestRepoURL, AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"}},
			}
			fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			db, err := sqlite.NewDB(":memory:")
			require.NoError(t, err)
			securityStore := sqlite.NewStore(db, ":memory:")
			handlers := NewHandlers(HandlersConfig{Client: fakeClient, SecurityStore: securityStore})
			if tt.run != nil {
				run := *tt.run
				run.Namespace = scan.Namespace
				run.RepositoryScan = scan.Name
				if tt.name != "legacy RepositoryScan binding" {
					run.RepositoryScanUID = string(scan.UID)
					run.RepositoryScanGeneration = scan.Generation
				}
				run.TaskName = "source-task"
				run.Mode = "manual"
				run.Phase = "succeeded"
				run.StartedAt = time.Now().UTC()
				run.Quality = store.LegacyScanQuality()
				require.NoError(t, securityStore.CreateScanRun(context.Background(), &run))
			}
			_, err = handlers.createSecurityPatchTask(context.Background(), nil, scan, &store.Finding{
				ID: "finding-1", Namespace: scan.Namespace, RepositoryScan: scan.Name, ScanRunID: tt.findingRun,
			})
			var fiberErr *fiber.Error
			require.ErrorAs(t, err, &fiberErr)
			require.Equal(t, fiber.StatusConflict, fiberErr.Code)
			var tasks corev1alpha1.TaskList
			require.NoError(t, fakeClient.List(context.Background(), &tasks))
			require.Empty(t, tasks.Items)
		})
	}
}

func clientObjectKey(name string) client.ObjectKey {
	return client.ObjectKey{Namespace: "demo", Name: name}
}

func envValue(envs []corev1.EnvVar, name string) string {
	for _, env := range envs {
		if env.Name == name {
			return env.Value
		}
	}
	return ""
}

func TestSecurityBundleReadsAuthorizeAgainstHistoricalGeneration(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := securityAuthzTestRepositoryScan("scan-bundle", securityTestRepoURL)
	scan.UID = types.UID("scan-bundle-uid")
	scan.Generation = 2
	app, handlers := setupSecurityHandlersWithAuthzFixture(
		t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan,
	)
	bundle := securityAuthzTestBundle(t, scan, 1)
	_, err := handlers.securityBundleStore.SealSecurityScanBundle(context.Background(), bundle)
	require.NoError(t, err)
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead,
		"tctx":  securityAuthzTestTctx(securityTestRepoURL),
	})
	for _, suffix := range []string{"bundle", "coverage"} {
		req := httptest.NewRequest(http.MethodGet,
			"/security/repositories/scan-bundle/scans/scan-run-api/"+suffix+"?namespace=demo", nil)
		req.Header.Set(TransactionTokenHeaderName, token)
		resp, requestErr := app.Test(req)
		require.NoError(t, requestErr)
		require.Equal(t, http.StatusOK, resp.StatusCode)
	}
}

func TestSecurityBundleReadsRejectReplacementTargetAuthorization(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	current := securityAuthzTestRepositoryScan("scan-bundle-replaced", "https://github.com/example/new-target")
	current.UID = types.UID("scan-bundle-replaced-uid")
	current.Generation = 2
	historical := current.DeepCopy()
	historical.Generation = 1
	historical.Spec.RepoURL = securityTestRepoURL
	app, handlers := setupSecurityHandlersWithAuthzFixture(
		t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, current,
	)
	bundle := securityAuthzTestBundle(t, historical, 1)
	_, err := handlers.securityBundleStore.SealSecurityScanBundle(context.Background(), bundle)
	require.NoError(t, err)
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead,
		"tctx":  securityAuthzTestTctx(current.Spec.RepoURL),
	})
	req := httptest.NewRequest(http.MethodGet,
		"/security/repositories/scan-bundle-replaced/scans/scan-run-api/bundle?namespace=demo", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, requestErr := app.Test(req)
	require.NoError(t, requestErr)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
}

func TestFindingHistoryRejectsSameUIDHistoricalGeneration(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := securityAuthzTestRepositoryScan("scan-history", securityTestRepoURL)
	scan.UID = types.UID("scan-history-uid")
	scan.Generation = 2
	app, handlers := setupSecurityHandlersWithAuthzFixture(
		t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan,
	)
	ctx := context.Background()
	require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
		ID: "scan-history-run", RunUID: "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Namespace: "demo", RepositoryScan: scan.Name, RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: 1,
		TaskName: "history-task", Mode: "manual", Phase: "succeeded", StartedAt: time.Now().UTC(),
		Quality: store.LegacyScanQuality(),
	}))
	require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
		ID: "finding-history", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "scan-history-run",
		Fingerprint: "history-fingerprint", Title: "History", Summary: "historical", Severity: "low",
		Confidence: "low", ValidationStatus: "unvalidated", State: "open",
	}))
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead,
		"tctx":  securityAuthzTestTctx(securityTestRepoURL),
	})
	req := httptest.NewRequest(http.MethodGet, "/security/findings/finding-history/occurrences?namespace=demo", nil)
	req.Header.Set(TransactionTokenHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	t.Cleanup(func() { _ = resp.Body.Close() })
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

type findingHistoryFixtureStore struct {
	store.SecurityIntegrityStore
	occurrences []store.FindingOccurrence
	decisions   []store.FindingDecision
	assessments []store.FindingAssessment
}

func fixtureHistoryPage[T any](items []T, limit int, cursor string) ([]T, string, error) {
	offset := 0
	if cursor != "" {
		parsed, err := strconv.Atoi(cursor)
		if err != nil || parsed < 0 {
			return nil, "", fmt.Errorf("invalid cursor")
		}
		offset = parsed
	}
	if offset >= len(items) {
		return []T{}, "", nil
	}
	end := min(offset+limit, len(items))
	next := ""
	if end-offset == limit {
		next = strconv.Itoa(end)
	}
	return append([]T(nil), items[offset:end]...), next, nil
}

func TestListAuthorizedFindingHistoryBoundsFilteringAndPreservesCursor(t *testing.T) {
	raw := make([]int, findingHistoryScanBudget+1)
	for i := range raw {
		raw[i] = i
	}
	listCalls := 0
	maxBatch := 0
	list := func(limit int, cursor string) ([]int, string, error) {
		listCalls++
		maxBatch = max(maxBatch, limit)
		return fixtureHistoryPage(raw, limit, cursor)
	}
	authorized := func(item *int) (bool, error) {
		return *item == findingHistoryScanBudget, nil
	}

	items, next, err := listAuthorizedFindingHistory(MaxLimit, "", list, authorized)
	require.NoError(t, err)
	require.Empty(t, items)
	require.Equal(t, strconv.Itoa(findingHistoryScanBudget), next)
	require.LessOrEqual(t, maxBatch, findingHistoryFilterBatchSize)
	require.LessOrEqual(t, listCalls, findingHistoryScanBudget/findingHistoryFilterBatchSize+1)

	items, next, err = listAuthorizedFindingHistory(MaxLimit, next, list, authorized)
	require.NoError(t, err)
	require.Equal(t, []int{findingHistoryScanBudget}, items)
	require.Empty(t, next)
}

func (s *findingHistoryFixtureStore) ListFindingOccurrences(
	_ context.Context, filter store.FindingOccurrenceFilter,
) ([]store.FindingOccurrence, string, error) {
	items := make([]store.FindingOccurrence, 0, len(s.occurrences))
	for _, item := range s.occurrences {
		if item.Namespace == filter.Namespace && item.RepositoryScan == filter.RepositoryScan &&
			item.PublicFindingID == filter.PublicFindingID && (filter.ScanRunID == "" || item.ScanRunID == filter.ScanRunID) {
			items = append(items, item)
		}
	}
	return fixtureHistoryPage(items, filter.Limit, filter.Cursor)
}

func (s *findingHistoryFixtureStore) GetFindingOccurrence(
	_ context.Context, namespace, id string,
) (*store.FindingOccurrence, error) {
	for i := range s.occurrences {
		if s.occurrences[i].Namespace == namespace && s.occurrences[i].ID == id {
			item := s.occurrences[i]
			return &item, nil
		}
	}
	return nil, store.ErrNotFound
}

func (s *findingHistoryFixtureStore) ListFindingDecisions(
	_ context.Context, filter store.FindingDecisionFilter,
) ([]store.FindingDecision, string, error) {
	items := make([]store.FindingDecision, 0, len(s.decisions))
	for _, item := range s.decisions {
		if item.Namespace == filter.Namespace && item.RepositoryScan == filter.RepositoryScan &&
			item.PublicFindingID == filter.PublicFindingID && (filter.OccurrenceID == "" || item.OccurrenceID == filter.OccurrenceID) {
			items = append(items, item)
		}
	}
	return fixtureHistoryPage(items, filter.Limit, filter.Cursor)
}

func (s *findingHistoryFixtureStore) ListFindingAssessments(
	_ context.Context, filter store.FindingAssessmentFilter,
) ([]store.FindingAssessment, string, error) {
	items := make([]store.FindingAssessment, 0, len(s.assessments))
	for _, item := range s.assessments {
		if item.Namespace == filter.Namespace && item.RepositoryScan == filter.RepositoryScan &&
			item.PublicFindingID == filter.PublicFindingID && (filter.OccurrenceID == "" || item.OccurrenceID == filter.OccurrenceID) &&
			(filter.Kind == "" || item.Kind == filter.Kind) {
			items = append(items, item)
		}
	}
	return fixtureHistoryPage(items, filter.Limit, filter.Cursor)
}

func TestFindingHistoryReturnsOnlyCurrentGenerationWithSafePagination(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := securityAuthzTestRepositoryScan("scan-history-filter", securityTestRepoURL)
	scan.UID = types.UID("scan-history-filter-uid")
	scan.Generation = 2
	app, handlers := setupSecurityHandlersWithAuthzFixture(
		t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan,
	)
	ctx := context.Background()
	runs := []store.ScanRun{
		{ID: "run-recreated", RunUID: "run_1111111111111111111111111111111111111111111111111111111111111111", RepositoryScanUID: "old-scan-uid", RepositoryScanGeneration: 1},
		{ID: "run-historical", RunUID: "run_2222222222222222222222222222222222222222222222222222222222222222", RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: 1},
		{ID: "run-current", RunUID: "run_3333333333333333333333333333333333333333333333333333333333333333", RepositoryScanUID: string(scan.UID), RepositoryScanGeneration: scan.Generation},
	}
	for i := range runs {
		runs[i].Namespace = "demo"
		runs[i].RepositoryScan = scan.Name
		runs[i].TaskName = "task-" + runs[i].ID
		runs[i].Mode = "manual"
		runs[i].Phase = "succeeded"
		runs[i].StartedAt = time.Now().UTC().Add(time.Duration(i) * time.Minute)
		runs[i].Quality = store.LegacyScanQuality()
		require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &runs[i]))
	}
	require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
		ID: "finding-history-filter", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "run-current",
		CurrentOccurrenceID: "occ-current", Fingerprint: "history-filter-fingerprint", Title: "History",
		Summary: "history", Severity: "low", Confidence: "low", ValidationStatus: "unvalidated", State: "open",
	}))
	fixture := &findingHistoryFixtureStore{SecurityIntegrityStore: handlers.securityIntegrityStore}
	fixture.occurrences = []store.FindingOccurrence{
		{ID: "occ-recreated", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "run-recreated", PublicFindingID: "finding-history-filter"},
		{ID: "occ-historical", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "run-historical", PublicFindingID: "finding-history-filter"},
		{ID: "occ-current", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "run-current", PublicFindingID: "finding-history-filter"},
	}
	fixture.decisions = []store.FindingDecision{
		{ID: "decision-recreated", Namespace: "demo", RepositoryScan: scan.Name, PublicFindingID: "finding-history-filter", OccurrenceID: "occ-recreated"},
		{ID: "decision-historical", Namespace: "demo", RepositoryScan: scan.Name, PublicFindingID: "finding-history-filter", OccurrenceID: "occ-historical"},
		{ID: "decision-current", Namespace: "demo", RepositoryScan: scan.Name, PublicFindingID: "finding-history-filter", OccurrenceID: "occ-current"},
	}
	fixture.assessments = []store.FindingAssessment{
		{ID: "assessment-recreated", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "run-recreated", PublicFindingID: "finding-history-filter"},
		{ID: "assessment-historical", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "run-historical", PublicFindingID: "finding-history-filter"},
		{ID: "assessment-current", Namespace: "demo", RepositoryScan: scan.Name, ScanRunID: "run-current", PublicFindingID: "finding-history-filter"},
	}
	handlers.securityIntegrityStore = fixture
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead, "tctx": securityAuthzTestTctx(securityTestRepoURL),
	})

	for _, tt := range []struct {
		endpoint string
		idField  string
		want     []string
	}{
		{endpoint: "occurrences", idField: "id", want: []string{"occ-current"}},
		{endpoint: "decisions", idField: "decisionID", want: []string{"decision-current"}},
		{endpoint: "assessments", idField: "id", want: []string{"assessment-current"}},
	} {
		t.Run(tt.endpoint, func(t *testing.T) {
			cursor := ""
			for page, wantID := range tt.want {
				path := "/security/findings/finding-history-filter/" + tt.endpoint + "?namespace=demo&limit=1"
				if cursor != "" {
					path += "&cursor=" + cursor
				}
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set(TransactionTokenHeaderName, token)
				resp, err := app.Test(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)
				var body struct {
					Items    []map[string]any `json:"items"`
					Metadata struct {
						Continue string `json:"continue"`
					} `json:"metadata"`
				}
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
				_ = resp.Body.Close()
				require.Len(t, body.Items, 1)
				require.Equal(t, wantID, body.Items[0][tt.idField])
				cursor = body.Metadata.Continue
				_ = page
			}
			if cursor != "" {
				path := "/security/findings/finding-history-filter/" + tt.endpoint + "?namespace=demo&limit=1&cursor=" + cursor
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set(TransactionTokenHeaderName, token)
				resp, err := app.Test(req)
				require.NoError(t, err)
				require.Equal(t, http.StatusOK, resp.StatusCode)
				var body struct {
					Items    []map[string]any `json:"items"`
					Metadata struct {
						Continue string `json:"continue"`
					} `json:"metadata"`
				}
				require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
				_ = resp.Body.Close()
				require.Empty(t, body.Items)
				require.Empty(t, body.Metadata.Continue)
			}
		})
	}
}

func TestParseFindingHistoryPagination(t *testing.T) {
	tests := []struct {
		name       string
		query      string
		wantLimit  int
		wantCursor string
		wantError  string
	}{
		{name: "default", query: "?cursor=next-page", wantLimit: 50, wantCursor: "next-page"},
		{name: "minimum", query: "?limit=1", wantLimit: 1},
		{name: "maximum", query: "?limit=500&cursor=max-page", wantLimit: MaxLimit, wantCursor: "max-page"},
		{name: "zero", query: "?limit=0", wantError: "limit must be at least 1"},
		{name: "negative", query: "?limit=-1", wantError: "limit must be at least 1"},
		{name: "above maximum", query: "?limit=501", wantError: "limit must not exceed 500"},
		{name: "malformed", query: "?limit=invalid", wantError: "invalid limit parameter"},
		{name: "integer overflow", query: "?limit=999999999999999999999999999999999", wantError: "invalid limit parameter"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()
			var got *findingHistoryPagination
			var parseErr error
			app.Get("/", func(c fiber.Ctx) error {
				got, parseErr = parseFindingHistoryPagination(c)
				return c.SendStatus(fiber.StatusNoContent)
			})

			resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/"+tt.query, nil))
			require.NoError(t, err)
			t.Cleanup(func() { _ = resp.Body.Close() })
			if tt.wantError != "" {
				require.ErrorContains(t, parseErr, tt.wantError)
				require.Nil(t, got)
				return
			}
			require.NoError(t, parseErr)
			require.Equal(t, &findingHistoryPagination{Limit: tt.wantLimit, Continue: tt.wantCursor}, got)
		})
	}
}

func TestFindingHistoryPaginationLimits(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := securityAuthzTestRepositoryScan("scan-history-pagination", securityTestRepoURL)
	scan.UID = types.UID("scan-history-pagination-uid")
	scan.Generation = 1
	app, handlers := setupSecurityHandlersWithAuthzFixture(
		t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan,
	)
	ctx := context.Background()
	require.NoError(t, handlers.securityStore.CreateScanRun(ctx, &store.ScanRun{
		ID:                       "scan-history-pagination-run",
		RunUID:                   "run_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb",
		Namespace:                "demo",
		RepositoryScan:           scan.Name,
		RepositoryScanUID:        string(scan.UID),
		RepositoryScanGeneration: scan.Generation,
		TaskName:                 "history-pagination-task",
		Mode:                     "manual",
		Phase:                    "succeeded",
		StartedAt:                time.Now().UTC(),
		Quality:                  store.LegacyScanQuality(),
	}))
	require.NoError(t, handlers.securityStore.UpsertFinding(ctx, &store.Finding{
		ID:                  "finding-history-pagination",
		Namespace:           "demo",
		RepositoryScan:      scan.Name,
		ScanRunID:           "scan-history-pagination-run",
		CurrentOccurrenceID: "occurrence-history-pagination",
		Fingerprint:         "history-pagination-fingerprint",
		Title:               "History pagination",
		Summary:             "pagination fixture",
		Severity:            "low",
		Confidence:          "low",
		ValidationStatus:    "unvalidated",
		State:               "open",
	}))

	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeSecurityRead,
		"tctx":  securityAuthzTestTctx(securityTestRepoURL),
	})
	endpoints := []string{"occurrences", "decisions", "assessments"}
	limits := []struct {
		name       string
		query      string
		wantStatus int
	}{
		{name: "default", wantStatus: http.StatusOK},
		{name: "minimum", query: "1", wantStatus: http.StatusOK},
		{name: "maximum", query: strconv.Itoa(MaxLimit), wantStatus: http.StatusOK},
		{name: "zero", query: "0", wantStatus: http.StatusBadRequest},
		{name: "negative", query: "-1", wantStatus: http.StatusBadRequest},
		{name: "above maximum", query: strconv.Itoa(MaxLimit + 1), wantStatus: http.StatusBadRequest},
		{name: "non numeric", query: "invalid", wantStatus: http.StatusBadRequest},
	}
	for _, endpoint := range endpoints {
		for _, tt := range limits {
			t.Run(endpoint+"/"+tt.name, func(t *testing.T) {
				path := "/security/findings/finding-history-pagination/" + endpoint + "?namespace=demo"
				if tt.query != "" {
					path += "&limit=" + tt.query
				}
				req := httptest.NewRequest(http.MethodGet, path, nil)
				req.Header.Set(TransactionTokenHeaderName, token)
				resp, err := app.Test(req)
				require.NoError(t, err)
				t.Cleanup(func() { _ = resp.Body.Close() })
				require.Equal(t, tt.wantStatus, resp.StatusCode)
			})
		}
	}
}
