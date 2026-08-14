/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/netip"
	"net/url"
	"path"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/endpointpolicy"
	"github.com/orka-agents/orka/internal/events"
	omsprotocol "github.com/orka-agents/orka/pkg/oms/protocol"
)

const (
	MemoryBackendProtectionFinalizer = "memory.orka.ai/backend-protection"

	MemoryBackendClientAuthLabel                     = "memory.orka.ai/client-auth"
	MemoryBackendClientAuthEnabledValue              = "true"
	MemoryBackendAuthBackendUIDAnnotation            = "memory.orka.ai/backend-uid"
	MemoryBackendAuthEndpointAnnotation              = "memory.orka.ai/endpoint"
	MemoryBackendAuthStoreNameAnnotation             = "memory.orka.ai/store-name"
	MemoryBackendAuthNamespaceUIDAnnotation          = "memory.orka.ai/namespace-uid"
	MemoryBackendAuthTenantIDAnnotation              = "memory.orka.ai/tenant-id"
	memoryBackendSecretReferenceField                = "spec.clientAuth.bearerTokenSecretRef.name"
	defaultMemoryBackendValidationTTL                = 5 * time.Minute
	defaultMemoryBackendProbeTimeout                 = 15 * time.Second
	defaultMemoryBackendRequeueInterval              = 30 * time.Second
	memoryBackendReasonBindingCoordinatorUnavailable = "BindingCoordinatorUnavailable"
	memoryBackendReasonBindingBarrierFailed          = "BindingBarrierFailed"
	memoryBackendReasonBindingResultRejected         = "BindingResultRejected"
	memoryBackendReasonUncachedReaderUnavailable     = "UncachedReaderUnavailable"
	memoryBackendReasonNamespaceReadFailed           = "NamespaceReadFailed"
)

var (
	memoryBackendSafeIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,255}$`)
	memoryBackendStoreNamePattern    = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)
	memoryBackendProtocolIDPattern   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,255}$`)
	memoryBackendStoreUUIDPattern    = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[1-5][0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)
	memoryBackendDigestPattern       = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

	requiredMemoryBackendCapabilities = []corev1alpha1.MemoryBackendCapability{
		corev1alpha1.MemoryBackendCapabilityDurableIdempotency,
		corev1alpha1.MemoryBackendCapabilityIdempotencyDigestConflicts,
		corev1alpha1.MemoryBackendCapabilityCreateIfAbsent,
		corev1alpha1.MemoryBackendCapabilityConditionalMutation,
		corev1alpha1.MemoryBackendCapabilityMonotonicGenerations,
		corev1alpha1.MemoryBackendCapabilityDeleteHighWatermark,
		corev1alpha1.MemoryBackendCapabilityDurableRoutingFence,
		corev1alpha1.MemoryBackendCapabilityOperationLookup,
		corev1alpha1.MemoryBackendCapabilityExactGet,
		corev1alpha1.MemoryBackendCapabilityStablePagination,
		corev1alpha1.MemoryBackendCapabilityExclusiveOwnership,
		corev1alpha1.MemoryBackendCapabilityKeywordSearch,
		corev1alpha1.MemoryBackendCapabilityAuditVersionVisibility,
	}
)

// MemoryBackendBindingCoordinator is the narrow durable-state boundary between
// the Kubernetes controller and the binding/catalog implementation. Preparing
// validation durably selects candidate epochs without making remote authority
// effective; ReconcileMemoryBackendBinding performs the requested barrier.
type MemoryBackendBindingCoordinator interface {
	PrepareMemoryBackendValidation(context.Context, MemoryBackendValidationSnapshot) (MemoryBackendValidationBinding, error)
	RecordMemoryBackendOwnershipClaimAttempt(context.Context, MemoryBackendOwnershipClaimAttemptSnapshot) error
	RetireMemoryBackendValidationCandidate(context.Context, MemoryBackendValidationCandidateRetirement) error
	ReconcileMemoryBackendBinding(context.Context, MemoryBackendBindingSnapshot) (MemoryBackendBindingResult, error)
	FinalizeMemoryBackendDeletion(context.Context, MemoryBackendDeletionSnapshot) (MemoryBackendDeletionResult, error)
}

// MemoryBackendValidationSnapshot contains the stable pre-claim identity and
// freshly resolved store metadata used to select durable candidate epochs.
type MemoryBackendValidationSnapshot struct {
	Namespace                  string
	NamespaceUID               string
	BackendUID                 string
	BackendGeneration          int64
	ClusterID                  string
	ClusterIdentityDigest      string
	TenantID                   string
	RequestedLifecycle         corev1alpha1.MemoryBackendLifecycleState
	SpecDigest                 string
	EndpointIdentity           string
	EndpointDigest             string
	ResolvedAddressDigest      string
	ServerCertificateDigest    string
	SecretName                 string
	SecretKey                  string
	SecretUID                  string
	SecretResourceVersion      string
	StoreName                  string
	StoreUUID                  string
	Protocol                   string
	PreviousEffectiveLifecycle corev1alpha1.MemoryBackendEffectiveLifecycleState
	PreviousAuthorityEpoch     int64
	PreviousRoutingEpoch       int64
	LifecycleIntentDigest      string
}

// MemoryBackendValidationBinding is a durable, non-effective candidate binding
// used by OMS capabilities and ownership claim probes.
type MemoryBackendValidationBinding struct {
	AuthorityEpoch      int64
	RoutingEpoch        int64
	CandidateDigest     string
	DrainRequired       bool
	RemoteFenceRequired bool
}

// MemoryBackendOwnershipClaimAttemptSnapshot identifies a durable candidate immediately
// before the controller may send an ownership-claim request. It contains no secret data.
type MemoryBackendOwnershipClaimAttemptSnapshot struct {
	Namespace          string
	NamespaceUID       string
	BackendUID         string
	RequestedLifecycle corev1alpha1.MemoryBackendLifecycleState
	AuthorityEpoch     int64
	RoutingEpoch       int64
	CandidateDigest    string
}

// MemoryBackendValidationCandidateRetirement records an exact candidate fence
// acknowledgement or an explicit namespace-termination orphan decision.
type MemoryBackendValidationCandidateRetirement struct {
	Namespace               string
	NamespaceUID            string
	BackendUID              string
	AuthorityEpoch          int64
	RoutingEpoch            int64
	CandidateDigest         string
	RemoteFenceAcknowledged bool
	NamespaceTerminating    bool
}

// MemoryBackendBindingSnapshot contains only validated, non-secret metadata.
type MemoryBackendBindingSnapshot struct {
	Namespace                  string
	NamespaceUID               string
	BackendUID                 string
	BackendGeneration          int64
	ClusterID                  string
	ClusterIdentityDigest      string
	TenantID                   string
	RequestedLifecycle         corev1alpha1.MemoryBackendLifecycleState
	SpecDigest                 string
	EndpointIdentity           string
	EndpointDigest             string
	ResolvedAddressDigest      string
	ServerCertificateDigest    string
	SecretName                 string
	SecretKey                  string
	SecretUID                  string
	SecretResourceVersion      string
	StoreName                  string
	Protocol                   string
	StoreUUID                  string
	OwnershipClaimIdentity     string
	Capabilities               corev1alpha1.MemoryBackendObservedCapabilities
	ValidationExpiresAt        time.Time
	AuthorityEpoch             int64
	RoutingEpoch               int64
	CandidateDigest            string
	PreviousEffectiveLifecycle corev1alpha1.MemoryBackendEffectiveLifecycleState
	PreviousAuthorityEpoch     int64
	PreviousRoutingEpoch       int64
	LifecycleIntentDigest      string
	RemoteFenceAcknowledged    bool
	AcknowledgedRoutingEpoch   int64
}

// MemoryBackendDurableRoute is the complete non-secret route identity persisted
// with an effective remote binding. The controller resolves the endpoint and
// Secret value freshly before any mutating egress.
type MemoryBackendDurableRoute struct {
	Namespace               string
	NamespaceUID            string
	ClusterID               string
	BackendUID              string
	BackendGeneration       int64
	AuthorityEpoch          int64
	RoutingEpoch            int64
	SpecDigest              string
	EndpointDigest          string
	ResolvedAddressDigest   string
	ServerCertificateDigest string
	SecretName              string
	SecretKey               string
	SecretUID               string
	SecretResourceVersion   string
	TenantID                string
	StoreName               string
	StoreUUID               string
	Protocol                string
}

// MemoryBackendBindingResult reports the durable barrier outcome.
type MemoryBackendBindingResult struct {
	EffectiveLifecycleState corev1alpha1.MemoryBackendEffectiveLifecycleState
	AuthorityEpoch          int64
	RoutingEpoch            int64
	Ready                   bool
	Reason                  string
	Message                 string
	Route                   MemoryBackendDurableRoute
}

// MemoryBackendDeletionSnapshot identifies the durable binding without requiring
// a Secret or reachable remote endpoint.
type MemoryBackendDeletionSnapshot struct {
	Namespace               string
	NamespaceUID            string
	BackendUID              string
	ClusterIdentityDigest   string
	RequestedLifecycle      corev1alpha1.MemoryBackendLifecycleState
	EffectiveLifecycleState corev1alpha1.MemoryBackendEffectiveLifecycleState
	AuthorityEpoch          int64
	RoutingEpoch            int64
	OwnershipClaimIdentity  string
	NamespaceTerminating    bool
}

// MemoryBackendDeletionResult reports whether the durable egress/ownership
// barrier makes finalizer removal safe.
type MemoryBackendDeletionResult struct {
	SafeToRemove            bool
	EffectiveLifecycleState corev1alpha1.MemoryBackendEffectiveLifecycleState
	AuthorityEpoch          int64
	RoutingEpoch            int64
	CandidateDigest         string
	Reason                  string
	Message                 string
	Route                   MemoryBackendDurableRoute
}

// MemoryBackendOMSProber validates the exact pkg/oms/protocol store,
// capability, and ownership contract. Implementations must never persist or log
// BearerToken. Store resolution is pre-authority; binding probes require durable
// positive candidate epochs from MemoryBackendBindingCoordinator.
type MemoryBackendOMSProber interface {
	ResolveStore(context.Context, MemoryBackendStoreProbeTarget) (MemoryBackendStoreProbeResult, error)
	ProbeCapabilities(context.Context, MemoryBackendProbeTarget) (MemoryBackendProbeResult, error)
	ProbeBinding(context.Context, MemoryBackendProbeTarget) (MemoryBackendProbeResult, error)
	AdvanceRoutingFence(context.Context, MemoryBackendRoutingFenceTarget) (MemoryBackendRoutingFenceResult, error)
}

// MemoryBackendStoreProbeTarget contains ephemeral pre-authority probe data.
type MemoryBackendStoreProbeTarget struct {
	Endpoint              string
	EndpointIdentity      string
	EndpointDigest        string
	ResolvedAddressDigest string
	ResolvedAddresses     []netip.Addr
	BearerToken           string
	Profile               corev1alpha1.MemoryBackendProfile
	ClusterID             string
	NamespaceUID          string
	BackendUID            string
	TenantID              string
	StoreName             string
	Timeout               time.Duration
}

// MemoryBackendStoreProbeResult is the exact store-resolution observation.
type MemoryBackendStoreProbeResult struct {
	StoreUUID               string
	ServerCertificateDigest string
}

// MemoryBackendProbeTarget contains the complete candidate binding used for
// capabilities and durable ownership claim probes.
type MemoryBackendProbeTarget struct {
	Endpoint              string
	EndpointIdentity      string
	EndpointDigest        string
	ResolvedAddressDigest string
	ResolvedAddresses     []netip.Addr
	BearerToken           string
	Profile               corev1alpha1.MemoryBackendProfile
	ClusterID             string
	NamespaceUID          string
	BackendUID            string
	TenantID              string
	StoreUUID             string
	AuthorityEpoch        int64
	RoutingEpoch          int64
	Timeout               time.Duration
}

// MemoryBackendProbeResult is bounded, non-secret OMS validation output.
type MemoryBackendProbeResult struct {
	AdapterName             string
	AdapterVersion          string
	OwnershipClaimIdentity  string
	Capabilities            []corev1alpha1.MemoryBackendCapability
	CapabilityRevision      string
	CapabilityLimits        corev1alpha1.MemoryBackendCapabilityLimits
	CapabilityExpiresAt     time.Time
	ServerCertificateDigest string
}

// MemoryBackendRoutingFenceTarget contains the freshly authorized, exact OMS
// authority identity whose durable routing fence must advance.
type MemoryBackendRoutingFenceTarget struct {
	Endpoint                        string
	EndpointIdentity                string
	EndpointDigest                  string
	ResolvedAddressDigest           string
	ResolvedAddresses               []netip.Addr
	ExpectedServerCertificateDigest string
	BearerToken                     string
	Profile                         corev1alpha1.MemoryBackendProfile
	ClusterID                       string
	NamespaceUID                    string
	BackendUID                      string
	TenantID                        string
	StoreUUID                       string
	AuthorityEpoch                  int64
	RoutingEpoch                    int64
	Timeout                         time.Duration
}

// MemoryBackendRoutingFenceResult is the bounded durable acknowledgement from
// the OMS adapter.
type MemoryBackendRoutingFenceResult struct {
	MaximumRoutingEpoch     int64
	ServerCertificateDigest string
}

// OMSHTTPProber implements the closed initial Orka OMS control-plane profile.
type OMSHTTPProber struct {
	Policy     endpointpolicy.PublicHTTPSPolicy
	BaseClient *http.Client
	newClient  func(*http.Client, time.Duration, endpointpolicy.Resolution) (*http.Client, error)
}

