package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type runtimeListRecordingClient struct {
	client.Client
	limit        int64
	continuation string
}

func (c *runtimeListRecordingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	listOptions := &client.ListOptions{}
	for _, opt := range opts {
		opt.ApplyToList(listOptions)
	}
	c.limit = listOptions.Limit
	c.continuation = listOptions.Continue
	return c.Client.List(ctx, list, client.InNamespace(listOptions.Namespace))
}

func TestRuntimeFabricResourceHandlers(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&corev1alpha1.RuntimePool{}, &corev1alpha1.AgentRuntime{}).Build()
	handlers := NewHandlers(HandlersConfig{Client: kubeClient})
	app := fiber.New()
	app.Get("/runtime-pools", handlers.ListRuntimePools)
	app.Get("/runtime-pools/:name", handlers.GetRuntimePool)
	app.Post("/agent-runtimes", handlers.CreateAgentRuntime)
	app.Get("/agent-runtimes", handlers.ListAgentRuntimes)

	pool := corev1alpha1.RuntimePool{
		ObjectMeta: objectMetaFromRequest("pool", "default", MetadataRequest{}),
		Spec: corev1alpha1.RuntimePoolSpec{
			TrustDomain: corev1alpha1.RuntimePoolTrustDomain{Namespace: "default", Identity: "namespace:default"},
			Runtime: corev1alpha1.RuntimePoolRuntimeSpec{
				Image:   "docker.io/example/acp@sha256:" + strings.Repeat("a", 64),
				Profile: corev1alpha1.RuntimePoolProfileSpec{Digest: "sha256:" + strings.Repeat("b", 64), DigestSchemaVersion: "1", ACPProfile: "acp.v1", AdapterDigests: map[string]string{"adapter": "sha256:" + strings.Repeat("c", 64)}, ProviderKind: "codex", Model: "gpt", AgentConfigurationDigest: "sha256:" + strings.Repeat("d", 64), ToolPolicyDigest: "sha256:" + strings.Repeat("e", 64), ApprovalPolicyDigest: "sha256:" + strings.Repeat("f", 64), MCPConfigurationDigest: "sha256:" + strings.Repeat("1", 64), WorkspaceIntent: corev1alpha1.WorkspaceIntentRead, ProxyCredentialRole: "provider", ProxyCredentialScope: "model:gpt", ResourceClass: "standard"},
			},
		},
	}
	if err := kubeClient.Create(t.Context(), &pool); err != nil {
		t.Fatal(err)
	}
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/runtime-pools?namespace=default", nil))
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("list pools status=%v err=%v", resp.StatusCode, err)
	}
	var listed corev1alpha1.RuntimePoolList
	if err := json.NewDecoder(resp.Body).Decode(&listed); err != nil || len(listed.Items) != 1 {
		t.Fatalf("listed pools=%#v err=%v", listed.Items, err)
	}
	digest := func(char string) string { return "sha256:" + strings.Repeat(char, 64) }
	profile := corev1alpha1.AgentRuntimeProfileSpec{Digest: digest("a"), DigestSchemaVersion: 1, ACPProfile: "acp.v1", AdapterName: "codex", AdapterDigest: digest("b"), ProviderKind: "codex", Model: "gpt", AgentConfigurationDigest: digest("c"), ToolPolicyDigest: digest("d"), ApprovalPolicyDigest: digest("e"), MCPConfigurationDigest: digest("f"), WorkspaceIntent: corev1alpha1.WorkspaceIntentRead, ProxyCredentialRole: "provider", ProxyCredentialScope: "model:gpt", ResourceClass: "standard"}
	claims := corev1alpha1.AgentRuntimeWorkspaceGovernanceCapabilities{Mode: corev1alpha1.AgentRuntimeWorkspaceGovernanceStrict, OrkaOwnedWorkspaceDeltas: true, PromptScopedBrokerAuthorization: true, NoDirectSCMPublication: true, OrkaOwnedCleanRoomPublication: true, ExactInstanceFencing: true, DuplicateSafeMutations: true, CancellationSettlement: true}
	limits := corev1alpha1.AgentRuntimeProtocolLimits{MaxResidentSessions: 10, MaxConcurrentPrompts: 4, MaxRequestBytes: 1 << 20, MaxEventLineBytes: 1 << 20, MaxTerminalResultBytes: 1 << 20, MaxBufferedEvents: 64, MaxUpdateEventsPerSecond: 100, MinPromptLeaseMillis: 5000, MaxPromptLeaseMillis: 120000, MaxPendingPermissions: 16, MaxWorkspaceDeltaBytes: 100 << 20}
	runtimeObject := corev1alpha1.AgentRuntime{
		ObjectMeta: objectMetaFromRequest("external", "default", MetadataRequest{}),
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{
			ContractVersion: new(corev1alpha1.AgentRuntimeContractHarnessV2),
			Deployment:      corev1alpha1.AgentRuntimeDeploymentSpec{Mode: corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint, Endpoint: "https://runtime.example.com"},
			ClientAuth:      corev1alpha1.AgentRuntimeClientAuth{ControllerBearerTokenSecretRef: &corev1alpha1.AgentRuntimeSecretKeyReference{Name: "auth", Key: "controller"}, OperationCapabilitySecretRef: &corev1alpha1.AgentRuntimeSecretKeyReference{Name: "auth", Key: "capability"}},
			Capabilities:    &corev1alpha1.AgentRuntimeCapabilitiesSpec{RuntimeInstanceID: "runtime-1", Profile: &profile, Limits: &limits, SupportsDrain: true, WorkspaceGovernance: &claims},
		},
	}
	body, _ := json.Marshal(runtimeObject)
	resp, err = app.Test(httptest.NewRequest(http.MethodPost, "/agent-runtimes", bytes.NewReader(body)))
	if err != nil || resp.StatusCode != http.StatusCreated {
		t.Fatalf("create agent runtime status=%v err=%v", resp.StatusCode, err)
	}
	resp, _ = app.Test(httptest.NewRequest(http.MethodGet, "/agent-runtimes?namespace=default", nil))
	var runtimes corev1alpha1.AgentRuntimeList
	if err := json.NewDecoder(resp.Body).Decode(&runtimes); err != nil || len(runtimes.Items) != 1 {
		t.Fatalf("listed runtimes=%#v err=%v", runtimes.Items, err)
	}
}

