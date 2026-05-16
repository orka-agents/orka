/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
)

const (
	securityTestRepoURL   = "https://github.com/sozercan/actions-test"
	securityTestRepoPRURL = securityTestRepoURL + "/pull/99"
)

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
			req.Header.Set(KontxtHeaderName, token)
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
		ContextTokenAuthorization: authz,
	})

	app := fiber.New()
	app.Use(NewAuthMiddleware(handlers.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Post("/security/repositories", handlers.CreateRepositoryScan)
	app.Get("/security/repositories", handlers.ListRepositoryScans)
	app.Put("/security/repositories/:name", handlers.UpdateRepositoryScan)
	app.Post("/security/repositories/:name/scans", handlers.CreateManualSecurityScan)
	app.Post("/security/findings/:id/dismiss", handlers.DismissSecurityFinding)
	app.Post("/security/findings/:id/reopen", handlers.ReopenSecurityFinding)
	app.Post("/security/findings/:id/validate", handlers.ValidateSecurityFinding)
	app.Post("/security/findings/:id/patch", handlers.GenerateSecurityPatch)
	app.Post("/security/findings/:id/pull-request", handlers.CreateSecurityPullRequest)
	return app, handlers
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
					Name:      "scan-1",
					Namespace: "demo",
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

			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx":  tt.tctx,
			})
			req := httptest.NewRequest(http.MethodPost, "/security/findings/finding-1/patch?namespace=demo", nil)
			req.Header.Set(KontxtHeaderName, token)
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
		name string
		tctx map[string]any
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
					AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
				},
			}
			app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan)
			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeSecurityWrite,
				"tctx":  tt.tctx,
			})
			req := httptest.NewRequest(http.MethodPost, "/security/repositories/scan-1/scans?namespace=demo", nil)
			req.Header.Set(KontxtHeaderName, token)
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusForbidden, resp.StatusCode)

			var tasks corev1alpha1.TaskList
			require.NoError(t, handlers.client.List(context.Background(), &tasks, client.InNamespace("demo")))
			require.Empty(t, tasks.Items)
		})
	}
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
			req.Header.Set(KontxtHeaderName, token)
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

func TestSecurityFindingMutations_ContextTokenTransactionContextAuthorizationDenials(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scan-1",
			Namespace: "demo",
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
			req.Header.Set(KontxtHeaderName, token)
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

func TestCreateSecurityPullRequest_ContextTokenTransactionContextAuthorizationDenied(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scan-1",
			Namespace: "demo",
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
	req.Header.Set(KontxtHeaderName, token)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)

	finding, err := handlers.securityStore.GetFinding(ctx, "demo", "finding-1")
	require.NoError(t, err)
	require.Equal(t, "validated", finding.State)
}

func TestCreateManualSecurityScan_ContextTokenStampsTaskRequesterAndTransaction(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")

	scan := &corev1alpha1.RepositoryScan{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scan-1",
			Namespace: "demo",
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL:          securityTestRepoURL,
			Branch:           "main",
			AnalysisAgentRef: corev1alpha1.AgentReference{Name: "analysis"},
		},
	}
	app, handlers := setupSecurityHandlersWithAuthzFixture(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce, scan)

	token := issueTestContextToken(t, provider, nil, map[string]any{"scope": ContextTokenScopeSecurityWrite})
	req := httptest.NewRequest(http.MethodPost, "/security/repositories/scan-1/scans?namespace=demo", nil)
	req.Header.Set(KontxtHeaderName, token)
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
			Name:      "scan-1",
			Namespace: "demo",
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
	require.NoError(t, securityStore.UpsertFinding(ctx, &store.Finding{
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
	require.NoError(t, securityStore.CreatePatchProposal(ctx, &store.PatchProposal{
		ID:             "patch-1",
		Namespace:      "demo",
		RepositoryScan: "scan-1",
		FindingID:      "finding-1",
		TaskName:       "patch-task-1",
		Branch:         "orka/security/fnd-123",
		Status:         "succeeded",
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
	require.Len(t, proposals, 1)
	require.Equal(t, "pr_opened", proposals[0].Status)
	require.Equal(t, securityTestRepoPRURL, proposals[0].PRURL)
	require.NotNil(t, proposals[0].PRNumber)
	require.Equal(t, 99, *proposals[0].PRNumber)

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
		ObjectMeta: metav1.ObjectMeta{
			Name:      "scan-1",
			Namespace: "demo",
		},
		Spec: corev1alpha1.RepositoryScanSpec{
			RepoURL: securityTestRepoURL,
			Branch:  "main",
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
		ID:         "fnd_123",
		Namespace:  "demo",
		Title:      "Command injection",
		Severity:   "high",
		Confidence: "high",
	}

	proposal, err := handlers.createSecurityPatchTask(context.Background(), nil, scan, finding)
	require.NoError(t, err)
	require.Regexp(t, `^orka/security/fnd-123-[a-f0-9]{12}$`, proposal.Branch)

	task := &corev1alpha1.Task{}
	require.NoError(t, fakeClient.Get(context.Background(), clientObjectKey(proposal.TaskName), task))
	require.Equal(t, "true", envValue(task.Spec.Env, "ORKA_REQUIRE_PUSH_BRANCH"))
	require.NotNil(t, task.Spec.AgentRuntime)
	require.NotNil(t, task.Spec.AgentRuntime.Workspace)
	require.Equal(t, proposal.Branch, task.Spec.AgentRuntime.Workspace.PushBranch)
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
