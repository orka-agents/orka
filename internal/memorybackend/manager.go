package memorybackend

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

// AdminResult is a bounded lifecycle action response.
type AdminResult struct {
	Backend        *corev1alpha1.MemoryBackend       `json:"backend,omitempty"`
	RestorePreview *store.LegacyMemoryRestorePreview `json:"restorePreview,omitempty"`
	Restored       int                               `json:"restored,omitempty"`
	DryRun         bool                              `json:"dryRun,omitempty"`
}

// Manager coordinates audited local intent with optimistic Kubernetes lifecycle updates.
type Manager struct {
	Client client.Client
	Reader client.Reader
	Store  store.GovernedMemoryStore
	Now    func() time.Time
}

// RecordIntent appends an immutable bounded administrative intent or completion record.
func (m *Manager) RecordIntent(ctx context.Context, namespace, actor, action, reason, requestID string) error {
	if strings.TrimSpace(reason) == "" {
		return store.ValidationErrorf("reason is required")
	}
	if m == nil || m.Store == nil {
		return fmt.Errorf("memory backend manager is not configured")
	}
	reader := m.Reader
	if reader == nil {
		reader = m.Client
	}
	if reader == nil {
		return fmt.Errorf("memory backend manager reader is not configured")
	}
	namespaceObject := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, namespaceObject); err != nil {
		return err
	}
	binding, err := m.Store.GetMemoryBackendBinding(ctx, string(namespaceObject.UID))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}
	if errors.Is(err, store.ErrNotFound) {
		binding = nil
	}
	return m.audit(ctx, store.MemoryAuditRecord{
		Namespace: namespace, NamespaceUID: string(namespaceObject.UID), Actor: actor,
		Action: action, Reason: reason, AuthorityEpoch: bindingAuthority(binding),
		RoutingEpoch: bindingRouting(binding), RequestID: requestID, CreatedAt: m.now(),
	})
}

//nolint:gocyclo // Lifecycle admission mirrors the coordinator state-transition matrix.
func validateManagedLifecycleSource(
	binding *store.MemoryBackendBinding,
	lifecycle corev1alpha1.MemoryBackendLifecycleState,
) error {
	switch lifecycle {
	case corev1alpha1.MemoryBackendLifecycleActive:
		if binding == nil || (binding.Mode == store.MemoryBackendModeLegacy && binding.State == store.MemoryBackendBindingLegacy) {
			return nil
		}
		if binding.Mode == store.MemoryBackendModeRemote &&
			(binding.State == store.MemoryBackendBindingAccepting || binding.State == store.MemoryBackendBindingDraining ||
				binding.State == store.MemoryBackendBindingRecovering) {
			return nil
		}
	case corev1alpha1.MemoryBackendLifecycleReadOnly:
		if binding != nil && binding.Mode == store.MemoryBackendModeRemote &&
			(binding.State == store.MemoryBackendBindingAccepting || binding.State == store.MemoryBackendBindingDraining) {
			return nil
		}
	case corev1alpha1.MemoryBackendLifecycleDisabled:
		if binding != nil && binding.Mode == store.MemoryBackendModeRemote &&
			(binding.State == store.MemoryBackendBindingAccepting || binding.State == store.MemoryBackendBindingDraining ||
				binding.State == store.MemoryBackendBindingRecovering) {
			return nil
		}
	case corev1alpha1.MemoryBackendLifecycleDecommissioning:
		if binding == nil {
			return nil
		}
		if binding.Mode == store.MemoryBackendModeRemote &&
			(binding.State == store.MemoryBackendBindingAccepting || binding.State == store.MemoryBackendBindingDraining ||
				binding.State == store.MemoryBackendBindingRecovering || binding.State == store.MemoryBackendBindingDecommissioned) {
			return nil
		}
	case corev1alpha1.MemoryBackendLifecycleStaged:
		if binding == nil || binding.Mode == store.MemoryBackendModeLegacy {
			return nil
		}
	default:
		return store.ValidationErrorf("unsupported MemoryBackend lifecycle %q", lifecycle)
	}
	state := store.MemoryBackendBindingState("missing")
	if binding != nil {
		state = binding.State
	}
	return fmt.Errorf("%w: MemoryBackend cannot become %s from binding state %s", store.ErrConflict, lifecycle, state)
}

