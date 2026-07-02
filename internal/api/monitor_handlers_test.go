package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
	"github.com/sozercan/orka/internal/workerenv"
)

const monitorTestRepoURL = "https://github.com/sozercan/orka"
const monitorTestReviewerSecret = "reviewer-credentials"

func setupRepositoryMonitorHandlers(t *testing.T, ctxTokenConfig ContextTokenConfig, mode string, objects ...crclient.Object) (*fiber.App, *Handlers) {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	objects = append([]crclient.Object{
		repositoryMonitorHandlerTestAgent("reviewer", corev1alpha1.AgentRuntimeClaude),
		repositoryMonitorHandlerTestReviewerSecret(),
	}, objects...)
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.RepositoryMonitor{}).
		WithObjects(objects...).
		Build()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	monitorStore := sqlite.NewStore(db, ":memory:")
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: mode})
	require.NoError(t, err)

	handlers := NewHandlers(HandlersConfig{
		Client:                    fakeClient,
		RepositoryMonitorStore:    monitorStore,
		ArtifactStore:             monitorStore,
		ContextTokenAuthorization: authz,
	})
	app := fiber.New()
	if ctxTokenConfig.Enabled() {
		app.Use(NewAuthMiddleware(handlers.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	}
	app.Post("/monitors/repositories", handlers.CreateRepositoryMonitor)
	app.Get("/monitors/repositories", handlers.ListRepositoryMonitors)
	app.Get("/monitors/repositories/:name", handlers.GetRepositoryMonitor)
	app.Put("/monitors/repositories/:name", handlers.UpdateRepositoryMonitor)
	app.Delete("/monitors/repositories/:name", handlers.DeleteRepositoryMonitor)
	app.Post("/monitors/repositories/:name/runs", handlers.CreateRepositoryMonitorRun)
	app.Get("/monitors/repositories/:name/runs", handlers.ListRepositoryMonitorRuns)
	app.Get("/monitors/repositories/:name/items", handlers.ListRepositoryMonitorItems)
	app.Post("/monitors/repositories/:name/commands", handlers.CreateRepositoryMonitorCommandEvent)
	app.Get("/monitors/work-actions", handlers.ListRepositoryMonitorWorkActions)
	app.Get("/monitors/work-actions/:id", handlers.GetRepositoryMonitorWorkAction)
	app.Get("/monitors/implementation-jobs", handlers.ListRepositoryMonitorImplementationJobs)
	app.Get("/monitors/implementation-jobs/:id", handlers.GetRepositoryMonitorImplementationJob)
	app.Get("/monitors/implementation-jobs/:id/patch-preview", handlers.GetRepositoryMonitorImplementationPatchPreview)
	app.Get("/monitors/mutations", handlers.ListRepositoryMonitorGitHubMutations)
	app.Get("/monitors/mutations/:id", handlers.GetRepositoryMonitorGitHubMutation)
	app.Get("/monitors/events", handlers.ListRepositoryMonitorEvents)
	return app, handlers
}

func repositoryMonitorHandlerTestAgent(name string, runtimeType corev1alpha1.AgentRuntimeType) *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"},
		Spec: corev1alpha1.AgentSpec{
			Runtime:   &corev1alpha1.AgentCLIRuntime{Type: runtimeType},
			SecretRef: &corev1.LocalObjectReference{Name: monitorTestReviewerSecret},
		},
	}
}

func repositoryMonitorHandlerTestReviewerSecret() *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: monitorTestReviewerSecret, Namespace: "demo"},
		Data: map[string][]byte{
			workerenv.AnthropicAPIKey: []byte("anthropic-key"),
		},
	}
}

