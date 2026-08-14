package api

import (
	"encoding/hex"
	"errors"
	"net/http"
	"reflect"
	"strings"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/gofiber/fiber/v3/middleware/requestid"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	memoryruntime "github.com/orka-agents/orka/internal/memory"
	"github.com/orka-agents/orka/internal/store"
)

func (h *Handlers) ensureMemoryBackendManager() error {
	if h.memoryBackendManager == nil || h.client == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "memory backend control plane is not configured")
	}
	return nil
}

// ListMemoryBackends lists the fixed-name backend in the selected namespace.
func (h *Handlers) ListMemoryBackends(c fiber.Ctx) error {
	if err := h.ensureMemoryBackendManager(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeMemoryBackend(c, namespace, "list", "", false); err != nil {
		return err
	}
	list := &corev1alpha1.MemoryBackendList{}
	if err := h.client.List(c.Context(), list, client.InNamespace(namespace)); err != nil {
		return fiber.NewError(fiber.StatusInternalServerError, "failed to list memory backends")
	}
	return c.JSON(ListResponse{Items: list.Items, Metadata: ListMeta{}})
}

// CreateMemoryBackend creates MemoryBackend/default in Staged or caller-selected requested lifecycle.
func (h *Handlers) CreateMemoryBackend(c fiber.Ctx) error {
	if err := h.ensureMemoryBackendManager(); err != nil {
		return err
	}
	var request corev1alpha1.MemoryBackend
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	namespace, err := h.resolveNamespace(c, request.Namespace)
	if err != nil {
		return err
	}
	if err := h.authorizeMemoryBackend(c, namespace, "create", "", true); err != nil {
		return err
	}
	request.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "MemoryBackend"}
	request.ObjectMeta = metav1.ObjectMeta{Name: corev1alpha1.MemoryBackendDefaultName, Namespace: namespace}
	request.Status = corev1alpha1.MemoryBackendStatus{}
	if request.Spec.LifecycleState == "" {
		request.Spec.LifecycleState = corev1alpha1.MemoryBackendLifecycleStaged
	}
	if request.Spec.LifecycleState != corev1alpha1.MemoryBackendLifecycleStaged {
		return fiber.NewError(fiber.StatusBadRequest, "memory backend creation must start in Staged")
	}
	reason := strings.TrimSpace(c.Query("reason", ""))
	if reason == "" {
		return fiber.NewError(fiber.StatusBadRequest, "reason is required")
	}
	actor, _ := memoryActor(c)
	if err := h.memoryBackendManager.RecordIntent(c.Context(), namespace, actor, "backend.create.intent", reason, requestid.FromContext(c)); err != nil {
		return memoryBackendServiceError(err)
	}
	if err := h.client.Create(c.Context(), &request); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return fiber.NewError(fiber.StatusConflict, "memory backend already exists")
		}
		return fiber.NewError(fiber.StatusBadRequest, "failed to create memory backend")
	}
	// The create intent is durable before the Kubernetes commit. Completion is
	// best-effort so an audit outage cannot make a committed create look failed.
	_ = h.memoryBackendManager.RecordIntent(c.Context(), namespace, actor, "backend.create.completed", reason, requestid.FromContext(c))
	return c.Status(http.StatusCreated).JSON(request)
}

// GetMemoryBackend gets MemoryBackend/default.
func (h *Handlers) GetMemoryBackend(c fiber.Ctx) error {
	backend, err := h.authorizedCurrentMemoryBackend(c, false, "get")
	if err != nil {
		return err
	}
	return c.JSON(backend)
}

// GetMemoryBackendStatus gets only bounded non-secret backend status.
func (h *Handlers) GetMemoryBackendStatus(c fiber.Ctx) error {
	backend, err := h.authorizedCurrentMemoryBackend(c, false, "get")
	if err != nil {
		return err
	}
	return c.JSON(backend.Status)
}