// MemoryBackendReconciler validates MemoryBackend/default and coordinates only
// explicit durable lifecycle barriers.
type MemoryBackendReconciler struct {
	client.Client
	APIReader          client.Reader
	EndpointPolicy     endpointpolicy.PublicHTTPSPolicy
	OMSProber          MemoryBackendOMSProber
	BindingCoordinator MemoryBackendBindingCoordinator
	ClusterIdentity    string
	ValidationTTL      time.Duration
	ProbeTimeout       time.Duration
	Now                func() time.Time
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=memorybackends,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=memorybackends/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=memorybackends/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile validates one fixed-name MemoryBackend and never changes effective
// authority without a successful durable coordinator result.
//
//nolint:gocyclo // Fail-closed identity, freshness, and lifecycle gates are intentionally explicit.
func (r *MemoryBackendReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	backend := &corev1alpha1.MemoryBackend{}
	if err := r.Get(ctx, req.NamespacedName, backend); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	logger := log.FromContext(ctx)
	logger.Info("Reconciling MemoryBackend", "namespace", backend.Namespace, "name", backend.Name, "requestedLifecycle", backend.Spec.RequestedLifecycle())

	if !backend.DeletionTimestamp.IsZero() {
		return r.reconcileMemoryBackendDeletion(ctx, backend)
	}

	now := r.now()
	status := initialMemoryBackendStatus(backend)
	conditions := memoryBackendConditionState{deletionSafe: true}

	if err := validateMemoryBackendSpec(backend); err != nil {
		status.Accepted = false
		status.Reason = "SpecRejected"
		status.Message = sanitizeMemoryBackendMessage(err.Error())
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	status.Accepted = true
	conditions.accepted = true

	if !controllerutil.ContainsFinalizer(backend, MemoryBackendProtectionFinalizer) {
		controllerutil.AddFinalizer(backend, MemoryBackendProtectionFinalizer)
		if err := r.Update(ctx, backend); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{Requeue: true}, nil
	}
	status.Protected = true
	conditions.protected = true

	specDigest, err := memoryBackendSpecDigest(backend.Spec)
	if err != nil {
		status.Reason = "SpecDigestFailed"
		status.Message = "backend spec could not be canonicalized"
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	status.ValidatedSpecDigest = specDigest
	if strings.TrimSpace(r.ClusterIdentity) != "" {
		status.ClusterIdentityDigest = memoryBackendDigest(r.ClusterIdentity)
	}

	requested := backend.Spec.RequestedLifecycle()
	lifecycleIntentDigest := strings.TrimSpace(backend.Annotations[corev1alpha1.MemoryBackendLifecycleIntentAnnotation])
	if protectedMemoryBackendLifecycle(requested) {
		expectedIntentDigest := memoryBackendLifecycleIntentDigest(string(backend.UID), backend.Generation, requested, specDigest)
		if lifecycleIntentDigest != expectedIntentDigest {
			status.Reason = "LifecycleIntentRequired"
			status.Message = "protected lifecycle request lacks a matching durable generation-specific intent"
			expireMemoryBackendValidation(&status, now)
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
	}

	if requested == corev1alpha1.MemoryBackendLifecycleDisabled || requested == corev1alpha1.MemoryBackendLifecycleDecommissioning {
		return r.reconcileMemoryBackendBarrierWithoutProbe(ctx, backend, status, conditions, now)
	}

	if strings.TrimSpace(r.ClusterIdentity) == "" {
		status.Reason = "ClusterIdentityUnavailable"
		status.Message = "controller cluster identity is not configured"
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if r.APIReader == nil {
		status.Reason = memoryBackendReasonUncachedReaderUnavailable
		status.Message = "uncached Kubernetes API reader is not configured"
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}

	namespace := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: backend.Namespace}, namespace); err != nil {
		status.Reason = memoryBackendReasonNamespaceReadFailed
		status.Message = "namespace identity could not be read from the Kubernetes API"
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if namespace.UID == "" || backend.UID == "" {
		status.Reason = "ObjectIdentityUnavailable"
		status.Message = "namespace or backend UID is unavailable"
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	status.NamespaceUID = string(namespace.UID)
	status.BackendUID = string(backend.UID)
	if !namespace.DeletionTimestamp.IsZero() {
		return r.reconcileMemoryBackendNamespaceTermination(ctx, backend, namespace, status, conditions, now)
	}
	tenantID := deriveMemoryBackendTenantID(r.ClusterIdentity, namespace.UID)

	var persistedCandidate *memoryBackendValidationCandidateSnapshot
	if raw := strings.TrimSpace(backend.Annotations[corev1alpha1.MemoryBackendValidationCandidateAnnotation]); raw != "" {
		candidate, candidateErr := decodeMemoryBackendValidationCandidate(raw)
		if candidateErr != nil {
			status.Reason = "ValidationCandidateRejected"
			status.Message = "durable validation candidate metadata is invalid and requires operator repair"
			expireMemoryBackendValidation(&status, now)
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		if !validationCandidateMatchesCurrent(candidate, backend, specDigest) {
			if candidateErr = r.fenceAndRetireMemoryBackendValidationCandidate(ctx, backend, candidate); candidateErr != nil {
				status.Reason, status.Message = memoryBackendProbeFailure(candidateErr)
				if status.Reason == "OMSProbeFailed" {
					status.Reason = "CandidateRetirementBlocked"
					status.Message = "previous validation candidate could not be safely fenced and retired"
				}
				expireMemoryBackendValidation(&status, now)
				return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
			}
			return ctrl.Result{Requeue: true}, nil
		}
		persistedCandidate = &candidate
	}

	probeTimeout := r.probeTimeout()
	resolution, err := r.resolveMemoryBackendEndpoint(ctx, backend.Spec.Deployment.Endpoint, probeTimeout)
	if err != nil {
		status.Reason = "EndpointRejected"
		status.Message = sanitizeMemoryBackendMessage(err.Error())
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	status.EndpointIdentity = resolution.Identity
	status.EndpointDigest = resolution.EndpointDigest
	status.ResolvedAddressDigest = resolution.ResolvedAddressDigest

	secret, err := readMemoryBackendBearerSecret(ctx, r.APIReader, backend, namespace.UID, tenantID, resolution.Identity)
	if err != nil {
		status.Reason = "SecretBindingRejected"
		status.Message = sanitizeMemoryBackendMessage(err.Error())
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	status.SecretUID = string(secret.UID)
	status.SecretResourceVersion = secret.ResourceVersion
	status.ResolvedRefs = true
	conditions.resolved = true

	prober := r.OMSProber
	if prober == nil {
		prober = &OMSHTTPProber{Policy: r.EndpointPolicy}
	}
	storeProbeCtx, cancelStoreProbe := context.WithTimeout(ctx, probeTimeout)
	storeProbe, err := prober.ResolveStore(storeProbeCtx, MemoryBackendStoreProbeTarget{
		Endpoint:              resolution.BaseURL,
		EndpointIdentity:      resolution.Identity,
		EndpointDigest:        resolution.EndpointDigest,
		ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses:     resolution.Addresses,
		BearerToken:           secret.Token,
		Profile:               backend.Spec.Protocol.Profile,
		ClusterID:             r.ClusterIdentity,
		NamespaceUID:          string(namespace.UID),
		BackendUID:            string(backend.UID),
		TenantID:              tenantID,
		StoreName:             backend.Spec.Store.Name,
		Timeout:               probeTimeout,
	})
	cancelStoreProbe()
	if err != nil {
		reason, message := memoryBackendProbeFailure(err)
		status.Reason = reason
		status.Message = message
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if err := validateMemoryBackendStoreProbeResult(storeProbe); err != nil {
		status.Reason = "StoreProbeResultRejected"
		status.Message = sanitizeMemoryBackendMessage(err.Error())
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	status.StoreUUID = storeProbe.StoreUUID
	status.ServerCertificateDigest = storeProbe.ServerCertificateDigest

	if persistedCandidate != nil && !validationCandidateMatchesResolvedRoute(*persistedCandidate, resolution, secret, storeProbe, tenantID) {
		if err := r.fenceAndRetireMemoryBackendValidationCandidate(ctx, backend, *persistedCandidate); err != nil {
			status.Reason, status.Message = memoryBackendProbeFailure(err)
			expireMemoryBackendValidation(&status, now)
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		return ctrl.Result{Requeue: true}, nil
	}

	if r.BindingCoordinator == nil {
		status.Reason = memoryBackendReasonBindingCoordinatorUnavailable
		status.Message = "durable backend validation coordinator is not configured"
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	prepared, err := r.BindingCoordinator.PrepareMemoryBackendValidation(ctx, MemoryBackendValidationSnapshot{
		Namespace:                  backend.Namespace,
		NamespaceUID:               string(namespace.UID),
		BackendUID:                 string(backend.UID),
		BackendGeneration:          backend.Generation,
		ClusterID:                  r.ClusterIdentity,
		ClusterIdentityDigest:      status.ClusterIdentityDigest,
		TenantID:                   tenantID,
		RequestedLifecycle:         requested,
		SpecDigest:                 specDigest,
		EndpointIdentity:           resolution.Identity,
		EndpointDigest:             resolution.EndpointDigest,
		ResolvedAddressDigest:      resolution.ResolvedAddressDigest,
		ServerCertificateDigest:    storeProbe.ServerCertificateDigest,
		SecretName:                 secret.Name,
		SecretKey:                  secret.Key,
		SecretUID:                  string(secret.UID),
		SecretResourceVersion:      secret.ResourceVersion,
		StoreName:                  backend.Spec.Store.Name,
		StoreUUID:                  storeProbe.StoreUUID,
		Protocol:                   string(backend.Spec.Protocol.Profile),
		PreviousEffectiveLifecycle: backend.Status.EffectiveLifecycleState,
		PreviousAuthorityEpoch:     backend.Status.AuthorityEpoch,
		PreviousRoutingEpoch:       backend.Status.RoutingEpoch,
		LifecycleIntentDigest:      lifecycleIntentDigest,
	})
	if err != nil {
		status.Reason = "ValidationBindingFailed"
		status.Message = "durable candidate binding could not be prepared"
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if prepared.AuthorityEpoch < 1 || prepared.RoutingEpoch < 1 {
		status.Reason = "ValidationBindingRejected"
		status.Message = "durable candidate binding returned invalid epochs"
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if prepared.DrainRequired {
		draining := backend.Status
		draining.Accepted = true
		draining.Protected = true
		draining.Ready = false
		draining.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleDraining
		draining.Reason = "Draining"
		draining.Message = "previously accepted memory operations are draining at the old routing epoch"
		drainConditions := conditions
		drainConditions.resolved = draining.ResolvedRefs
		drainConditions.connected = draining.Connected
		drainConditions.capabilitiesCompatible = draining.ObservedCapabilities != nil
		drainConditions.fresh = draining.ValidationExpiresAt != nil && draining.ValidationExpiresAt.After(now)
		return r.publishMemoryBackendStatus(ctx, backend, draining, drainConditions, now, defaultMemoryBackendRequeueInterval)
	}
	status.AuthorityEpoch = prepared.AuthorityEpoch
	status.RoutingEpoch = prepared.RoutingEpoch
	var validationCandidate memoryBackendValidationCandidateSnapshot
	if requested == corev1alpha1.MemoryBackendLifecycleActive || requested == corev1alpha1.MemoryBackendLifecycleReadOnly {
		if strings.TrimSpace(prepared.CandidateDigest) == "" {
			status.Reason = "ValidationBindingRejected"
			status.Message = "durable candidate binding omitted its exact candidate digest"
			expireMemoryBackendValidation(&status, now)
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		validationCandidate = memoryBackendValidationCandidateSnapshot{
			Version: "v1", Namespace: backend.Namespace, NamespaceUID: string(namespace.UID),
			BackendUID: string(backend.UID), BackendGeneration: backend.Generation, RequestedLifecycle: requested,
			LifecycleIntentDigest: lifecycleIntentDigest, SpecDigest: specDigest, EndpointIdentity: resolution.Identity,
			EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
			ServerCertificateDigest: storeProbe.ServerCertificateDigest, SecretName: secret.Name, SecretKey: secret.Key,
			SecretUID: string(secret.UID), SecretResourceVersion: secret.ResourceVersion, TenantID: tenantID,
			StoreName: backend.Spec.Store.Name, StoreUUID: storeProbe.StoreUUID, Protocol: backend.Spec.Protocol.Profile,
			AuthorityEpoch: prepared.AuthorityEpoch, RoutingEpoch: prepared.RoutingEpoch,
			CandidateDigest: prepared.CandidateDigest,
		}
		if persistedCandidate != nil {
			if !reflect.DeepEqual(*persistedCandidate, validationCandidate) {
				status.Reason = "ValidationCandidateChanged"
				status.Message = "durable validation candidate changed before ownership claim"
				expireMemoryBackendValidation(&status, now)
				return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
			}
		} else {
			freshBackend, persistErr := r.persistMemoryBackendValidationCandidate(ctx, backend, validationCandidate)
			if persistErr != nil {
				status.Reason = "ValidationCandidatePersistenceFailed"
				status.Message = "ownership claim was not sent because its complete route snapshot was not persisted"
				expireMemoryBackendValidation(&status, now)
				return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
			}
			backend = freshBackend
		}
	}
	probeTarget := MemoryBackendProbeTarget{
		Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses: resolution.Addresses, BearerToken: secret.Token,
		Profile: backend.Spec.Protocol.Profile, ClusterID: r.ClusterIdentity,
		NamespaceUID: string(namespace.UID), BackendUID: string(backend.UID), TenantID: tenantID,
		StoreUUID: storeProbe.StoreUUID, AuthorityEpoch: prepared.AuthorityEpoch,
		RoutingEpoch: prepared.RoutingEpoch, Timeout: probeTimeout,
	}
	remoteFenceAcknowledged := false
	if prepared.RemoteFenceRequired {
		freshRoute, fenceErr := r.revalidateMemoryBackendCandidateRoute(ctx, validationCandidate, prober, probeTimeout, false)
		if fenceErr != nil {
			status.Reason, status.Message = memoryBackendProbeFailure(fenceErr)
			expireMemoryBackendValidation(&status, now)
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		probeTarget = memoryBackendProbeTargetFromFreshCandidate(freshRoute, validationCandidate, r.ClusterIdentity, probeTimeout)
		fenceCtx, cancelFence := context.WithTimeout(ctx, probeTimeout)
		fence, fenceErr := prober.AdvanceRoutingFence(fenceCtx, MemoryBackendRoutingFenceTarget{
			Endpoint: probeTarget.Endpoint, EndpointIdentity: probeTarget.EndpointIdentity,
			EndpointDigest: probeTarget.EndpointDigest, ResolvedAddressDigest: probeTarget.ResolvedAddressDigest,
			ResolvedAddresses: probeTarget.ResolvedAddresses, ExpectedServerCertificateDigest: validationCandidate.ServerCertificateDigest,
			BearerToken: probeTarget.BearerToken, Profile: probeTarget.Profile, ClusterID: probeTarget.ClusterID,
			NamespaceUID: probeTarget.NamespaceUID, BackendUID: probeTarget.BackendUID, TenantID: probeTarget.TenantID,
			StoreUUID: probeTarget.StoreUUID, AuthorityEpoch: probeTarget.AuthorityEpoch,
			RoutingEpoch: probeTarget.RoutingEpoch, Timeout: probeTarget.Timeout,
		})
		cancelFence()
		if fenceErr != nil || fence.MaximumRoutingEpoch != prepared.RoutingEpoch ||
			fence.ServerCertificateDigest != validationCandidate.ServerCertificateDigest {
			if fenceErr != nil {
				status.Reason, status.Message = memoryBackendProbeFailure(fenceErr)
			} else {
				status.Reason = "OMSRoutingFenceAcknowledgementInvalid"
				status.Message = "OMS routing fence acknowledgement did not match the validation candidate"
			}
			expireMemoryBackendValidation(&status, now)
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		remoteFenceAcknowledged = true
	}

	var probe MemoryBackendProbeResult
	if requested == corev1alpha1.MemoryBackendLifecycleStaged {
		capabilitiesCtx, cancelCapabilities := context.WithTimeout(ctx, probeTimeout)
		probe, err = prober.ProbeCapabilities(capabilitiesCtx, probeTarget)
		cancelCapabilities()
	} else {
		err = r.BindingCoordinator.RecordMemoryBackendOwnershipClaimAttempt(ctx, MemoryBackendOwnershipClaimAttemptSnapshot{
			Namespace: backend.Namespace, NamespaceUID: string(namespace.UID), BackendUID: string(backend.UID),
			RequestedLifecycle: requested, AuthorityEpoch: prepared.AuthorityEpoch, RoutingEpoch: prepared.RoutingEpoch,
			CandidateDigest: prepared.CandidateDigest,
		})
		if err != nil {
			status.Reason = "OwnershipClaimAttemptPersistenceFailed"
			status.Message = "ownership claim was not sent because its durable attempt marker could not be persisted"
			expireMemoryBackendValidation(&status, now)
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		freshRoute, revalidateErr := r.revalidateMemoryBackendCandidateRoute(ctx, validationCandidate, prober, probeTimeout, false)
		if revalidateErr != nil {
			status.Reason = "OwnershipClaimRevalidationFailed"
			status.Message = "ownership claim was not sent because the uncached route snapshot changed"
			expireMemoryBackendValidation(&status, now)
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		probeTarget = memoryBackendProbeTargetFromFreshCandidate(freshRoute, validationCandidate, r.ClusterIdentity, probeTimeout)
		bindingCtx, cancelBinding := context.WithTimeout(ctx, probeTimeout)
		probe, err = prober.ProbeBinding(bindingCtx, probeTarget)
		cancelBinding()
	}
	if err != nil {
		reason, message := memoryBackendProbeFailure(err)
		status.Reason = reason
		status.Message = message
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if requested == corev1alpha1.MemoryBackendLifecycleStaged {
		err = validateMemoryBackendCapabilityProbeResult(probe, now)
	} else {
		err = validateMemoryBackendProbeResult(probe, now)
	}
	if err != nil {
		status.Reason = "ProbeResultRejected"
		status.Message = sanitizeMemoryBackendMessage(err.Error())
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if probe.ServerCertificateDigest != storeProbe.ServerCertificateDigest {
		status.Reason = "OMSCertificateChanged"
		status.Message = "OMS server certificate identity changed during validation"
		expireMemoryBackendValidation(&status, now)
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}

	native := sortedMemoryBackendCapabilities(probe.Capabilities)
	effective := append([]corev1alpha1.MemoryBackendCapability(nil), native...)
	capabilityExpiry := metav1.NewTime(probe.CapabilityExpiresAt.UTC())
	status.ObservedCapabilities = &corev1alpha1.MemoryBackendObservedCapabilities{
		AdapterName:    probe.AdapterName,
		AdapterVersion: probe.AdapterVersion,
		Revision:       probe.CapabilityRevision,
		Native:         native,
		Effective:      effective,
		Limits:         probe.CapabilityLimits,
		ExpiresAt:      capabilityExpiry,
	}
	status.OwnershipClaimIdentity = probe.OwnershipClaimIdentity
	status.ServerCertificateDigest = probe.ServerCertificateDigest
	status.Connected = true
	conditions.connected = true
	conditions.capabilitiesCompatible = true

	lastValidated := metav1.NewTime(now)
	validationExpiry := now.Add(r.validationTTL())
	if probe.CapabilityExpiresAt.Before(validationExpiry) {
		validationExpiry = probe.CapabilityExpiresAt
	}
	expiresAt := metav1.NewTime(validationExpiry.UTC())
	status.LastValidated = &lastValidated
	status.ValidationExpiresAt = &expiresAt
	conditions.fresh = validationExpiry.After(now)

	snapshot := MemoryBackendBindingSnapshot{
		Namespace:                  backend.Namespace,
		NamespaceUID:               string(namespace.UID),
		BackendUID:                 string(backend.UID),
		BackendGeneration:          backend.Generation,
		ClusterID:                  r.ClusterIdentity,
		ClusterIdentityDigest:      status.ClusterIdentityDigest,
		TenantID:                   tenantID,
		RequestedLifecycle:         requested,
		SpecDigest:                 specDigest,
		EndpointIdentity:           resolution.Identity,
		EndpointDigest:             resolution.EndpointDigest,
		ResolvedAddressDigest:      resolution.ResolvedAddressDigest,
		ServerCertificateDigest:    probe.ServerCertificateDigest,
		SecretName:                 secret.Name,
		SecretKey:                  secret.Key,
		SecretUID:                  string(secret.UID),
		SecretResourceVersion:      secret.ResourceVersion,
		StoreName:                  backend.Spec.Store.Name,
		Protocol:                   string(backend.Spec.Protocol.Profile),
		StoreUUID:                  storeProbe.StoreUUID,
		OwnershipClaimIdentity:     probe.OwnershipClaimIdentity,
		Capabilities:               *status.ObservedCapabilities,
		ValidationExpiresAt:        validationExpiry,
		AuthorityEpoch:             prepared.AuthorityEpoch,
		RoutingEpoch:               prepared.RoutingEpoch,
		CandidateDigest:            prepared.CandidateDigest,
		PreviousEffectiveLifecycle: backend.Status.EffectiveLifecycleState,
		PreviousAuthorityEpoch:     backend.Status.AuthorityEpoch,
		PreviousRoutingEpoch:       backend.Status.RoutingEpoch,
		LifecycleIntentDigest:      lifecycleIntentDigest,
		RemoteFenceAcknowledged:    remoteFenceAcknowledged,
		AcknowledgedRoutingEpoch:   map[bool]int64{true: prepared.RoutingEpoch}[remoteFenceAcknowledged],
	}

	if requested == corev1alpha1.MemoryBackendLifecycleStaged {
		status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleStaged
		status.Ready = conditions.fresh
		conditions.ready = status.Ready
		status.Reason = "StagedValidated"
		status.Message = "backend validation is fresh; legacy memory remains authoritative"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, memoryBackendRequeueAfter(now, validationExpiry))
	}

	commitRoute, revalidateErr := r.revalidateMemoryBackendCandidateRoute(ctx, validationCandidate, prober, probeTimeout, false)
	if revalidateErr != nil {
		status.Ready = false
		status.Reason = "BindingCommitRevalidationFailed"
		status.Message = "durable binding was not committed because the uncached route snapshot changed"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	backend = commitRoute.Backend
	binding, err := r.BindingCoordinator.ReconcileMemoryBackendBinding(ctx, snapshot)
	if err != nil {
		status.Ready = false
		status.Reason = memoryBackendReasonBindingBarrierFailed
		status.Message = "durable backend lifecycle barrier did not complete"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if err := validateMemoryBackendBindingResult(requested, binding); err != nil {
		status.Ready = false
		status.Reason = memoryBackendReasonBindingResultRejected
		status.Message = sanitizeMemoryBackendMessage(err.Error())
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if binding.AuthorityEpoch != prepared.AuthorityEpoch || binding.RoutingEpoch < prepared.RoutingEpoch ||
		(binding.Ready && binding.RoutingEpoch != prepared.RoutingEpoch) {
		status.Ready = false
		status.Reason = "BindingEpochMismatch"
		status.Message = "durable lifecycle result did not preserve the validated candidate authority"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if binding.Ready {
		freshBackend, clearErr := r.clearMemoryBackendValidationCandidate(ctx, backend, prepared.CandidateDigest)
		if clearErr != nil {
			status.Ready = false
			status.Reason = "ValidationCandidateRetirementFailed"
			status.Message = "durable binding completed but its validation candidate metadata could not be retired"
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		backend = freshBackend
	}
	status.EffectiveLifecycleState = binding.EffectiveLifecycleState
	status.AuthorityEpoch = binding.AuthorityEpoch
	status.RoutingEpoch = binding.RoutingEpoch
	status.Ready = binding.Ready && conditions.fresh
	conditions.ready = status.Ready
	status.Reason = sanitizeMemoryBackendReason(binding.Reason, "BindingReconciled")
	status.Message = sanitizeMemoryBackendMessage(binding.Message)
	if status.Message == "" {
		status.Message = "durable backend lifecycle barrier is reconciled"
	}
	return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, memoryBackendRequeueAfter(now, validationExpiry))
}

func (r *MemoryBackendReconciler) reconcileMemoryBackendNamespaceTermination(
	ctx context.Context,
	backend *corev1alpha1.MemoryBackend,
	namespace *corev1.Namespace,
	status corev1alpha1.MemoryBackendStatus,
	conditions memoryBackendConditionState,
	now time.Time,
) (ctrl.Result, error) {
	status.Ready = false
	conditions.ready = false
	conditions.deletionSafe = false
	if r.BindingCoordinator == nil {
		status.Reason = memoryBackendReasonBindingCoordinatorUnavailable
		status.Message = "namespace termination is blocked because the durable coordinator is not configured"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	result, err := r.BindingCoordinator.FinalizeMemoryBackendDeletion(ctx, MemoryBackendDeletionSnapshot{
		Namespace: backend.Namespace, NamespaceUID: string(namespace.UID), BackendUID: string(backend.UID),
		ClusterIdentityDigest: status.ClusterIdentityDigest, RequestedLifecycle: backend.Spec.RequestedLifecycle(),
		EffectiveLifecycleState: backend.Status.EffectiveLifecycleState,
		AuthorityEpoch:          backend.Status.AuthorityEpoch, RoutingEpoch: backend.Status.RoutingEpoch,
		OwnershipClaimIdentity: backend.Status.OwnershipClaimIdentity, NamespaceTerminating: true,
	})
	if err != nil {
		status.Reason = "NamespaceTerminationBarrierFailed"
		status.Message = "namespace termination could not establish the irreversible local memory barrier"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	status.EffectiveLifecycleState = result.EffectiveLifecycleState
	status.AuthorityEpoch = result.AuthorityEpoch
	status.RoutingEpoch = result.RoutingEpoch
	status.Reason = sanitizeMemoryBackendReason(result.Reason, "NamespaceTerminating")
	status.Message = sanitizeMemoryBackendMessage(result.Message)
	if result.SafeToRemove {
		status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleRemoved
		status.Reason = "NamespaceTerminating"
		status.Message = "namespace termination established an irreversible local Removed barrier"
	}
	expireMemoryBackendValidation(&status, now)
	return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
}

func (r *MemoryBackendReconciler) reconcileMemoryBackendBarrierWithoutProbe(
	ctx context.Context,
	backend *corev1alpha1.MemoryBackend,
	status corev1alpha1.MemoryBackendStatus,
	conditions memoryBackendConditionState,
	now time.Time,
) (ctrl.Result, error) {
	status.Ready = false
	if r.BindingCoordinator == nil {
		expireMemoryBackendValidation(&status, now)
		status.Reason = memoryBackendReasonBindingCoordinatorUnavailable
		status.Message = "durable backend lifecycle barrier is blocked because the coordinator is not configured"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if r.APIReader == nil {
		status.Reason = memoryBackendReasonUncachedReaderUnavailable
		status.Message = "durable backend lifecycle barrier requires an uncached Kubernetes API reader"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	fresh := &corev1alpha1.MemoryBackend{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(backend), fresh); err != nil {
		return ctrl.Result{}, err
	}
	if fresh.UID != backend.UID || fresh.Generation != backend.Generation || !fresh.DeletionTimestamp.IsZero() {
		status.Reason = "LifecycleRevalidationFailed"
		status.Message = "MemoryBackend identity, generation, or deletion state changed before lifecycle barrier"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	backend = fresh
	specDigest, err := memoryBackendSpecDigest(backend.Spec)
	if err != nil {
		return ctrl.Result{}, err
	}
	intentDigest := strings.TrimSpace(backend.Annotations[corev1alpha1.MemoryBackendLifecycleIntentAnnotation])
	if intentDigest != memoryBackendLifecycleIntentDigest(string(backend.UID), backend.Generation, backend.Spec.RequestedLifecycle(), specDigest) {
		status.Reason = "LifecycleIntentRequired"
		status.Message = "protected lifecycle request lacks a matching durable generation-specific intent"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	namespace := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: backend.Namespace}, namespace); err != nil {
		status.Reason = memoryBackendReasonNamespaceReadFailed
		status.Message = "namespace identity could not be read from the Kubernetes API"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if !namespace.DeletionTimestamp.IsZero() {
		return r.reconcileMemoryBackendNamespaceTermination(ctx, backend, namespace, status, conditions, now)
	}
	snapshot := MemoryBackendBindingSnapshot{
		Namespace: backend.Namespace, NamespaceUID: string(namespace.UID), BackendUID: string(backend.UID),
		BackendGeneration: backend.Generation, ClusterID: r.ClusterIdentity,
		ClusterIdentityDigest: memoryBackendDigest(r.ClusterIdentity), RequestedLifecycle: backend.Spec.RequestedLifecycle(),
		SpecDigest: specDigest, StoreName: backend.Spec.Store.Name, Protocol: string(backend.Spec.Protocol.Profile),
		LifecycleIntentDigest: intentDigest,
	}
	binding, err := r.BindingCoordinator.ReconcileMemoryBackendBinding(ctx, snapshot)
	if err != nil {
		expireMemoryBackendValidation(&status, now)
		status.Reason = memoryBackendReasonBindingBarrierFailed
		status.Message = "durable backend lifecycle barrier did not complete"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if binding.Reason == "RemoteFencePending" {
		observation, fenceErr := r.advanceMemoryBackendRemoteFence(ctx, backend, binding.Route)
		if fenceErr != nil {
			expireMemoryBackendValidation(&status, now)
			status.Reason, status.Message = memoryBackendProbeFailure(fenceErr)
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		recordMemoryBackendRemoteFenceObservation(&status, &conditions, observation)
		if _, revalidateErr := r.resolveMemoryBackendDurableRoute(ctx, backend, binding.Route); revalidateErr != nil {
			status.Reason = "LifecycleCommitRevalidationFailed"
			status.Message = "remote fence was acknowledged but the uncached route changed before durable commit"
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		snapshot.RemoteFenceAcknowledged = true
		snapshot.AcknowledgedRoutingEpoch = binding.RoutingEpoch
		binding, err = r.BindingCoordinator.ReconcileMemoryBackendBinding(ctx, snapshot)
		if err != nil {
			status.Reason = memoryBackendReasonBindingBarrierFailed
			status.Message = "remote fence was acknowledged but the durable lifecycle commit failed"
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
	}
	if !knownMemoryBackendEffectiveLifecycle(binding.EffectiveLifecycleState) || binding.AuthorityEpoch < 0 || binding.RoutingEpoch < 0 {
		expireMemoryBackendValidation(&status, now)
		status.Reason = memoryBackendReasonBindingResultRejected
		status.Message = "durable backend lifecycle result was invalid"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	expireMemoryBackendValidation(&status, now)
	status.NamespaceUID = string(namespace.UID)
	status.EffectiveLifecycleState = binding.EffectiveLifecycleState
	status.AuthorityEpoch = binding.AuthorityEpoch
	status.RoutingEpoch = binding.RoutingEpoch
	status.Reason = sanitizeMemoryBackendReason(binding.Reason, "BindingBarrierPending")
	status.Message = sanitizeMemoryBackendMessage(binding.Message)
	if status.Message == "" {
		status.Message = "backend is fenced while the durable lifecycle barrier converges"
	}
	return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
}

type memoryBackendRemoteFenceObservation struct {
	resolution              endpointpolicy.Resolution
	secret                  memoryBackendSecret
	serverCertificateDigest string
}

func recordMemoryBackendRemoteFenceObservation(
	status *corev1alpha1.MemoryBackendStatus,
	conditions *memoryBackendConditionState,
	observation *memoryBackendRemoteFenceObservation,
) {
	if status == nil || conditions == nil || observation == nil {
		return
	}
	status.ResolvedRefs = true
	conditions.resolved = true
	status.SecretUID = string(observation.secret.UID)
	status.SecretResourceVersion = observation.secret.ResourceVersion
	status.EndpointIdentity = observation.resolution.Identity
	status.EndpointDigest = observation.resolution.EndpointDigest
	status.ResolvedAddressDigest = observation.resolution.ResolvedAddressDigest
	status.ServerCertificateDigest = observation.serverCertificateDigest
}

//nolint:gocyclo // Each identity check is an explicit fail-closed routing-fence gate.
func (r *MemoryBackendReconciler) resolveMemoryBackendDurableRoute(
	ctx context.Context,
	backend *corev1alpha1.MemoryBackend,
	route MemoryBackendDurableRoute,
) (*memoryBackendRemoteFenceObservation, error) {
	if route.AuthorityEpoch < 1 || route.RoutingEpoch < 1 || route.NamespaceUID == "" || route.BackendUID == "" ||
		route.EndpointDigest == "" || route.ResolvedAddressDigest == "" || route.ServerCertificateDigest == "" ||
		route.SecretName == "" || route.SecretKey == "" || route.SecretUID == "" ||
		route.SecretResourceVersion == "" || route.TenantID == "" || route.StoreName == "" || route.StoreUUID == "" {
		return nil, newOMSProbeError("OMSRoutingFenceIdentityInvalid", "durable routing fence target contains incomplete identity")
	}
	if r.APIReader == nil || strings.TrimSpace(r.ClusterIdentity) == "" {
		return nil, newOMSProbeError("OMSRoutingFenceConfigurationInvalid", "durable routing fence requires uncached Kubernetes identity reads")
	}
	fresh := &corev1alpha1.MemoryBackend{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(backend), fresh); err != nil {
		return nil, newOMSProbeError("OMSRoutingFenceBackendReadFailed", "durable routing fence backend identity could not be read")
	}
	if fresh.UID != backend.UID || fresh.Generation != backend.Generation || !fresh.DeletionTimestamp.IsZero() ||
		string(fresh.UID) != route.BackendUID || fresh.Namespace != route.Namespace ||
		fresh.Spec.Store.Name != route.StoreName || string(fresh.Spec.Protocol.Profile) != route.Protocol ||
		strings.TrimSpace(fresh.Spec.ClientAuth.BearerTokenSecretRef.Name) != route.SecretName ||
		strings.TrimSpace(fresh.Spec.ClientAuth.BearerTokenSecretRef.Key) != route.SecretKey {
		return nil, newOMSProbeError("OMSRoutingFenceIdentityInvalid", "durable routing fence backend or route identity changed")
	}
	namespace := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: fresh.Namespace}, namespace); err != nil || namespace.UID == "" {
		return nil, newOMSProbeError("OMSRoutingFenceNamespaceReadFailed", "durable routing fence namespace identity could not be read")
	}
	if string(namespace.UID) != route.NamespaceUID || !namespace.DeletionTimestamp.IsZero() {
		return nil, newOMSProbeError("OMSRoutingFenceIdentityInvalid", "durable routing fence namespace identity or termination state changed")
	}
	tenantID := deriveMemoryBackendTenantID(r.ClusterIdentity, namespace.UID)
	if tenantID != route.TenantID || route.ClusterID != "" && route.ClusterID != r.ClusterIdentity {
		return nil, newOMSProbeError("OMSRoutingFenceIdentityInvalid", "durable routing fence tenant or cluster identity changed")
	}
	timeout := r.probeTimeout()
	resolution, err := r.resolveMemoryBackendEndpoint(ctx, fresh.Spec.Deployment.Endpoint, timeout)
	if err != nil {
		return nil, newOMSProbeError("OMSEndpointResolutionFailed", "OMS endpoint failed fresh public DNS validation")
	}
	if resolution.EndpointDigest != route.EndpointDigest || resolution.ResolvedAddressDigest != route.ResolvedAddressDigest {
		return nil, newOMSProbeError("OMSEndpointIdentityChanged", "OMS endpoint or resolved address identity changed before durable routing fence acknowledgement")
	}
	secret, err := readMemoryBackendBearerSecret(ctx, r.APIReader, fresh, namespace.UID, tenantID, resolution.Identity)
	if err != nil {
		return nil, newOMSProbeError("OMSSecretBindingRejected", "OMS routing fence bearer Secret failed fresh identity validation")
	}
	if secret.Name != route.SecretName || secret.Key != route.SecretKey || string(secret.UID) != route.SecretUID ||
		secret.ResourceVersion != route.SecretResourceVersion {
		return nil, newOMSProbeError("OMSSecretIdentityChanged", "OMS routing fence bearer Secret identity or resourceVersion changed")
	}
	prober := r.OMSProber
	if prober == nil {
		prober = &OMSHTTPProber{Policy: r.EndpointPolicy}
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	storeProbe, err := prober.ResolveStore(probeCtx, MemoryBackendStoreProbeTarget{
		Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses: resolution.Addresses, BearerToken: secret.Token, Profile: fresh.Spec.Protocol.Profile,
		ClusterID: r.ClusterIdentity, NamespaceUID: route.NamespaceUID, BackendUID: route.BackendUID,
		TenantID: route.TenantID, StoreName: route.StoreName, Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}
	if storeProbe.StoreUUID != route.StoreUUID || storeProbe.ServerCertificateDigest != route.ServerCertificateDigest {
		return nil, newOMSProbeError("OMSStoreIdentityChanged", "OMS store UUID or server certificate changed before durable routing fence acknowledgement")
	}
	return &memoryBackendRemoteFenceObservation{
		resolution: resolution, secret: secret, serverCertificateDigest: storeProbe.ServerCertificateDigest,
	}, nil
}

func (r *MemoryBackendReconciler) advanceMemoryBackendRemoteFence(
	ctx context.Context,
	backend *corev1alpha1.MemoryBackend,
	route MemoryBackendDurableRoute,
) (*memoryBackendRemoteFenceObservation, error) {
	observation, err := r.resolveMemoryBackendDurableRoute(ctx, backend, route)
	if err != nil {
		return nil, err
	}
	timeout := r.probeTimeout()
	prober := r.OMSProber
	if prober == nil {
		prober = &OMSHTTPProber{Policy: r.EndpointPolicy}
	}
	fenceCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := prober.AdvanceRoutingFence(fenceCtx, MemoryBackendRoutingFenceTarget{
		Endpoint: observation.resolution.BaseURL, EndpointIdentity: observation.resolution.Identity,
		EndpointDigest:                  observation.resolution.EndpointDigest,
		ResolvedAddressDigest:           observation.resolution.ResolvedAddressDigest,
		ResolvedAddresses:               observation.resolution.Addresses,
		ExpectedServerCertificateDigest: observation.serverCertificateDigest,
		BearerToken:                     observation.secret.Token, Profile: corev1alpha1.MemoryBackendProfile(route.Protocol),
		ClusterID: r.ClusterIdentity, NamespaceUID: route.NamespaceUID, BackendUID: route.BackendUID,
		TenantID: route.TenantID, StoreUUID: route.StoreUUID,
		AuthorityEpoch: route.AuthorityEpoch, RoutingEpoch: route.RoutingEpoch, Timeout: timeout,
	})
	if err != nil {
		return nil, err
	}
	if result.MaximumRoutingEpoch != route.RoutingEpoch || result.ServerCertificateDigest != observation.serverCertificateDigest {
		return nil, newOMSProbeError("OMSRoutingFenceAcknowledgementInvalid", "OMS routing fence acknowledgement did not match the exact durable target")
	}
	observation.serverCertificateDigest = result.ServerCertificateDigest
	return observation, nil
}

func (r *MemoryBackendReconciler) reconcileMemoryBackendDeletion(
	ctx context.Context,
	backend *corev1alpha1.MemoryBackend,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(backend, MemoryBackendProtectionFinalizer) {
		return ctrl.Result{}, nil
	}
	now := r.now()
	status := initialMemoryBackendStatus(backend)
	status.Accepted = backend.Name == corev1alpha1.MemoryBackendDefaultName
	status.Protected = true
	status.Ready = false
	status.Connected = false
	status.Reason = "DeletionBlocked"
	status.Message = "deletion is blocked until the durable backend barrier confirms removal is safe"
	expireMemoryBackendValidation(&status, now)
	conditions := memoryBackendConditionState{
		accepted:     backend.Name == corev1alpha1.MemoryBackendDefaultName,
		protected:    true,
		deletionSafe: false,
	}

	if r.BindingCoordinator == nil {
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	namespaceUID := backend.Status.NamespaceUID
	namespaceTerminating := false
	if r.APIReader != nil {
		namespace := &corev1.Namespace{}
		namespaceErr := r.APIReader.Get(ctx, client.ObjectKey{Name: backend.Namespace}, namespace)
		switch {
		case namespaceErr == nil:
			if namespace.UID != "" {
				namespaceUID = string(namespace.UID)
			}
			namespaceTerminating = !namespace.DeletionTimestamp.IsZero()
		case apierrors.IsNotFound(namespaceErr):
			namespaceTerminating = true
		default:
			status.Reason = memoryBackendReasonNamespaceReadFailed
			status.Message = "deletion remains blocked because namespace identity could not be read"
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
	}
	result, err := r.BindingCoordinator.FinalizeMemoryBackendDeletion(ctx, MemoryBackendDeletionSnapshot{
		Namespace:               backend.Namespace,
		NamespaceUID:            namespaceUID,
		BackendUID:              string(backend.UID),
		ClusterIdentityDigest:   backend.Status.ClusterIdentityDigest,
		RequestedLifecycle:      backend.Spec.RequestedLifecycle(),
		EffectiveLifecycleState: backend.Status.EffectiveLifecycleState,
		AuthorityEpoch:          backend.Status.AuthorityEpoch,
		RoutingEpoch:            backend.Status.RoutingEpoch,
		OwnershipClaimIdentity:  backend.Status.OwnershipClaimIdentity,
		NamespaceTerminating:    namespaceTerminating,
	})
	if err != nil {
		status.Reason = "DeletionBarrierFailed"
		status.Message = "deletion remains blocked because the durable backend barrier failed"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if result.AuthorityEpoch < 0 || result.RoutingEpoch < 0 ||
		(result.EffectiveLifecycleState != "" && !knownMemoryBackendEffectiveLifecycle(result.EffectiveLifecycleState)) {
		status.Reason = "DeletionResultRejected"
		status.Message = "deletion barrier returned an invalid lifecycle or epoch"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if !result.SafeToRemove && result.Reason == "CandidateFencePending" {
		raw := strings.TrimSpace(backend.Annotations[corev1alpha1.MemoryBackendValidationCandidateAnnotation])
		candidate, candidateErr := decodeMemoryBackendValidationCandidate(raw)
		if candidateErr != nil || candidate.AuthorityEpoch != result.AuthorityEpoch ||
			candidate.RoutingEpoch != result.RoutingEpoch || candidate.CandidateDigest != result.CandidateDigest {
			status.Reason = "CandidateSnapshotUnavailable"
			status.Message = "deletion remains blocked because the exact durable candidate route snapshot is unavailable"
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		if candidateErr = r.fenceAndRetireMemoryBackendValidationCandidate(ctx, backend, candidate); candidateErr != nil {
			status.Reason, status.Message = memoryBackendProbeFailure(candidateErr)
			return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
		}
		return ctrl.Result{Requeue: true}, nil
	}
	if !result.SafeToRemove {
		status.Reason = sanitizeMemoryBackendReason(result.Reason, "DeletionBlocked")
		status.Message = sanitizeMemoryBackendMessage(result.Message)
		if status.Message == "" {
			status.Message = "deletion is blocked until decommission or force-orphan completes"
		}
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	if result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned &&
		result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleRemoved &&
		(result.AuthorityEpoch != 0 || result.RoutingEpoch != 0) {
		status.Reason = "DeletionResultRejected"
		status.Message = "deletion barrier did not prove a decommissioned, removed, or never-activated backend"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}

	if r.APIReader == nil {
		status.Reason = memoryBackendReasonUncachedReaderUnavailable
		status.Message = "safe finalizer removal requires an uncached backend revalidation"
		return r.publishMemoryBackendStatus(ctx, backend, status, conditions, now, defaultMemoryBackendRequeueInterval)
	}
	freshBackend := &corev1alpha1.MemoryBackend{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(backend), freshBackend); err != nil {
		return ctrl.Result{}, err
	}
	if freshBackend.UID != backend.UID || freshBackend.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, fmt.Errorf("MemoryBackend changed before finalizer removal")
	}
	controllerutil.RemoveFinalizer(freshBackend, MemoryBackendProtectionFinalizer)
	if err := r.Update(ctx, freshBackend); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

func memoryBackendPrimaryWatchPredicate() predicate.Predicate {
	generationChanged := predicate.GenerationChangedPredicate{}
	return predicate.Funcs{
		UpdateFunc: func(updateEvent event.UpdateEvent) bool {
			if updateEvent.ObjectOld == nil || updateEvent.ObjectNew == nil {
				return false
			}
			if generationChanged.Update(updateEvent) {
				return true
			}
			oldDeletionTimestamp := updateEvent.ObjectOld.GetDeletionTimestamp()
			newDeletionTimestamp := updateEvent.ObjectNew.GetDeletionTimestamp()
			oldDeleting := oldDeletionTimestamp != nil && !oldDeletionTimestamp.IsZero()
			newDeleting := newDeletionTimestamp != nil && !newDeletionTimestamp.IsZero()
			return oldDeleting != newDeleting
		},
	}
}

// SetupWithManager registers fixed-name, Secret-reference, and Namespace watches.
func (r *MemoryBackendReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(),
		&corev1alpha1.MemoryBackend{},
		memoryBackendSecretReferenceField,
		func(object client.Object) []string {
			backend, ok := object.(*corev1alpha1.MemoryBackend)
			if !ok {
				return nil
			}
			name := strings.TrimSpace(backend.Spec.ClientAuth.BearerTokenSecretRef.Name)
			if name == "" {
				return nil
			}
			return []string{name}
		},
	); err != nil {
		return fmt.Errorf("index MemoryBackend Secret references: %w", err)
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.MemoryBackend{}, builder.WithPredicates(memoryBackendPrimaryWatchPredicate())).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(r.memoryBackendsForSecret)).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.memoryBackendForNamespace)).
		Named("memorybackend").
		Complete(r)
}

func (r *MemoryBackendReconciler) memoryBackendsForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	secret, ok := object.(*corev1.Secret)
	if !ok || secret.Namespace == "" || secret.Name == "" {
		return nil
	}
	backends := &corev1alpha1.MemoryBackendList{}
	if err := r.List(
		ctx,
		backends,
		client.InNamespace(secret.Namespace),
		client.MatchingFields{memoryBackendSecretReferenceField: secret.Name},
	); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0, len(backends.Items))
	for i := range backends.Items {
		requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&backends.Items[i])})
	}
	return requests
}

func (*MemoryBackendReconciler) memoryBackendForNamespace(_ context.Context, object client.Object) []reconcile.Request {
	namespace, ok := object.(*corev1.Namespace)
	if !ok || namespace.Name == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{
		Namespace: namespace.Name,
		Name:      corev1alpha1.MemoryBackendDefaultName,
	}}}
}

func (r *MemoryBackendReconciler) publishMemoryBackendStatus(
	ctx context.Context,
	backend *corev1alpha1.MemoryBackend,
	status corev1alpha1.MemoryBackendStatus,
	conditions memoryBackendConditionState,
	now time.Time,
	requeueAfter time.Duration,
) (ctrl.Result, error) {
	status.Message = sanitizeMemoryBackendMessage(status.Message)
	status.Reason = sanitizeMemoryBackendReason(status.Reason, "NotReady")
	setMemoryBackendConditions(&status, backend.Generation, metav1.NewTime(now), conditions)
	if reflect.DeepEqual(backend.Status, status) {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	backend.Status = status
	if err := r.Status().Update(ctx, backend); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: requeueAfter}, nil
}

func (r *MemoryBackendReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now().UTC()
	}
	return time.Now().UTC()
}

func (r *MemoryBackendReconciler) validationTTL() time.Duration {
	if r.ValidationTTL > 0 {
		return r.ValidationTTL
	}
	return defaultMemoryBackendValidationTTL
}

func (r *MemoryBackendReconciler) probeTimeout() time.Duration {
	if r.ProbeTimeout > 0 {
		return r.ProbeTimeout
	}
	return defaultMemoryBackendProbeTimeout
}

func (r *MemoryBackendReconciler) resolveMemoryBackendEndpoint(
	ctx context.Context,
	endpoint string,
	timeout time.Duration,
) (endpointpolicy.Resolution, error) {
	if timeout <= 0 {
		timeout = defaultMemoryBackendProbeTimeout
	}
	resolveCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return r.EndpointPolicy.Resolve(resolveCtx, endpoint)
}

func initialMemoryBackendStatus(backend *corev1alpha1.MemoryBackend) corev1alpha1.MemoryBackendStatus {
	status := backend.DeepCopy().Status
	status.Accepted = false
	status.Protected = false
	status.ResolvedRefs = false
	status.Connected = false
	status.Ready = false
	status.ObservedGeneration = backend.Generation
	status.BackendUID = string(backend.UID)
	status.Reason = ""
	status.Message = ""
	if status.EffectiveLifecycleState == "" {
		status.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleStaged
	}
	return status
}

type memoryBackendConditionState struct {
	accepted               bool
	protected              bool
	resolved               bool
	connected              bool
	capabilitiesCompatible bool
	fresh                  bool
	ready                  bool
	deletionSafe           bool
}

func setMemoryBackendConditions(
	status *corev1alpha1.MemoryBackendStatus,
	generation int64,
	now metav1.Time,
	state memoryBackendConditionState,
) {
	set := func(
		conditionType corev1alpha1.MemoryBackendConditionType,
		value bool,
		trueReason, falseReason, trueMessage, falseMessage string,
	) {
		conditionStatus := metav1.ConditionFalse
		reason, message := falseReason, falseMessage
		if value {
			conditionStatus = metav1.ConditionTrue
			reason, message = trueReason, trueMessage
		}
		meta.SetStatusCondition(&status.Conditions, metav1.Condition{
			Type:               string(conditionType),
			Status:             conditionStatus,
			Reason:             reason,
			Message:            message,
			ObservedGeneration: generation,
			LastTransitionTime: now,
		})
	}
	set(corev1alpha1.MemoryBackendConditionAccepted, state.accepted, "SpecAccepted", "SpecRejected", "backend spec is accepted", status.Message)
	set(corev1alpha1.MemoryBackendConditionProtected, state.protected, "FinalizerPersisted", "FinalizerMissing", "backend protection finalizer is persisted", status.Message)
	set(corev1alpha1.MemoryBackendConditionResolvedRefs, state.resolved, "ReferencesResolved", "ReferencesUnresolved", "namespace and Secret bindings are freshly resolved", status.Message)
	set(corev1alpha1.MemoryBackendConditionConnected, state.connected, "OMSProbesPassed", "OMSProbesFailed", "strict OMS probes passed", status.Message)
	set(corev1alpha1.MemoryBackendConditionCapabilitiesCompatible, state.capabilitiesCompatible, "CapabilitiesCompatible", "CapabilitiesIncompatible", "required OMS capabilities are present", status.Message)
	set(corev1alpha1.MemoryBackendConditionFresh, state.fresh, "ValidationFresh", "ValidationExpired", "backend validation is fresh", status.Message)
	set(corev1alpha1.MemoryBackendConditionReady, state.ready, "Ready", "NotReady", "backend is ready for its requested lifecycle", status.Message)
	set(corev1alpha1.MemoryBackendConditionDeletionSafe, state.deletionSafe, "NotDeleting", "DeletionBlocked", "backend is not awaiting deletion", status.Message)
}

func expireMemoryBackendValidation(status *corev1alpha1.MemoryBackendStatus, now time.Time) {
	expires := metav1.NewTime(now.UTC())
	status.ValidationExpiresAt = &expires
	status.Ready = false
	status.Connected = false
}

func validateMemoryBackendSpec(backend *corev1alpha1.MemoryBackend) error {
	if backend == nil {
		return fmt.Errorf("MemoryBackend is required")
	}
	if backend.Name != corev1alpha1.MemoryBackendDefaultName {
		return fmt.Errorf("MemoryBackend metadata.name must be default")
	}
	if strings.TrimSpace(backend.Namespace) == "" {
		return fmt.Errorf("MemoryBackend namespace is required")
	}
	if backend.Spec.Protocol.OMSVersion != corev1alpha1.MemoryBackendOMSVersionV01 ||
		backend.Spec.Protocol.Profile != corev1alpha1.MemoryBackendProfileV0Alpha1 {
		return fmt.Errorf("unsupported OMS protocol/profile")
	}
	if backend.Spec.Deployment.Mode != corev1alpha1.MemoryBackendDeploymentModeExternalEndpoint {
		return fmt.Errorf("deployment mode must be external-endpoint")
	}
	if strings.TrimSpace(backend.Spec.Deployment.Endpoint) == "" || len(backend.Spec.Deployment.Endpoint) > 2048 {
		return fmt.Errorf("deployment endpoint is required and bounded")
	}
	secretName := strings.TrimSpace(backend.Spec.ClientAuth.BearerTokenSecretRef.Name)
	secretKey := strings.TrimSpace(backend.Spec.ClientAuth.BearerTokenSecretRef.Key)
	if secretName == "" || secretKey == "" || len(utilvalidation.IsDNS1123Subdomain(secretName)) != 0 ||
		len(utilvalidation.IsConfigMapKey(secretKey)) != 0 {
		return fmt.Errorf("bearer token Secret name or key is invalid")
	}
	storeName := strings.TrimSpace(backend.Spec.Store.Name)
	if storeName == "" || len(storeName) > 128 || !memoryBackendStoreNamePattern.MatchString(storeName) {
		return fmt.Errorf("store name is invalid")
	}
	switch backend.Spec.RequestedLifecycle() {
	case corev1alpha1.MemoryBackendLifecycleStaged,
		corev1alpha1.MemoryBackendLifecycleActive,
		corev1alpha1.MemoryBackendLifecycleReadOnly,
		corev1alpha1.MemoryBackendLifecycleDisabled,
		corev1alpha1.MemoryBackendLifecycleDecommissioning:
		return nil
	default:
		return fmt.Errorf("lifecycleState is unsupported")
	}
}

func memoryBackendSpecDigest(spec corev1alpha1.MemoryBackendSpec) (string, error) {
	spec.LifecycleState = spec.RequestedLifecycle()
	encoded, err := json.Marshal(spec)
	if err != nil {
		return "", err
	}
	return memoryBackendDigestBytes(encoded), nil
}

func memoryBackendDigest(value string) string {
	return memoryBackendDigestBytes([]byte(strings.TrimSpace(value)))
}

func memoryBackendDigestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return fmt.Sprintf("sha256:%x", sum[:])
}

func deriveMemoryBackendTenantID(clusterID string, namespaceUID types.UID) string {
	return omsprotocol.DeriveTenantID(strings.TrimSpace(clusterID), string(namespaceUID))
}

type memoryBackendValidationCandidateSnapshot struct {
	Version                 string                                   `json:"version"`
	Namespace               string                                   `json:"namespace"`
	NamespaceUID            string                                   `json:"namespaceUid"`
	BackendUID              string                                   `json:"backendUid"`
	BackendGeneration       int64                                    `json:"backendGeneration"`
	RequestedLifecycle      corev1alpha1.MemoryBackendLifecycleState `json:"requestedLifecycle"`
	LifecycleIntentDigest   string                                   `json:"lifecycleIntentDigest"`
	SpecDigest              string                                   `json:"specDigest"`
	EndpointIdentity        string                                   `json:"endpointIdentity"`
	EndpointDigest          string                                   `json:"endpointDigest"`
	ResolvedAddressDigest   string                                   `json:"resolvedAddressDigest"`
	ServerCertificateDigest string                                   `json:"serverCertificateDigest"`
	SecretName              string                                   `json:"secretName"`
	SecretKey               string                                   `json:"secretKey"`
	SecretUID               string                                   `json:"secretUid"`
	SecretResourceVersion   string                                   `json:"secretResourceVersion"`
	TenantID                string                                   `json:"tenantId"`
	StoreName               string                                   `json:"storeName"`
	StoreUUID               string                                   `json:"storeUuid"`
	Protocol                corev1alpha1.MemoryBackendProfile        `json:"protocol"`
	AuthorityEpoch          int64                                    `json:"authorityEpoch"`
	RoutingEpoch            int64                                    `json:"routingEpoch"`
	CandidateDigest         string                                   `json:"candidateDigest"`
}

func decodeMemoryBackendValidationCandidate(value string) (memoryBackendValidationCandidateSnapshot, error) {
	var candidate memoryBackendValidationCandidateSnapshot
	decoder := json.NewDecoder(strings.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&candidate); err != nil {
		return memoryBackendValidationCandidateSnapshot{}, fmt.Errorf("decode validation candidate: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return memoryBackendValidationCandidateSnapshot{}, fmt.Errorf("decode validation candidate: trailing data")
	}
	if candidate.Version != "v1" || candidate.Namespace == "" || candidate.NamespaceUID == "" ||
		candidate.BackendUID == "" || candidate.BackendGeneration <= 0 ||
		!protectedMemoryBackendLifecycle(candidate.RequestedLifecycle) ||
		candidate.LifecycleIntentDigest == "" || candidate.SpecDigest == "" ||
		candidate.EndpointIdentity == "" || candidate.EndpointDigest == "" || candidate.ResolvedAddressDigest == "" ||
		candidate.ServerCertificateDigest == "" || candidate.SecretName == "" || candidate.SecretKey == "" ||
		candidate.SecretUID == "" || candidate.SecretResourceVersion == "" || candidate.TenantID == "" ||
		candidate.StoreName == "" || candidate.StoreUUID == "" || candidate.Protocol == "" ||
		candidate.AuthorityEpoch <= 0 || candidate.RoutingEpoch <= 0 || candidate.CandidateDigest == "" {
		return memoryBackendValidationCandidateSnapshot{}, fmt.Errorf("validation candidate identity is incomplete")
	}
	return candidate, nil
}

func encodeMemoryBackendValidationCandidate(candidate memoryBackendValidationCandidateSnapshot) (string, error) {
	encoded, err := json.Marshal(candidate)
	if err != nil {
		return "", err
	}
	if len(encoded) > 16<<10 {
		return "", fmt.Errorf("validation candidate snapshot exceeds 16 KiB")
	}
	return string(encoded), nil
}

func validationCandidateMatchesCurrent(
	candidate memoryBackendValidationCandidateSnapshot,
	backend *corev1alpha1.MemoryBackend,
	specDigest string,
) bool {
	if backend == nil {
		return false
	}
	return candidate.Namespace == backend.Namespace && candidate.BackendUID == string(backend.UID) &&
		candidate.BackendGeneration == backend.Generation && candidate.RequestedLifecycle == backend.Spec.RequestedLifecycle() &&
		candidate.LifecycleIntentDigest == strings.TrimSpace(backend.Annotations[corev1alpha1.MemoryBackendLifecycleIntentAnnotation]) &&
		candidate.SpecDigest == specDigest && candidate.StoreName == backend.Spec.Store.Name &&
		candidate.Protocol == backend.Spec.Protocol.Profile &&
		candidate.SecretName == strings.TrimSpace(backend.Spec.ClientAuth.BearerTokenSecretRef.Name) &&
		candidate.SecretKey == strings.TrimSpace(backend.Spec.ClientAuth.BearerTokenSecretRef.Key)
}

func validationCandidateMatchesResolvedRoute(
	candidate memoryBackendValidationCandidateSnapshot,
	resolution endpointpolicy.Resolution,
	secret memoryBackendSecret,
	storeProbe MemoryBackendStoreProbeResult,
	tenantID string,
) bool {
	return candidate.EndpointIdentity == resolution.Identity && candidate.EndpointDigest == resolution.EndpointDigest &&
		candidate.ResolvedAddressDigest == resolution.ResolvedAddressDigest &&
		candidate.ServerCertificateDigest == storeProbe.ServerCertificateDigest &&
		candidate.SecretName == secret.Name && candidate.SecretKey == secret.Key && candidate.SecretUID == string(secret.UID) &&
		candidate.SecretResourceVersion == secret.ResourceVersion && candidate.TenantID == tenantID &&
		candidate.StoreUUID == storeProbe.StoreUUID
}

type memoryBackendSecret struct {
	Name            string
	Key             string
	Token           string
	UID             types.UID
	ResourceVersion string
}

type memoryBackendFreshCandidateRoute struct {
	Backend    *corev1alpha1.MemoryBackend
	Namespace  *corev1.Namespace
	Resolution endpointpolicy.Resolution
	Secret     memoryBackendSecret
	Store      MemoryBackendStoreProbeResult
}

func memoryBackendProbeTargetFromFreshCandidate(
	fresh memoryBackendFreshCandidateRoute,
	candidate memoryBackendValidationCandidateSnapshot,
	clusterID string,
	probeTimeout time.Duration,
) MemoryBackendProbeTarget {
	return MemoryBackendProbeTarget{
		Endpoint: fresh.Resolution.BaseURL, EndpointIdentity: fresh.Resolution.Identity,
		EndpointDigest: fresh.Resolution.EndpointDigest, ResolvedAddressDigest: fresh.Resolution.ResolvedAddressDigest,
		ResolvedAddresses: fresh.Resolution.Addresses, BearerToken: fresh.Secret.Token,
		Profile: candidate.Protocol, ClusterID: clusterID, NamespaceUID: candidate.NamespaceUID,
		BackendUID: candidate.BackendUID, TenantID: candidate.TenantID, StoreUUID: candidate.StoreUUID,
		AuthorityEpoch: candidate.AuthorityEpoch, RoutingEpoch: candidate.RoutingEpoch, Timeout: probeTimeout,
	}
}

func (r *MemoryBackendReconciler) persistMemoryBackendValidationCandidate(
	ctx context.Context,
	backend *corev1alpha1.MemoryBackend,
	candidate memoryBackendValidationCandidateSnapshot,
) (*corev1alpha1.MemoryBackend, error) {
	if r.APIReader == nil {
		return nil, fmt.Errorf("uncached Kubernetes API reader is not configured")
	}
	encoded, err := encodeMemoryBackendValidationCandidate(candidate)
	if err != nil {
		return nil, err
	}
	fresh := &corev1alpha1.MemoryBackend{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(backend), fresh); err != nil {
		return nil, err
	}
	if fresh.UID != backend.UID || fresh.Generation != backend.Generation || !fresh.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("MemoryBackend changed before validation candidate persistence")
	}
	specDigest, err := memoryBackendSpecDigest(fresh.Spec)
	if err != nil || !validationCandidateMatchesCurrent(candidate, fresh, specDigest) {
		return nil, fmt.Errorf("MemoryBackend spec changed before validation candidate persistence")
	}
	if fresh.Annotations == nil {
		fresh.Annotations = make(map[string]string)
	}
	if current := strings.TrimSpace(fresh.Annotations[corev1alpha1.MemoryBackendValidationCandidateAnnotation]); current != "" && current != encoded {
		return nil, fmt.Errorf("a different durable validation candidate must be retired before replacement")
	}
	if fresh.Annotations[corev1alpha1.MemoryBackendValidationCandidateAnnotation] == encoded {
		return fresh, nil
	}
	fresh.Annotations[corev1alpha1.MemoryBackendValidationCandidateAnnotation] = encoded
	if err := r.Update(ctx, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

func (r *MemoryBackendReconciler) clearMemoryBackendValidationCandidate(
	ctx context.Context,
	backend *corev1alpha1.MemoryBackend,
	candidateDigest string,
) (*corev1alpha1.MemoryBackend, error) {
	if r.APIReader == nil {
		return nil, fmt.Errorf("uncached Kubernetes API reader is not configured")
	}
	fresh := &corev1alpha1.MemoryBackend{}
	if err := r.APIReader.Get(ctx, client.ObjectKeyFromObject(backend), fresh); err != nil {
		return nil, err
	}
	raw := strings.TrimSpace(fresh.Annotations[corev1alpha1.MemoryBackendValidationCandidateAnnotation])
	if raw == "" {
		return fresh, nil
	}
	candidate, err := decodeMemoryBackendValidationCandidate(raw)
	if err != nil {
		return nil, err
	}
	if candidate.CandidateDigest != candidateDigest {
		return nil, fmt.Errorf("validation candidate changed before retirement")
	}
	delete(fresh.Annotations, corev1alpha1.MemoryBackendValidationCandidateAnnotation)
	if err := r.Update(ctx, fresh); err != nil {
		return nil, err
	}
	return fresh, nil
}

func (r *MemoryBackendReconciler) revalidateMemoryBackendCandidateRoute(
	ctx context.Context,
	candidate memoryBackendValidationCandidateSnapshot,
	prober MemoryBackendOMSProber,
	probeTimeout time.Duration,
	allowDeleting bool,
) (memoryBackendFreshCandidateRoute, error) {
	if r.APIReader == nil {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("uncached Kubernetes API reader is not configured")
	}
	fresh := &corev1alpha1.MemoryBackend{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: candidate.Namespace, Name: corev1alpha1.MemoryBackendDefaultName}, fresh); err != nil {
		return memoryBackendFreshCandidateRoute{}, err
	}
	if fresh.UID != types.UID(candidate.BackendUID) || fresh.Generation != candidate.BackendGeneration ||
		(!allowDeleting && !fresh.DeletionTimestamp.IsZero()) {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("MemoryBackend identity, generation, or deletion state changed")
	}
	specDigest, err := memoryBackendSpecDigest(fresh.Spec)
	if err != nil || !validationCandidateMatchesCurrent(candidate, fresh, specDigest) {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("MemoryBackend spec or lifecycle intent changed")
	}
	raw := strings.TrimSpace(fresh.Annotations[corev1alpha1.MemoryBackendValidationCandidateAnnotation])
	persisted, err := decodeMemoryBackendValidationCandidate(raw)
	if err != nil || !reflect.DeepEqual(persisted, candidate) {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("durable validation candidate changed")
	}
	namespace := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: candidate.Namespace}, namespace); err != nil {
		return memoryBackendFreshCandidateRoute{}, err
	}
	if string(namespace.UID) != candidate.NamespaceUID || !namespace.DeletionTimestamp.IsZero() {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("namespace identity or termination state changed")
	}
	resolution, err := r.resolveMemoryBackendEndpoint(ctx, fresh.Spec.Deployment.Endpoint, probeTimeout)
	if err != nil {
		return memoryBackendFreshCandidateRoute{}, err
	}
	if resolution.Identity != candidate.EndpointIdentity || resolution.EndpointDigest != candidate.EndpointDigest ||
		resolution.ResolvedAddressDigest != candidate.ResolvedAddressDigest {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("endpoint or resolved network identity changed")
	}
	tenantID := deriveMemoryBackendTenantID(r.ClusterIdentity, namespace.UID)
	if tenantID != candidate.TenantID {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("tenant identity changed")
	}
	secret, err := readMemoryBackendBearerSecret(ctx, r.APIReader, fresh, namespace.UID, tenantID, resolution.Identity)
	if err != nil {
		return memoryBackendFreshCandidateRoute{}, err
	}
	if secret.Name != candidate.SecretName || secret.Key != candidate.SecretKey || string(secret.UID) != candidate.SecretUID ||
		secret.ResourceVersion != candidate.SecretResourceVersion {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("bound Secret identity or resourceVersion changed")
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	storeProbe, err := prober.ResolveStore(probeCtx, MemoryBackendStoreProbeTarget{
		Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses: resolution.Addresses, BearerToken: secret.Token, Profile: fresh.Spec.Protocol.Profile,
		ClusterID: r.ClusterIdentity, NamespaceUID: candidate.NamespaceUID, BackendUID: candidate.BackendUID,
		TenantID: tenantID, StoreName: candidate.StoreName, Timeout: probeTimeout,
	})
	if err != nil {
		return memoryBackendFreshCandidateRoute{}, err
	}
	if storeProbe.StoreUUID != candidate.StoreUUID || storeProbe.ServerCertificateDigest != candidate.ServerCertificateDigest {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("store UUID or server certificate identity changed")
	}
	return memoryBackendFreshCandidateRoute{
		Backend: fresh, Namespace: namespace, Resolution: resolution, Secret: secret, Store: storeProbe,
	}, nil
}

func (r *MemoryBackendReconciler) revalidateRetiringMemoryBackendCandidateRoute(
	ctx context.Context,
	candidate memoryBackendValidationCandidateSnapshot,
	prober MemoryBackendOMSProber,
	probeTimeout time.Duration,
) (memoryBackendFreshCandidateRoute, error) {
	if r.APIReader == nil {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("uncached Kubernetes API reader is not configured")
	}
	fresh := &corev1alpha1.MemoryBackend{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: candidate.Namespace, Name: corev1alpha1.MemoryBackendDefaultName}, fresh); err != nil {
		return memoryBackendFreshCandidateRoute{}, err
	}
	if fresh.UID != types.UID(candidate.BackendUID) {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("MemoryBackend identity changed")
	}
	raw := strings.TrimSpace(fresh.Annotations[corev1alpha1.MemoryBackendValidationCandidateAnnotation])
	persisted, err := decodeMemoryBackendValidationCandidate(raw)
	if err != nil || !reflect.DeepEqual(persisted, candidate) {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("durable validation candidate changed")
	}
	namespace := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Name: candidate.Namespace}, namespace); err != nil {
		return memoryBackendFreshCandidateRoute{}, err
	}
	if string(namespace.UID) != candidate.NamespaceUID {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("namespace identity changed")
	}
	resolution, err := r.resolveMemoryBackendEndpoint(ctx, candidate.EndpointIdentity, probeTimeout)
	if err != nil {
		return memoryBackendFreshCandidateRoute{}, err
	}
	if resolution.Identity != candidate.EndpointIdentity || resolution.EndpointDigest != candidate.EndpointDigest ||
		resolution.ResolvedAddressDigest != candidate.ResolvedAddressDigest {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("persisted candidate endpoint or resolved network identity changed")
	}
	tenantID := deriveMemoryBackendTenantID(r.ClusterIdentity, namespace.UID)
	if tenantID != candidate.TenantID {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("tenant identity changed")
	}
	candidateBackend := fresh.DeepCopy()
	candidateBackend.Spec.Protocol.Profile = candidate.Protocol
	candidateBackend.Spec.Deployment.Endpoint = candidate.EndpointIdentity
	candidateBackend.Spec.ClientAuth.BearerTokenSecretRef.Name = candidate.SecretName
	candidateBackend.Spec.ClientAuth.BearerTokenSecretRef.Key = candidate.SecretKey
	candidateBackend.Spec.Store.Name = candidate.StoreName
	secret, err := readMemoryBackendBearerSecret(ctx, r.APIReader, candidateBackend, namespace.UID, tenantID, resolution.Identity)
	if err != nil {
		return memoryBackendFreshCandidateRoute{}, err
	}
	if secret.Name != candidate.SecretName || secret.Key != candidate.SecretKey || string(secret.UID) != candidate.SecretUID ||
		secret.ResourceVersion != candidate.SecretResourceVersion {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("persisted candidate Secret identity or resourceVersion changed")
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	storeProbe, err := prober.ResolveStore(probeCtx, MemoryBackendStoreProbeTarget{
		Endpoint: resolution.BaseURL, EndpointIdentity: resolution.Identity,
		EndpointDigest: resolution.EndpointDigest, ResolvedAddressDigest: resolution.ResolvedAddressDigest,
		ResolvedAddresses: resolution.Addresses, BearerToken: secret.Token, Profile: candidate.Protocol,
		ClusterID: r.ClusterIdentity, NamespaceUID: candidate.NamespaceUID, BackendUID: candidate.BackendUID,
		TenantID: tenantID, StoreName: candidate.StoreName, Timeout: probeTimeout,
	})
	if err != nil {
		return memoryBackendFreshCandidateRoute{}, err
	}
	if storeProbe.StoreUUID != candidate.StoreUUID || storeProbe.ServerCertificateDigest != candidate.ServerCertificateDigest {
		return memoryBackendFreshCandidateRoute{}, fmt.Errorf("persisted candidate store UUID or server certificate identity changed")
	}
	return memoryBackendFreshCandidateRoute{
		Backend: fresh, Namespace: namespace, Resolution: resolution, Secret: secret, Store: storeProbe,
	}, nil
}

func (r *MemoryBackendReconciler) fenceAndRetireMemoryBackendValidationCandidate(
	ctx context.Context,
	backend *corev1alpha1.MemoryBackend,
	candidate memoryBackendValidationCandidateSnapshot,
) error {
	if r.BindingCoordinator == nil {
		return fmt.Errorf("durable backend coordinator is not configured")
	}
	probeTimeout := r.probeTimeout()
	prober := r.OMSProber
	if prober == nil {
		prober = &OMSHTTPProber{Policy: r.EndpointPolicy}
	}
	fresh, err := r.revalidateRetiringMemoryBackendCandidateRoute(ctx, candidate, prober, probeTimeout)
	if err != nil {
		return err
	}
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	fence, err := prober.AdvanceRoutingFence(probeCtx, MemoryBackendRoutingFenceTarget{
		Endpoint: fresh.Resolution.BaseURL, EndpointIdentity: fresh.Resolution.Identity,
		EndpointDigest: fresh.Resolution.EndpointDigest, ResolvedAddressDigest: fresh.Resolution.ResolvedAddressDigest,
		ResolvedAddresses:               fresh.Resolution.Addresses,
		ExpectedServerCertificateDigest: candidate.ServerCertificateDigest,
		BearerToken:                     fresh.Secret.Token, Profile: candidate.Protocol, ClusterID: r.ClusterIdentity,
		NamespaceUID: candidate.NamespaceUID, BackendUID: candidate.BackendUID, TenantID: candidate.TenantID,
		StoreUUID: candidate.StoreUUID, AuthorityEpoch: candidate.AuthorityEpoch,
		RoutingEpoch: candidate.RoutingEpoch, Timeout: probeTimeout,
	})
	if err != nil {
		return err
	}
	if fence.MaximumRoutingEpoch != candidate.RoutingEpoch || fence.ServerCertificateDigest != candidate.ServerCertificateDigest {
		return fmt.Errorf("OMS routing fence acknowledgement did not match validation candidate")
	}
	if err := r.BindingCoordinator.RetireMemoryBackendValidationCandidate(ctx, MemoryBackendValidationCandidateRetirement{
		Namespace: candidate.Namespace, NamespaceUID: candidate.NamespaceUID, BackendUID: candidate.BackendUID,
		AuthorityEpoch: candidate.AuthorityEpoch, RoutingEpoch: candidate.RoutingEpoch,
		CandidateDigest: candidate.CandidateDigest, RemoteFenceAcknowledged: true,
	}); err != nil {
		return err
	}
	_, err = r.clearMemoryBackendValidationCandidate(ctx, backend, candidate.CandidateDigest)
	return err
}

func readMemoryBackendBearerSecret(
	ctx context.Context,
	reader client.Reader,
	backend *corev1alpha1.MemoryBackend,
	namespaceUID types.UID,
	tenantID, endpointIdentity string,
) (memoryBackendSecret, error) {
	secret := &corev1.Secret{}
	name := strings.TrimSpace(backend.Spec.ClientAuth.BearerTokenSecretRef.Name)
	if err := reader.Get(ctx, client.ObjectKey{Namespace: backend.Namespace, Name: name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return memoryBackendSecret{}, fmt.Errorf("bound bearer Secret was not found")
		}
		return memoryBackendSecret{}, fmt.Errorf("bound bearer Secret could not be read")
	}
	if secret.Namespace != backend.Namespace || secret.UID == "" || strings.TrimSpace(secret.ResourceVersion) == "" {
		return memoryBackendSecret{}, fmt.Errorf("bound bearer Secret identity metadata is incomplete")
	}
	if secret.Labels[MemoryBackendClientAuthLabel] != MemoryBackendClientAuthEnabledValue {
		return memoryBackendSecret{}, fmt.Errorf("bound bearer Secret must opt in for MemoryBackend client authentication")
	}
	expectedAnnotations := map[string]string{
		MemoryBackendAuthBackendUIDAnnotation:   string(backend.UID),
		MemoryBackendAuthEndpointAnnotation:     endpointIdentity,
		MemoryBackendAuthStoreNameAnnotation:    backend.Spec.Store.Name,
		MemoryBackendAuthNamespaceUIDAnnotation: string(namespaceUID),
		MemoryBackendAuthTenantIDAnnotation:     tenantID,
	}
	for key, expected := range expectedAnnotations {
		if secret.Annotations[key] != expected {
			return memoryBackendSecret{}, fmt.Errorf("bound bearer Secret annotation %s does not match the backend identity", key)
		}
	}
	key := strings.TrimSpace(backend.Spec.ClientAuth.BearerTokenSecretRef.Key)
	token := strings.TrimSpace(string(secret.Data[key]))
	if token == "" || len(token) > 4096 || strings.ContainsFunc(token, unicode.IsSpace) || strings.ContainsFunc(token, unicode.IsControl) {
		return memoryBackendSecret{}, fmt.Errorf("bound bearer Secret token key is empty or invalid")
	}
	return memoryBackendSecret{Name: name, Key: key, Token: token, UID: secret.UID, ResourceVersion: secret.ResourceVersion}, nil
}

func validateMemoryBackendStoreProbeResult(result MemoryBackendStoreProbeResult) error {
	if !memoryBackendStoreUUIDPattern.MatchString(result.StoreUUID) {
		return fmt.Errorf("OMS store UUID is invalid")
	}
	if !memoryBackendDigestPattern.MatchString(result.ServerCertificateDigest) {
		return fmt.Errorf("OMS store probe TLS identity is invalid")
	}
	return nil
}

func validateMemoryBackendProbeResult(result MemoryBackendProbeResult, now time.Time) error {
	if err := validateMemoryBackendCapabilityProbeResult(result, now); err != nil {
		return err
	}
	if !memoryBackendSafeIdentityPattern.MatchString(result.OwnershipClaimIdentity) {
		return fmt.Errorf("OMS ownership claim identity is invalid")
	}
	return nil
}

func validateMemoryBackendCapabilityProbeResult(result MemoryBackendProbeResult, now time.Time) error {
	if !memoryBackendProtocolIDPattern.MatchString(result.AdapterName) ||
		!memoryBackendProtocolIDPattern.MatchString(result.AdapterVersion) {
		return fmt.Errorf("OMS adapter identity is invalid")
	}
	if len(result.CapabilityRevision) == 0 || len(result.CapabilityRevision) > 128 ||
		!memoryBackendProtocolIDPattern.MatchString(result.CapabilityRevision) {
		return fmt.Errorf("OMS capability revision is invalid")
	}
	if !result.CapabilityExpiresAt.After(now) {
		return fmt.Errorf("OMS capability response is already expired")
	}
	if !memoryBackendDigestPattern.MatchString(result.ServerCertificateDigest) {
		return fmt.Errorf("OMS server certificate identity is invalid")
	}
	if err := validateMemoryBackendCapabilityLimits(result.CapabilityLimits); err != nil {
		return err
	}
	seen := make(map[corev1alpha1.MemoryBackendCapability]struct{}, len(result.Capabilities))
	for _, capability := range result.Capabilities {
		if !knownMemoryBackendCapability(capability) {
			return fmt.Errorf("OMS advertised an unknown capability")
		}
		if _, duplicate := seen[capability]; duplicate {
			return fmt.Errorf("OMS advertised a duplicate capability")
		}
		seen[capability] = struct{}{}
	}
	for _, required := range requiredMemoryBackendCapabilities {
		if _, exists := seen[required]; !exists {
			return fmt.Errorf("OMS is missing required capability %s", required)
		}
	}
	return nil
}

func validateMemoryBackendCapabilityLimits(limits corev1alpha1.MemoryBackendCapabilityLimits) error {
	if limits.MaxRequestBytes < 1 || limits.MaxRequestBytes > omsprotocol.MaxHTTPBodyBytes ||
		limits.MaxResponseBytes < 1 || limits.MaxResponseBytes > omsprotocol.MaxAdapterResponseBytes ||
		limits.MaxContentBytes < 1 || limits.MaxContentBytes > omsprotocol.MaxContentBytes ||
		limits.MaxTags < 1 || limits.MaxTags > omsprotocol.MaxTags ||
		limits.MaxTagBytes < 1 || limits.MaxTagBytes > 128 ||
		limits.MaxMetadataEntries < 1 || limits.MaxMetadataEntries > 32 ||
		limits.MaxMetadataKeyBytes < 1 || limits.MaxMetadataKeyBytes > 64 ||
		limits.MaxMetadataValueBytes < 1 || limits.MaxMetadataValueBytes > 1024 ||
		limits.MaxQueryBytes < 1 || limits.MaxQueryBytes > 1024 ||
		limits.MaxPageSize < 1 || limits.MaxPageSize > 8 ||
		limits.MaxSnapshotRecords < 1 || limits.MaxSnapshotRecords > 1024 ||
		limits.SnapshotTTLSeconds < 1 || limits.SnapshotTTLSeconds > omsprotocol.MaxSnapshotTTLSeconds {
		return fmt.Errorf("OMS capability limits are invalid")
	}
	return nil
}

func knownMemoryBackendCapability(capability corev1alpha1.MemoryBackendCapability) bool {
	switch capability {
	case corev1alpha1.MemoryBackendCapabilityDurableIdempotency,
		corev1alpha1.MemoryBackendCapabilityIdempotencyDigestConflicts,
		corev1alpha1.MemoryBackendCapabilityCreateIfAbsent,
		corev1alpha1.MemoryBackendCapabilityConditionalMutation,
		corev1alpha1.MemoryBackendCapabilityMonotonicGenerations,
		corev1alpha1.MemoryBackendCapabilityDeleteHighWatermark,
		corev1alpha1.MemoryBackendCapabilityDurableRoutingFence,
		corev1alpha1.MemoryBackendCapabilityOperationLookup,
		corev1alpha1.MemoryBackendCapabilityExactGet,
		corev1alpha1.MemoryBackendCapabilityStablePagination,
		corev1alpha1.MemoryBackendCapabilityExclusiveOwnership,
		corev1alpha1.MemoryBackendCapabilityKeywordSearch,
		corev1alpha1.MemoryBackendCapabilityAuditVersionVisibility,
		corev1alpha1.MemoryBackendCapabilitySemanticSearch,
		corev1alpha1.MemoryBackendCapabilityHybridSearch:
		return true
	default:
		return false
	}
}

func sortedMemoryBackendCapabilities(values []corev1alpha1.MemoryBackendCapability) []corev1alpha1.MemoryBackendCapability {
	result := append([]corev1alpha1.MemoryBackendCapability(nil), values...)
	slices.Sort(result)
	return result
}

func knownMemoryBackendEffectiveLifecycle(value corev1alpha1.MemoryBackendEffectiveLifecycleState) bool {
	switch value {
	case corev1alpha1.MemoryBackendEffectiveLifecycleStaged,
		corev1alpha1.MemoryBackendEffectiveLifecycleValidating,
		corev1alpha1.MemoryBackendEffectiveLifecycleActive,
		corev1alpha1.MemoryBackendEffectiveLifecycleReadOnly,
		corev1alpha1.MemoryBackendEffectiveLifecycleDisabled,
		corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioning,
		corev1alpha1.MemoryBackendEffectiveLifecycleDraining,
		corev1alpha1.MemoryBackendEffectiveLifecycleRecovering,
		corev1alpha1.MemoryBackendEffectiveLifecycleIdentityMismatch,
		corev1alpha1.MemoryBackendEffectiveLifecycleIdentityConflict,
		corev1alpha1.MemoryBackendEffectiveLifecycleDiverged,
		corev1alpha1.MemoryBackendEffectiveLifecycleRemoved,
		corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned:
		return true
	default:
		return false
	}
}

func validateMemoryBackendBindingResult(
	requested corev1alpha1.MemoryBackendLifecycleState,
	result MemoryBackendBindingResult,
) error {
	if result.AuthorityEpoch < 0 || result.RoutingEpoch < 0 || !knownMemoryBackendEffectiveLifecycle(result.EffectiveLifecycleState) {
		return fmt.Errorf("durable binding result contains invalid epochs or lifecycle")
	}
	if result.Ready {
		if result.AuthorityEpoch < 1 || result.RoutingEpoch < 1 {
			return fmt.Errorf("ready durable binding result must have positive epochs")
		}
		switch requested {
		case corev1alpha1.MemoryBackendLifecycleActive:
			if result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleActive {
				return fmt.Errorf("active readiness requires an effective Active binding")
			}
		case corev1alpha1.MemoryBackendLifecycleReadOnly:
			if result.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleReadOnly {
				return fmt.Errorf("read-only readiness requires an effective ReadOnly binding")
			}
		default:
			return fmt.Errorf("requested lifecycle cannot be ready through the binding coordinator")
		}
	}
	return nil
}

func memoryBackendProbeFailure(err error) (string, string) {
	if failure, ok := err.(*omsProbeError); ok {
		return sanitizeMemoryBackendReason(failure.reason, "OMSProbeFailed"), sanitizeMemoryBackendMessage(failure.message)
	}
	return "OMSProbeFailed", "strict OMS validation probe failed"
}

func sanitizeMemoryBackendReason(reason, fallback string) string {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 64 || !isASCIIAlpha(reason[0]) {
		return fallback
	}
	for i := range len(reason) {
		if !isASCIIAlphaNumeric(reason[i]) {
			return fallback
		}
	}
	return reason
}

func isASCIIAlpha(value byte) bool {
	return value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z'
}

func isASCIIAlphaNumeric(value byte) bool {
	return isASCIIAlpha(value) || value >= '0' && value <= '9'
}

func sanitizeMemoryBackendMessage(message string) string {
	message = events.RedactExecutionEventText(strings.TrimSpace(message))
	if len(message) > 1024 {
		message = message[:1024]
		for len(message) > 0 && !utf8.ValidString(message) {
			message = message[:len(message)-1]
		}
	}
	return message
}

func memoryBackendRequeueAfter(now, expiry time.Time) time.Duration {
	remaining := expiry.Sub(now)
	if remaining <= 0 {
		return time.Second
	}
	candidate := remaining / 2
	candidate = max(candidate, time.Second)
	return min(candidate, defaultMemoryBackendRequeueInterval)
}

// --- Strict OMS HTTP probe implementation ---

type omsProbeError struct {
	reason  string
	message string
}

func (e *omsProbeError) Error() string { return e.message }

func newOMSProbeError(reason, message string) error {
	return &omsProbeError{reason: reason, message: message}
}

// ResolveStore performs the exact pre-authority /v1/stores/resolve exchange.
func (p *OMSHTTPProber) ResolveStore(
	ctx context.Context,
	target MemoryBackendStoreProbeTarget,
) (MemoryBackendStoreProbeResult, error) {
	if p == nil || strings.TrimSpace(target.BearerToken) == "" || string(target.Profile) != omsprotocol.Version {
		return MemoryBackendStoreProbeResult{}, newOMSProbeError("OMSProbeConfigurationInvalid", "OMS store probe configuration is incomplete")
	}
	httpClient, err := p.httpClient(target.Timeout, endpointpolicy.Resolution{
		BaseURL: target.Endpoint, Identity: target.EndpointIdentity, EndpointDigest: target.EndpointDigest,
		ResolvedAddressDigest: target.ResolvedAddressDigest, Addresses: target.ResolvedAddresses,
	})
	if err != nil {
		return MemoryBackendStoreProbeResult{}, err
	}
	if transport, ok := httpClient.Transport.(*http.Transport); ok {
		defer transport.CloseIdleConnections()
	}
	binding := omsprotocol.StoreResolutionBinding{
		ClusterID: target.ClusterID, NamespaceUID: target.NamespaceUID,
		BackendUID: target.BackendUID, TenantID: target.TenantID,
	}
	requestValue := omsprotocol.StoreResolveRequest{
		ProtocolVersion: omsprotocol.Version,
		Binding:         binding,
		StoreName:       target.StoreName,
	}
	if err := omsprotocol.ValidateStoreResolveRequest(&requestValue); err != nil {
		return MemoryBackendStoreProbeResult{}, newOMSProbeError("OMSStoreProbeRequestInvalid", "OMS store probe identity was invalid")
	}
	payload, certificate, _, err := doOMSProbeRaw(
		ctx, httpClient, target.Endpoint, omsprotocol.PathStoreResolve, target.BearerToken, requestValue, http.StatusOK,
	)
	if err != nil {
		return MemoryBackendStoreProbeResult{}, err
	}
	response, err := omsprotocol.DecodeStoreResolveResponse(payload)
	if err != nil {
		return MemoryBackendStoreProbeResult{}, newOMSProbeError("OMSStoreProbeResponseInvalid", "OMS store probe response failed strict OMS decoding")
	}
	if !omsprotocol.StoreResolutionBindingEqual(response.Binding, binding) || response.StoreName != target.StoreName {
		return MemoryBackendStoreProbeResult{}, newOMSProbeError("OMSStoreIdentityMismatch", "OMS store probe returned mismatched identity")
	}
	return MemoryBackendStoreProbeResult{StoreUUID: response.StoreUUID, ServerCertificateDigest: certificate}, nil
}

// ProbeCapabilities performs the non-mutating /v1/capabilities exchange used by Staged validation.
func (p *OMSHTTPProber) ProbeCapabilities(
	ctx context.Context,
	target MemoryBackendProbeTarget,
) (MemoryBackendProbeResult, error) {
	if p == nil || strings.TrimSpace(target.BearerToken) == "" || string(target.Profile) != omsprotocol.Version ||
		target.AuthorityEpoch < 1 || target.RoutingEpoch < 1 {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSProbeConfigurationInvalid", "OMS capability probe configuration is incomplete")
	}
	httpClient, err := p.httpClient(target.Timeout, endpointpolicy.Resolution{
		BaseURL: target.Endpoint, Identity: target.EndpointIdentity, EndpointDigest: target.EndpointDigest,
		ResolvedAddressDigest: target.ResolvedAddressDigest, Addresses: target.ResolvedAddresses,
	})
	if err != nil {
		return MemoryBackendProbeResult{}, err
	}
	if transport, ok := httpClient.Transport.(*http.Transport); ok {
		defer transport.CloseIdleConnections()
	}
	binding := omsprotocol.Binding{
		ClusterID: target.ClusterID, NamespaceUID: target.NamespaceUID, BackendUID: target.BackendUID,
		AuthorityEpoch: uint64(target.AuthorityEpoch), RoutingEpoch: uint64(target.RoutingEpoch),
		TenantID: target.TenantID, StoreUUID: target.StoreUUID,
	}
	request := omsprotocol.CapabilitiesRequest{ProtocolVersion: omsprotocol.Version, Binding: binding}
	if err := omsprotocol.ValidateCapabilitiesRequest(&request); err != nil {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSCapabilityRequestInvalid", "OMS capability probe binding was invalid")
	}
	payload, certificate, _, err := doOMSProbeRaw(
		ctx, httpClient, target.Endpoint, omsprotocol.PathCapabilities, target.BearerToken, request, http.StatusOK,
	)
	if err != nil {
		return MemoryBackendProbeResult{}, err
	}
	capabilities, err := omsprotocol.DecodeCapabilitiesResponse(payload)
	if err != nil {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSCapabilityResponseInvalid", "OMS capability response failed strict OMS decoding")
	}
	if !omsprotocol.BindingEqual(capabilities.Binding, binding) {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSCapabilityIdentityMismatch", "OMS capability probe returned mismatched identity")
	}
	return MemoryBackendProbeResult{
		AdapterName: capabilities.AdapterName, AdapterVersion: capabilities.AdapterVersion,
		Capabilities: memoryBackendCapabilitiesFromOMS(capabilities.Capabilities), CapabilityRevision: capabilities.Revision,
		CapabilityLimits:    memoryBackendLimitsFromOMS(capabilities.Limits),
		CapabilityExpiresAt: capabilities.ExpiresAt.UTC(), ServerCertificateDigest: certificate,
	}, nil
}

// ProbeBinding performs exact /v1/capabilities and /v1/ownership/claim exchanges.
func (p *OMSHTTPProber) ProbeBinding(
	ctx context.Context,
	target MemoryBackendProbeTarget,
) (MemoryBackendProbeResult, error) {
	if p == nil || strings.TrimSpace(target.BearerToken) == "" || string(target.Profile) != omsprotocol.Version ||
		target.AuthorityEpoch < 1 || target.RoutingEpoch < 1 {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSProbeConfigurationInvalid", "OMS binding probe configuration is incomplete")
	}
	httpClient, err := p.httpClient(target.Timeout, endpointpolicy.Resolution{
		BaseURL: target.Endpoint, Identity: target.EndpointIdentity, EndpointDigest: target.EndpointDigest,
		ResolvedAddressDigest: target.ResolvedAddressDigest, Addresses: target.ResolvedAddresses,
	})
	if err != nil {
		return MemoryBackendProbeResult{}, err
	}
	if transport, ok := httpClient.Transport.(*http.Transport); ok {
		defer transport.CloseIdleConnections()
	}
	binding := omsprotocol.Binding{
		ClusterID: target.ClusterID, NamespaceUID: target.NamespaceUID, BackendUID: target.BackendUID,
		AuthorityEpoch: uint64(target.AuthorityEpoch), RoutingEpoch: uint64(target.RoutingEpoch),
		TenantID: target.TenantID, StoreUUID: target.StoreUUID,
	}
	capabilityRequest := omsprotocol.CapabilitiesRequest{ProtocolVersion: omsprotocol.Version, Binding: binding}
	if err := omsprotocol.ValidateCapabilitiesRequest(&capabilityRequest); err != nil {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSCapabilityRequestInvalid", "OMS capability probe binding was invalid")
	}
	payload, capabilityCertificate, _, err := doOMSProbeRaw(
		ctx, httpClient, target.Endpoint, omsprotocol.PathCapabilities, target.BearerToken, capabilityRequest, http.StatusOK,
	)
	if err != nil {
		return MemoryBackendProbeResult{}, err
	}
	capabilities, err := omsprotocol.DecodeCapabilitiesResponse(payload)
	if err != nil {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSCapabilityResponseInvalid", "OMS capability response failed strict OMS decoding")
	}
	if !omsprotocol.BindingEqual(capabilities.Binding, binding) {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSCapabilityIdentityMismatch", "OMS capability probe returned mismatched identity")
	}

	ownershipRequest := omsprotocol.OwnershipClaimRequest{ProtocolVersion: omsprotocol.Version, Binding: binding}
	if err := omsprotocol.ValidateOwnershipClaimRequest(&ownershipRequest); err != nil {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSOwnershipRequestInvalid", "OMS ownership claim binding was invalid")
	}
	payload, ownershipCertificate, statusCode, err := doOMSProbeRaw(
		ctx, httpClient, target.Endpoint, omsprotocol.PathOwnershipClaim, target.BearerToken, ownershipRequest,
		http.StatusOK, http.StatusConflict,
	)
	if err != nil {
		return MemoryBackendProbeResult{}, err
	}
	ownership, err := omsprotocol.DecodeOwnershipClaimResponse(payload)
	if err != nil {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSOwnershipResponseInvalid", "OMS ownership response failed strict OMS decoding")
	}
	if !omsprotocol.BindingEqual(ownership.Binding, binding) {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSOwnershipIdentityMismatch", "OMS ownership claim returned mismatched identity")
	}
	if statusCode == http.StatusConflict || ownership.Result == omsprotocol.ResultIdentityConflict {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSOwnershipConflict", "OMS store is owned by a different authority identity")
	}
	if ownership.Result != omsprotocol.ResultApplied || ownership.MaximumRoutingEpoch != binding.RoutingEpoch {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSOwnershipFenceMismatch", "OMS ownership claim did not preserve the candidate routing epoch")
	}
	if capabilityCertificate != ownershipCertificate {
		return MemoryBackendProbeResult{}, newOMSProbeError("OMSCertificateChanged", "OMS server certificate identity changed during validation")
	}
	return MemoryBackendProbeResult{
		AdapterName:             capabilities.AdapterName,
		AdapterVersion:          capabilities.AdapterVersion,
		OwnershipClaimIdentity:  ownership.ClaimIdentity,
		Capabilities:            memoryBackendCapabilitiesFromOMS(capabilities.Capabilities),
		CapabilityRevision:      capabilities.Revision,
		CapabilityLimits:        memoryBackendLimitsFromOMS(capabilities.Limits),
		CapabilityExpiresAt:     capabilities.ExpiresAt.UTC(),
		ServerCertificateDigest: capabilityCertificate,
	}, nil
}

// AdvanceRoutingFence performs the authenticated durable
// /v1/routing-fences/advance exchange for one exact target epoch.
func (p *OMSHTTPProber) AdvanceRoutingFence(
	ctx context.Context,
	target MemoryBackendRoutingFenceTarget,
) (MemoryBackendRoutingFenceResult, error) {
	if p == nil || strings.TrimSpace(target.BearerToken) == "" || string(target.Profile) != omsprotocol.Version ||
		target.AuthorityEpoch < 1 || target.RoutingEpoch < 1 ||
		!memoryBackendDigestPattern.MatchString(target.ExpectedServerCertificateDigest) {
		return MemoryBackendRoutingFenceResult{}, newOMSProbeError("OMSProbeConfigurationInvalid", "OMS routing fence configuration is incomplete")
	}
	httpClient, err := p.httpClient(target.Timeout, endpointpolicy.Resolution{
		BaseURL: target.Endpoint, Identity: target.EndpointIdentity, EndpointDigest: target.EndpointDigest,
		ResolvedAddressDigest: target.ResolvedAddressDigest, Addresses: target.ResolvedAddresses,
	})
	if err != nil {
		return MemoryBackendRoutingFenceResult{}, err
	}
	if transport, ok := httpClient.Transport.(*http.Transport); ok {
		defer transport.CloseIdleConnections()
	}
	binding := omsprotocol.Binding{
		ClusterID: target.ClusterID, NamespaceUID: target.NamespaceUID, BackendUID: target.BackendUID,
		AuthorityEpoch: uint64(target.AuthorityEpoch), RoutingEpoch: uint64(target.RoutingEpoch),
		TenantID: target.TenantID, StoreUUID: target.StoreUUID,
	}
	requestValue := omsprotocol.RoutingFenceRequest{ProtocolVersion: omsprotocol.Version, Binding: binding}
	if err := omsprotocol.ValidateRoutingFenceRequest(&requestValue); err != nil {
		return MemoryBackendRoutingFenceResult{}, newOMSProbeError("OMSRoutingFenceRequestInvalid", "OMS routing fence binding was invalid")
	}
	payload, certificate, statusCode, err := doOMSProbeRaw(
		ctx, httpClient, target.Endpoint, omsprotocol.PathRoutingFence, target.BearerToken, requestValue,
		http.StatusOK, http.StatusConflict,
	)
	if err != nil {
		return MemoryBackendRoutingFenceResult{}, err
	}
	response, err := omsprotocol.DecodeRoutingFenceResponse(payload)
	if err != nil {
		return MemoryBackendRoutingFenceResult{}, newOMSProbeError("OMSRoutingFenceResponseInvalid", "OMS routing fence response failed strict OMS decoding")
	}
	if !omsprotocol.BindingEqual(response.Binding, binding) {
		return MemoryBackendRoutingFenceResult{}, newOMSProbeError("OMSRoutingFenceIdentityMismatch", "OMS routing fence returned mismatched identity")
	}
	if statusCode == http.StatusConflict || response.Result != omsprotocol.ResultApplied {
		return MemoryBackendRoutingFenceResult{}, newOMSProbeError("OMSRoutingFenceRejected", "OMS routing fence rejected the exact target authority")
	}
	if response.MaximumRoutingEpoch != binding.RoutingEpoch {
		return MemoryBackendRoutingFenceResult{}, newOMSProbeError("OMSRoutingFenceEpochMismatch", "OMS routing fence did not durably acknowledge the exact target epoch")
	}
	if certificate != target.ExpectedServerCertificateDigest {
		return MemoryBackendRoutingFenceResult{}, newOMSProbeError("OMSCertificateChanged", "OMS server certificate identity changed before routing fence acknowledgement")
	}
	return MemoryBackendRoutingFenceResult{
		MaximumRoutingEpoch: int64(response.MaximumRoutingEpoch), ServerCertificateDigest: certificate,
	}, nil
}

func (p *OMSHTTPProber) httpClient(timeout time.Duration, resolution endpointpolicy.Resolution) (*http.Client, error) {
	if timeout <= 0 {
		timeout = defaultMemoryBackendProbeTimeout
	}
	newClient := p.newClient
	if newClient == nil {
		newClient = p.Policy.NewPinnedHTTPClient
	}
	httpClient, err := newClient(p.BaseClient, timeout, resolution)
	if err != nil {
		return nil, newOMSProbeError("OMSClientPolicyRejected", "OMS probe HTTP client could not enforce endpoint policy")
	}
	return httpClient, nil
}

func doOMSProbeRaw(
	ctx context.Context,
	httpClient *http.Client,
	baseURL, probePath, bearerToken string,
	requestValue any,
	allowedStatuses ...int,
) ([]byte, string, int, error) {
	body, err := json.Marshal(requestValue)
	if err != nil || len(body) > omsprotocol.MaxHTTPBodyBytes {
		return nil, "", 0, newOMSProbeError("OMSProbeRequestInvalid", "OMS probe request was invalid or oversized")
	}
	requestURL, err := joinOMSProbeURL(baseURL, probePath)
	if err != nil {
		return nil, "", 0, newOMSProbeError("OMSProbeRequestInvalid", "OMS probe URL was invalid")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, "", 0, newOMSProbeError("OMSProbeRequestInvalid", "OMS probe request could not be created")
	}
	request.Header.Set("Authorization", "Bearer "+bearerToken)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("User-Agent", "orka-memorybackend-controller/1")

	response, err := httpClient.Do(request)
	if err != nil {
		return nil, "", 0, newOMSProbeError("OMSProbeTransportFailed", "OMS probe request failed")
	}
	defer response.Body.Close() //nolint:errcheck
	if !slices.Contains(allowedStatuses, response.StatusCode) {
		return nil, "", response.StatusCode, newOMSProbeError("OMSProbeHTTPRejected", "OMS probe returned an unexpected status")
	}
	if response.ContentLength > omsprotocol.MaxAdapterResponseBytes {
		return nil, "", response.StatusCode, newOMSProbeError("OMSProbeResponseOversized", "OMS probe response exceeded the size limit")
	}
	if encoding := strings.TrimSpace(response.Header.Get("Content-Encoding")); encoding != "" && encoding != "identity" {
		return nil, "", response.StatusCode, newOMSProbeError("OMSProbeResponseEncodingRejected", "OMS probe response used an unsupported content encoding")
	}
	mediaType, _, err := mime.ParseMediaType(response.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return nil, "", response.StatusCode, newOMSProbeError("OMSProbeResponseTypeRejected", "OMS probe response was not application/json")
	}
	payload, err := io.ReadAll(io.LimitReader(response.Body, omsprotocol.MaxAdapterResponseBytes+1))
	if err != nil || len(payload) > omsprotocol.MaxAdapterResponseBytes {
		return nil, "", response.StatusCode, newOMSProbeError("OMSProbeResponseOversized", "OMS probe response was invalid or oversized")
	}
	certificateDigest, err := endpointpolicy.CertificateDigest(response.TLS)
	if err != nil {
		return nil, "", response.StatusCode, newOMSProbeError("OMSProbeTLSIdentityMissing", "OMS probe response did not include verified TLS identity")
	}
	return payload, certificateDigest, response.StatusCode, nil
}

func joinOMSProbeURL(baseURL, suffix string) (string, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Scheme != "https" || parsed.Host == "" {
		return "", fmt.Errorf("invalid OMS base URL")
	}
	parsed.Path = path.Join(parsed.Path, suffix)
	parsed.RawPath = ""
	parsed.RawQuery = ""
	parsed.Fragment = ""
	return parsed.String(), nil
}

func memoryBackendCapabilitiesFromOMS(capabilities omsprotocol.Capabilities) []corev1alpha1.MemoryBackendCapability {
	result := make([]corev1alpha1.MemoryBackendCapability, 0, 15)
	appendIf := func(enabled bool, capability corev1alpha1.MemoryBackendCapability) {
		if enabled {
			result = append(result, capability)
		}
	}
	appendIf(capabilities.DurableIdempotency, corev1alpha1.MemoryBackendCapabilityDurableIdempotency)
	appendIf(capabilities.IdempotencyDigestConflicts, corev1alpha1.MemoryBackendCapabilityIdempotencyDigestConflicts)
	appendIf(capabilities.CreateIfAbsent, corev1alpha1.MemoryBackendCapabilityCreateIfAbsent)
	appendIf(capabilities.ConditionalMutation, corev1alpha1.MemoryBackendCapabilityConditionalMutation)
	appendIf(capabilities.MonotonicGenerations, corev1alpha1.MemoryBackendCapabilityMonotonicGenerations)
	appendIf(capabilities.DeleteHighWatermark, corev1alpha1.MemoryBackendCapabilityDeleteHighWatermark)
	appendIf(capabilities.DurableRoutingFence, corev1alpha1.MemoryBackendCapabilityDurableRoutingFence)
	appendIf(capabilities.OperationLookup, corev1alpha1.MemoryBackendCapabilityOperationLookup)
	appendIf(capabilities.ExactGet, corev1alpha1.MemoryBackendCapabilityExactGet)
	appendIf(capabilities.StablePagination, corev1alpha1.MemoryBackendCapabilityStablePagination)
	appendIf(capabilities.ExclusiveOwnership, corev1alpha1.MemoryBackendCapabilityExclusiveOwnership)
	appendIf(capabilities.KeywordSearch, corev1alpha1.MemoryBackendCapabilityKeywordSearch)
	appendIf(capabilities.AuditVersionVisibility, corev1alpha1.MemoryBackendCapabilityAuditVersionVisibility)
	appendIf(capabilities.SemanticSearch, corev1alpha1.MemoryBackendCapabilitySemanticSearch)
	appendIf(capabilities.HybridSearch, corev1alpha1.MemoryBackendCapabilityHybridSearch)
	return result
}

func memoryBackendLimitsFromOMS(limits omsprotocol.CapabilityLimits) corev1alpha1.MemoryBackendCapabilityLimits {
	return corev1alpha1.MemoryBackendCapabilityLimits{
		MaxRequestBytes: int64(limits.MaxRequestBytes), MaxResponseBytes: int64(limits.MaxResponseBytes),
		MaxContentBytes: int64(limits.MaxContentBytes), MaxTags: int32(limits.MaxTags), MaxTagBytes: int32(limits.MaxTagBytes),
		MaxMetadataEntries: int32(limits.MaxMetadataEntries), MaxMetadataKeyBytes: int32(limits.MaxMetadataKeyBytes),
		MaxMetadataValueBytes: int32(limits.MaxMetadataValueBytes), MaxQueryBytes: int32(limits.MaxQueryBytes),
		MaxPageSize: int32(limits.MaxPageSize), MaxSnapshotRecords: int32(limits.MaxSnapshotRecords),
		SnapshotTTLSeconds: int32(limits.SnapshotTTLSeconds),
	}
}