func TestRepositoryMonitorHandlers_CRUDAndManualRun(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{
		"name":"repo-monitor",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"agents":{"reviewer":{"name":"reviewer"}}
		}
	}`, monitorTestRepoURL)

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var created corev1alpha1.RepositoryMonitor
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.Equal(t, "github", created.Spec.Provider)
	require.Equal(t, "main", created.Spec.Branch)
	require.NotNil(t, created.Spec.Targets.PullRequests.Enabled)
	require.True(t, *created.Spec.Targets.PullRequests.Enabled)
	require.Equal(t, "sozercan", created.Spec.Owner)
	require.Equal(t, "orka", created.Spec.Repository)

	resp, err = app.Test(httptest.NewRequest(http.MethodGet, "/monitors/repositories?namespace=demo", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	unsupportedRunReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/runs?namespace=demo", strings.NewReader(`{"targetKind":"commit","targetNumber":42}`))
	unsupportedRunReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(unsupportedRunReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)

	runReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/runs?namespace=demo", strings.NewReader(`{"trigger":"schedule","targetKind":"pull_request","targetNumber":42}`))
	runReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(runReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var run store.MonitorRun
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&run))
	require.Equal(t, "manual", run.Trigger)
	require.Equal(t, int64(42), run.TargetNumber)
	require.Equal(t, "queued", run.Phase)

	runs, _, err := handlers.repositoryMonitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{
		Namespace:   "demo",
		MonitorName: "repo-monitor",
		Limit:       10,
	})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, run.ID, runs[0].ID)

	duplicateRunReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/runs?namespace=demo", strings.NewReader(`{"targetKind":"pull_request","targetNumber":42}`))
	duplicateRunReq.Header.Set("Content-Type", "application/json")
	resp, err = app.Test(duplicateRunReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, resp.StatusCode)
}

func TestRepositoryMonitorHandlers_ListSubresourcesAcceptContinueToken(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	createRepositoryMonitorForHandlerTest(t, app, "repo-monitor", "demo")

	now := time.Now().UTC().Truncate(time.Second)
	for _, run := range []store.MonitorRun{
		{ID: "run-old", MonitorNamespace: "demo", MonitorName: "repo-monitor", Trigger: "manual", Phase: "succeeded", StartedAt: now.Add(-time.Minute)},
		{ID: "run-new", MonitorNamespace: "demo", MonitorName: "repo-monitor", Trigger: "manual", Phase: "failed", StartedAt: now},
	} {
		require.NoError(t, handlers.repositoryMonitorStore.CreateMonitorRun(t.Context(), &run))
	}

	firstReq := httptest.NewRequest(http.MethodGet, "/monitors/repositories/repo-monitor/runs?namespace=demo&limit=1", nil)
	firstResp, err := app.Test(firstReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, firstResp.StatusCode)
	var firstPage struct {
		Items    []store.MonitorRun `json:"items"`
		Metadata struct {
			Continue string `json:"continue"`
		} `json:"metadata"`
	}
	require.NoError(t, json.NewDecoder(firstResp.Body).Decode(&firstPage))
	require.Len(t, firstPage.Items, 1)
	require.Equal(t, "run-new", firstPage.Items[0].ID)
	require.NotEmpty(t, firstPage.Metadata.Continue)

	secondReq := httptest.NewRequest(http.MethodGet, "/monitors/repositories/repo-monitor/runs?namespace=demo&limit=1&continue="+firstPage.Metadata.Continue, nil)
	secondResp, err := app.Test(secondReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, secondResp.StatusCode)
	var secondPage struct {
		Items []store.MonitorRun `json:"items"`
	}
	require.NoError(t, json.NewDecoder(secondResp.Body).Decode(&secondPage))
	require.Len(t, secondPage.Items, 1)
	require.Equal(t, "run-old", secondPage.Items[0].ID)
}

func TestCreateRepositoryMonitor_DerivesRepositoryIdentityFromURL(t *testing.T) {
	app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{
		"name":"repo-monitor",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"owner":"other",
			"repository":"different",
			"agents":{"reviewer":{"name":"reviewer"}}
		}
	}`, monitorTestRepoURL)

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)

	var created corev1alpha1.RepositoryMonitor
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.Equal(t, "sozercan", created.Spec.Owner)
	require.Equal(t, "orka", created.Spec.Repository)
}