// SetLifecycle commits a generation- and target-specific durable intent, then
// updates the CR. The coordinator requires both the pre-update intent and the
// post-update requested record before any protected lifecycle effect.
func (m *Manager) SetLifecycle(
	ctx context.Context,
	namespace string,
	lifecycle corev1alpha1.MemoryBackendLifecycleState,
	actor, reason, requestID string,
	dryRun bool,
) (*AdminResult, error) {
	return m.SetLifecycleAtResourceVersion(ctx, namespace, lifecycle, "", actor, reason, requestID, dryRun)
}

// SetLifecycleAtResourceVersion applies the audited lifecycle transition only
// when the caller-observed Kubernetes resourceVersion is still current.
//
//nolint:gocyclo // Lifecycle intent and transition checks are deliberately fail-closed and explicit.
func (m *Manager) SetLifecycleAtResourceVersion(
	ctx context.Context,
	namespace string,
	lifecycle corev1alpha1.MemoryBackendLifecycleState,
	expectedResourceVersion string,
	actor, reason, requestID string,
	dryRun bool,
) (*AdminResult, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, store.ValidationErrorf("reason is required")
	}
	backend, namespaceUID, err := m.current(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if expected := strings.TrimSpace(expectedResourceVersion); expected != "" && backend.ResourceVersion != expected {
		return nil, fmt.Errorf("%w: MemoryBackend resourceVersion changed", store.ErrConflict)
	}
	binding, bindingErr := m.Store.GetMemoryBackendBinding(ctx, namespaceUID)
	if bindingErr != nil && !errors.Is(bindingErr, store.ErrNotFound) {
		return nil, bindingErr
	}
	if errors.Is(bindingErr, store.ErrNotFound) {
		binding = nil
	}
	if err := validateManagedLifecycleSource(binding, lifecycle); err != nil {
		return nil, err
	}

	planned := backend.DeepCopy()
	planned.Spec.LifecycleState = lifecycle
	if dryRun {
		return &AdminResult{Backend: planned, DryRun: true}, nil
	}
	plannedGeneration := backend.Generation
	if backend.Spec.RequestedLifecycle() != lifecycle {
		plannedGeneration++
	}
	plannedSpecDigest, err := canonicalMemoryBackendSpecDigest(planned.Spec)
	if err != nil {
		return nil, err
	}
	intentDigest := memoryBackendLifecycleIntentDigest(string(backend.UID), plannedGeneration, lifecycle, plannedSpecDigest)
	if protectedMemoryBackendLifecycle(lifecycle) {
		if err := m.appendLifecycleRecord(ctx, backend, namespaceUID, binding, actor, reason, requestID,
			memoryBackendLifecycleIntentAuditAction, lifecycle, plannedSpecDigest, intentDigest); err != nil {
			return nil, err
		}
	}
	if planned.Annotations == nil {
		planned.Annotations = make(map[string]string)
	}
	if protectedMemoryBackendLifecycle(lifecycle) {
		planned.Annotations[corev1alpha1.MemoryBackendLifecycleIntentAnnotation] = intentDigest
	} else {
		delete(planned.Annotations, corev1alpha1.MemoryBackendLifecycleIntentAnnotation)
	}
	if err := m.Client.Update(ctx, planned); err != nil {
		return nil, err
	}

	fresh := &corev1alpha1.MemoryBackend{}
	reader := m.Reader
	if reader == nil {
		reader = m.Client
	}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(planned), fresh); err != nil {
		return nil, err
	}
	actualSpecDigest, err := canonicalMemoryBackendSpecDigest(fresh.Spec)
	if err != nil {
		return nil, err
	}
	if fresh.UID != planned.UID || fresh.Generation != plannedGeneration ||
		fresh.Spec.RequestedLifecycle() != lifecycle || actualSpecDigest != plannedSpecDigest ||
		fresh.Annotations[corev1alpha1.MemoryBackendLifecycleIntentAnnotation] != intentDigest {
		return nil, fmt.Errorf("%w: MemoryBackend changed after lifecycle update", store.ErrConflict)
	}
	if protectedMemoryBackendLifecycle(lifecycle) {
		if err := m.appendLifecycleRecord(ctx, fresh, namespaceUID, binding, actor, reason, requestID,
			memoryBackendLifecycleRequestedAuditAction, lifecycle, plannedSpecDigest, intentDigest); err != nil {
			return nil, err
		}
	}
	return &AdminResult{Backend: fresh}, nil
}

