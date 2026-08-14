package memory

import (
	"context"
	"errors"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/apierror"
	"github.com/orka-agents/orka/internal/endpointpolicy"
	"github.com/orka-agents/orka/internal/store"
)

type backendResolverStaticDNS map[string][]netip.Addr

func (r backendResolverStaticDNS) LookupNetIP(_ context.Context, _ string, host string) ([]netip.Addr, error) {
	return append([]netip.Addr(nil), r[host]...), nil
}

func TestValidateRuntimeResolutionRequiresValidatedAddressDigest(t *testing.T) {
	policy := endpointpolicy.PublicHTTPSPolicy{Resolver: backendResolverStaticDNS{
		"memory.example.com": {netip.MustParseAddr("8.8.8.8")},
	}}
	resolution, err := policy.Resolve(context.Background(), "https://memory.example.com")
	if err != nil {
		t.Fatal(err)
	}
	backend := &corev1alpha1.MemoryBackend{}
	backend.Status.EndpointIdentity = resolution.Identity
	backend.Status.EndpointDigest = resolution.EndpointDigest
	backend.Status.ResolvedAddressDigest = resolution.ResolvedAddressDigest
	binding := &store.MemoryBackendBinding{EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest}

	if err := validateRuntimeResolution(backend, binding, resolution); err != nil {
		t.Fatalf("matching resolution rejected: %v", err)
	}
	backend.Status.ResolvedAddressDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := validateRuntimeResolution(backend, binding, resolution); err == nil {
		t.Fatal("runtime resolution accepted an address set different from validated status")
	}
}

type backendResolverBindingStore struct {
	store.GovernedMemoryStore
	binding store.MemoryBackendBinding
}

func (s *backendResolverBindingStore) GetMemoryBackendBinding(context.Context, string) (*store.MemoryBackendBinding, error) {
	copy := s.binding
	return &copy, nil
}

func TestBackendResolverRejectsConfiguredClusterIdentityDriftBeforeDispatch(t *testing.T) {
	const namespace = "team-a"
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace, UID: types.UID("namespace-a")},
	}).Build()
	resolver := &BackendResolver{
		Reader: reader,
		Store: &backendResolverBindingStore{binding: store.MemoryBackendBinding{
			Namespace: namespace, NamespaceUID: "namespace-a", ClusterID: "cluster-a",
			Mode: store.MemoryBackendModeRemote, State: store.MemoryBackendBindingAccepting,
		}},
		ClusterIdentity: "cluster-b",
	}

	_, err := resolver.ResolveLocal(context.Background(), namespace)
	var structured *apierror.Error
	if !errors.As(err, &structured) || structured.Status != http.StatusServiceUnavailable ||
		structured.Reason != ReasonIdentityMismatch {
		t.Fatalf("ResolveLocal() error = %#v, want fail-closed cluster identity mismatch", err)
	}
}

func TestBackendResolverLegacyModeBypassesRemoteClusterIdentity(t *testing.T) {
	const namespace = "team-a"
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace, UID: types.UID("namespace-a")},
	}).Build()
	resolver := &BackendResolver{
		Reader: reader,
		Store: &backendResolverBindingStore{binding: store.MemoryBackendBinding{
			Namespace: namespace, NamespaceUID: "namespace-a", ClusterID: "retired-cluster",
			Mode: store.MemoryBackendModeLegacy, State: store.MemoryBackendBindingLegacy,
		}},
	}
	authority, err := resolver.ResolveLocal(context.Background(), namespace)
	if err != nil {
		t.Fatalf("ResolveLocal() legacy error = %v", err)
	}
	if authority == nil || authority.Remote() || authority.Binding == nil || authority.Binding.Mode != store.MemoryBackendModeLegacy {
		t.Fatalf("legacy authority = %#v", authority)
	}
}

