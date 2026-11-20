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
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/controller"
)

const agentHostTestNamespace = "team"

type agentHostAPI struct {
	t        *testing.T
	client   client.Client
	reader   client.Reader
	app      *fiber.App
	identity *UserInfo
}

func person(subject string) *UserInfo {
	return &UserInfo{AuthType: AuthTypeOIDC, Username: subject, Subject: subject, Issuer: connectorTestIssuer, Namespace: agentHostTestNamespace}
}

// linkedHost is the host the controller would create for subject's link.
func linkedHost(name, subject string) *corev1alpha1.AgentHost {
	return &corev1alpha1.AgentHost{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: agentHostTestNamespace},
		Spec: corev1alpha1.AgentHostSpec{
			TemplateRef: corev1alpha1.AgentHostTemplateReference{Name: "copilotd"},
			GitHubTokenFrom: &corev1alpha1.AgentHostTokenSource{ConnectionRef: corev1alpha1.LocalObjectReference{
				Name: connectors.ConnectionName("github", connectorTestIssuer, subject),
			}},
		},
		Status: corev1alpha1.AgentHostStatus{
			Phase:  corev1alpha1.AgentHostPhaseReady,
			GitHub: &corev1alpha1.AgentHostGitHubAccount{ID: 1, Login: name[len("gh-"):]},
			Conditions: []metav1.Condition{{
				Type: "Ready", Status: metav1.ConditionTrue, Reason: "HostReady", LastTransitionTime: metav1.Now(),
			}},
		},
	}
}

func newAgentHostAPI(t *testing.T, objects ...client.Object) *agentHostAPI {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	provider := &corev1alpha1.ConnectorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: agentHostTestNamespace},
		Spec: corev1alpha1.ConnectorProviderSpec{
			AgentHost: &corev1alpha1.ConnectorAgentHost{TemplateRef: corev1alpha1.LocalObjectReference{Name: "copilotd"}},
		},
	}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(append(objects, provider)...).Build()
	h := &agentHostAPI{t: t, client: c, identity: person("alice")}
	h.rebuild()
	return h
}

// rebuild serves the routes from h.client.
func (h *agentHostAPI) rebuild() {
	reader := h.reader
	if reader == nil {
		reader = h.client
	}
	handlers := NewHandlers(HandlersConfig{
		Client: h.client, APIReader: reader, WatchNamespace: agentHostTestNamespace,
		Connectors: ConnectorConfig{Enabled: true},
	})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		if h.identity != nil {
			c.Locals(UserInfoContextKey, h.identity)
		}
		return c.Next()
	})
	app.Get("/api/v1/agent-hosts/me", handlers.GetOwnAgentHost)
	app.Post("/api/v1/agent-hosts/me/wake", handlers.WakeOwnAgentHost)
	app.Delete("/api/v1/agent-hosts/me", handlers.DeleteOwnAgentHost)
	h.app = app
}