func (m *Manager) appendLifecycleRecord(
	ctx context.Context,
	backend *corev1alpha1.MemoryBackend,
	namespaceUID string,
	binding *store.MemoryBackendBinding,
	actor, reason, requestID, action string,
	target corev1alpha1.MemoryBackendLifecycleState,
	specDigest, intentDigest string,
) error {
	previous := ""
	if backend != nil {
		previous = string(backend.Spec.RequestedLifecycle())
	}
	return m.audit(ctx, store.MemoryAuditRecord{
		Namespace: backend.Namespace, NamespaceUID: namespaceUID, Actor: actor,
		Action: action, Reason: reason, PreviousState: previous, NewState: string(target),
		AuthorityEpoch: bindingAuthority(binding), RoutingEpoch: bindingRouting(binding),
		RequestDigest: intentDigest, MutationDigest: specDigest, RequestID: requestID, CreatedAt: m.now(),
	})
}

// RequestDelete records immutable intent before requesting Kubernetes deletion.
func (m *Manager) RequestDelete(ctx context.Context, namespace, actor, reason, requestID string) error {
	if strings.TrimSpace(reason) == "" {
		return store.ValidationErrorf("reason is required")
	}
	backend, namespaceUID, err := m.current(ctx, namespace)
	if err != nil {
		return err
	}
	binding, bindingErr := m.Store.GetMemoryBackendBinding(ctx, namespaceUID)
	if bindingErr != nil && !errors.Is(bindingErr, store.ErrNotFound) {
		return bindingErr
	}
	if errors.Is(bindingErr, store.ErrNotFound) {
		binding = nil
	}
	if binding != nil {
		sameBackend := binding.BackendUID == string(backend.UID)
		terminal := binding.State == store.MemoryBackendBindingDecommissioned || binding.State == store.MemoryBackendBindingRemoved
		legacy := binding.Mode == store.MemoryBackendModeLegacy && binding.State == store.MemoryBackendBindingLegacy
		if (sameBackend && !terminal && !legacy) || (!sameBackend && !terminal && !legacy) {
			return fmt.Errorf("%w: MemoryBackend must be never activated, Decommissioned, or Removed before explicit deletion", store.ErrConflict)
		}
	}
	if err := m.audit(ctx, store.MemoryAuditRecord{
		Namespace: namespace, NamespaceUID: namespaceUID, Actor: actor,
		Action: "backend.delete.intent", Reason: reason,
		PreviousState:  string(backend.Spec.RequestedLifecycle()),
		AuthorityEpoch: bindingAuthority(binding), RoutingEpoch: bindingRouting(binding),
		RequestID: requestID, CreatedAt: m.now(),
	}); err != nil {
		return err
	}
	if err := m.deleteBackend(ctx, backend); err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

// ForceOrphan establishes a local egress barrier, orphans unresolved work, preserves the legacy fence, and requests CR deletion.
func (m *Manager) ForceOrphan(
	ctx context.Context,
	namespace, actor, reason, requestID string,
	dryRun bool,
) (*AdminResult, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, store.ValidationErrorf("reason is required")
	}
	backend, namespaceUID, err := m.current(ctx, namespace)
	if err != nil {
		return nil, err
	}
	binding, err := m.Store.GetMemoryBackendBinding(ctx, namespaceUID)
	if err != nil {
		return nil, err
	}
	if err := validateAdminBackendUID(backend, binding); err != nil {
		return nil, err
	}
	if dryRun {
		return &AdminResult{Backend: backend, DryRun: true}, nil
	}
	if err := m.audit(ctx, store.MemoryAuditRecord{
		Namespace: namespace, NamespaceUID: namespaceUID, Actor: actor, Action: "backend.force_orphan.intent",
		Reason: reason, PreviousState: string(binding.State), AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, RequestID: requestID, CreatedAt: m.now(),
	}); err != nil {
		return nil, err
	}
	if binding.State == store.MemoryBackendBindingRemoved {
		if err := m.deleteBackend(ctx, backend); err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		return &AdminResult{Backend: backend}, nil
	}
	switch binding.State {
	case store.MemoryBackendBindingAccepting, store.MemoryBackendBindingDraining:
		binding, err = m.Store.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
			NamespaceUID: namespaceUID, BackendUID: binding.BackendUID,
			ExpectedState: binding.State, State: store.MemoryBackendBindingRecovering,
			ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
			Actor: actor, Reason: reason, RequestID: requestID, Now: m.now(),
		})
		if err != nil {
			return nil, err
		}
	case store.MemoryBackendBindingRecovering, store.MemoryBackendBindingDecommissioned:
		// These states already block all remote egress.
	default:
		return nil, fmt.Errorf("%w: force-orphan requires an active, draining, recovering, or decommissioned remote binding", store.ErrConflict)
	}
	if _, err := m.Store.OrphanMemoryOperations(ctx, store.MemoryOperationOrphaning{
		NamespaceUID: namespaceUID, BackendUID: binding.BackendUID,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: actor, Reason: reason, RequestID: requestID, Now: m.now(),
	}); err != nil {
		return nil, err
	}
	binding, err = m.Store.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: namespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingRemoved,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch + 1,
		Actor: actor, Reason: reason, RequestID: requestID, Now: m.now(),
	})
	if err != nil {
		return nil, err
	}
	if binding.State != store.MemoryBackendBindingRemoved {
		return nil, fmt.Errorf("%w: force-orphan did not reach irreversible Removed state", store.ErrConflict)
	}
	if err := m.deleteBackend(ctx, backend); err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	return &AdminResult{Backend: backend}, nil
}