func TestBackendResolverRemoteDisabledFailsClosed(t *testing.T) {
	const namespace = "team-a"
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(&corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: namespace, UID: types.UID("namespace-a")},
	}).Build()
	resolver := &BackendResolver{
		Reader: reader, RemoteDisabled: true,
		Store: &backendResolverBindingStore{binding: store.MemoryBackendBinding{
			Namespace: namespace, NamespaceUID: "namespace-a", ClusterID: "cluster-a",
			Mode: store.MemoryBackendModeRemote, State: store.MemoryBackendBindingAccepting,
		}},
		ClusterIdentity: "cluster-a",
	}
	_, err := resolver.ResolveLocal(context.Background(), namespace)
	var structured *apierror.Error
	if !errors.As(err, &structured) || structured.Reason != ReasonBackendUnavailable {
		t.Fatalf("ResolveLocal() error = %#v, want fail-closed feature-off error", err)
	}
}

func TestBackendResolverConfiguresAdvertisedOMSResponseLimit(t *testing.T) {
	const (
		namespace          = "team-a"
		advertisedRequest  = int64(1024)
		advertisedResponse = int64(2048)
	)
	now := time.Date(2026, 8, 2, 12, 0, 0, 0, time.UTC)
	policy := endpointpolicy.PublicHTTPSPolicy{Resolver: backendResolverStaticDNS{
		"memory.example.com": {netip.MustParseAddr("8.8.8.8")},
	}}
	resolution, err := policy.Resolve(context.Background(), "https://memory.example.com")
	if err != nil {
		t.Fatal(err)
	}
	binding := store.MemoryBackendBinding{
		Namespace: namespace, NamespaceUID: "namespace-a", ClusterID: "cluster-a",
		Mode: store.MemoryBackendModeRemote, State: store.MemoryBackendBindingAccepting,
		BackendUID: "backend-a", BackendGeneration: 1, AuthorityEpoch: 1, RoutingEpoch: 1,
		SpecDigest: "sha256:" + strings.Repeat("1", 64), EndpointDigest: resolution.EndpointDigest,
		ResolvedAddressDigest:   resolution.ResolvedAddressDigest,
		ServerCertificateDigest: "sha256:" + strings.Repeat("2", 64),
		SecretName:              "backend-auth", SecretKey: "token", SecretUID: "secret-a", SecretResourceVersion: "1",
		TenantID: "tenant-a", StoreName: "store-a", StoreUUID: "00000000-0000-4000-8000-000000000001",
		CapabilityRevision: "revision-1", ValidationExpiresAt: now.Add(time.Hour),
	}
	backend := &corev1alpha1.MemoryBackend{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: corev1alpha1.MemoryBackendDefaultName,
			UID: types.UID(binding.BackendUID), Generation: binding.BackendGeneration,
		},
	}
	backend.Spec.Deployment.Endpoint = resolution.Identity
	backend.Spec.ClientAuth.BearerTokenSecretRef.Name = binding.SecretName
	backend.Spec.ClientAuth.BearerTokenSecretRef.Key = binding.SecretKey
	backend.Status = corev1alpha1.MemoryBackendStatus{
		ObservedGeneration:      binding.BackendGeneration,
		ValidatedSpecDigest:     binding.SpecDigest,
		NamespaceUID:            binding.NamespaceUID,
		BackendUID:              binding.BackendUID,
		AuthorityEpoch:          binding.AuthorityEpoch,
		RoutingEpoch:            binding.RoutingEpoch,
		SecretUID:               binding.SecretUID,
		SecretResourceVersion:   binding.SecretResourceVersion,
		EndpointIdentity:        resolution.Identity,
		EndpointDigest:          resolution.EndpointDigest,
		ResolvedAddressDigest:   resolution.ResolvedAddressDigest,
		ServerCertificateDigest: binding.ServerCertificateDigest,
		StoreUUID:               binding.StoreUUID,
		ObservedCapabilities: &corev1alpha1.MemoryBackendObservedCapabilities{
			Revision: binding.CapabilityRevision,
			Limits: corev1alpha1.MemoryBackendCapabilityLimits{
				MaxRequestBytes: advertisedRequest, MaxResponseBytes: advertisedResponse,
			},
		},
		ValidationExpiresAt: &metav1.Time{Time: binding.ValidationExpiresAt},
	}
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: namespace, Name: binding.SecretName,
			UID: types.UID(binding.SecretUID), ResourceVersion: binding.SecretResourceVersion,
			Labels: map[string]string{memoryBackendAuthLabel: memoryBackendAuthEnabled},
			Annotations: map[string]string{
				memoryBackendAuthBackendUID:   binding.BackendUID,
				memoryBackendAuthEndpoint:     resolution.Identity,
				memoryBackendAuthStoreName:    binding.StoreName,
				memoryBackendAuthNamespaceUID: binding.NamespaceUID,
				memoryBackendAuthTenantID:     binding.TenantID,
			},
		},
		Data: map[string][]byte{binding.SecretKey: []byte("secret")},
	}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, UID: types.UID(binding.NamespaceUID)}},
		backend,
		secret,
	).Build()
	resolver := &BackendResolver{
		Reader: reader, Store: &backendResolverBindingStore{binding: binding},
		ClusterIdentity: binding.ClusterID, Policy: policy, Now: func() time.Time { return now },
	}

	authority, err := resolver.Resolve(context.Background(), namespace)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	client, ok := authority.Adapter.(*OMSClient)
	if !ok {
		t.Fatalf("Resolve() adapter = %T, want *OMSClient", authority.Adapter)
	}
	if client.maxRequestBytes != advertisedRequest {
		t.Fatalf("OMS maxRequestBytes = %d, want advertised %d", client.maxRequestBytes, advertisedRequest)
	}
	if client.maxResponseBytes != advertisedResponse {
		t.Fatalf("OMS maxResponseBytes = %d, want advertised %d", client.maxResponseBytes, advertisedResponse)
	}
}