func (h *agentHostAPI) do(method, path string) (int, AgentHostResponse) {
	h.t.Helper()
	resp, err := h.app.Test(httptest.NewRequest(method, path, nil))
	if err != nil {
		h.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(resp.Body)
	var body AgentHostResponse
	if resp.StatusCode < 300 {
		if err := json.Unmarshal(raw, &body); err != nil {
			h.t.Fatalf("decode %s: %v", raw, err)
		}
	}
	return resp.StatusCode, body
}

func (h *agentHostAPI) host(name string) (*corev1alpha1.AgentHost, bool) {
	h.t.Helper()
	host := &corev1alpha1.AgentHost{}
	err := h.client.Get(context.Background(), types.NamespacedName{Namespace: agentHostTestNamespace, Name: name}, host)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	if err != nil {
		h.t.Fatal(err)
	}
	return host, true
}

// Each person reaches only the host fed by their own link.
func TestOwnAgentHostIsTheCallersOnly(t *testing.T) {
	// A host someone created by hand names alice's link; it is not the
	// controller's, and the API never serves it.
	stray := linkedHost("gh-stray", "alice")
	stray.Spec.GitHubTokenFrom.ConnectionRef.Name = "github-someone-else"
	aliceHost, bobHost := linkedHost("gh-alice", "alice"), linkedHost("gh-bob", "bob")
	aliceHost.Status.ObservedConnectionUID, aliceHost.Status.ObservedGrantSequence = "alice-link", 1
	bobHost.Status.ObservedConnectionUID, bobHost.Status.ObservedGrantSequence = "bob-link", 1
	h := newAgentHostAPI(t, aliceHost, bobHost, stray,
		readyHostConnection(aliceHost, "alice-link", 1), readyHostConnection(bobHost, "bob-link", 1))

	status, alice := h.do(http.MethodGet, "/api/v1/agent-hosts/me")
	if status != http.StatusOK || alice.Name != "gh-alice" || alice.Environment != "orka-gh-alice" || !alice.Ready || alice.GitHubLogin != "alice" {
		t.Fatalf("alice: %d %+v", status, alice)
	}
	h.identity = person("bob")
	if status, bob := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusOK || bob.Name != "gh-bob" {
		t.Fatalf("bob: %d %+v", status, bob)
	}
	h.identity = person("carol")
	if status, _ := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusNotFound {
		t.Fatalf("carol: %d, want 404", status)
	}
	// A host being deleted keeps its last Ready condition, but is not ready.
	if deleting := agentHostResponse(func() *corev1alpha1.AgentHost {
		host := linkedHost("gh-alice", "alice")
		now := metav1.Now()
		host.DeletionTimestamp = &now
		return host
	}()); deleting.Ready || !deleting.Deleting {
		t.Fatalf("deleting host = %+v", deleting)
	}
	if status, _ := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusNotFound {
		t.Fatalf("carol: %d, want 404", status)
	}
	// Deleting reaches only the caller's own host.
	h.identity = person("bob")
	if status, deleted := h.do(http.MethodDelete, "/api/v1/agent-hosts/me"); status != http.StatusAccepted || deleted.Name != "gh-bob" ||
		!deleted.Deleting || deleted.Ready {
		t.Fatalf("delete: %d %+v", status, deleted)
	}
	if _, ok := h.host("gh-bob"); ok {
		t.Fatal("bob's host must be deleted")
	}
	if _, ok := h.host("gh-alice"); !ok {
		t.Fatal("alice's host must stay")
	}
}

func TestOwnAgentHostNeedsAVerifiedPerson(t *testing.T) {
	h := newAgentHostAPI(t, linkedHost("gh-alice", "alice"))
	h.identity = &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:team:robot", Namespace: agentHostTestNamespace}
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if status, _ := h.do(method, "/api/v1/agent-hosts/me"); status != http.StatusForbidden {
			t.Fatalf("%s as a ServiceAccount: %d, want 403", method, status)
		}
	}
	if _, ok := h.host("gh-alice"); !ok {
		t.Fatal("nothing may be deleted")
	}
}

func TestWakeOwnAgentHost(t *testing.T) {
	idle := linkedHost("gh-alice", "alice")
	// Suspended this very second: the wake must still date after it.
	asleep := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
	idle.Status.IdleSuspendedAt = &asleep
	idle.Status.Phase = corev1alpha1.AgentHostPhaseSuspended
	h := newAgentHostAPI(t, idle)
	status, woken := h.do(http.MethodPost, "/api/v1/agent-hosts/me/wake")
	if status != http.StatusOK || !woken.Suspended {
		t.Fatalf("wake: %d %+v", status, woken)
	}
	if host, _ := h.host("gh-alice"); host.Spec.WakeRequestedAt == nil || !host.Spec.WakeRequestedAt.After(asleep.Time) {
		t.Fatalf("wakeRequestedAt = %v, want after the idle suspension", host.Spec.WakeRequestedAt)
	}

	// An operator's suspension is theirs to lift.
	held := linkedHost("gh-alice", "alice")
	held.Spec.Suspended = true
	h = newAgentHostAPI(t, held)
	if status, _ := h.do(http.MethodPost, "/api/v1/agent-hosts/me/wake"); status != http.StatusConflict {
		t.Fatalf("wake an operator-suspended host: %d, want 409", status)
	}
}

