package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	crclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/memorybackend"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestMemoryBackendConfigurationRejectsOIDCIdentity(t *testing.T) {
	h, app := memoryBackendTestHandler(t, &UserInfo{AuthType: AuthTypeOIDC, Username: "oidc-user"}, false)
	app.Get("/api/v1/memory-backends", h.ListMemoryBackends)
	response, err := app.Test(newRequest(http.MethodGet, "/api/v1/memory-backends?namespace=default", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusForbidden, response.StatusCode)
}

func TestMemoryBackendListUsesExactKubernetesRBAC(t *testing.T) {
	user := &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:operator", Namespace: "default"}
	h, app := memoryBackendTestHandler(t, user, true)
	app.Get("/api/v1/memory-backends", h.ListMemoryBackends)
	response, err := app.Test(newRequest(http.MethodGet, "/api/v1/memory-backends?namespace=default", nil))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
}

func TestCreateMemoryBackendReturnsCommittedResourceWhenCompletionAuditFails(t *testing.T) {
	governed := newMemoryBackendAPITestStore(t)
	failing := &completionAuditFailStore{GovernedMemoryStore: governed, action: "backend.create.completed"}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: types.UID("namespace-a")}}
	user := &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:operator", Namespace: namespace.Name}
	h, app := memoryBackendTestHandlerWithStore(t, user, true, failing, namespace)
	app.Post("/api/v1/memory-backends", h.CreateMemoryBackend)
	body, err := json.Marshal(corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name},
		Spec:       memoryBackendAPITestSpec("https://memory.example.com"),
	})
	require.NoError(t, err)
	response, err := app.Test(newRequest(http.MethodPost, "/api/v1/memory-backends?reason=create", body))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, response.StatusCode)
	created := &corev1alpha1.MemoryBackend{}
	require.NoError(t, h.client.Get(t.Context(), crclient.ObjectKey{Namespace: namespace.Name, Name: corev1alpha1.MemoryBackendDefaultName}, created))
	require.True(t, failing.failed)
}

func TestUpdateMemoryBackendReturnsCommittedResourceWhenCompletionAuditFails(t *testing.T) {
	governed := newMemoryBackendAPITestStore(t)
	failing := &completionAuditFailStore{GovernedMemoryStore: governed, action: "backend.update.completed"}
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: types.UID("namespace-a")}}
	backend := &corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name,
			UID: types.UID("backend-a"), ResourceVersion: "1"},
		Spec: memoryBackendAPITestSpec("https://old.example.com"),
	}
	user := &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:operator", Namespace: namespace.Name}
	h, app := memoryBackendTestHandlerWithStore(t, user, true, failing, namespace, backend)
	app.Put("/api/v1/memory-backends/default", h.UpdateMemoryBackend)
	body, err := json.Marshal(corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{ResourceVersion: backend.ResourceVersion},
		Spec:       memoryBackendAPITestSpec("https://new.example.com"),
	})
	require.NoError(t, err)
	response, err := app.Test(newRequest(http.MethodPut, "/api/v1/memory-backends/default?reason=update", body))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	updated := &corev1alpha1.MemoryBackend{}
	require.NoError(t, h.client.Get(t.Context(), crclient.ObjectKeyFromObject(backend), updated))
	require.Equal(t, "https://new.example.com", updated.Spec.Deployment.Endpoint)
	require.True(t, failing.failed)
}

func memoryBackendTestHandler(t *testing.T, user *UserInfo, allow bool) (*Handlers, *fiber.App) {
	t.Helper()
	governed := newMemoryBackendAPITestStore(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: types.UID("namespace-a")}}
	backend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{
		Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name, UID: types.UID("backend-a"),
	}}
	return memoryBackendTestHandlerWithStore(t, user, allow, governed, namespace, backend)
}