// RecordMemoryBackendCheckpoint records either the matched pre-activation
// recovery receipt for a freshly validated Staged backend or a runtime
// adapter/content-store checkpoint for an already active remote authority.
func (h *Handlers) RecordMemoryBackendCheckpoint(c fiber.Ctx) error {
	if err := h.ensureMemoryBackendManager(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeMemoryBackend(c, namespace, "get", corev1alpha1.MemoryBackendDefaultName, true); err != nil {
		return err
	}
	backend, err := h.authorizedMemoryBackendInNamespace(c, namespace, true, "update")
	if err != nil {
		return err
	}
	var request struct {
		ManifestDigest           string `json:"manifestDigest"`
		MaximumOperationSequence int64  `json:"maximumOperationSequence,omitempty"`
		Reason                   string `json:"reason"`
	}
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	request.ManifestDigest = strings.TrimSpace(request.ManifestDigest)
	request.Reason = strings.TrimSpace(request.Reason)
	if !validMemoryBackendManifestDigest(request.ManifestDigest) {
		return fiber.NewError(fiber.StatusBadRequest, "manifestDigest must be a sha256 digest")
	}
	if request.MaximumOperationSequence < 0 {
		return fiber.NewError(fiber.StatusBadRequest, "maximumOperationSequence cannot be negative")
	}
	if request.Reason == "" {
		return fiber.NewError(fiber.StatusBadRequest, "reason is required")
	}
	reader := h.apiReader
	if reader == nil {
		reader = h.client
	}
	namespaceObject := &corev1.Namespace{}
	if err := reader.Get(c.Context(), client.ObjectKey{Name: namespace}, namespaceObject); err != nil || namespaceObject.UID == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "namespace identity is unavailable")
	}
	now := time.Now().UTC()
	if h.memoryBackendManager.Now != nil {
		now = h.memoryBackendManager.Now().UTC()
	}
	actor, _ := memoryActor(c)
	governed := h.memoryBackendManager.Store
	binding, bindingErr := governed.GetMemoryBackendBinding(c.Context(), string(namespaceObject.UID))
	if errors.Is(bindingErr, store.ErrNotFound) || binding != nil && binding.Mode == store.MemoryBackendModeLegacy {
		if request.MaximumOperationSequence != 0 {
			return fiber.NewError(fiber.StatusBadRequest, "pre-activation recovery receipt must use maximumOperationSequence 0")
		}
		identity, identityErr := validatedMemoryRecoveryRouteIdentity(backend, string(namespaceObject.UID), now)
		if identityErr != nil {
			return identityErr
		}
		receipt, recordErr := governed.RecordMemoryActivationRecoveryReceipt(c.Context(), store.MemoryActivationRecoveryReceipt{
			Namespace: namespace, NamespaceUID: string(namespaceObject.UID), BackendUID: string(backend.UID),
			RouteDigest: identity.Digest(), StoreUUID: backend.Status.StoreUUID,
			ManifestDigest: request.ManifestDigest, Actor: actor, Reason: request.Reason,
			RequestID: requestid.FromContext(c), VerifiedAt: now,
		})
		if recordErr != nil {
			return memoryBackendServiceError(recordErr)
		}
		return c.Status(http.StatusCreated).JSON(fiber.Map{"kind": "activationRecoveryReceipt", "receipt": receipt})
	}
	if bindingErr != nil {
		return memoryBackendServiceError(bindingErr)
	}
	if binding == nil || binding.Mode != store.MemoryBackendModeRemote || binding.BackendUID != string(backend.UID) {
		return fiber.NewError(fiber.StatusConflict, "memory backend does not match the durable remote authority")
	}
	checkpoint, recordErr := governed.RecordMemoryVerifiedCheckpoint(c.Context(), store.MemoryVerifiedCheckpoint{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		StoreUUID: binding.StoreUUID, MaximumOperationSequence: request.MaximumOperationSequence,
		CheckpointDigest: request.ManifestDigest, Actor: actor, Reason: request.Reason,
		RequestID: requestid.FromContext(c), VerifiedAt: now,
	})
	if recordErr != nil {
		return memoryBackendServiceError(recordErr)
	}
	return c.Status(http.StatusCreated).JSON(fiber.Map{"kind": "verifiedCheckpoint", "checkpoint": checkpoint})
}