// A host replaced under the same name after the ownership check, which may
// be another person's, is neither woken nor deleted.
func TestOwnAgentHostWritesAreFencedToTheHostThatWasChecked(t *testing.T) {
	for _, method := range []string{http.MethodPost, http.MethodDelete} {
		t.Run(method, func(t *testing.T) {
			idle := linkedHost("gh-alice", "alice")
			asleep := metav1.NewTime(time.Now().UTC().Truncate(time.Second))
			idle.Status.IdleSuspendedAt = &asleep
			h := newAgentHostAPI(t, idle)
			replace := func(ctx context.Context, c client.WithWatch) {
				current := &corev1alpha1.AgentHost{}
				if err := c.Get(ctx, types.NamespacedName{Namespace: agentHostTestNamespace, Name: "gh-alice"}, current); err != nil {
					t.Fatal(err)
				}
				if err := c.Delete(ctx, current); err != nil {
					t.Fatal(err)
				}
				other := linkedHost("gh-alice", "mallory")
				other.Status.IdleSuspendedAt = &asleep
				if err := c.Create(ctx, other); err != nil {
					t.Fatal(err)
				}
			}
			h.client = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
				Patch: func(ctx context.Context, c client.WithWatch, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
					replace(ctx, c)
					return c.Patch(ctx, obj, patch, opts...)
				},
				Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
					replace(ctx, c)
					return c.Delete(ctx, obj, opts...)
				},
			})
			h.rebuild()
			path := "/api/v1/agent-hosts/me"
			if method == http.MethodPost {
				path += "/wake"
			}
			if status, _ := h.do(method, path); status != http.StatusConflict {
				t.Fatalf("%s on a replaced host: %d, want 409", method, status)
			}
			replacement, ok := h.host("gh-alice")
			if !ok || replacement.Spec.WakeRequestedAt != nil {
				t.Fatal("the replacement host must be untouched")
			}
		})
	}
}

func readyHostConnection(host *corev1alpha1.AgentHost, uid string, grant int64) *corev1alpha1.Connection {
	connection := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name: host.Spec.GitHubTokenFrom.ConnectionRef.Name, Namespace: host.Namespace, UID: types.UID(uid), Generation: 1,
		},
		Status: corev1alpha1.ConnectionStatus{GrantSequence: grant},
	}
	for _, kind := range []string{corev1alpha1.ConnectionConditionReady, corev1alpha1.ConnectionConditionProviderResolved,
		corev1alpha1.ConnectionConditionScopesGranted} {
		connection.Status.Conditions = append(connection.Status.Conditions, metav1.Condition{
			Type: kind, Status: metav1.ConditionTrue, Reason: "Linked", ObservedGeneration: 1,
		})
	}
	return connection
}

// A Ready condition from an earlier consent, or an older Connection with the
// same name, cannot make a freshly linked account look ready.
func TestOwnAgentHostRequiresCurrentConnectionGrant(t *testing.T) {
	for _, tc := range []struct {
		name     string
		uid      string
		grant    int64
		missing  bool
		unlinked bool
		deleting bool
		want     bool
	}{
		{name: "not observed"},
		{name: "previous consent", uid: "current-link", grant: 1},
		{name: "recreated link", uid: "old-link", grant: 2},
		{name: "different grant", uid: "current-link", grant: 3},
		{name: "missing link", uid: "current-link", grant: 2, missing: true},
		{name: "unlinked", uid: "current-link", grant: 2, unlinked: true},
		{name: "deleting", uid: "current-link", grant: 2, deleting: true},
		{name: "current grant", uid: "current-link", grant: 2, want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := linkedHost("gh-alice", "alice")
			host.Status.ObservedConnectionUID = tc.uid
			host.Status.ObservedGrantSequence = tc.grant
			objects := []client.Object{host}
			if !tc.missing {
				connection := readyHostConnection(host, "current-link", 2)
				if tc.unlinked {
					connection.Status.Conditions[0].Status = metav1.ConditionFalse
				}
				if tc.deleting {
					now := metav1.Now()
					connection.DeletionTimestamp = &now
					connection.Finalizers = []string{"test.orka.ai/held"}
				}
				objects = append(objects, connection)
			}
			h := newAgentHostAPI(t, objects...)
			status, response := h.do(http.MethodGet, "/api/v1/agent-hosts/me")
			if status != http.StatusOK || response.Ready != tc.want {
				t.Fatalf("status = %d, ready = %v, want %v", status, response.Ready, tc.want)
			}
		})
	}
}