func TestDrainCompatibleResolutionUsesValidatedOldCredentialIdentity(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	binding := store.MemoryBackendBinding{
		Namespace: "team-a", NamespaceUID: "namespace-a", ClusterID: "cluster-a", Mode: store.MemoryBackendModeRemote,
		BackendUID: "backend-a", BackendGeneration: 2, AuthorityEpoch: 1, RoutingEpoch: 3,
		SpecDigest: "sha256:old-spec", EndpointDigest: "sha256:old-endpoint", SecretName: "secret-old", SecretKey: "old-key",
		SecretUID: "secret-old", SecretResourceVersion: "1", TenantID: "tenant-a", StoreName: "store-a",
		StoreUUID:          "11111111-1111-4111-8111-111111111111",
		CapabilityRevision: "cap-a", State: store.MemoryBackendBindingAccepting, ValidationExpiresAt: now.Add(time.Hour),
	}
	backend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", UID: types.UID("backend-a"), Generation: 2}}
	backend.Spec.ClientAuth.BearerTokenSecretRef.Name = "secret-new"
	backend.Spec.ClientAuth.BearerTokenSecretRef.Key = "new-key"
	backend.Status = corev1alpha1.MemoryBackendStatus{
		ObservedGeneration: 1, BackendUID: binding.BackendUID, NamespaceUID: binding.NamespaceUID,
		EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleDraining,
		AuthorityEpoch:          binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		ValidatedSpecDigest: binding.SpecDigest, EndpointIdentity: "https://old.example.com",
		EndpointDigest: binding.EndpointDigest, SecretUID: binding.SecretUID, SecretResourceVersion: binding.SecretResourceVersion,
		StoreUUID: binding.StoreUUID, ValidationExpiresAt: &metav1.Time{Time: binding.ValidationExpiresAt},
		ObservedCapabilities: &corev1alpha1.MemoryBackendObservedCapabilities{Revision: binding.CapabilityRevision},
	}
	if err := validateFreshBackendBinding(backend, &binding, now); err != nil {
		t.Fatalf("drain-compatible binding rejected: %v", err)
	}
	oldSecret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: "team-a", Name: "secret-old", UID: types.UID(binding.SecretUID), ResourceVersion: binding.SecretResourceVersion,
		Labels: map[string]string{memoryBackendAuthLabel: memoryBackendAuthEnabled},
		Annotations: map[string]string{
			memoryBackendAuthBackendUID: binding.BackendUID, memoryBackendAuthEndpoint: backend.Status.EndpointIdentity,
			memoryBackendAuthStoreName: binding.StoreName, memoryBackendAuthNamespaceUID: binding.NamespaceUID,
			memoryBackendAuthTenantID: binding.TenantID,
		},
	}, Data: map[string][]byte{"old-key": []byte("old-token")}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(oldSecret).Build()
	resolver := &BackendResolver{Reader: reader}
	token, err := resolver.resolveFreshBackendSecret(context.Background(), "team-a", backend, &binding, backend.Status.EndpointIdentity, true)
	if err != nil || token != "old-token" {
		t.Fatalf("resolveFreshBackendSecret() token=%q err=%v", token, err)
	}
}