func TestCreateRepositoryMonitor_RejectsUnsupportedTargets(t *testing.T) {
	tests := []struct {
		name    string
		targets string
	}{
		{name: "commit target", targets: `"targets":{"pullRequests":{"enabled":false},"commits":{"enabled":true}}`},
		{name: "pull requests disabled", targets: `"targets":{"pullRequests":{"enabled":false}}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
			body := fmt.Sprintf(`{
				"name":"repo-monitor",
				"namespace":"demo",
				"spec":{
					"repoURL":%q,
					%s,
					"agents":{"reviewer":{"name":"reviewer"}}
				}
			}`, monitorTestRepoURL, tt.targets)

			req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func TestCreateRepositoryMonitor_AllowsRequireGreenCI(t *testing.T) {
	app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{
		"name":"repo-monitor",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"review":{"requireGreenCI":true},
			"agents":{"reviewer":{"name":"reviewer"}}
		}
	}`, monitorTestRepoURL)
	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestCreateRepositoryMonitor_RejectsUnsupportedPublishConfig(t *testing.T) {
	tests := []struct {
		name    string
		publish string
		want    string
	}{
		{name: "unsupported event", publish: `{"event":"APPROVE"}`, want: "only supports COMMENT"},
		{name: "unsupported same head policy", publish: `{"sameHeadPolicy":"replace"}`, want: "sameHeadPolicy only supports skip"},
		{name: "invalid min priority", publish: `{"inline":{"minPriority":"P4"}}`, want: "minPriority must be one of"},
		{name: "negative max comments", publish: `{"inline":{"maxComments":-1}}`, want: "maxComments must be between 0 and 50"},
		{name: "too many max comments", publish: `{"inline":{"maxComments":51}}`, want: "maxComments must be between 0 and 50"},
		{name: "unsupported mode", publish: `{"mode":"inline_findings"}`, want: "mode must be summary_only"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
			body := fmt.Sprintf(`{
				"name":"repo-monitor",
				"namespace":"demo",
				"spec":{
					"repoURL":%q,
					"review":{"publish":%s},
					"agents":{"reviewer":{"name":"reviewer"}}
				}
			}`, monitorTestRepoURL, tt.publish)

			req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			require.Contains(t, readRespBody(t, resp), tt.want)
		})
	}
}

func TestCreateRepositoryMonitor_AcceptsSafePublishConfig(t *testing.T) {
	app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{
		"name":"repo-monitor",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"review":{"publish":{
				"enabled":true,
				"mode":"summary_with_inline_findings",
				"event":"COMMENT",
				"postPassed":true,
				"sameHeadPolicy":"skip",
				"inline":{"enabled":true,"minPriority":"P2","maxComments":10,"onlyChangedLines":true}
			}},
			"agents":{"reviewer":{"name":"reviewer"}}
		}
	}`, monitorTestRepoURL)

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var created corev1alpha1.RepositoryMonitor
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&created))
	require.True(t, created.Spec.Review.Publish.Enabled)
	require.Equal(t, "COMMENT", created.Spec.Review.Publish.Event)
	require.NotNil(t, created.Spec.Review.Publish.PostPassed)
	require.True(t, *created.Spec.Review.Publish.PostPassed)
}

func TestCreateRepositoryMonitor_RejectsUnsupportedReviewerAgent(t *testing.T) {
	tests := []struct {
		name     string
		agent    *corev1alpha1.Agent
		reviewer string
		want     string
	}{
		{name: "missing agent", reviewer: "missing-reviewer", want: "not found"},
		{
			name:     "missing runtime",
			agent:    &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Name: "no-runtime", Namespace: "demo"}},
			reviewer: "no-runtime",
			want:     "must use the claude runtime",
		},
		{
			name:     "codex runtime",
			agent:    repositoryMonitorHandlerTestAgent("codex-reviewer", corev1alpha1.AgentRuntimeCodex),
			reviewer: "codex-reviewer",
			want:     "is not supported",
		},
		{
			name:     "copilot runtime",
			agent:    repositoryMonitorHandlerTestAgent("copilot-reviewer", corev1alpha1.AgentRuntimeCopilot),
			reviewer: "copilot-reviewer",
			want:     "is not supported",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var objects []crclient.Object
			if tt.agent != nil {
				objects = append(objects, tt.agent)
			}
			app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff, objects...)
			body := fmt.Sprintf(`{
				"name":"repo-monitor",
				"namespace":"demo",
				"spec":{
					"repoURL":%q,
					"agents":{"reviewer":{"name":%q}}
				}
			}`, monitorTestRepoURL, tt.reviewer)

			req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			require.Contains(t, readRespBody(t, resp), tt.want)
		})
	}
}

func TestCreateRepositoryMonitor_RejectsReviewerWithoutClaudeCredentials(t *testing.T) {
	tests := []struct {
		name     string
		objects  []crclient.Object
		reviewer string
		want     string
	}{
		{
			name: "missing secretRef",
			objects: []crclient.Object{
				&corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{Name: "no-secret", Namespace: "demo"},
					Spec: corev1alpha1.AgentSpec{
						Runtime: &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
					},
				},
			},
			reviewer: "no-secret",
			want:     "must reference a Secret",
		},
		{
			name: "secret without auth key",
			objects: []crclient.Object{
				&corev1alpha1.Agent{
					ObjectMeta: metav1.ObjectMeta{Name: "bad-secret-reviewer", Namespace: "demo"},
					Spec: corev1alpha1.AgentSpec{
						Runtime:   &corev1alpha1.AgentCLIRuntime{Type: corev1alpha1.AgentRuntimeClaude},
						SecretRef: &corev1.LocalObjectReference{Name: "bad-reviewer-secret"},
					},
				},
				&corev1.Secret{
					ObjectMeta: metav1.ObjectMeta{Name: "bad-reviewer-secret", Namespace: "demo"},
					Data:       map[string][]byte{workerenv.AnthropicBaseURL: []byte("https://anthropic.example")},
				},
			},
			reviewer: "bad-secret-reviewer",
			want:     "must contain a supported Claude auth key",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff, tt.objects...)
			body := fmt.Sprintf(`{
				"name":"repo-monitor",
				"namespace":"demo",
				"spec":{
					"repoURL":%q,
					"agents":{"reviewer":{"name":%q}}
				}
			}`, monitorTestRepoURL, tt.reviewer)

			req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
			require.Contains(t, readRespBody(t, resp), tt.want)
		})
	}
}

func createRepositoryMonitorForHandlerTest(t *testing.T, app *fiber.App, name, namespace string) {
	t.Helper()
	body := fmt.Sprintf(`{
		"name":%q,
		"namespace":%q,
		"spec":{
			"repoURL":%q,
			"agents":{"reviewer":{"name":"reviewer"}}
		}
	}`, name, namespace, monitorTestRepoURL)
	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
}

func TestCreateRepositoryMonitor_RejectsNonGitHubAndCredentialURLs(t *testing.T) {
	tests := []struct {
		name    string
		repoURL string
	}{
		{name: "non GitHub HTTPS host", repoURL: "https://evil.example/sozercan/orka"},
		{name: "HTTPS credentials", repoURL: "https://token@github.com/sozercan/orka"},
		{name: "non GitHub SSH host", repoURL: "git@evil.example:sozercan/orka.git"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
			body := fmt.Sprintf(`{
				"name":"repo-monitor",
				"namespace":"demo",
				"spec":{
					"repoURL":%q,
					"agents":{"reviewer":{"name":"reviewer"}}
				}
			}`, tt.repoURL)

			req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func TestRepositoryMonitorActions_ContextTokenAuthorization(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	createBody := fmt.Sprintf(`{
		"name":"repo-monitor",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"agents":{"reviewer":{"name":"reviewer"}}
		}
	}`, monitorTestRepoURL)

	tests := []struct {
		name   string
		method string
		path   string
		body   string
		scope  string
		want   int
	}{
		{
			name:   "list allowed with monitor read scope",
			method: http.MethodGet,
			path:   "/monitors/repositories?namespace=demo",
			scope:  ContextTokenScopeMonitorsRead,
			want:   http.StatusOK,
		},
		{
			name:   "list denied without monitor read scope",
			method: http.MethodGet,
			path:   "/monitors/repositories?namespace=demo",
			scope:  ContextTokenScopeSecurityRead,
			want:   http.StatusForbidden,
		},
		{
			name:   "create allowed with monitor write scope",
			method: http.MethodPost,
			path:   "/monitors/repositories",
			body:   createBody,
			scope:  ContextTokenScopeMonitorsWrite,
			want:   http.StatusCreated,
		},
		{
			name:   "create denied without monitor write scope",
			method: http.MethodPost,
			path:   "/monitors/repositories",
			body:   createBody,
			scope:  ContextTokenScopeMonitorsRead,
			want:   http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := setupRepositoryMonitorHandlers(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce)
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

func TestCreateRepositoryMonitor_ContextTokenAgentScopeRejectsExtraAgents(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	createBody := fmt.Sprintf(`{
		"name":"repo-monitor",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"agents":{
				"reviewer":{"name":"reviewer"},
				"repairer":{"name":"repairer"}
			}
		}
	}`, monitorTestRepoURL)

	tests := []struct {
		name               string
		transactionContext map[string]any
		want               int
	}{
		{
			name: "denies extra agent outside single agent context",
			transactionContext: map[string]any{
				"agent": "demo/reviewer",
			},
			want: http.StatusForbidden,
		},
		{
			name: "allows extra agent when allowed agents covers it",
			transactionContext: map[string]any{
				"agent":         "demo/reviewer",
				"allowedAgents": []any{"demo/reviewer", "demo/repairer"},
			},
			want: http.StatusCreated,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app, _ := setupRepositoryMonitorHandlers(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce)
			token := issueTestContextToken(t, provider, nil, map[string]any{
				"scope": ContextTokenScopeMonitorsWrite,
				"tctx":  tt.transactionContext,
			})
			req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(createBody))
			req.Header.Set(KontxtHeaderName, token)
			req.Header.Set("Content-Type", "application/json")
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, tt.want, resp.StatusCode)
		})
	}
}

func TestCreateRepositoryMonitor_ContextTokenAgentScopeAuthorizesBeforeReviewerLookup(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	app, _ := setupRepositoryMonitorHandlers(t, ctxTokenConfig, ContextTokenAuthorizationModeEnforce)
	body := fmt.Sprintf(`{
		"name":"repo-monitor",
		"namespace":"demo",
		"spec":{
			"repoURL":%q,
			"agents":{"reviewer":{"name":"missing-reviewer"}}
		}
	}`, monitorTestRepoURL)
	token := issueTestContextToken(t, provider, nil, map[string]any{
		"scope": ContextTokenScopeMonitorsWrite,
		"tctx": map[string]any{
			"agent": "demo/reviewer",
		},
	})

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	req.Header.Set(KontxtHeaderName, token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, resp.StatusCode)
	require.NotContains(t, readRespBody(t, resp), "missing-reviewer")
}

func TestGetRepositoryMonitorImplementationPatchPreview(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"targets":{"pullRequests":{"enabled":false},"issues":{"enabled":true}},"agents":{}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, handlers.repositoryMonitorStore.CreateImplementationJob(t.Context(), &store.ImplementationJob{
		ID:               "impl-preview",
		MonitorNamespace: "demo",
		MonitorName:      "repo-monitor",
		IssueNumber:      42,
		Phase:            "patch_ready",
		TaskName:         "impl-task",
		PatchArtifactID:  "orka-issue-42-summary.json",
	}))
	artifact := []byte(`{"schemaVersion":"orka.patch.v1","patchArtifactID":"orka-issue-42.diff","changedFiles":["internal/x.go"]}`)
	require.NoError(t, handlers.artifactStore.SaveArtifact(t.Context(), "demo", "impl-task", "orka-issue-42-summary.json", "application/json", artifact))

	req := httptest.NewRequest(http.MethodGet, "/monitors/implementation-jobs/impl-preview/patch-preview?namespace=demo", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	var result map[string]any
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Equal(t, "application/json", result["contentType"])
	patch, ok := result["patch"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "orka.patch.v1", patch["schemaVersion"])
}

func TestCreateRepositoryMonitorCommandEventRejectsIssueTargetSHA(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"targets":{"pullRequests":{"enabled":false},"issues":{"enabled":true}},"agents":{}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, handlers.repositoryMonitorStore.UpsertMonitorItem(t.Context(), &store.MonitorItem{MonitorNamespace: "demo", MonitorName: "repo-monitor", Kind: repositoryMonitorTargetKindIssue, ItemKey: "13", Number: 13, State: "open", SnapshotDigest: "sha256:issue13"}))
	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/commands?namespace=demo", strings.NewReader(`{"kind":"issue","number":13,"intent":"plan","targetSHA":"not-for-issues"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readRespBody(t, resp), "targetSHA is only supported")
}