func memoryBackendTestHandlerWithStore(
	t *testing.T,
	user *UserInfo,
	allow bool,
	governed store.GovernedMemoryStore,
	objects ...crclient.Object,
) (*Handlers, *fiber.App) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))
	client := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	kubeClient := kubefake.NewSimpleClientset()
	kubeClient.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		require.NotNil(t, review.Spec.ResourceAttributes)
		require.Equal(t, corev1alpha1.GroupVersion.Group, review.Spec.ResourceAttributes.Group)
		require.Equal(t, "memorybackends", review.Spec.ResourceAttributes.Resource)
		review.Status.Allowed = allow
		return true, review, nil
	})
	h := NewHandlers(HandlersConfig{
		Client: client, APIReader: client, KubeClient: kubeClient,
		MemoryBackendManager: &memorybackend.Manager{Client: client, Reader: client, Store: governed},
	})
	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, user)
		return c.Next()
	})
	return h, app
}

func newMemoryBackendAPITestStore(t *testing.T) store.GovernedMemoryStore {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return sqlite.NewStore(db, ":memory:")
}

func memoryBackendAPITestSpec(endpoint string) corev1alpha1.MemoryBackendSpec {
	return corev1alpha1.MemoryBackendSpec{
		Protocol: corev1alpha1.MemoryBackendProtocol{
			OMSVersion: corev1alpha1.MemoryBackendOMSVersionV01,
			Profile:    corev1alpha1.MemoryBackendProfileV0Alpha1,
		},
		Deployment: corev1alpha1.MemoryBackendDeployment{
			Mode: corev1alpha1.MemoryBackendDeploymentModeExternalEndpoint, Endpoint: endpoint,
		},
		ClientAuth: corev1alpha1.MemoryBackendClientAuth{BearerTokenSecretRef: corev1alpha1.MemoryBackendBearerTokenSecretReference{
			Name: "memory-auth", Key: "token",
		}},
		Store:          corev1alpha1.MemoryBackendStore{Name: "memory-store"},
		LifecycleState: corev1alpha1.MemoryBackendLifecycleStaged,
	}
}

type completionAuditFailStore struct {
	store.GovernedMemoryStore
	action string
	failed bool
}

func (s *completionAuditFailStore) AppendMemoryAudit(ctx context.Context, record store.MemoryAuditRecord) error {
	if record.Action == s.action {
		s.failed = true
		return errors.New("injected completion audit failure")
	}
	return s.GovernedMemoryStore.AppendMemoryAudit(ctx, record)
}

func newRequest(method, target string, body any) *http.Request {
	var payload []byte
	switch value := body.(type) {
	case nil:
	case []byte:
		payload = value
	default:
		payload, _ = json.Marshal(value)
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(payload))
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	return request
}

func TestUpdateMemoryBackendRejectsStaleResourceVersion(t *testing.T) {
	governed := newMemoryBackendAPITestStore(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: types.UID("namespace-a")}}
	backend := &corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name,
			UID: types.UID("backend-a"), ResourceVersion: "2"},
		Spec: memoryBackendAPITestSpec("https://old.example.com"),
	}
	user := &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:operator", Namespace: namespace.Name}
	h, app := memoryBackendTestHandlerWithStore(t, user, true, governed, namespace, backend)
	app.Put("/api/v1/memory-backends/default", h.UpdateMemoryBackend)
	request := corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{ResourceVersion: "1"},
		Spec:       memoryBackendAPITestSpec("https://new.example.com"),
	}
	response, err := app.Test(newRequest(http.MethodPut, "/api/v1/memory-backends/default?reason=update", request))
	require.NoError(t, err)
	require.Equal(t, http.StatusConflict, response.StatusCode)
}

func TestUpdateMemoryBackendRejectsCombinedProtectedLifecycleAndSpecChange(t *testing.T) {
	governed := newMemoryBackendAPITestStore(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: types.UID("namespace-a")}}
	backend := &corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name,
			UID: types.UID("backend-a"), ResourceVersion: "1"},
		Spec: memoryBackendAPITestSpec("https://old.example.com"),
	}
	user := &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:operator", Namespace: namespace.Name}
	h, app := memoryBackendTestHandlerWithStore(t, user, true, governed, namespace, backend)
	app.Put("/api/v1/memory-backends/default", h.UpdateMemoryBackend)
	updatedSpec := memoryBackendAPITestSpec("https://new.example.com")
	updatedSpec.LifecycleState = corev1alpha1.MemoryBackendLifecycleReadOnly
	request := corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{ResourceVersion: backend.ResourceVersion},
		Spec:       updatedSpec,
	}
	response, err := app.Test(newRequest(http.MethodPut, "/api/v1/memory-backends/default?reason=update", request))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
}

