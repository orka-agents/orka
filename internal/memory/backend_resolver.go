package memory

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/apierror"
	"github.com/orka-agents/orka/internal/endpointpolicy"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/pkg/oms/protocol"
)

const (
	memoryBackendAuthLabel              = "memory.orka.ai/client-auth"
	memoryBackendAuthBackendUID         = "memory.orka.ai/backend-uid"
	memoryBackendAuthEndpoint           = "memory.orka.ai/endpoint"
	memoryBackendAuthStoreName          = "memory.orka.ai/store-name"
	memoryBackendAuthNamespaceUID       = "memory.orka.ai/namespace-uid"
	memoryBackendAuthTenantID           = "memory.orka.ai/tenant-id"
	memoryBackendAuthEnabled            = "true"
	defaultBackendResolveRequestTimeout = 15 * time.Second
)

// ResolvedAuthority is one freshly revalidated legacy or remote authority.
type ResolvedAuthority struct {
	Namespace    string
	NamespaceUID string
	Binding      *store.MemoryBackendBinding
	Backend      *corev1alpha1.MemoryBackend
	Adapter      OMSAdapter
}

// Remote reports whether this namespace has durably activated remote authority.
func (a *ResolvedAuthority) Remote() bool {
	return a != nil && a.Binding != nil && a.Binding.Mode == store.MemoryBackendModeRemote
}

// AuthorityResolver supports separate local-only and fresh remote resolution.
type AuthorityResolver interface {
	Resolve(context.Context, string) (*ResolvedAuthority, error)
	ResolveLocal(context.Context, string) (*ResolvedAuthority, error)
}

// BackendResolver resolves namespace incarnation, durable binding, current CR,
// endpoint DNS identity, and bound Secret from uncached Kubernetes reads.
type BackendResolver struct {
	Reader          client.Reader
	Store           store.GovernedMemoryStore
	ClusterIdentity string
	Policy          endpointpolicy.PublicHTTPSPolicy
	HTTPClient      *http.Client
	RequestTimeout  time.Duration
	RemoteDisabled  bool
	Now             func() time.Time
}

// ResolveLocal returns the durable authority and current backend object without
// reading credentials or contacting DNS. It is used for local suppression,
// tombstone admission, operation inspection, and decommission safety actions.
func (r *BackendResolver) ResolveLocal(ctx context.Context, namespace string) (*ResolvedAuthority, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil, apierror.New(http.StatusBadRequest, "", "namespace is required")
	}
	if r == nil || r.Store == nil {
		return &ResolvedAuthority{Namespace: namespace}, nil
	}
	if r.Reader == nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory authority identity cannot be resolved")
	}

	namespaceObject := &corev1.Namespace{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Name: namespace}, namespaceObject); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, apierror.New(http.StatusNotFound, "", "namespace not found")
		}
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory namespace identity is unavailable")
	}
	namespaceUID := string(namespaceObject.UID)
	binding, err := r.Store.GetMemoryBackendBinding(ctx, namespaceUID)
	if errors.Is(err, store.ErrNotFound) {
		incarnationErr := forEachMemoryBackendBinding(ctx, r.Store, store.MemoryBackendBindingFilter{
			Modes: []store.MemoryBackendMode{store.MemoryBackendModeRemote},
		}, func(previous store.MemoryBackendBinding) error {
			if previous.Namespace == namespace && previous.NamespaceUID != namespaceUID {
				return apierror.New(http.StatusConflict, ReasonIdentityMismatch, "namespace name belongs to a different remote memory authority incarnation")
			}
			return nil
		})
		if incarnationErr != nil {
			var structured *apierror.Error
			if errors.As(incarnationErr, &structured) {
				return nil, structured
			}
			return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory authority binding is unavailable")
		}
		return &ResolvedAuthority{Namespace: namespace, NamespaceUID: namespaceUID}, nil
	}
	if err != nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory authority binding is unavailable")
	}
	if binding.Mode != store.MemoryBackendModeRemote {
		return &ResolvedAuthority{Namespace: namespace, NamespaceUID: namespaceUID, Binding: binding}, nil
	}
	if r.RemoteDisabled {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable,
			"remote memory support is disabled on this controller replica")
	}
	if err := validateConfiguredClusterIdentity(r.ClusterIdentity, binding); err != nil {
		return nil, err
	}
	if binding.Namespace != namespace || binding.NamespaceUID != namespaceUID {
		return nil, apierror.New(http.StatusConflict, ReasonIdentityMismatch, "memory authority namespace identity mismatch")
	}
	if binding.State == store.MemoryBackendBindingRemoved || binding.State == store.MemoryBackendBindingDecommissioned {
		return nil, apierror.New(http.StatusGone, ReasonBackendRemoved, "memory backend has been removed")
	}

	backend := &corev1alpha1.MemoryBackend{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: corev1alpha1.MemoryBackendDefaultName}, backend); err != nil {
		if apierrors.IsNotFound(err) {
			return &ResolvedAuthority{Namespace: namespace, NamespaceUID: namespaceUID, Binding: binding}, nil
		}
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "active memory backend resource is unavailable")
	}
	return &ResolvedAuthority{Namespace: namespace, NamespaceUID: namespaceUID, Binding: binding, Backend: backend}, nil
}