func TestCreateRepositoryMonitorCommandEventQueuesRun(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"targets":{"pullRequests":{"enabled":false},"issues":{"enabled":true}},"agents":{}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, handlers.repositoryMonitorStore.UpsertMonitorItem(t.Context(), &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "repo-monitor",
		Kind:             repositoryMonitorTargetKindIssue,
		ItemKey:          "7",
		Number:           7,
		State:            "open",
		SnapshotDigest:   "sha256:issue7",
	}))
	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/commands?namespace=demo", strings.NewReader(`{"kind":"issue","number":7,"intent":"plan"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var command store.CommandEvent
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&command))
	require.Equal(t, "api", command.Source)
	require.Equal(t, "sozercan/orka", command.Repo)
	require.Equal(t, "plan", command.Intent)
	runs, _, err := handlers.repositoryMonitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "demo", MonitorName: "repo-monitor", TargetKind: "issue", TargetNumber: 7, Limit: 10})
	require.NoError(t, err)
	require.Len(t, runs, 1)
	require.Equal(t, command.ID, runs[0].CommandEventID)

	actions, _, err := handlers.repositoryMonitorStore.ListWorkActions(t.Context(), store.WorkActionFilter{Namespace: "demo", MonitorName: "repo-monitor", TargetKind: "issue", TargetNumber: 7, DesiredAction: "plan", Limit: 10})
	require.NoError(t, err)
	require.Len(t, actions, 1)
	require.Equal(t, command.ID, actions[0].CommandEventID)
	require.Equal(t, runs[0].ID, actions[0].RunID)
	require.Equal(t, "queued", actions[0].Status)

	listReq := httptest.NewRequest(http.MethodGet, "/monitors/work-actions?namespace=demo&name=repo-monitor&kind=issue&number=7", nil)
	listResp, err := app.Test(listReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, listResp.StatusCode)
	var listed struct {
		Items []store.WorkAction `json:"items"`
	}
	require.NoError(t, json.NewDecoder(listResp.Body).Decode(&listed))
	require.Len(t, listed.Items, 1)
}

