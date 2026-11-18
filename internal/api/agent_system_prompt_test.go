package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func systemPromptTestObjects(prompt *corev1alpha1.PromptSource) []runtime.Object {
	return []runtime.Object{
		&corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Name: "responder", Namespace: "demo"},
			Spec:       corev1alpha1.AgentSpec{SystemPrompt: prompt},
		},
		&corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: "responder-prompt", Namespace: "demo"},
			Data:       map[string]string{"system.md": "You are the incident responder.", "other.md": "unrelated"},
		},
	}
}

func configMapPromptRef(key string) *corev1alpha1.PromptSource {
	return &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "responder-prompt", Key: key}}
}

func systemPromptTestApp(t *testing.T, objs []runtime.Object, config func(*HandlersConfig), userInfo *UserInfo) *fiber.App {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	cfg := HandlersConfig{Client: fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()}
	if config != nil {
		config(&cfg)
	}
	handlers := NewHandlers(cfg)
	app := fiber.New()
	if userInfo != nil {
		app.Use(tokenReviewUserMiddleware(userInfo))
	}
	app.Get("/agents/:name/system-prompt", handlers.GetAgentSystemPrompt)
	return app
}

func getSystemPrompt(t *testing.T, app *fiber.App) (int, AgentSystemPromptResponse) {
	t.Helper()
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/agents/responder/system-prompt?namespace=demo", nil))
	require.NoError(t, err)
	var body AgentSystemPromptResponse
	if resp.StatusCode == http.StatusOK {
		require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	}
	return resp.StatusCode, body
}

func TestGetAgentSystemPromptReturnsReferencedKey(t *testing.T) {
	status, body := getSystemPrompt(t, systemPromptTestApp(t, systemPromptTestObjects(configMapPromptRef("system.md")), nil, nil))
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, AgentSystemPromptResponse{
		ConfigMapRef: corev1alpha1.ConfigMapKeySelector{Name: "responder-prompt", Key: "system.md"},
		Content:      "You are the incident responder.",
	}, body)
}

func TestGetAgentSystemPromptNotFound(t *testing.T) {
	tests := map[string][]runtime.Object{
		"inline prompt":     systemPromptTestObjects(&corev1alpha1.PromptSource{Inline: "inline"}),
		"no prompt":         systemPromptTestObjects(nil),
		"missing key":       systemPromptTestObjects(configMapPromptRef("absent.md")),
		"missing ConfigMap": systemPromptTestObjects(configMapPromptRef("system.md"))[:1],
		"missing agent":     nil,
	}
	for name, objs := range tests {
		t.Run(name, func(t *testing.T) {
			status, _ := getSystemPrompt(t, systemPromptTestApp(t, objs, nil, nil))
			require.Equal(t, http.StatusNotFound, status)
		})
	}
}

func TestGetAgentSystemPromptRequiresConfigMapGetForTokenReviewUser(t *testing.T) {
	objs := systemPromptTestObjects(configMapPromptRef("system.md"))
	kubeClient := denyingSubjectAccessReviewClient(t, nil, func(review *authorizationv1.SubjectAccessReview) {
		require.Equal(t, authorizationv1.ResourceAttributes{
			Namespace: "demo", Verb: "get", Group: "", Resource: "configmaps", Name: "responder-prompt",
		}, *review.Spec.ResourceAttributes)
	})
	app := systemPromptTestApp(t, objs, func(cfg *HandlersConfig) { cfg.KubeClient = kubeClient }, limitedTokenReviewUser("demo"))
	status, _ := getSystemPrompt(t, app)
	require.Equal(t, http.StatusForbidden, status)
	require.Len(t, kubeClient.Actions(), 1)

	allowing := kubefake.NewSimpleClientset()
	allowing.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		review.Status.Allowed = true
		return true, review, nil
	})
	app = systemPromptTestApp(t, objs, func(cfg *HandlersConfig) { cfg.KubeClient = allowing }, limitedTokenReviewUser("demo"))
	status, body := getSystemPrompt(t, app)
	require.Equal(t, http.StatusOK, status)
	require.Equal(t, "You are the incident responder.", body.Content)
}

func TestGetAgentSystemPromptRequiresConfigMapReadScopeForContextToken(t *testing.T) {
	provider := newTestOIDCProvider(t)
	ctxTokenConfig := testContextTokenConfig(t, provider, "")
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	require.NoError(t, err)
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	handlers := NewHandlers(HandlersConfig{
		Client:                    fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(systemPromptTestObjects(configMapPromptRef("system.md"))...).Build(),
		ContextTokenAuthorization: authz,
	})
	app := fiber.New()
	app.Use(NewAuthMiddleware(handlers.client, AuthConfig{ContextTokens: ctxTokenConfig}))
	app.Get("/agents/:name/system-prompt", handlers.GetAgentSystemPrompt)

	for scope, want := range map[string]int{
		ContextTokenScopeAgentsRead:                                         http.StatusForbidden,
		ContextTokenScopeAgentsRead + " " + ContextTokenScopeConfigMapsRead: http.StatusOK,
	} {
		t.Run(scope, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, "/agents/responder/system-prompt?namespace=demo", nil)
			req.Header.Set(TransactionTokenHeaderName, issueTestContextToken(t, provider, nil, map[string]any{"scope": scope}))
			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, want, resp.StatusCode)
		})
	}
}