// Resolve returns the effective authority after fresh CR, DNS, capability, and
// Secret revalidation. Absence of a durable binding is the only condition that
// selects the legacy SQLite path.
func (r *BackendResolver) Resolve(ctx context.Context, namespace string) (*ResolvedAuthority, error) {
	authority, err := r.ResolveLocal(ctx, namespace)
	if err != nil || !authority.Remote() {
		return authority, err
	}
	binding := authority.Binding
	backend := authority.Backend
	if binding.State == store.MemoryBackendBindingRecovering {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend is recovering")
	}
	if backend == nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "active memory backend resource is unavailable")
	}
	if !backend.DeletionTimestamp.IsZero() {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend is deleting")
	}
	drainCompatible := isDrainCompatibleBackend(backend, binding)
	if err := validateFreshBackendBinding(backend, binding, r.now()); err != nil {
		return nil, err
	}
	endpoint := backend.Spec.Deployment.Endpoint
	if drainCompatible {
		endpoint = backend.Status.EndpointIdentity
	}
	resolveCtx, cancel := context.WithTimeout(ctx, r.requestTimeout())
	resolution, err := r.Policy.Resolve(resolveCtx, endpoint)
	cancel()
	if err != nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonIdentityMismatch, "memory backend endpoint identity changed")
	}
	if err := validateRuntimeResolution(backend, binding, resolution); err != nil {
		return nil, err
	}

	token, err := r.resolveFreshBackendSecret(ctx, authority.Namespace, backend, binding, resolution.Identity, drainCompatible)
	if err != nil {
		return nil, err
	}
	adapter, err := NewOMSClient(
		r.Policy,
		r.HTTPClient,
		resolution,
		binding.ServerCertificateDigest,
		token,
		backend.Status.ObservedCapabilities.Limits.MaxRequestBytes,
		backend.Status.ObservedCapabilities.Limits.MaxResponseBytes,
		r.requestTimeout(),
	)
	if err != nil {
		return nil, apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend client could not be created")
	}
	authority.Adapter = adapter
	return authority, nil
}

func isDrainCompatibleBackend(backend *corev1alpha1.MemoryBackend, binding *store.MemoryBackendBinding) bool {
	return backend != nil && binding != nil &&
		backend.Status.EffectiveLifecycleState == corev1alpha1.MemoryBackendEffectiveLifecycleDraining &&
		backend.Generation == binding.BackendGeneration
}

func (r *BackendResolver) resolveFreshBackendSecret(
	ctx context.Context,
	namespace string,
	backend *corev1alpha1.MemoryBackend,
	binding *store.MemoryBackendBinding,
	endpointIdentity string,
	drainCompatible bool,
) (string, error) {
	if binding == nil || strings.TrimSpace(binding.SecretName) == "" || strings.TrimSpace(binding.SecretKey) == "" {
		return "", apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend credential identity is unavailable")
	}
	if !drainCompatible {
		secretRef := backend.Spec.ClientAuth.BearerTokenSecretRef
		if strings.TrimSpace(secretRef.Name) != binding.SecretName || strings.TrimSpace(secretRef.Key) != binding.SecretKey {
			return "", apierror.New(http.StatusServiceUnavailable, ReasonIdentityMismatch, "memory backend credential reference changed")
		}
	}
	secret := &corev1.Secret{}
	if err := r.Reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: binding.SecretName}, secret); err != nil {
		return "", apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend credential is unavailable")
	}
	if err := validateFreshBackendSecret(secret, backend, binding, endpointIdentity); err != nil {
		return "", err
	}
	if token := validBackendBearerToken(secret.Data[binding.SecretKey]); token != "" {
		return token, nil
	}
	return "", apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend credential is unavailable")
}

func validBackendBearerToken(value []byte) string {
	token := strings.TrimSpace(string(value))
	if token == "" || len(token) > 4096 || strings.ContainsFunc(token, unicode.IsSpace) || strings.ContainsFunc(token, unicode.IsControl) {
		return ""
	}
	return token
}

func validateConfiguredClusterIdentity(configured string, binding *store.MemoryBackendBinding) error {
	configured = strings.TrimSpace(configured)
	if binding == nil || configured == "" || binding.ClusterID != configured {
		return apierror.New(http.StatusServiceUnavailable, ReasonIdentityMismatch, "configured memory cluster identity does not match the durable binding")
	}
	return nil
}