// PurgeMemoryBackendGovernance reclaims checkpoint-covered local retention
// state under the exact active binding identity.
func (h *Handlers) PurgeMemoryBackendGovernance(c fiber.Ctx) error {
	if err := h.ensureMemoryBackendManager(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeMemoryBackend(c, namespace, "get", corev1alpha1.MemoryBackendDefaultName, true); err != nil {
		return err
	}
	backend, err := h.authorizedMemoryBackendInNamespace(c, namespace, true, "update")
	if err != nil {
		return err
	}
	var request struct {
		CheckpointID             string    `json:"checkpointId"`
		MaximumOperationSequence int64     `json:"maximumOperationSequence,omitempty"`
		Before                   time.Time `json:"before"`
		PurgePayloads            bool      `json:"purgePayloads,omitempty"`
		PurgeReceipts            bool      `json:"purgeReceipts,omitempty"`
		PurgeExpiredIdempotency  bool      `json:"purgeExpiredIdempotency,omitempty"`
		PurgeTombstones          bool      `json:"purgeTombstones,omitempty"`
		PurgeAudit               bool      `json:"purgeAudit,omitempty"`
		Reason                   string    `json:"reason"`
	}
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	request.CheckpointID = strings.TrimSpace(request.CheckpointID)
	request.Reason = strings.TrimSpace(request.Reason)
	if request.CheckpointID == "" || request.Before.IsZero() || request.MaximumOperationSequence < 0 || request.Reason == "" {
		return fiber.NewError(fiber.StatusBadRequest, "checkpointId, before, non-negative maximumOperationSequence, and reason are required")
	}
	if !request.PurgePayloads && !request.PurgeReceipts && !request.PurgeExpiredIdempotency &&
		!request.PurgeTombstones && !request.PurgeAudit {
		return fiber.NewError(fiber.StatusBadRequest, "at least one purge target is required")
	}
	reader := h.apiReader
	if reader == nil {
		reader = h.client
	}
	namespaceObject := &corev1.Namespace{}
	if err := reader.Get(c.Context(), client.ObjectKey{Name: namespace}, namespaceObject); err != nil || namespaceObject.UID == "" {
		return fiber.NewError(fiber.StatusServiceUnavailable, "namespace identity is unavailable")
	}
	binding, err := h.memoryBackendManager.Store.GetMemoryBackendBinding(c.Context(), string(namespaceObject.UID))
	if err != nil {
		return memoryBackendServiceError(err)
	}
	if binding == nil || binding.Mode != store.MemoryBackendModeRemote || binding.BackendUID != string(backend.UID) {
		return fiber.NewError(fiber.StatusConflict, "memory backend does not match the durable remote authority")
	}
	now := time.Now().UTC()
	if h.memoryBackendManager.Now != nil {
		now = h.memoryBackendManager.Now().UTC()
	}
	actor, _ := memoryActor(c)
	result, err := h.memoryBackendManager.Store.PurgeMemoryGovernance(c.Context(), store.MemoryGovernancePurge{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch, StoreUUID: binding.StoreUUID,
		CheckpointID: request.CheckpointID, MaximumOperationSequence: request.MaximumOperationSequence,
		Before: request.Before.UTC(), PurgePayloads: request.PurgePayloads, PurgeReceipts: request.PurgeReceipts,
		PurgeExpiredIdempotency: request.PurgeExpiredIdempotency, PurgeTombstones: request.PurgeTombstones,
		PurgeAudit: request.PurgeAudit, Actor: actor, Reason: request.Reason,
		RequestID: requestid.FromContext(c), Now: now,
	})
	if err != nil {
		return memoryBackendServiceError(err)
	}
	return c.JSON(result)
}

func validatedMemoryRecoveryRouteIdentity(
	backend *corev1alpha1.MemoryBackend,
	namespaceUID string,
	now time.Time,
) (store.MemoryRecoveryRouteIdentity, error) {
	if backend == nil || backend.UID == "" || backend.Status.ObservedGeneration != backend.Generation ||
		backend.Spec.RequestedLifecycle() != corev1alpha1.MemoryBackendLifecycleStaged ||
		backend.Status.EffectiveLifecycleState != corev1alpha1.MemoryBackendEffectiveLifecycleStaged ||
		!backend.Status.Accepted || !backend.Status.Connected || !backend.Status.Ready ||
		backend.Status.ValidationExpiresAt == nil || !backend.Status.ValidationExpiresAt.After(now) ||
		backend.Status.NamespaceUID != namespaceUID || backend.Status.BackendUID != string(backend.UID) ||
		backend.Status.ObservedCapabilities == nil {
		return store.MemoryRecoveryRouteIdentity{}, fiber.NewError(
			fiber.StatusConflict,
			"a fresh Ready Staged backend validation is required before recording the activation recovery receipt",
		)
	}
	identity := store.MemoryRecoveryRouteIdentity{
		NamespaceUID: namespaceUID, BackendUID: string(backend.UID),
		ClusterIdentityDigest: backend.Status.ClusterIdentityDigest,
		EndpointDigest:        backend.Status.EndpointDigest, ResolvedAddressDigest: backend.Status.ResolvedAddressDigest,
		ServerCertificateDigest: backend.Status.ServerCertificateDigest,
		SecretName:              backend.Spec.ClientAuth.BearerTokenSecretRef.Name,
		SecretKey:               backend.Spec.ClientAuth.BearerTokenSecretRef.Key,
		SecretUID:               backend.Status.SecretUID, SecretResourceVersion: backend.Status.SecretResourceVersion,
		StoreName: backend.Spec.Store.Name, StoreUUID: backend.Status.StoreUUID,
		CapabilityRevision: backend.Status.ObservedCapabilities.Revision,
		Protocol:           string(backend.Spec.Protocol.Profile),
	}
	if !validMemoryBackendManifestDigest(identity.ClusterIdentityDigest) ||
		!validMemoryBackendManifestDigest(identity.EndpointDigest) ||
		!validMemoryBackendManifestDigest(identity.ResolvedAddressDigest) ||
		!validMemoryBackendManifestDigest(identity.ServerCertificateDigest) ||
		strings.TrimSpace(identity.SecretName) == "" || strings.TrimSpace(identity.SecretKey) == "" ||
		strings.TrimSpace(identity.SecretUID) == "" || strings.TrimSpace(identity.SecretResourceVersion) == "" ||
		strings.TrimSpace(identity.StoreName) == "" || strings.TrimSpace(identity.StoreUUID) == "" ||
		strings.TrimSpace(identity.CapabilityRevision) == "" || strings.TrimSpace(identity.Protocol) == "" {
		return store.MemoryRecoveryRouteIdentity{}, fiber.NewError(fiber.StatusConflict, "staged backend validation identity is incomplete")
	}
	return identity, nil
}

func validMemoryBackendManifestDigest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(value, "sha256:"))
	return err == nil
}