// RestoreLegacy previews or performs clean audited legacy restoration, then requests CR deletion.
func (m *Manager) RestoreLegacy(
	ctx context.Context,
	namespace, actor, reason, requestID string,
	dryRun bool,
) (*AdminResult, error) {
	if strings.TrimSpace(reason) == "" {
		return nil, store.ValidationErrorf("reason is required")
	}
	backend, namespaceUID, err := m.current(ctx, namespace)
	if err != nil {
		return nil, err
	}
	binding, err := m.Store.GetMemoryBackendBinding(ctx, namespaceUID)
	if err != nil {
		return nil, err
	}
	if err := validateAdminBackendUID(backend, binding); err != nil {
		return nil, err
	}
	if binding.Mode == store.MemoryBackendModeLegacy && binding.State == store.MemoryBackendBindingLegacy {
		if dryRun {
			return &AdminResult{Backend: backend, DryRun: true}, nil
		}
		if err := m.deleteBackend(ctx, backend); err != nil && !apierrors.IsNotFound(err) {
			return nil, err
		}
		return &AdminResult{Backend: backend}, nil
	}
	preview, err := m.Store.PreviewLegacyMemoryRestore(ctx, namespaceUID, binding.BackendUID)
	if err != nil {
		return nil, err
	}
	if dryRun {
		return &AdminResult{Backend: backend, RestorePreview: preview, DryRun: true}, nil
	}
	if !preview.Restorable {
		return nil, fmt.Errorf("%w: legacy restore is blocked: %s", store.ErrConflict, preview.Reason)
	}
	if err := m.audit(ctx, store.MemoryAuditRecord{
		Namespace: namespace, NamespaceUID: namespaceUID, Actor: actor, Action: "backend.restore_legacy.intent",
		Reason: reason, PreviousState: string(binding.State), NewState: string(store.MemoryBackendBindingLegacy),
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		RequestID: requestID, CreatedAt: m.now(),
	}); err != nil {
		return nil, err
	}
	restored, err := m.Store.RestoreLegacyMemories(ctx, store.LegacyMemoryRestore{
		NamespaceUID: namespaceUID, BackendUID: binding.BackendUID,
		Actor: actor, Reason: reason, RequestID: requestID, Now: m.now(),
	})
	if err != nil {
		return nil, err
	}
	if err := m.deleteBackend(ctx, backend); err != nil && !apierrors.IsNotFound(err) {
		return nil, err
	}
	return &AdminResult{Backend: backend, RestorePreview: preview, Restored: restored.RestoredMemories}, nil
}