func TestOwnAgentHostReadsTheCurrentGrantWithoutTheInformerCache(t *testing.T) {
	host := linkedHost("gh-alice", "alice")
	host.Status.ObservedConnectionUID, host.Status.ObservedGrantSequence = "current-link", 1
	h := newAgentHostAPI(t, host, readyHostConnection(host, "current-link", 1))
	h.reader = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, inner client.WithWatch, key client.ObjectKey, object client.Object, opts ...client.GetOption) error {
			if err := inner.Get(ctx, key, object, opts...); err != nil {
				return err
			}
			if connection, ok := object.(*corev1alpha1.Connection); ok {
				connection.Status.GrantSequence = 2
			}
			return nil
		},
	})
	h.rebuild()
	if status, response := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusOK || response.Ready {
		t.Fatalf("cached grant advertised as ready: status = %d, ready = %v", status, response.Ready)
	}
}

func TestOwnAgentHostReadFailureCannotAdvertiseReadiness(t *testing.T) {
	host := linkedHost("gh-alice", "alice")
	host.Status.ObservedConnectionUID, host.Status.ObservedGrantSequence = "current-link", 1
	h := newAgentHostAPI(t, host, readyHostConnection(host, "current-link", 1))
	h.reader = interceptor.NewClient(h.client.(client.WithWatch), interceptor.Funcs{
		Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
			return context.DeadlineExceeded
		},
	})
	h.rebuild()
	if status, response := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusInternalServerError || response.Ready {
		t.Fatalf("unverified grant advertised as ready: status = %d, ready = %v", status, response.Ready)
	}
}

// A host kept after its provider stopped serving hosts stays its person's to
// see and delete.
func TestOwnAgentHostOutlivesItsProvidersOptIn(t *testing.T) {
	h := newAgentHostAPI(t, linkedHost("gh-alice", "alice"))
	provider := &corev1alpha1.ConnectorProvider{}
	if err := h.client.Get(context.Background(), types.NamespacedName{Namespace: agentHostTestNamespace, Name: "github"}, provider); err != nil {
		t.Fatal(err)
	}
	provider.Spec.AgentHost = nil
	if err := h.client.Update(context.Background(), provider); err != nil {
		t.Fatal(err)
	}
	if status, host := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusOK || host.Name != "gh-alice" {
		t.Fatalf("get: %d %+v", status, host)
	}
	if status, _ := h.do(http.MethodDelete, "/api/v1/agent-hosts/me"); status != http.StatusAccepted {
		t.Fatalf("delete: %d", status)
	}
	if _, ok := h.host("gh-alice"); ok {
		t.Fatal("the host must be deleted")
	}
}

// A host kept after its person disconnected stays theirs to see and delete
// after an operator deletes its provider, and stays no one else's.
func TestOwnAgentHostOutlivesItsProvider(t *testing.T) {
	retired := linkedHost("gh-alice-old", "alice")
	retired.Spec.GitHubTokenFrom.ConnectionRef.Name = connectors.ConnectionName("github-retired", connectorTestIssuer, "alice")
	h := newAgentHostAPI(t, retired)
	h.identity = person("bob")
	if status, _ := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusNotFound {
		t.Fatalf("bob: %d, want 404", status)
	}
	h.identity = person("alice")
	if status, host := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusOK || host.Name != "gh-alice-old" {
		t.Fatalf("get: %d %+v, want the kept host", status, host)
	}
	if status, _ := h.do(http.MethodDelete, "/api/v1/agent-hosts/me"); status != http.StatusAccepted {
		t.Fatalf("delete: %d", status)
	}
	if _, ok := h.host("gh-alice-old"); ok {
		t.Fatal("the kept host must be deleted")
	}
}