func TestCreateRepositoryMonitorCommandEventRequiresInventoryForIssuePlan(t *testing.T) {
	app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"targets":{"pullRequests":{"enabled":false},"issues":{"enabled":true}},"agents":{}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/commands?namespace=demo", strings.NewReader(`{"kind":"issue","number":8,"intent":"plan"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readRespBody(t, resp), "must be present in monitor inventory")
}

func TestCreateRepositoryMonitorCommandEventBlocksGuardedTarget(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"targets":{"pullRequests":{"enabled":false},"issues":{"enabled":true}},"policy":{"protectedLabels":["security-sensitive"]},"agents":{}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, handlers.repositoryMonitorStore.UpsertMonitorItem(t.Context(), &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "repo-monitor",
		Kind:             repositoryMonitorTargetKindIssue,
		ItemKey:          "9",
		Number:           9,
		State:            "open",
		LabelsJSON:       `["security-sensitive"]`,
		SnapshotDigest:   "sha256:guarded",
	}))

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/commands?namespace=demo", strings.NewReader(`{"kind":"issue","number":9,"intent":"implement"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, resp.StatusCode)
	var command store.CommandEvent
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&command))
	require.Equal(t, githubCommandStatusBlocked, command.Status)
	require.Contains(t, command.Error, "security-sensitive")
	runs, _, err := handlers.repositoryMonitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "demo", MonitorName: "repo-monitor", TargetKind: repositoryMonitorTargetKindIssue, TargetNumber: 9, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, runs)
	actions, _, err := handlers.repositoryMonitorStore.ListWorkActions(t.Context(), store.WorkActionFilter{Namespace: "demo", MonitorName: "repo-monitor", TargetKind: repositoryMonitorTargetKindIssue, TargetNumber: 9, Limit: 10})
	require.NoError(t, err)
	require.Len(t, actions, 1)
	require.Empty(t, actions[0].RunID)
}