func TestDrainCompatibleResolutionNeverScansAlternateSecretValues(t *testing.T) {
	binding := store.MemoryBackendBinding{
		Namespace: "team-a", NamespaceUID: "namespace-a", BackendUID: "backend-a", TenantID: "tenant-a",
		StoreName: "store-a", SecretName: "secret-old", SecretKey: "missing-key",
		SecretUID: "secret-old-uid", SecretResourceVersion: "1",
	}
	backend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{Namespace: binding.Namespace, UID: types.UID(binding.BackendUID)}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: binding.Namespace, Name: binding.SecretName, UID: types.UID(binding.SecretUID), ResourceVersion: binding.SecretResourceVersion,
		Labels: map[string]string{memoryBackendAuthLabel: memoryBackendAuthEnabled},
		Annotations: map[string]string{
			memoryBackendAuthBackendUID: binding.BackendUID, memoryBackendAuthEndpoint: "https://old.example.com",
			memoryBackendAuthStoreName: binding.StoreName, memoryBackendAuthNamespaceUID: binding.NamespaceUID,
			memoryBackendAuthTenantID: binding.TenantID,
		},
	}, Data: map[string][]byte{"other-key": []byte("must-not-be-discovered")}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	resolver := &BackendResolver{Reader: fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()}
	if token, err := resolver.resolveFreshBackendSecret(
		context.Background(), binding.Namespace, backend, &binding, "https://old.example.com", true,
	); err == nil || token != "" {
		t.Fatalf("resolveFreshBackendSecret() token=%q err=%v, want exact-key failure", token, err)
	}
}

func TestBackendResolverFreshResolutionFailsClosedWhenBackendIsDeleting(t *testing.T) {
	const namespace = "team-a"
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	deletingAt := metav1.NewTime(now)
	binding := store.MemoryBackendBinding{
		Namespace: namespace, NamespaceUID: "namespace-a", ClusterID: "cluster-a",
		Mode: store.MemoryBackendModeRemote, State: store.MemoryBackendBindingAccepting,
		BackendUID: "backend-a", AuthorityEpoch: 1, RoutingEpoch: 1,
	}
	backend := &corev1alpha1.MemoryBackend{ObjectMeta: metav1.ObjectMeta{
		Namespace: namespace, Name: corev1alpha1.MemoryBackendDefaultName,
		UID: types.UID(binding.BackendUID), DeletionTimestamp: &deletingAt, Finalizers: []string{"test.finalizer"},
	}}
	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace, UID: types.UID(binding.NamespaceUID)}},
		backend,
	).Build()
	resolver := &BackendResolver{
		Reader: reader, Store: &backendResolverBindingStore{binding: binding}, ClusterIdentity: binding.ClusterID,
	}
	_, err := resolver.Resolve(context.Background(), namespace)
	var structured *apierror.Error
	if !errors.As(err, &structured) || structured.Status != http.StatusServiceUnavailable ||
		structured.Reason != ReasonBackendUnavailable {
		t.Fatalf("Resolve() error = %#v, want deleting backend failure", err)
	}
}