func TestUpdateMemoryBackendRejectsQueryBodyNamespaceMismatch(t *testing.T) {
	governed := newMemoryBackendAPITestStore(t)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "team-a", UID: types.UID("namespace-a")}}
	backend := &corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name,
			UID: types.UID("backend-a"), ResourceVersion: "1"},
		Spec: memoryBackendAPITestSpec("https://old.example.com"),
	}
	user := &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:team-a:operator", Namespace: namespace.Name}
	h, app := memoryBackendTestHandlerWithStore(t, user, true, governed, namespace, backend)
	app.Put("/api/v1/memory-backends/default", h.UpdateMemoryBackend)
	request := corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{Namespace: namespace.Name, ResourceVersion: backend.ResourceVersion},
		Spec:       backend.Spec,
	}
	response, err := app.Test(newRequest(http.MethodPut,
		"/api/v1/memory-backends/default?namespace=team-b&reason=update", request))
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, response.StatusCode)
}

type memoryGovernancePurgeCaptureStore struct {
	store.GovernedMemoryStore
	binding store.MemoryBackendBinding
	purge   store.MemoryGovernancePurge
}

func (s *memoryGovernancePurgeCaptureStore) GetMemoryBackendBinding(context.Context, string) (*store.MemoryBackendBinding, error) {
	binding := s.binding
	return &binding, nil
}

func (s *memoryGovernancePurgeCaptureStore) PurgeMemoryGovernance(
	_ context.Context,
	purge store.MemoryGovernancePurge,
) (*store.MemoryGovernancePurgeResult, error) {
	s.purge = purge
	return &store.MemoryGovernancePurgeResult{PayloadsPurged: 3, PurgeDigest: "sha256:purge"}, nil
}

func TestPurgeMemoryBackendGovernanceUsesActiveBindingIdentity(t *testing.T) {
	base := newMemoryBackendAPITestStore(t)
	now := time.Date(2026, 8, 1, 9, 30, 0, 0, time.UTC)
	namespace := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "default", UID: types.UID("namespace-a")}}
	backend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{
		Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace.Name, UID: types.UID("backend-a"),
	}}
	capture := &memoryGovernancePurgeCaptureStore{GovernedMemoryStore: base, binding: store.MemoryBackendBinding{
		Namespace: namespace.Name, NamespaceUID: string(namespace.UID), Mode: store.MemoryBackendModeRemote,
		BackendUID: string(backend.UID), AuthorityEpoch: 3, RoutingEpoch: 4, StoreUUID: "store-uuid-a",
	}}
	user := &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:serviceaccount:default:operator", Namespace: namespace.Name}
	h, app := memoryBackendTestHandlerWithStore(t, user, true, capture, namespace, backend)
	h.memoryBackendManager.Now = func() time.Time { return now }
	app.Post("/api/v1/memory-backends/default/purge", h.PurgeMemoryBackendGovernance)
	before := now.Add(-time.Hour)
	response, err := app.Test(newRequest(http.MethodPost, "/api/v1/memory-backends/default/purge", map[string]any{
		"checkpointId": "mcheckpoint-a", "maximumOperationSequence": 42,
		"before": before, "purgePayloads": true, "reason": "reclaim retained payload capacity",
	}))
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)
	require.Equal(t, capture.binding.NamespaceUID, capture.purge.NamespaceUID)
	require.Equal(t, capture.binding.BackendUID, capture.purge.BackendUID)
	require.Equal(t, capture.binding.AuthorityEpoch, capture.purge.AuthorityEpoch)
	require.Equal(t, capture.binding.RoutingEpoch, capture.purge.RoutingEpoch)
	require.Equal(t, capture.binding.StoreUUID, capture.purge.StoreUUID)
	require.Equal(t, "mcheckpoint-a", capture.purge.CheckpointID)
	require.Equal(t, int64(42), capture.purge.MaximumOperationSequence)
	require.True(t, capture.purge.PurgePayloads)
	require.Equal(t, before, capture.purge.Before)
	require.Equal(t, now, capture.purge.Now)
}