func TestCreateRepositoryMonitorCommandEventRejectsIssueOutsideLabelScope(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"targets":{"pullRequests":{"enabled":false},"issues":{"enabled":true,"excludeLabels":["blocked"]}},"agents":{}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, handlers.repositoryMonitorStore.UpsertMonitorItem(t.Context(), &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "repo-monitor",
		Kind:             repositoryMonitorTargetKindIssue,
		ItemKey:          "11",
		Number:           11,
		State:            "open",
		LabelsJSON:       `["blocked"]`,
		SnapshotDigest:   "sha256:blocked",
	}))

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/commands?namespace=demo", strings.NewReader(`{"kind":"issue","number":11,"intent":"implement"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readRespBody(t, resp), "outside issue label scope")
}

func TestCreateRepositoryMonitorCommandEventRejectsClosedTarget(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"targets":{"pullRequests":{"enabled":false},"issues":{"enabled":true}},"agents":{}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, handlers.repositoryMonitorStore.UpsertMonitorItem(t.Context(), &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "repo-monitor",
		Kind:             repositoryMonitorTargetKindIssue,
		ItemKey:          "10",
		Number:           10,
		State:            "closed",
		SnapshotDigest:   "sha256:closed",
	}))

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/commands?namespace=demo", strings.NewReader(`{"kind":"issue","number":10,"intent":"implement"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readRespBody(t, resp), "must be open")
	runs, _, err := handlers.repositoryMonitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "demo", MonitorName: "repo-monitor", TargetKind: repositoryMonitorTargetKindIssue, TargetNumber: 10, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, runs)
}