func (m *Manager) current(ctx context.Context, namespace string) (*corev1alpha1.MemoryBackend, string, error) {
	if m == nil || m.Client == nil || m.Store == nil {
		return nil, "", fmt.Errorf("memory backend manager is not configured")
	}
	reader := m.Reader
	if reader == nil {
		reader = m.Client
	}
	namespaceObject := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, namespaceObject); err != nil {
		return nil, "", err
	}
	backend := &corev1alpha1.MemoryBackend{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: corev1alpha1.MemoryBackendDefaultName}, backend); err != nil {
		return nil, "", err
	}
	return backend, string(namespaceObject.UID), nil
}

func (m *Manager) audit(ctx context.Context, audit store.MemoryAuditRecord) error {
	if m.Store == nil {
		return fmt.Errorf("memory backend store is not configured")
	}
	return m.Store.AppendMemoryAudit(ctx, audit)
}

func (m *Manager) deleteBackend(ctx context.Context, backend *corev1alpha1.MemoryBackend) error {
	if m == nil || m.Client == nil {
		return fmt.Errorf("memory backend manager is not configured")
	}
	if backend == nil || backend.UID == "" {
		return fmt.Errorf("%w: MemoryBackend UID is required for deletion", store.ErrConflict)
	}
	uid := backend.UID
	preconditions := client.Preconditions{UID: &uid}
	if backend.ResourceVersion != "" {
		resourceVersion := backend.ResourceVersion
		preconditions.ResourceVersion = &resourceVersion
	}
	return m.Client.Delete(ctx, backend, preconditions)
}

func validateAdminBackendUID(backend *corev1alpha1.MemoryBackend, binding *store.MemoryBackendBinding) error {
	if backend == nil || binding == nil || strings.TrimSpace(string(backend.UID)) == "" ||
		strings.TrimSpace(binding.BackendUID) == "" || string(backend.UID) != binding.BackendUID {
		return fmt.Errorf("%w: MemoryBackend UID does not match the durable binding", store.ErrConflict)
	}
	return nil
}

func (m *Manager) now() time.Time {
	if m.Now != nil {
		return m.Now().UTC()
	}
	return time.Now().UTC()
}

func bindingAuthority(binding *store.MemoryBackendBinding) int64 {
	if binding == nil {
		return 0
	}
	return binding.AuthorityEpoch
}

func bindingRouting(binding *store.MemoryBackendBinding) int64 {
	if binding == nil {
		return 0
	}
	return binding.RoutingEpoch
}