func validateRuntimeResolution(
	backend *corev1alpha1.MemoryBackend,
	binding *store.MemoryBackendBinding,
	resolution endpointpolicy.Resolution,
) error {
	if backend == nil || binding == nil || resolution.Identity != backend.Status.EndpointIdentity ||
		resolution.EndpointDigest != backend.Status.EndpointDigest || resolution.EndpointDigest != binding.EndpointDigest ||
		resolution.ResolvedAddressDigest != backend.Status.ResolvedAddressDigest ||
		resolution.ResolvedAddressDigest != binding.ResolvedAddressDigest {
		return apierror.New(http.StatusServiceUnavailable, ReasonIdentityMismatch, "memory backend endpoint identity changed")
	}
	return nil
}

func validateFreshBackendBinding(
	backend *corev1alpha1.MemoryBackend,
	binding *store.MemoryBackendBinding,
	now time.Time,
) error {
	drainCompatible := isDrainCompatibleBackend(backend, binding)
	observedGenerationMatches := backend != nil && backend.Status.ObservedGeneration == backend.Generation
	if drainCompatible {
		observedGenerationMatches = backend.Status.ObservedGeneration > 0 && backend.Status.ObservedGeneration <= backend.Generation
	}
	if backend == nil || binding == nil || string(backend.UID) != binding.BackendUID ||
		backend.Generation != binding.BackendGeneration || backend.Status.BackendUID != binding.BackendUID ||
		backend.Status.NamespaceUID != binding.NamespaceUID || backend.Status.AuthorityEpoch != binding.AuthorityEpoch ||
		backend.Status.RoutingEpoch != binding.RoutingEpoch || !observedGenerationMatches ||
		backend.Status.ValidatedSpecDigest != binding.SpecDigest || backend.Status.EndpointDigest != binding.EndpointDigest ||
		backend.Status.ResolvedAddressDigest != binding.ResolvedAddressDigest ||
		backend.Status.ServerCertificateDigest != binding.ServerCertificateDigest ||
		backend.Status.SecretUID != binding.SecretUID || backend.Status.SecretResourceVersion != binding.SecretResourceVersion ||
		backend.Status.StoreUUID != binding.StoreUUID || backend.Status.ObservedCapabilities == nil ||
		backend.Status.ObservedCapabilities.Revision != binding.CapabilityRevision {
		return apierror.New(http.StatusServiceUnavailable, ReasonIdentityMismatch, "memory backend validation no longer matches the durable binding")
	}
	if backend.Status.ValidationExpiresAt == nil || !backend.Status.ValidationExpiresAt.After(now) ||
		!binding.ValidationExpiresAt.After(now) {
		return apierror.New(http.StatusServiceUnavailable, ReasonBackendUnavailable, "memory backend validation expired")
	}
	return nil
}

func validateFreshBackendSecret(
	secret *corev1.Secret,
	backend *corev1alpha1.MemoryBackend,
	binding *store.MemoryBackendBinding,
	endpointIdentity string,
) error {
	if secret == nil || backend == nil || binding == nil || string(secret.UID) != binding.SecretUID ||
		secret.ResourceVersion != binding.SecretResourceVersion || secret.Labels[memoryBackendAuthLabel] != memoryBackendAuthEnabled {
		return apierror.New(http.StatusServiceUnavailable, ReasonIdentityMismatch, "memory backend credential identity changed")
	}
	expected := map[string]string{
		memoryBackendAuthBackendUID:   binding.BackendUID,
		memoryBackendAuthEndpoint:     endpointIdentity,
		memoryBackendAuthStoreName:    binding.StoreName,
		memoryBackendAuthNamespaceUID: binding.NamespaceUID,
		memoryBackendAuthTenantID:     binding.TenantID,
	}
	for key, value := range expected {
		if secret.Annotations[key] != value {
			return apierror.New(http.StatusServiceUnavailable, ReasonIdentityMismatch, "memory backend credential binding changed")
		}
	}
	return nil
}

func (r *BackendResolver) now() time.Time {
	if r != nil && r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *BackendResolver) requestTimeout() time.Duration {
	if r != nil && r.RequestTimeout > 0 && r.RequestTimeout <= time.Minute {
		return r.RequestTimeout
	}
	return defaultBackendResolveRequestTimeout
}

func protocolBinding(binding *store.MemoryBackendBinding) (protocol.Binding, error) {
	if binding == nil {
		return protocol.Binding{}, fmt.Errorf("memory backend binding is required")
	}
	result := protocol.Binding{
		ClusterID: binding.ClusterID, NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: uint64(binding.AuthorityEpoch), RoutingEpoch: uint64(binding.RoutingEpoch),
		TenantID: binding.TenantID, StoreUUID: binding.StoreUUID,
	}
	if result.TenantID == "" {
		result.TenantID = protocol.DeriveTenantID(result.ClusterID, result.NamespaceUID)
	}
	return result, protocol.ValidateBinding(result)
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}