func TestCreateRepositoryMonitorCommandEventRequiresPRTargetSHA(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"agents":{"reviewer":{"name":"reviewer"}}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, handlers.repositoryMonitorStore.UpsertMonitorItem(t.Context(), &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "repo-monitor",
		Kind:             repositoryMonitorTargetKindPullRequest,
		ItemKey:          "12",
		Number:           12,
		State:            "open",
		HeadSHA:          "current-head",
	}))

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/commands?namespace=demo", strings.NewReader(`{"kind":"pull_request","number":12,"intent":"review"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readRespBody(t, resp), "targetSHA is required")
	runs, _, err := handlers.repositoryMonitorStore.ListMonitorRuns(t.Context(), store.MonitorRunFilter{Namespace: "demo", MonitorName: "repo-monitor", TargetKind: repositoryMonitorTargetKindPullRequest, TargetNumber: 12, Limit: 10})
	require.NoError(t, err)
	require.Empty(t, runs)
}

func TestCreateRepositoryMonitorCommandEventRejectsStalePRTargetSHA(t *testing.T) {
	app, handlers := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"agents":{"reviewer":{"name":"reviewer"}}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)
	require.NoError(t, handlers.repositoryMonitorStore.UpsertMonitorItem(t.Context(), &store.MonitorItem{
		MonitorNamespace: "demo",
		MonitorName:      "repo-monitor",
		Kind:             repositoryMonitorTargetKindPullRequest,
		ItemKey:          "12",
		Number:           12,
		State:            "open",
		HeadSHA:          "current-head",
	}))

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/commands?namespace=demo", strings.NewReader(`{"kind":"pull_request","number":12,"intent":"review","targetSHA":"stale-head"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readRespBody(t, resp), "targetSHA must match")
}

func TestCreateRepositoryMonitorCommandEventRequiresInventoryForPR(t *testing.T) {
	app, _ := setupRepositoryMonitorHandlers(t, ContextTokenConfig{}, ContextTokenAuthorizationModeOff)
	body := fmt.Sprintf(`{"name":"repo-monitor","namespace":"demo","spec":{"repoURL":%q,"agents":{"reviewer":{"name":"reviewer"}}}}`, monitorTestRepoURL)
	createReq := httptest.NewRequest(http.MethodPost, "/monitors/repositories", strings.NewReader(body))
	createReq.Header.Set("Content-Type", "application/json")
	createResp, err := app.Test(createReq)
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, createResp.StatusCode)

	req := httptest.NewRequest(http.MethodPost, "/monitors/repositories/repo-monitor/commands?namespace=demo", strings.NewReader(`{"kind":"pull_request","number":7,"intent":"review"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
	require.Contains(t, readRespBody(t, resp), "must be present in monitor inventory")
}