// UpdateMemoryBackend updates only caller-owned spec fields under the current resourceVersion.
func (h *Handlers) UpdateMemoryBackend(c fiber.Ctx) error {
	var request corev1alpha1.MemoryBackend
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	bodyNamespace := strings.TrimSpace(request.Namespace)
	queryNamespace := strings.TrimSpace(c.Query("namespace", ""))
	if bodyNamespace != "" && queryNamespace != "" && bodyNamespace != queryNamespace {
		return fiber.NewError(fiber.StatusBadRequest, "memory backend namespace mismatch")
	}
	explicitNamespace := bodyNamespace
	if explicitNamespace == "" {
		explicitNamespace = queryNamespace
	}
	namespace, err := h.resolveNamespace(c, explicitNamespace)
	if err != nil {
		return err
	}
	current, err := h.authorizedMemoryBackendInNamespace(c, namespace, true, "update")
	if err != nil {
		return err
	}
	if request.Name != "" && request.Name != corev1alpha1.MemoryBackendDefaultName {
		return fiber.NewError(fiber.StatusBadRequest, "MemoryBackend metadata.name must be default")
	}
	reason := strings.TrimSpace(c.Query("reason", ""))
	if reason == "" {
		return fiber.NewError(fiber.StatusBadRequest, "reason is required")
	}
	if strings.TrimSpace(request.ResourceVersion) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "metadata.resourceVersion is required")
	}
	if request.ResourceVersion != current.ResourceVersion {
		return fiber.NewError(fiber.StatusConflict, "memory backend changed; refresh and retry")
	}
	actor, _ := memoryActor(c)
	requestedLifecycle := request.Spec.RequestedLifecycle()
	if requestedLifecycle != current.Spec.RequestedLifecycle() {
		specWithoutLifecycle := request.Spec
		specWithoutLifecycle.LifecycleState = current.Spec.RequestedLifecycle()
		if !reflect.DeepEqual(specWithoutLifecycle, current.Spec) {
			return fiber.NewError(fiber.StatusBadRequest,
				"update backend configuration separately before requesting a lifecycle transition")
		}
		result, lifecycleErr := h.memoryBackendManager.SetLifecycleAtResourceVersion(
			c.Context(), current.Namespace, requestedLifecycle, request.ResourceVersion,
			actor, reason, requestid.FromContext(c), false,
		)
		if lifecycleErr != nil {
			return memoryBackendServiceError(lifecycleErr)
		}
		return c.JSON(result.Backend)
	}
	if err := h.memoryBackendManager.RecordIntent(c.Context(), current.Namespace, actor, "backend.update.intent", reason, requestid.FromContext(c)); err != nil {
		return memoryBackendServiceError(err)
	}
	current.Spec = request.Spec
	if err := h.client.Update(c.Context(), current); err != nil {
		if apierrors.IsConflict(err) {
			return fiber.NewError(fiber.StatusConflict, "memory backend changed; refresh and retry")
		}
		return fiber.NewError(fiber.StatusBadRequest, "failed to update memory backend")
	}
	// The update intent is durable before the Kubernetes commit. Completion is
	// best-effort so callers can safely observe the committed resource state.
	_ = h.memoryBackendManager.RecordIntent(c.Context(), current.Namespace, actor, "backend.update.completed", reason, requestid.FromContext(c))
	return c.JSON(current)
}