func TestRuntimeFabricReadsRequireAgentReadScopeForContextTokens(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	pool := &corev1alpha1.RuntimePool{ObjectMeta: objectMetaFromRequest("pool", "default", MetadataRequest{})}
	agentRuntime := &corev1alpha1.AgentRuntime{ObjectMeta: objectMetaFromRequest("external", "default", MetadataRequest{})}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool, agentRuntime).Build()
	authz, err := NewContextTokenAuthorizationConfig(ContextTokenAuthorizationConfigOptions{Mode: ContextTokenAuthorizationModeEnforce})
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		route   string
		path    string
		handler func(*Handlers) fiber.Handler
	}{
		{name: "list runtime pools", route: "/runtime-pools", path: "/runtime-pools?namespace=default", handler: func(h *Handlers) fiber.Handler { return h.ListRuntimePools }},
		{name: "get runtime pool", route: "/runtime-pools/:name", path: "/runtime-pools/pool?namespace=default", handler: func(h *Handlers) fiber.Handler { return h.GetRuntimePool }},
		{name: "list agent runtimes", route: "/agent-runtimes", path: "/agent-runtimes?namespace=default", handler: func(h *Handlers) fiber.Handler { return h.ListAgentRuntimes }},
		{name: "get agent runtime", route: "/agent-runtimes/:name", path: "/agent-runtimes/external?namespace=default", handler: func(h *Handlers) fiber.Handler { return h.GetAgentRuntime }},
	}
	for _, test := range tests {
		for _, authorization := range []struct {
			name       string
			scopes     []string
			wantStatus int
		}{
			{name: "denies unrelated scope", scopes: []string{ContextTokenScopeTaskCreate}, wantStatus: http.StatusForbidden},
			{name: "allows agent read scope", scopes: []string{ContextTokenScopeAgentsRead}, wantStatus: http.StatusOK},
		} {
			t.Run(test.name+"/"+authorization.name, func(t *testing.T) {
				handlers := NewHandlers(HandlersConfig{Client: kubeClient, ContextTokenAuthorization: authz})
				app := fiber.New()
				app.Use(func(c fiber.Ctx) error {
					ctxToken := &ContextToken{Scopes: authorization.scopes}
					c.Locals(UserInfoContextKey, &UserInfo{
						AuthType:     AuthTypeContextToken,
						ContextToken: ctxToken,
					})
					return c.Next()
				})
				app.Get(test.route, test.handler(handlers))

				response, err := app.Test(httptest.NewRequest(http.MethodGet, test.path, nil))
				if err != nil {
					t.Fatal(err)
				}
				if response.StatusCode != authorization.wantStatus {
					t.Fatalf("status=%d, want %d", response.StatusCode, authorization.wantStatus)
				}
			})
		}
	}
}

func TestRuntimeFabricListsHonorContinuationTokens(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name    string
		path    string
		handler func(*Handlers) fiber.Handler
	}{
		{name: "runtime pools", path: "/runtime-pools?namespace=default&limit=17&continue=next-page", handler: func(h *Handlers) fiber.Handler { return h.ListRuntimePools }},
		{name: "agent runtimes", path: "/agent-runtimes?namespace=default&limit=17&continue=next-page", handler: func(h *Handlers) fiber.Handler { return h.ListAgentRuntimes }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			// Limited lists must be served by the uncached API reader: the
			// cache cannot resume from a continue token.
			baseClient := fake.NewClientBuilder().WithScheme(scheme).Build()
			recordingClient := &runtimeListRecordingClient{Client: baseClient}
			handlers := NewHandlers(HandlersConfig{Client: listFailingClient{Client: baseClient}, APIReader: recordingClient})
			app := fiber.New()
			app.Get(strings.Split(test.path, "?")[0], test.handler(handlers))

			response, err := app.Test(httptest.NewRequest(http.MethodGet, test.path, nil))
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != http.StatusOK {
				t.Fatalf("status=%d, want %d", response.StatusCode, http.StatusOK)
			}
			if recordingClient.limit != 17 || recordingClient.continuation != "next-page" {
				t.Fatalf("list options = limit %d continue %q, want 17 and next-page", recordingClient.limit, recordingClient.continuation)
			}
		})
	}
}