// A link's name keeps only a prefix of a long provider name, so after such a
// provider is deleted the kept host's own record says which provider it was.
func TestOwnAgentHostOutlivesALongNamedProvider(t *testing.T) {
	provider := "github-enterprise-cloud-agent-hosts-for-the-platform-team"
	link := connectors.ConnectionName(provider, connectorTestIssuer, "alice")
	if strings.HasPrefix(link, provider+"-") {
		t.Fatalf("link %q keeps the whole provider name; the test needs a truncated one", link)
	}
	kept := linkedHost("gh-alice-old", "alice")
	kept.Spec.GitHubTokenFrom.ConnectionRef.Name = link
	kept.Annotations = map[string]string{connectors.AgentHostProviderAnnotation: provider}
	h := newAgentHostAPI(t, kept)
	h.identity = person("bob")
	if status, _ := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusNotFound {
		t.Fatalf("bob: %d, want 404", status)
	}
	h.identity = person("alice")
	if status, host := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusOK || host.Name != "gh-alice-old" {
		t.Fatalf("get: %d %+v, want the kept host", status, host)
	}
}

// An upgraded provisioner repairs a legacy host while its Connection still
// exists, so losing a long-named provider cannot strand self-service cleanup.
func TestOwnAgentHostOutlivesBackfilledProvider(t *testing.T) {
	provider := &corev1alpha1.ConnectorProvider{ObjectMeta: metav1.ObjectMeta{
		Name: "github-enterprise-cloud-agent-hosts-for-the-platform-team", Namespace: agentHostTestNamespace,
	}}
	connection := &corev1alpha1.Connection{
		ObjectMeta: metav1.ObjectMeta{
			Name: connectors.ConnectionName(provider.Name, connectorTestIssuer, "alice"), Namespace: agentHostTestNamespace,
		},
		Spec: corev1alpha1.ConnectionSpec{
			ProviderRef: corev1alpha1.LocalObjectReference{Name: provider.Name},
			Subject:     corev1alpha1.ConnectionSubject{Issuer: connectorTestIssuer, Subject: "alice"},
		},
	}
	kept := linkedHost("gh-alice-old", "alice")
	kept.Spec.GitHubTokenFrom.ConnectionRef.Name = connection.Name
	h := newAgentHostAPI(t, kept, connection, provider)
	provisioner := &controller.AgentHostProvisioner{Client: h.client}
	if _, err := provisioner.Reconcile(context.Background(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(connection)}); err != nil {
		t.Fatal(err)
	}
	for _, object := range []client.Object{connection, provider} {
		if err := h.client.Delete(context.Background(), object); err != nil {
			t.Fatal(err)
		}
	}
	h.identity = person("bob")
	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		if status, _ := h.do(method, "/api/v1/agent-hosts/me"); status != http.StatusNotFound {
			t.Fatalf("bob %s: %d, want 404", method, status)
		}
	}
	h.identity = person("alice")
	if status, host := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusOK || host.Name != kept.Name {
		t.Fatalf("get: %d %+v, want the kept host", status, host)
	}
	if status, _ := h.do(http.MethodDelete, "/api/v1/agent-hosts/me"); status != http.StatusAccepted {
		t.Fatalf("delete: %d, want 202", status)
	}
	if _, ok := h.host(kept.Name); ok {
		t.Fatal("the kept host must be deleted")
	}
}

// With a retired provider's host kept, the host of the provider serving hosts
// now is the person's host.
func TestOwnAgentHostPrefersTheCurrentProvidersHost(t *testing.T) {
	retired := linkedHost("gh-alice-old", "alice")
	retired.Spec.GitHubTokenFrom.ConnectionRef.Name = connectors.ConnectionName("github-old", connectorTestIssuer, "alice")
	h := newAgentHostAPI(t, retired, linkedHost("gh-alice", "alice"), &corev1alpha1.ConnectorProvider{
		ObjectMeta: metav1.ObjectMeta{Name: "github-old", Namespace: agentHostTestNamespace},
	})
	if status, host := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusOK || host.Name != "gh-alice" {
		t.Fatalf("get: %d %+v, want the current provider's host", status, host)
	}
	if status, _ := h.do(http.MethodDelete, "/api/v1/agent-hosts/me"); status != http.StatusAccepted {
		t.Fatalf("delete: %d", status)
	}
	// Once that one is gone, the retained host is still the person's.
	if status, host := h.do(http.MethodGet, "/api/v1/agent-hosts/me"); status != http.StatusOK || host.Name != "gh-alice-old" {
		t.Fatalf("get after delete: %d %+v, want the retained host", status, host)
	}
}