// DeleteMemoryBackend requests deletion; the protection finalizer remains authoritative.
func (h *Handlers) DeleteMemoryBackend(c fiber.Ctx) error {
	backend, err := h.authorizedCurrentMemoryBackend(c, true, "delete")
	if err != nil {
		return err
	}
	var request memoryruntime.BackendActionRequest
	if len(strings.TrimSpace(string(c.Body()))) > 0 {
		if err := bindStrictMemoryJSON(c, &request); err != nil {
			return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
		}
	}
	if strings.TrimSpace(request.Reason) == "" {
		return fiber.NewError(fiber.StatusBadRequest, "reason is required")
	}
	actor, _ := memoryActor(c)
	if err := h.memoryBackendManager.RequestDelete(c.Context(), backend.Namespace, actor, request.Reason, requestid.FromContext(c)); err != nil {
		return memoryBackendServiceError(err)
	}
	return c.SendStatus(fiber.StatusNoContent)
}

func (h *Handlers) ActivateMemoryBackend(c fiber.Ctx) error {
	return h.memoryBackendLifecycleAction(c, corev1alpha1.MemoryBackendLifecycleActive)
}

func (h *Handlers) DecommissionMemoryBackend(c fiber.Ctx) error {
	return h.memoryBackendLifecycleAction(c, corev1alpha1.MemoryBackendLifecycleDecommissioning)
}

func (h *Handlers) memoryBackendLifecycleAction(c fiber.Ctx, lifecycle corev1alpha1.MemoryBackendLifecycleState) error {
	if err := h.ensureMemoryBackendManager(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeMemoryBackend(c, namespace, "update", corev1alpha1.MemoryBackendDefaultName, true); err != nil {
		return err
	}
	var request memoryruntime.BackendActionRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	actor, _ := memoryActor(c)
	result, err := h.memoryBackendManager.SetLifecycle(c.Context(), namespace, lifecycle, actor,
		request.Reason, requestid.FromContext(c), request.DryRun)
	if err != nil {
		return memoryBackendServiceError(err)
	}
	return c.JSON(result)
}

// ForceOrphanMemoryBackend completes the fail-closed local orphan barrier.
func (h *Handlers) ForceOrphanMemoryBackend(c fiber.Ctx) error {
	return h.memoryBackendTerminalAction(c, true)
}

// RestoreLegacyMemoryBackend previews or restores archived legacy memory.
func (h *Handlers) RestoreLegacyMemoryBackend(c fiber.Ctx) error {
	return h.memoryBackendTerminalAction(c, false)
}

func (h *Handlers) memoryBackendTerminalAction(c fiber.Ctx, forceOrphan bool) error {
	if err := h.ensureMemoryBackendManager(); err != nil {
		return err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return err
	}
	if err := h.authorizeMemoryBackend(c, namespace, "update", corev1alpha1.MemoryBackendDefaultName, true); err != nil {
		return err
	}
	if err := h.authorizeMemoryBackend(c, namespace, "delete", corev1alpha1.MemoryBackendDefaultName, true); err != nil {
		return err
	}
	var request memoryruntime.BackendActionRequest
	if err := bindStrictMemoryJSON(c, &request); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, "invalid request body")
	}
	actor, _ := memoryActor(c)
	if forceOrphan {
		result, actionErr := h.memoryBackendManager.ForceOrphan(c.Context(), namespace, actor,
			request.Reason, requestid.FromContext(c), request.DryRun)
		if actionErr != nil {
			return memoryBackendServiceError(actionErr)
		}
		return c.JSON(result)
	}
	result, actionErr := h.memoryBackendManager.RestoreLegacy(c.Context(), namespace, actor,
		request.Reason, requestid.FromContext(c), request.DryRun)
	if actionErr != nil {
		return memoryBackendServiceError(actionErr)
	}
	return c.JSON(result)
}

