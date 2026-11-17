package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func enableProviderUseAuthorization(t *testing.T) {
	t.Helper()
	previous := providerUseAuthorization.Load()
	providerUseAuthorization.Store(true)
	t.Cleanup(func() { providerUseAuthorization.Store(previous) })
}

// providerUseClientset allows `use` on the listed namespace/name Providers,
// allows every other review, and records the Provider use reviews it saw.
func providerUseClientset(allowed ...string) (*kubefake.Clientset, *[]authorizationv1.ResourceAttributes) {
	grants := map[string]bool{}
	for _, key := range allowed {
		grants[key] = true
	}
	seen := &[]authorizationv1.ResourceAttributes{}
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		attributes := review.Spec.ResourceAttributes
		if attributes != nil && attributes.Verb == providerUseVerb && attributes.Resource == providersResource {
			*seen = append(*seen, *attributes)
			review.Status.Allowed = grants[attributes.Namespace+"/"+attributes.Name]
			return true, review, nil
		}
		review.Status.Allowed = true
		return true, review, nil
	})
	return clientset, seen
}

func providerUseTask(providerRef *corev1alpha1.ProviderReference, agentRef *corev1alpha1.AgentReference) *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "work"},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAI, AgentRef: agentRef,
			AI: &corev1alpha1.AISpec{ProviderRef: providerRef, Prompt: "hi"},
		},
	}
}

func providerUseReader(t *testing.T, objects ...client.Object) client.Reader {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
}

func TestProviderUseAuthorizationIsOffByDefault(t *testing.T) {
	clientset, seen := providerUseClientset()
	user := &UserInfo{Username: "alice", AuthType: AuthTypeTokenReview}
	task := providerUseTask(&corev1alpha1.ProviderReference{Name: "anthropic"}, nil)
	require.NoError(t, authorizeKubernetesTaskCreate(context.Background(), nil, clientset, user, task))
	require.Empty(t, *seen, "no Provider use review runs while the feature is off")
}

func TestTaskCreateRequiresUseOnItsProvider(t *testing.T) {
	enableProviderUseAuthorization(t)
	user := &UserInfo{Username: "alice", Groups: []string{"team-a"}, AuthType: AuthTypeTokenReview}

	denied, seen := providerUseClientset()
	task := providerUseTask(&corev1alpha1.ProviderReference{Name: "anthropic"}, nil)
	requireForbidden(t, authorizeKubernetesTaskCreate(context.Background(), nil, denied, user, task))
	require.Equal(t, []authorizationv1.ResourceAttributes{{
		Namespace: "team-a", Verb: "use", Group: "core.orka.ai", Version: "v1alpha1", Resource: "providers", Name: "anthropic",
	}}, *seen)

	allowed, _ := providerUseClientset("team-a/anthropic")
	require.NoError(t, authorizeKubernetesTaskCreate(context.Background(), nil, allowed, user, task))

	remote, seen := providerUseClientset("models/anthropic")
	require.NoError(t, authorizeKubernetesTaskCreate(context.Background(), nil, remote, user,
		providerUseTask(&corev1alpha1.ProviderReference{Name: "anthropic", Namespace: "models"}, nil)))
	require.Equal(t, "models", (*seen)[0].Namespace, "a cross-namespace Provider is checked in its own namespace")

	// A Task that runs its same-namespace Agent's Provider needs only Agent use.
	none, seen := providerUseClientset()
	require.NoError(t, authorizeKubernetesTaskCreate(context.Background(), nil, none, user,
		providerUseTask(nil, &corev1alpha1.AgentReference{Name: "reviewer"})))
	require.Empty(t, *seen)
}

func TestTaskCreateRefusesACrossNamespaceAgentsTaskRelativeProviders(t *testing.T) {
	enableProviderUseAuthorization(t)
	user := &UserInfo{Username: "alice", AuthType: AuthTypeTokenReview}
	agentRef := &corev1alpha1.AgentReference{Name: "reviewer", Namespace: "agents"}
	agent := func(primary *corev1alpha1.ProviderReference, fallbacks ...string) *corev1alpha1.Agent {
		a := &corev1alpha1.Agent{
			ObjectMeta: metav1.ObjectMeta{Namespace: "agents", Name: "reviewer"},
			Spec:       corev1alpha1.AgentSpec{ProviderRef: primary},
		}
		for _, name := range fallbacks {
			if a.Spec.Model == nil {
				a.Spec.Model = &corev1alpha1.ModelConfig{}
			}
			a.Spec.Model.Fallbacks = append(a.Spec.Model.Fallbacks, corev1alpha1.ModelFallback{ProviderRef: name})
		}
		return a
	}
	all, _ := providerUseClientset("team-a/primary", "team-a/backup", "team-a/mine")

	// The Agent's namespace-less Provider and its fallbacks would resolve in
	// the Task's namespace and may change after this check, so the Task is
	// refused even when the caller may use those Providers.
	for name, a := range map[string]*corev1alpha1.Agent{
		"relative primary": agent(&corev1alpha1.ProviderReference{Name: "primary"}),
		"fallback":         agent(&corev1alpha1.ProviderReference{Name: "primary", Namespace: "models"}, "backup"),
	} {
		t.Run(name, func(t *testing.T) {
			requireForbidden(t, authorizeKubernetesTaskCreate(context.Background(), providerUseReader(t, a), all, user, providerUseTask(nil, agentRef)))
		})
	}

	// The Task's own Provider replaces the Agent's primary, so with no
	// fallbacks nothing is taken from the remote Agent.
	require.NoError(t, authorizeKubernetesTaskCreate(context.Background(), providerUseReader(t, agent(&corev1alpha1.ProviderReference{Name: "primary"})), all, user,
		providerUseTask(&corev1alpha1.ProviderReference{Name: "mine"}, agentRef)))
	// An explicit namespace does not help: the Agent's namespace may be
	// outside the policies that checked it.
	requireForbidden(t, authorizeKubernetesTaskCreate(context.Background(),
		providerUseReader(t, agent(&corev1alpha1.ProviderReference{Name: "primary", Namespace: "team-a"})), all, user, providerUseTask(nil, agentRef)))

	// Without a reader, or without the Agent, nothing can be checked.
	requireForbidden(t, authorizeKubernetesTaskCreate(context.Background(), nil, all, user, providerUseTask(nil, agentRef)))
	requireForbidden(t, authorizeKubernetesTaskCreate(context.Background(), providerUseReader(t), all, user, providerUseTask(nil, agentRef)))
}

func TestAgentProviderRefsUseChecksOnlyNewlyNamedProviders(t *testing.T) {
	enableProviderUseAuthorization(t)
	user := &UserInfo{Username: "alice", AuthType: AuthTypeTokenReview}
	agent := func(primary string, fallbacks ...string) *corev1alpha1.Agent {
		a := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "reviewer"}}
		if primary != "" {
			a.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: primary}
		}
		if len(fallbacks) > 0 {
			a.Spec.Model = &corev1alpha1.ModelConfig{}
			for _, name := range fallbacks {
				a.Spec.Model.Fallbacks = append(a.Spec.Model.Fallbacks, corev1alpha1.ModelFallback{ProviderRef: name})
			}
		}
		return a
	}

	clientset, seen := providerUseClientset("team-a/primary", "team-a/backup")
	require.NoError(t, authorizeAgentProviderRefsUse(context.Background(), clientset, user, agent("primary", "backup", "backup"), nil))
	require.Equal(t, []string{"team-a/primary", "team-a/backup"}, reviewedProviders(*seen), "a repeated Provider is checked once")

	denied, _ := providerUseClientset("team-a/primary")
	requireForbidden(t, authorizeAgentProviderRefsUse(context.Background(), denied, user, agent("primary", "other"), nil))

	kept, seen := providerUseClientset()
	require.NoError(t, authorizeAgentProviderRefsUse(context.Background(), kept, user,
		agent("primary", "backup"), agent("backup", "primary")),
		"an edit that keeps the Agent's Providers, in any role, needs no grant")
	require.Empty(t, *seen)

	remote, seen := providerUseClientset("models/primary")
	pinned := agent("")
	pinned.Spec.ProviderRef = &corev1alpha1.ProviderReference{Name: "primary", Namespace: "models"}
	require.NoError(t, authorizeAgentProviderRefsUse(context.Background(), remote, user, pinned, agent("primary")),
		"the same name in another namespace is a different Provider")
	require.Equal(t, []string{"models/primary"}, reviewedProviders(*seen))
}

func TestProviderUseFailsClosedWithoutAnIdentityOrClient(t *testing.T) {
	enableProviderUseAuthorization(t)
	task := providerUseTask(&corev1alpha1.ProviderReference{Name: "anthropic"}, nil)
	requireForbidden(t, authorizeTaskProviderUse(context.Background(), nil, nil, &UserInfo{Username: "alice", AuthType: AuthTypeTokenReview}, task))
	clientset, _ := providerUseClientset("team-a/anthropic")
	requireForbidden(t, authorizeTaskProviderUse(context.Background(), nil, clientset, nil, task))
	// An OIDC caller without an issuer and subject has no reviewable identity.
	requireForbidden(t, authorizeTaskProviderUse(context.Background(), nil, clientset, &UserInfo{AuthType: AuthTypeOIDC, Username: "alice"}, task))
}