func (h *Handlers) authorizedCurrentMemoryBackend(c fiber.Ctx, mutate bool, verb string) (*corev1alpha1.MemoryBackend, error) {
	if err := h.ensureMemoryBackendManager(); err != nil {
		return nil, err
	}
	namespace, err := h.resolveNamespace(c, c.Query("namespace", ""))
	if err != nil {
		return nil, err
	}
	return h.authorizedMemoryBackendInNamespace(c, namespace, mutate, verb)
}

func (h *Handlers) authorizedMemoryBackendInNamespace(
	c fiber.Ctx,
	namespace string,
	mutate bool,
	verb string,
) (*corev1alpha1.MemoryBackend, error) {
	if err := h.ensureMemoryBackendManager(); err != nil {
		return nil, err
	}
	if err := h.authorizeMemoryBackend(c, namespace, verb, corev1alpha1.MemoryBackendDefaultName, mutate); err != nil {
		return nil, err
	}
	backend := &corev1alpha1.MemoryBackend{}
	reader := h.apiReader
	if reader == nil {
		reader = h.client
	}
	if err := reader.Get(c.Context(), client.ObjectKey{Namespace: namespace, Name: corev1alpha1.MemoryBackendDefaultName}, backend); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fiber.NewError(fiber.StatusNotFound, "memory backend not found")
		}
		return nil, fiber.NewError(fiber.StatusServiceUnavailable, "memory backend is unavailable")
	}
	return backend, nil
}

func (h *Handlers) authorizeMemoryBackend(c fiber.Ctx, namespace, verb, name string, mutate bool) error {
	user := GetUserInfo(c)
	if user == nil || user.AuthType != AuthTypeTokenReview {
		return fiber.NewError(fiber.StatusForbidden, "memory backend configuration requires Kubernetes TokenReview authentication")
	}
	scopes := h.contextTokenAuthorization.MemoryReadScopes
	if mutate {
		scopes = h.contextTokenAuthorization.MemoryOperateScopes
	}
	if err := h.authorizeContextTokenAction(c, "memoryBackend"+strings.ToUpper(verb[:1])+verb[1:], scopes); err != nil {
		return err
	}
	return authorizeKubernetesResourceAction(c.Context(), h.clientset, user, namespace, verb,
		corev1alpha1.GroupVersion.Group, "memorybackends", name)
}

func memoryBackendServiceError(err error) error {
	if err == nil {
		return nil
	}
	switch {
	case apierrors.IsConflict(err), errors.Is(err, store.ErrConflict):
		return fiber.NewError(fiber.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNotFound), apierrors.IsNotFound(err):
		return fiber.NewError(fiber.StatusNotFound, "memory backend not found")
	case errors.Is(err, store.ErrValidation):
		return fiber.NewError(fiber.StatusBadRequest, err.Error())
	case errors.Is(err, store.ErrNotReady):
		return fiber.NewError(fiber.StatusServiceUnavailable, "memory backend transition is not ready")
	default:
		return fiber.NewError(fiber.StatusInternalServerError, "memory backend operation failed")
	}
}