func TestProviderUseReviewsExternalCallersByIssuerAndSubject(t *testing.T) {
	enableProviderUseAuthorization(t)
	var specs []authorizationv1.SubjectAccessReviewSpec
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		specs = append(specs, review.Spec)
		review.Status.Allowed = true
		return true, review, nil
	})
	user := &UserInfo{AuthType: AuthTypeOIDC, Username: "Alice", Issuer: "https://issuer.example", Subject: "1234", Groups: []string{"admins"}}
	require.NoError(t, authorizeTaskProviderUse(context.Background(), nil, clientset, user,
		providerUseTask(&corev1alpha1.ProviderReference{Name: "anthropic"}, nil)))
	require.Len(t, specs, 1)
	require.Equal(t, "https://issuer.example#1234", specs[0].User)
	require.Equal(t, []string{"https://issuer.example#admins"}, specs[0].Groups)
}

func reviewedProviders(seen []authorizationv1.ResourceAttributes) []string {
	keys := make([]string, 0, len(seen))
	for _, attributes := range seen {
		keys = append(keys, attributes.Namespace+"/"+attributes.Name)
	}
	return keys
}

// providerUseGrant is the grant the external authorization fixture's Provider needs.
var providerUseGrant = authorizationv1.ResourceAttributes{
	Namespace: "default", Group: corev1alpha1.GroupVersion.Group, Version: corev1alpha1.GroupVersion.Version,
	Resource: providersResource, Verb: providerUseVerb, Name: "protected",
}

// newProviderUseFixture enables Provider use after NewServer, which sets it
// from the fixture's configuration.
func newProviderUseFixture(t *testing.T) *externalAuthorizationFixture {
	t.Helper()
	f := newExternalAuthorizationFixture(t)
	enableProviderUseAuthorization(t)
	return f
}

func TestExternalAPIChatRequiresUseOnTheResolvedProvider(t *testing.T) {
	for _, path := range []string{"/api/v1/chat", "/openai/v1/chat/completions", "/openai/v1/responses", "/anthropic/v1/messages"} {
		t.Run(path, func(t *testing.T) {
			requestBody := `{"message":"hello","model":"protected/fixture","max_tokens":128,"messages":[{"role":"user","content":"hello"}]}`
			if path == "/openai/v1/responses" {
				requestBody = `{"model":"protected/fixture","store":false,"input":"hello"}`
			}

			f := newProviderUseFixture(t)
			f.allowRoute(t, "POST "+path)
			status, body := f.request(t, http.MethodPost, path, requestBody)
			require.Equal(t, http.StatusForbidden, status, body)
			require.Zero(t, f.externalCalls.Load(), "the Provider is never called without use on it")

			f = newProviderUseFixture(t)
			f.allowRoute(t, "POST "+path, providerUseGrant)
			status, body = f.request(t, http.MethodPost, path, requestBody)
			require.Equal(t, http.StatusOK, status, body)
			require.Equal(t, int64(1), f.externalCalls.Load())
		})
	}
}

func TestExternalAPIAgentAndTaskWritesRequireProviderUse(t *testing.T) {
	for _, tc := range []struct{ path, spec string }{
		{"agents", `{"providerRef":{"name":"protected"},"model":{"name":"fixture"}}`},
		{"agents", `{"model":{"name":"fixture","fallbacks":[{"providerRef":"protected"}]}}`},
		{"tasks", `{"type":"ai","ai":{"providerRef":{"name":"protected"},"prompt":"hi"}}`},
	} {
		t.Run(tc.path, func(t *testing.T) {
			body := `{"name":"created","spec":` + tc.spec + `}`
			f := newProviderUseFixture(t)
			f.allowRoute(t, "POST /api/v1/"+tc.path)
			status, response := f.request(t, http.MethodPost, "/api/v1/"+tc.path, body)
			require.Equal(t, http.StatusForbidden, status, response)

			f = newProviderUseFixture(t)
			f.allowRoute(t, "POST /api/v1/"+tc.path, providerUseGrant)
			status, response = f.request(t, http.MethodPost, "/api/v1/"+tc.path, body)
			require.Equal(t, http.StatusCreated, status, response)
		})
	}
}
