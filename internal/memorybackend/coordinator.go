package memorybackend

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	"github.com/orka-agents/orka/internal/store"
)

const (
	// FoundationFeatureEpoch is the fence-aware schema/readiness release.
	FoundationFeatureEpoch int64 = 1
	// ActivationFeatureEpoch is the first epoch permitted to activate remote authority.
	ActivationFeatureEpoch int64 = 2
)

// StoreCoordinator implements the controller's durable lifecycle barrier over GovernedMemoryStore.
type StoreCoordinator struct {
	Store                store.GovernedMemoryStore
	ActivationEnabled    bool
	RequiredFeatureEpoch int64
	Actor                string
	Now                  func() time.Time
}

var _ controller.MemoryBackendBindingCoordinator = (*StoreCoordinator)(nil)

// PrepareMemoryBackendValidation deterministically selects candidate epochs.
// The candidate is re-derived from durable binding state on every reconciliation.
//
//nolint:gocyclo // Candidate selection enforces durable epochs, retirement, drain floors, and claim tracking.
func (c *StoreCoordinator) PrepareMemoryBackendValidation(
	ctx context.Context,
	snapshot controller.MemoryBackendValidationSnapshot,
) (controller.MemoryBackendValidationBinding, error) {
	if c == nil || c.Store == nil {
		return controller.MemoryBackendValidationBinding{}, fmt.Errorf("memory backend store is not configured")
	}
	if snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleActive && !c.ActivationEnabled {
		return controller.MemoryBackendValidationBinding{}, fmt.Errorf(
			"%w: remote memory activation is disabled by the foundation-release gate",
			store.ErrConflict,
		)
	}
	if _, err := c.requireLifecycleIntent(
		ctx,
		snapshot.NamespaceUID,
		snapshot.BackendUID,
		snapshot.BackendGeneration,
		snapshot.RequestedLifecycle,
		snapshot.SpecDigest,
		snapshot.LifecycleIntentDigest,
	); err != nil {
		return controller.MemoryBackendValidationBinding{}, err
	}
	existing, err := c.Store.GetMemoryBackendBinding(ctx, snapshot.NamespaceUID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return controller.MemoryBackendValidationBinding{}, err
	}
	if existing != nil && snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleActive {
		switch existing.State {
		case store.MemoryBackendBindingDecommissioned, store.MemoryBackendBindingRemoved:
			return controller.MemoryBackendValidationBinding{}, fmt.Errorf(
				"%w: %s memory backend binding cannot reconcile Active",
				store.ErrConflict,
				existing.State,
			)
		}
	}

	if errors.Is(err, store.ErrNotFound) {
		if snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleReadOnly {
			return controller.MemoryBackendValidationBinding{}, fmt.Errorf(
				"%w: MemoryBackend cannot become ReadOnly before remote authority activation",
				store.ErrConflict,
			)
		}
		return c.persistValidationCandidate(ctx, snapshot, 1, 1, true)
	}
	if existing.Mode == store.MemoryBackendModeLegacy && existing.State == store.MemoryBackendBindingLegacy {
		if snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleReadOnly {
			return controller.MemoryBackendValidationBinding{}, fmt.Errorf(
				"%w: MemoryBackend cannot become ReadOnly while legacy memory remains authoritative",
				store.ErrConflict,
			)
		}
		return c.persistValidationCandidate(ctx, snapshot, existing.AuthorityEpoch+1, existing.RoutingEpoch+1, true)
	}
	if snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleStaged {
		return controller.MemoryBackendValidationBinding{}, fmt.Errorf(
			"%w: Staged cannot be requested after remote authority activation",
			store.ErrConflict,
		)
	}
	if existing.BackendUID != snapshot.BackendUID || existing.ClusterID != snapshot.ClusterID ||
		existing.Namespace != snapshot.Namespace || existing.NamespaceUID != snapshot.NamespaceUID ||
		existing.StoreName != snapshot.StoreName || existing.StoreUUID != snapshot.StoreUUID || existing.Protocol != snapshot.Protocol {
		return controller.MemoryBackendValidationBinding{}, fmt.Errorf("%w: memory backend validation identity conflicts with durable binding", store.ErrConflict)
	}

	routeMetadataChanged := validationRouteMetadataChanged(existing, snapshot)
	if existing.State == store.MemoryBackendBindingAccepting &&
		(snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleReadOnly || routeMetadataChanged) {
		reason := "MemoryBackend entered durable draining state before route validation"
		if snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleReadOnly {
			reason = "MemoryBackend entered durable draining state before ReadOnly validation"
		}
		existing, err = c.enterDrainingBarrier(ctx, existing, reason, c.now())
		if err != nil {
			return controller.MemoryBackendValidationBinding{}, err
		}
	}
	readOnlyFenceRequired := snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleReadOnly &&
		existing.State == store.MemoryBackendBindingDraining &&
		snapshot.PreviousEffectiveLifecycle != corev1alpha1.MemoryBackendEffectiveLifecycleReadOnly
	needsRoutingAdvance := routeMetadataChanged ||
		(snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleActive && existing.State == store.MemoryBackendBindingDraining) ||
		(snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleActive && existing.State == store.MemoryBackendBindingRecovering) ||
		readOnlyFenceRequired
	if !needsRoutingAdvance {
		return c.persistValidationCandidateAtExactEpoch(
			ctx, snapshot, existing.AuthorityEpoch, existing.RoutingEpoch,
		)
	}
	unresolved, err := c.hasUnresolvedOperations(ctx, existing.NamespaceUID)
	if err != nil {
		return controller.MemoryBackendValidationBinding{}, err
	}
	if unresolved {
		if existing.State == store.MemoryBackendBindingRecovering {
			return controller.MemoryBackendValidationBinding{}, fmt.Errorf(
				"%w: unresolved fenced memory operations must be resolved before recovering authority can resume",
				store.ErrConflict,
			)
		}
		// The controller has validated the current CR generation, but old-route
		// operations must continue using the previously validated endpoint and
		// credential identity until they drain. Persist only the generation
		// acknowledgement at the existing routing epoch; every route-sensitive
		// field remains bound to the old route.
		if existing.BackendGeneration != snapshot.BackendGeneration {
			drainCompatible := *existing
			drainCompatible.BackendGeneration = snapshot.BackendGeneration
			refreshed, refreshErr := c.Store.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
				Binding: drainCompatible, ExpectedRoutingEpoch: existing.RoutingEpoch,
				Actor: c.actor(), Reason: "MemoryBackend current generation validated while old-route operations drain", Now: c.now(),
			})
			if refreshErr != nil {
				return controller.MemoryBackendValidationBinding{}, refreshErr
			}
			existing = refreshed
		}
		return controller.MemoryBackendValidationBinding{
			AuthorityEpoch: existing.AuthorityEpoch,
			RoutingEpoch:   existing.RoutingEpoch,
			DrainRequired:  true,
		}, nil
	}
	candidate, err := c.persistValidationCandidate(
		ctx, snapshot, existing.AuthorityEpoch, existing.RoutingEpoch+1, false,
	)
	if err != nil {
		return controller.MemoryBackendValidationBinding{}, err
	}
	candidate.RemoteFenceRequired = true
	return candidate, nil
}

// ReconcileMemoryBackendBinding applies the requested durable lifecycle barrier.
//
//nolint:gocyclo // Lifecycle barriers are explicit to keep fail-closed transitions auditable.
func (c *StoreCoordinator) ReconcileMemoryBackendBinding(
	ctx context.Context,
	snapshot controller.MemoryBackendBindingSnapshot,
) (controller.MemoryBackendBindingResult, error) {
	if c == nil || c.Store == nil {
		return controller.MemoryBackendBindingResult{}, fmt.Errorf("memory backend store is not configured")
	}
	intentDigest, err := c.requireLifecycleIntent(
		ctx,
		snapshot.NamespaceUID,
		snapshot.BackendUID,
		snapshot.BackendGeneration,
		snapshot.RequestedLifecycle,
		snapshot.SpecDigest,
		snapshot.LifecycleIntentDigest,
	)
	if err != nil {
		return controller.MemoryBackendBindingResult{}, err
	}
	if snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleActive ||
		snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleReadOnly {
		expectedCandidateDigest := validationCandidateDigest(controller.MemoryBackendValidationSnapshot{
			Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, BackendUID: snapshot.BackendUID,
			BackendGeneration: snapshot.BackendGeneration, ClusterID: snapshot.ClusterID, TenantID: snapshot.TenantID,
			RequestedLifecycle: snapshot.RequestedLifecycle, SpecDigest: snapshot.SpecDigest,
			EndpointIdentity: snapshot.EndpointIdentity, EndpointDigest: snapshot.EndpointDigest,
			ResolvedAddressDigest: snapshot.ResolvedAddressDigest, ServerCertificateDigest: snapshot.ServerCertificateDigest,
			SecretName: snapshot.SecretName, SecretKey: snapshot.SecretKey, SecretUID: snapshot.SecretUID,
			SecretResourceVersion: snapshot.SecretResourceVersion, StoreName: snapshot.StoreName,
			StoreUUID: snapshot.StoreUUID, Protocol: snapshot.Protocol,
		})
		if strings.TrimSpace(snapshot.CandidateDigest) == "" || snapshot.CandidateDigest != expectedCandidateDigest {
			return controller.MemoryBackendBindingResult{}, fmt.Errorf(
				"%w: binding snapshot does not match the claimed validation candidate", store.ErrConflict,
			)
		}
	}
	if snapshot.RequestedLifecycle == corev1alpha1.MemoryBackendLifecycleActive && !c.ActivationEnabled {
		return controller.MemoryBackendBindingResult{}, fmt.Errorf(
			"%w: remote memory activation is disabled by the foundation-release gate", store.ErrConflict,
		)
	}
	if err := c.recordValidationCandidateClaim(ctx, snapshot); err != nil {
		return controller.MemoryBackendBindingResult{}, err
	}
	now := c.now()
	existing, err := c.Store.GetMemoryBackendBinding(ctx, snapshot.NamespaceUID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return controller.MemoryBackendBindingResult{}, err
	}

	switch snapshot.RequestedLifecycle {
	case corev1alpha1.MemoryBackendLifecycleActive:
		if errors.Is(err, store.ErrNotFound) {
			activationBinding := bindingFromSnapshot(snapshot, store.MemoryBackendBindingAccepting)
			activationBinding.MinimumFeatureEpoch = c.requiredFeatureEpoch()
			result, activateErr := c.Store.ActivateMemoryBackend(ctx, store.MemoryBackendActivation{
				Binding:              activationBinding,
				RequiredFeatureEpoch: c.requiredFeatureEpoch(), Actor: c.actor(),
				Reason: "MemoryBackend requested Active", RequestID: intentDigest, Now: now,
			})
			if activateErr != nil {
				return controller.MemoryBackendBindingResult{}, activateErr
			}
			return c.bindingResultAfterCandidate(ctx, snapshot, result.Binding,
				corev1alpha1.MemoryBackendEffectiveLifecycleActive, "Activated", "remote memory authority is active")
		}
		if existing.Mode == store.MemoryBackendModeLegacy && existing.State == store.MemoryBackendBindingLegacy {
			activationBinding := bindingFromSnapshot(snapshot, store.MemoryBackendBindingAccepting)
			activationBinding.MinimumFeatureEpoch = c.requiredFeatureEpoch()
			result, activateErr := c.Store.ActivateMemoryBackend(ctx, store.MemoryBackendActivation{
				Binding: activationBinding, RequiredFeatureEpoch: c.requiredFeatureEpoch(),
				Actor: c.actor(), Reason: "MemoryBackend requested a new remote authority", RequestID: intentDigest, Now: now,
			})
			if activateErr != nil {
				return controller.MemoryBackendBindingResult{}, activateErr
			}
			return c.bindingResultAfterCandidate(ctx, snapshot, result.Binding,
				corev1alpha1.MemoryBackendEffectiveLifecycleActive, "Activated", "new remote memory authority is active")
		}
		if err := validateExistingBinding(existing, snapshot); err != nil {
			return controller.MemoryBackendBindingResult{}, err
		}
		switch existing.State {
		case store.MemoryBackendBindingRecovering, store.MemoryBackendBindingDecommissioned, store.MemoryBackendBindingRemoved:
			if existing.State != store.MemoryBackendBindingRecovering {
				return controller.MemoryBackendBindingResult{}, fmt.Errorf(
					"%w: %s memory backend binding cannot reconcile Active",
					store.ErrConflict,
					existing.State,
				)
			}
			if snapshot.RoutingEpoch <= existing.RoutingEpoch || !snapshot.RemoteFenceAcknowledged ||
				snapshot.AcknowledgedRoutingEpoch != snapshot.RoutingEpoch {
				return controller.MemoryBackendBindingResult{}, fmt.Errorf(
					"%w: recovering Active resume requires an acknowledged routing advance",
					store.ErrConflict,
				)
			}
			unresolved, listErr := c.hasUnresolvedOperations(ctx, existing.NamespaceUID)
			if listErr != nil {
				return controller.MemoryBackendBindingResult{}, listErr
			}
			if unresolved {
				return controller.MemoryBackendBindingResult{}, fmt.Errorf(
					"%w: unresolved fenced memory operations block recovering Active resume",
					store.ErrConflict,
				)
			}
			refreshed, refreshErr := c.refreshBinding(
				ctx, existing, snapshot, store.MemoryBackendBindingAccepting, now,
			)
			if refreshErr != nil {
				return controller.MemoryBackendBindingResult{}, refreshErr
			}
			return c.bindingResultAfterCandidate(ctx, snapshot, *refreshed,
				corev1alpha1.MemoryBackendEffectiveLifecycleActive, "Recovered", "remote memory authority resumed from its fenced recovery barrier")
		case store.MemoryBackendBindingDraining:
			if snapshot.RoutingEpoch <= existing.RoutingEpoch || !snapshot.RemoteFenceAcknowledged ||
				snapshot.AcknowledgedRoutingEpoch != snapshot.RoutingEpoch {
				return controller.MemoryBackendBindingResult{}, fmt.Errorf("%w: Active resume requires an acknowledged routing advance", store.ErrConflict)
			}
			refreshed, refreshErr := c.refreshBinding(
				ctx, existing, snapshot, store.MemoryBackendBindingAccepting, now,
			)
			if refreshErr != nil {
				return controller.MemoryBackendBindingResult{}, refreshErr
			}
			return c.bindingResultAfterCandidate(ctx, snapshot, *refreshed,
				corev1alpha1.MemoryBackendEffectiveLifecycleActive, "Active", "remote memory authority is active")
		case store.MemoryBackendBindingAccepting:
			if bindingSnapshotRouteMetadataChanged(existing, snapshot) || snapshot.RoutingEpoch > existing.RoutingEpoch {
				existing, err = c.enterDrainingBarrier(
					ctx, existing, "MemoryBackend entered durable draining state before route refresh", now,
				)
				if err != nil {
					return controller.MemoryBackendBindingResult{}, err
				}
				unresolved, listErr := c.hasUnresolvedOperations(ctx, existing.NamespaceUID)
				if listErr != nil {
					return controller.MemoryBackendBindingResult{}, listErr
				}
				message := "route-sensitive memory operations are drained; waiting for remote fence acknowledgement"
				if unresolved {
					message = "previously accepted memory operations are draining at the old routing epoch"
				}
				return bindingResult(*existing, corev1alpha1.MemoryBackendEffectiveLifecycleDraining, false,
					"Draining", message), nil
			}
		default:
			return controller.MemoryBackendBindingResult{}, fmt.Errorf("%w: memory backend binding is not activatable", store.ErrConflict)
		}
		refreshed, refreshErr := c.refreshBinding(ctx, existing, snapshot, existing.State, now)
		if refreshErr != nil {
			return controller.MemoryBackendBindingResult{}, refreshErr
		}
		return c.bindingResultAfterCandidate(ctx, snapshot, *refreshed,
			corev1alpha1.MemoryBackendEffectiveLifecycleActive, "Active", "remote memory authority is active")

	case corev1alpha1.MemoryBackendLifecycleReadOnly:
		if errors.Is(err, store.ErrNotFound) {
			return controller.MemoryBackendBindingResult{}, store.ErrNotFound
		}
		if err := validateExistingBinding(existing, snapshot); err != nil {
			return controller.MemoryBackendBindingResult{}, err
		}
		if existing.State == store.MemoryBackendBindingAccepting {
			existing, err = c.enterDrainingBarrier(ctx, existing, "MemoryBackend entered durable draining state before ReadOnly", now)
			if err != nil {
				return controller.MemoryBackendBindingResult{}, err
			}
		}
		unresolved, listErr := c.hasUnresolvedOperations(ctx, existing.NamespaceUID)
		if listErr != nil {
			return controller.MemoryBackendBindingResult{}, listErr
		}
		if unresolved {
			return bindingResult(*existing, corev1alpha1.MemoryBackendEffectiveLifecycleDraining, false,
				"Draining", "previously accepted memory operations are draining at the old routing epoch"), nil
		}
		if existing.State != store.MemoryBackendBindingDraining {
			return controller.MemoryBackendBindingResult{}, fmt.Errorf("%w: memory backend binding is not read-only compatible", store.ErrConflict)
		}
		if bindingSnapshotRouteMetadataChanged(existing, snapshot) && snapshot.RoutingEpoch <= existing.RoutingEpoch {
			return bindingResult(*existing, corev1alpha1.MemoryBackendEffectiveLifecycleDraining, false,
				"Draining", "route-sensitive memory operations are drained; waiting for remote fence acknowledgement"), nil
		}
		if snapshot.RoutingEpoch > existing.RoutingEpoch && (!snapshot.RemoteFenceAcknowledged ||
			snapshot.AcknowledgedRoutingEpoch != snapshot.RoutingEpoch) {
			return controller.MemoryBackendBindingResult{}, fmt.Errorf("%w: ReadOnly requires an acknowledged routing advance", store.ErrConflict)
		}
		refreshed, refreshErr := c.refreshBinding(ctx, existing, snapshot, store.MemoryBackendBindingDraining, now)
		if refreshErr != nil {
			return controller.MemoryBackendBindingResult{}, refreshErr
		}
		return c.bindingResultAfterCandidate(ctx, snapshot, *refreshed,
			corev1alpha1.MemoryBackendEffectiveLifecycleReadOnly, "ReadOnly", "remote memory authority is read-only")

	case corev1alpha1.MemoryBackendLifecycleDisabled:
		if errors.Is(err, store.ErrNotFound) {
			return controller.MemoryBackendBindingResult{}, fmt.Errorf(
				"%w: MemoryBackend cannot be Disabled before remote authority activation",
				store.ErrConflict,
			)
		}
		if existing.Mode == store.MemoryBackendModeLegacy && existing.State == store.MemoryBackendBindingLegacy {
			return controller.MemoryBackendBindingResult{}, fmt.Errorf(
				"%w: MemoryBackend cannot be Disabled while legacy memory remains authoritative",
				store.ErrConflict,
			)
		}
		if err := validateLifecycleBinding(existing, snapshot); err != nil {
			return controller.MemoryBackendBindingResult{}, err
		}
		switch existing.State {
		case store.MemoryBackendBindingAccepting, store.MemoryBackendBindingDraining:
			transitioned, transitionErr := c.Store.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
				NamespaceUID: existing.NamespaceUID, BackendUID: existing.BackendUID,
				ExpectedState: existing.State, State: store.MemoryBackendBindingRecovering,
				ExpectedRoutingEpoch: existing.RoutingEpoch, RoutingEpoch: existing.RoutingEpoch + 1,
				Actor: c.actor(), Reason: "MemoryBackend entered durable Disabled egress barrier",
				RequestID: intentDigest, Now: now,
			})
			if transitionErr != nil {
				return controller.MemoryBackendBindingResult{}, transitionErr
			}
			existing = transitioned
		case store.MemoryBackendBindingRecovering:
			// The durable local egress barrier already exists. Its routing epoch is
			// reused until the exact remote acknowledgement is persisted.
		default:
			return controller.MemoryBackendBindingResult{}, fmt.Errorf(
				"%w: memory backend binding is not disable-compatible", store.ErrConflict,
			)
		}
		if snapshot.RemoteFenceAcknowledged && snapshot.AcknowledgedRoutingEpoch == existing.RoutingEpoch {
			if err := c.recordRoutingFenceAcknowledgement(ctx, *existing, intentDigest, corev1alpha1.MemoryBackendLifecycleDisabled); err != nil {
				return controller.MemoryBackendBindingResult{}, err
			}
		}
		acknowledged, ackErr := c.hasRoutingFenceAcknowledgement(
			ctx, existing.NamespaceUID, existing.BackendUID, intentDigest, existing.AuthorityEpoch, existing.RoutingEpoch,
		)
		if ackErr != nil {
			return controller.MemoryBackendBindingResult{}, ackErr
		}
		if !acknowledged {
			return controller.MemoryBackendBindingResult{
				EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleRecovering,
				AuthorityEpoch:          existing.AuthorityEpoch, RoutingEpoch: existing.RoutingEpoch,
				Ready: false, Reason: "RemoteFencePending",
				Message: "waiting for exact remote routing-fence acknowledgement before Disabled becomes effective",
				Route:   durableRouteFromBinding(*existing),
			}, nil
		}
		return bindingResult(*existing, corev1alpha1.MemoryBackendEffectiveLifecycleDisabled, false,
			"Disabled", "remote memory egress is durably disabled"), nil

	case corev1alpha1.MemoryBackendLifecycleDecommissioning:
		if errors.Is(err, store.ErrNotFound) {
			return controller.MemoryBackendBindingResult{EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned}, nil
		}
		if err := validateLifecycleBinding(existing, snapshot); err != nil {
			return controller.MemoryBackendBindingResult{}, err
		}
		if existing.State == store.MemoryBackendBindingDecommissioned {
			return bindingResult(*existing, corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned, false,
				"Decommissioned", "remote memory authority is decommissioned"), nil
		}
		if existing.State == store.MemoryBackendBindingAccepting {
			existing, err = c.enterDrainingBarrier(ctx, existing, "MemoryBackend entered durable draining state before decommissioning", now)
			if err != nil {
				return controller.MemoryBackendBindingResult{}, err
			}
		}
		if existing.State != store.MemoryBackendBindingDraining && existing.State != store.MemoryBackendBindingRecovering {
			return controller.MemoryBackendBindingResult{}, fmt.Errorf("%w: memory backend binding is not decommissionable", store.ErrConflict)
		}
		targetRoutingEpoch := existing.RoutingEpoch
		if existing.State == store.MemoryBackendBindingDraining {
			targetRoutingEpoch++
		}
		if snapshot.RemoteFenceAcknowledged && snapshot.AcknowledgedRoutingEpoch == targetRoutingEpoch {
			acknowledgedBinding := *existing
			acknowledgedBinding.RoutingEpoch = targetRoutingEpoch
			if err := c.recordRoutingFenceAcknowledgement(ctx, acknowledgedBinding, intentDigest, corev1alpha1.MemoryBackendLifecycleDecommissioning); err != nil {
				return controller.MemoryBackendBindingResult{}, err
			}
		}
		acknowledged, ackErr := c.hasRoutingFenceAcknowledgement(
			ctx, existing.NamespaceUID, existing.BackendUID, intentDigest, existing.AuthorityEpoch, targetRoutingEpoch,
		)
		if ackErr != nil {
			return controller.MemoryBackendBindingResult{}, ackErr
		}
		if !acknowledged {
			result := controller.MemoryBackendBindingResult{
				EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioning,
				AuthorityEpoch:          existing.AuthorityEpoch, RoutingEpoch: targetRoutingEpoch,
				Ready: false, Reason: "RemoteFencePending",
				Message: "waiting for exact remote routing-fence acknowledgement before resolving decommission work",
				Route:   durableRouteFromBinding(*existing),
			}
			result.Route.RoutingEpoch = targetRoutingEpoch
			return result, nil
		}
		if existing.State == store.MemoryBackendBindingDraining {
			transitioned, transitionErr := c.Store.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
				NamespaceUID: existing.NamespaceUID, BackendUID: existing.BackendUID,
				ExpectedState: existing.State, State: store.MemoryBackendBindingRecovering,
				ExpectedRoutingEpoch: existing.RoutingEpoch, RoutingEpoch: targetRoutingEpoch,
				Actor: c.actor(), Reason: "MemoryBackend decommission fence acknowledgement installed the local recovery barrier",
				RequestID: intentDigest, Now: now,
			})
			if transitionErr != nil {
				return controller.MemoryBackendBindingResult{}, transitionErr
			}
			existing = transitioned
		}
		if _, resolveErr := c.Store.ResolveMemoryOperationsForDecommission(ctx, store.MemoryDecommissionResolution{
			NamespaceUID: existing.NamespaceUID, BackendUID: existing.BackendUID,
			AuthorityEpoch: existing.AuthorityEpoch, RoutingEpoch: existing.RoutingEpoch,
			Actor: c.actor(), Reason: "MemoryBackend decommission explicitly resolved fenced operations",
			RequestID: intentDigest, Now: now,
		}); resolveErr != nil {
			return controller.MemoryBackendBindingResult{}, resolveErr
		}
		transitioned, transitionErr := c.Store.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
			NamespaceUID: existing.NamespaceUID, BackendUID: existing.BackendUID,
			ExpectedState: store.MemoryBackendBindingRecovering, State: store.MemoryBackendBindingDecommissioned,
			ExpectedRoutingEpoch: existing.RoutingEpoch, RoutingEpoch: existing.RoutingEpoch,
			Actor: c.actor(), Reason: "MemoryBackend decommission completed after fenced operation resolution",
			RequestID: intentDigest, Now: now,
		})
		if transitionErr != nil {
			return controller.MemoryBackendBindingResult{}, transitionErr
		}
		return bindingResult(*transitioned, corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned, false,
			"Decommissioned", "remote memory authority is decommissioned"), nil

	default:
		return controller.MemoryBackendBindingResult{}, fmt.Errorf("unsupported requested lifecycle %q", snapshot.RequestedLifecycle)
	}
}

// FinalizeMemoryBackendDeletion permits finalizer removal only for never-activated
// resources, terminal bindings, or the irreversible namespace-termination orphan path.
//
//nolint:gocyclo // Deletion convergence intentionally enumerates every fail-closed terminal path.
func (c *StoreCoordinator) FinalizeMemoryBackendDeletion(
	ctx context.Context,
	snapshot controller.MemoryBackendDeletionSnapshot,
) (controller.MemoryBackendDeletionResult, error) {
	if c == nil || c.Store == nil {
		return controller.MemoryBackendDeletionResult{}, fmt.Errorf("memory backend store is not configured")
	}
	binding, err := c.lookupDeletionBinding(ctx, snapshot)
	bindingMissing := errors.Is(err, store.ErrNotFound)
	if err != nil && !bindingMissing {
		return controller.MemoryBackendDeletionResult{}, err
	}
	if snapshot.NamespaceUID == "" && binding != nil {
		snapshot.NamespaceUID = binding.NamespaceUID
	}

	candidates, historyErr := c.outstandingValidationCandidates(ctx, snapshot)
	if historyErr != nil && snapshot.NamespaceUID != "" {
		return controller.MemoryBackendDeletionResult{}, historyErr
	}
	if snapshot.NamespaceTerminating {
		for _, candidate := range candidates {
			if err := c.recordValidationCandidateRemoval(
				ctx, snapshot, candidate,
				"claimed validation candidate was irreversibly orphaned during namespace termination",
			); err != nil {
				return controller.MemoryBackendDeletionResult{}, err
			}
		}
		if bindingMissing || binding == nil ||
			(binding.Mode == store.MemoryBackendModeLegacy && binding.State == store.MemoryBackendBindingLegacy) {
			return controller.MemoryBackendDeletionResult{
				SafeToRemove: true, EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleRemoved,
				Reason: "NamespaceTerminated", Message: "namespace termination retired all local candidate ownership",
			}, nil
		}
		removed, removeErr := c.forceRemoveBindingForNamespaceTermination(ctx, binding)
		if removeErr != nil {
			return controller.MemoryBackendDeletionResult{}, removeErr
		}
		return controller.MemoryBackendDeletionResult{
			SafeToRemove: true, EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleRemoved,
			AuthorityEpoch: removed.AuthorityEpoch, RoutingEpoch: removed.RoutingEpoch,
			Reason: "NamespaceTerminated", Message: "namespace termination forced an irreversible local Removed barrier",
			Route: durableRouteFromBinding(*removed),
		}, nil
	}

	if len(candidates) > 0 {
		next := candidates[0]
		return controller.MemoryBackendDeletionResult{
			AuthorityEpoch: next.authorityEpoch, RoutingEpoch: next.routingEpoch, CandidateDigest: next.digest,
			EffectiveLifecycleState: corev1alpha1.MemoryBackendEffectiveLifecycleValidating,
			Reason:                  "CandidateFencePending", Message: "claimed validation candidate must be remotely fenced before deletion",
		}, nil
	}

	if bindingMissing || binding == nil {
		return controller.MemoryBackendDeletionResult{
			SafeToRemove: true, Reason: "NeverActivated", Message: "backend never established remote authority",
		}, nil
	}
	if binding.BackendUID != snapshot.BackendUID {
		if binding.State == store.MemoryBackendBindingDecommissioned || binding.State == store.MemoryBackendBindingRemoved ||
			(binding.Mode == store.MemoryBackendModeLegacy && binding.State == store.MemoryBackendBindingLegacy) {
			return controller.MemoryBackendDeletionResult{
				SafeToRemove: true, Reason: "NeverActivatedReplacement",
				Message: "replacement backend never activated over the unrelated terminal binding",
			}, nil
		}
		return controller.MemoryBackendDeletionResult{
			Reason: "DeletionBlocked", Message: "an unrelated nonterminal remote binding still owns the namespace",
		}, nil
	}

	result := controller.MemoryBackendDeletionResult{
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		Route: durableRouteFromBinding(*binding),
	}
	if binding.Mode == store.MemoryBackendModeLegacy && binding.State == store.MemoryBackendBindingLegacy {
		result.SafeToRemove = true
		result.Reason = "NeverActivated"
		result.Message = "backend never established remote authority"
		return result, nil
	}
	switch binding.State {
	case store.MemoryBackendBindingDecommissioned:
		result.SafeToRemove = true
		result.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleDecommissioned
		result.Reason = "Decommissioned"
		result.Message = "decommissioned backend is safe to delete"
	case store.MemoryBackendBindingRemoved:
		result.SafeToRemove = true
		result.EffectiveLifecycleState = corev1alpha1.MemoryBackendEffectiveLifecycleRemoved
		result.Reason = "Removed"
		result.Message = "orphaned backend is safe to delete"
	default:
		result.Reason = "DeletionBlocked"
		result.Message = "decommission or force-orphan must complete before deletion"
	}
	return result, nil
}

func (c *StoreCoordinator) lookupDeletionBinding(
	ctx context.Context,
	snapshot controller.MemoryBackendDeletionSnapshot,
) (*store.MemoryBackendBinding, error) {
	if strings.TrimSpace(snapshot.NamespaceUID) != "" {
		binding, err := c.Store.GetMemoryBackendBinding(ctx, snapshot.NamespaceUID)
		if err == nil || !errors.Is(err, store.ErrNotFound) {
			return binding, err
		}
	}
	if strings.TrimSpace(snapshot.Namespace) == "" {
		return nil, store.ErrNotFound
	}
	return c.Store.GetMemoryBackendBindingByNamespace(ctx, snapshot.Namespace)
}

func (c *StoreCoordinator) forceRemoveBindingForNamespaceTermination(
	ctx context.Context,
	binding *store.MemoryBackendBinding,
) (*store.MemoryBackendBinding, error) {
	if binding == nil {
		return nil, store.ErrNotFound
	}
	now := c.now()
	current := binding
	switch current.State {
	case store.MemoryBackendBindingRemoved:
		copy := *current
		return &copy, nil
	case store.MemoryBackendBindingAccepting, store.MemoryBackendBindingDraining:
		transitioned, err := c.Store.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
			NamespaceUID: current.NamespaceUID, BackendUID: current.BackendUID,
			ExpectedState: current.State, State: store.MemoryBackendBindingRecovering,
			ExpectedRoutingEpoch: current.RoutingEpoch, RoutingEpoch: current.RoutingEpoch + 1,
			Actor: c.actor(), Reason: "namespace termination installed a local egress barrier", Now: now,
		})
		if err != nil {
			return nil, err
		}
		current = transitioned
	case store.MemoryBackendBindingRecovering, store.MemoryBackendBindingDecommissioned:
		// Already fail-closed for egress.
	default:
		return nil, fmt.Errorf("%w: namespace termination cannot orphan binding state %s", store.ErrConflict, current.State)
	}
	if _, err := c.Store.OrphanMemoryOperations(ctx, store.MemoryOperationOrphaning{
		NamespaceUID: current.NamespaceUID, BackendUID: current.BackendUID,
		AuthorityEpoch: current.AuthorityEpoch, RoutingEpoch: current.RoutingEpoch,
		Actor: c.actor(), Reason: "namespace termination orphaned unresolved remote memory work", Now: now,
	}); err != nil {
		return nil, err
	}
	if current.State == store.MemoryBackendBindingRemoved {
		return current, nil
	}
	return c.Store.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: current.NamespaceUID, BackendUID: current.BackendUID,
		ExpectedState: current.State, State: store.MemoryBackendBindingRemoved,
		ExpectedRoutingEpoch: current.RoutingEpoch, RoutingEpoch: current.RoutingEpoch + 1,
		Actor: c.actor(), Reason: "namespace termination finalized irreversible Removed state", Now: now,
	})
}

func (c *StoreCoordinator) refreshBinding(
	ctx context.Context,
	existing *store.MemoryBackendBinding,
	snapshot controller.MemoryBackendBindingSnapshot,
	targetState store.MemoryBackendBindingState,
	now time.Time,
) (*store.MemoryBackendBinding, error) {
	desired := desiredBindingFromSnapshot(existing, snapshot, targetState)
	if snapshot.RoutingEpoch < existing.RoutingEpoch {
		return nil, fmt.Errorf("%w: validated routing epoch is stale", store.ErrConflict)
	}
	if bindingRouteMetadataChanged(existing, &desired) && snapshot.RoutingEpoch == existing.RoutingEpoch {
		return nil, fmt.Errorf("%w: route-sensitive validation refresh requires an acknowledged routing advance", store.ErrConflict)
	}
	if !bindingMetadataChanged(existing, &desired) && snapshot.RoutingEpoch == existing.RoutingEpoch && targetState == existing.State {
		copy := *existing
		return &copy, nil
	}
	return c.Store.RefreshMemoryBackendBinding(ctx, store.MemoryBackendBindingRefresh{
		Binding: desired, ExpectedRoutingEpoch: existing.RoutingEpoch,
		Actor: c.actor(), Reason: "MemoryBackend validation refreshed", Now: now,
	})
}

func desiredBindingFromSnapshot(
	existing *store.MemoryBackendBinding,
	snapshot controller.MemoryBackendBindingSnapshot,
	targetState store.MemoryBackendBindingState,
) store.MemoryBackendBinding {
	desired := bindingFromSnapshot(snapshot, targetState)
	desired.ActivatedAt = existing.ActivatedAt
	desired.ActivationEpoch = existing.ActivationEpoch
	desired.MinimumFeatureEpoch = existing.MinimumFeatureEpoch
	desired.Mode = existing.Mode
	return desired
}

func bindingSnapshotRouteMetadataChanged(
	existing *store.MemoryBackendBinding,
	snapshot controller.MemoryBackendBindingSnapshot,
) bool {
	desired := desiredBindingFromSnapshot(existing, snapshot, existing.State)
	return bindingRouteMetadataChanged(existing, &desired)
}

func (c *StoreCoordinator) enterDrainingBarrier(
	ctx context.Context,
	binding *store.MemoryBackendBinding,
	reason string,
	now time.Time,
) (*store.MemoryBackendBinding, error) {
	if binding == nil {
		return nil, store.ErrNotFound
	}
	if binding.State == store.MemoryBackendBindingDraining {
		copy := *binding
		return &copy, nil
	}
	if binding.State != store.MemoryBackendBindingAccepting {
		return nil, fmt.Errorf("%w: memory backend binding cannot enter draining from %s", store.ErrConflict, binding.State)
	}
	return c.Store.TransitionMemoryBackendBinding(ctx, store.MemoryBackendTransition{
		NamespaceUID: binding.NamespaceUID, BackendUID: binding.BackendUID,
		ExpectedState: binding.State, State: store.MemoryBackendBindingDraining,
		ExpectedRoutingEpoch: binding.RoutingEpoch, RoutingEpoch: binding.RoutingEpoch,
		Actor: c.actor(), Reason: reason, Now: now,
	})
}

func durableRouteFromBinding(binding store.MemoryBackendBinding) controller.MemoryBackendDurableRoute {
	return controller.MemoryBackendDurableRoute{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, ClusterID: binding.ClusterID,
		BackendUID: binding.BackendUID, BackendGeneration: binding.BackendGeneration,
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		SpecDigest: binding.SpecDigest, EndpointDigest: binding.EndpointDigest,
		ResolvedAddressDigest: binding.ResolvedAddressDigest, ServerCertificateDigest: binding.ServerCertificateDigest,
		SecretName: binding.SecretName, SecretKey: binding.SecretKey,
		SecretUID: binding.SecretUID, SecretResourceVersion: binding.SecretResourceVersion,
		TenantID: binding.TenantID, StoreName: binding.StoreName, StoreUUID: binding.StoreUUID,
		Protocol: binding.Protocol,
	}
}

func (c *StoreCoordinator) hasRoutingFenceAcknowledgement(
	ctx context.Context,
	namespaceUID, backendUID, intentDigest string,
	authorityEpoch, routingEpoch int64,
) (bool, error) {
	found := false
	err := c.visitMemoryAudit(ctx, namespaceUID, func(record store.MemoryAuditRecord) bool {
		if record.Action == memoryBackendRoutingFenceAckAuditAction && record.RequestID == backendUID &&
			record.RequestDigest == intentDigest && record.AuthorityEpoch == authorityEpoch && record.RoutingEpoch == routingEpoch {
			found = true
			return false
		}
		return true
	})
	return found, err
}

func (c *StoreCoordinator) recordRoutingFenceAcknowledgement(
	ctx context.Context,
	binding store.MemoryBackendBinding,
	intentDigest string,
	target corev1alpha1.MemoryBackendLifecycleState,
) error {
	found, err := c.hasRoutingFenceAcknowledgement(
		ctx, binding.NamespaceUID, binding.BackendUID, intentDigest, binding.AuthorityEpoch, binding.RoutingEpoch,
	)
	if err != nil || found {
		return err
	}
	return c.Store.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: binding.Namespace, NamespaceUID: binding.NamespaceUID, Actor: c.actor(),
		Action: memoryBackendRoutingFenceAckAuditAction,
		Reason: "exact OMS routing fence acknowledgement persisted", NewState: string(target),
		AuthorityEpoch: binding.AuthorityEpoch, RoutingEpoch: binding.RoutingEpoch,
		RequestDigest: intentDigest, RequestID: binding.BackendUID, CreatedAt: c.now(),
	})
}

const (
	memoryValidationCandidateAuditAction    = "backend.validation.candidate"
	memoryValidationClaimAttemptAuditAction = "backend.validation.claim_attempt"
	memoryValidationClaimedAuditAction      = "backend.validation.claimed"
	memoryValidationRemovedAuditAction      = "backend.validation.removed"
	memoryValidationIncorporatedAuditAction = "backend.validation.incorporated"

	memoryValidationCandidateTrackingState      = "claim-tracked"
	memoryValidationCandidateIncorporatedReason = "claimed validation candidate was incorporated into the durable binding"
)

//nolint:gocyclo // Candidate recovery validates durable history and every identity edge explicitly.
func (c *StoreCoordinator) persistValidationCandidate(
	ctx context.Context,
	snapshot controller.MemoryBackendValidationSnapshot,
	authorityEpoch, routingEpoch int64,
	advanceAuthorityFromHistory bool,
) (controller.MemoryBackendValidationBinding, error) {
	digest := validationCandidateDigest(snapshot)
	outstanding, err := c.outstandingValidationCandidates(ctx, controller.MemoryBackendDeletionSnapshot{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, BackendUID: snapshot.BackendUID,
	})
	if err != nil {
		return controller.MemoryBackendValidationBinding{}, err
	}
	for _, candidate := range outstanding {
		if candidate.digest == digest && candidate.authorityEpoch >= authorityEpoch && candidate.routingEpoch >= routingEpoch {
			return controller.MemoryBackendValidationBinding{
				AuthorityEpoch: candidate.authorityEpoch, RoutingEpoch: candidate.routingEpoch, CandidateDigest: digest,
			}, nil
		}
	}
	for _, candidate := range outstanding {
		if candidate.authorityEpoch > authorityEpoch || candidate.routingEpoch >= routingEpoch {
			return controller.MemoryBackendValidationBinding{}, fmt.Errorf(
				"%w: a previously claimed validation candidate must be retired before route rotation", store.ErrConflict,
			)
		}
	}
	var (
		cursorTime   *time.Time
		cursorID     string
		maxAuthority int64
		maxRouting   int64
		matches      []validationCandidateEpoch
	)
	retired := make(map[validationCandidateEpoch]struct{})
	retiredEpoch := make(map[[2]int64]struct{})
	for {
		records, err := c.Store.ListMemoryAudit(ctx, store.MemoryAuditFilter{
			NamespaceUID: snapshot.NamespaceUID, BeforeCreatedAt: cursorTime, BeforeID: cursorID, Limit: 100,
		})
		if err != nil {
			return controller.MemoryBackendValidationBinding{}, err
		}
		for _, record := range records {
			if record.Action != memoryValidationCandidateAuditAction && record.Action != memoryValidationRemovedAuditAction {
				continue
			}
			candidate := validationCandidateEpoch{authorityEpoch: record.AuthorityEpoch, routingEpoch: record.RoutingEpoch, digest: record.RequestDigest}
			if !candidate.valid() {
				continue
			}
			maxAuthority = max(maxAuthority, candidate.authorityEpoch)
			maxRouting = max(maxRouting, candidate.routingEpoch)
			if record.Action == memoryValidationRemovedAuditAction {
				if record.RequestDigest == "" {
					retiredEpoch[[2]int64{candidate.authorityEpoch, candidate.routingEpoch}] = struct{}{}
				} else {
					retired[candidate] = struct{}{}
				}
				continue
			}
			if record.RequestID == snapshot.BackendUID && record.RequestDigest == digest {
				matches = append(matches, candidate)
			}
		}
		if len(records) < 100 {
			break
		}
		last := records[len(records)-1]
		cursor := last.CreatedAt.UTC()
		cursorTime = &cursor
		cursorID = last.ID
	}

	currentAuthority := authorityEpoch
	if advanceAuthorityFromHistory && maxAuthority > currentAuthority {
		currentAuthority = maxAuthority
	}
	for _, candidate := range matches {
		_, wasRetired := retired[candidate]
		if _, retiredAll := retiredEpoch[[2]int64{candidate.authorityEpoch, candidate.routingEpoch}]; retiredAll {
			wasRetired = true
		}
		authorityCurrent := candidate.authorityEpoch == authorityEpoch
		if advanceAuthorityFromHistory {
			authorityCurrent = candidate.authorityEpoch == currentAuthority
		}
		if !wasRetired && authorityCurrent && candidate.routingEpoch >= routingEpoch && candidate.routingEpoch == maxRouting {
			return controller.MemoryBackendValidationBinding{
				AuthorityEpoch:  candidate.authorityEpoch,
				RoutingEpoch:    candidate.routingEpoch,
				CandidateDigest: digest,
			}, nil
		}
	}

	if advanceAuthorityFromHistory && maxAuthority >= authorityEpoch {
		authorityEpoch = maxAuthority + 1
	}
	if maxRouting >= routingEpoch {
		routingEpoch = maxRouting + 1
	}
	if err := c.Store.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, Actor: c.actor(),
		Action: memoryValidationCandidateAuditAction, Reason: "durable OMS validation candidate prepared",
		PreviousState: memoryValidationCandidateTrackingState, NewState: string(snapshot.RequestedLifecycle),
		AuthorityEpoch: authorityEpoch, RoutingEpoch: routingEpoch,
		RequestDigest: digest, RequestID: snapshot.BackendUID, CreatedAt: c.now(),
	}); err != nil {
		return controller.MemoryBackendValidationBinding{}, err
	}
	return controller.MemoryBackendValidationBinding{
		AuthorityEpoch: authorityEpoch, RoutingEpoch: routingEpoch, CandidateDigest: digest,
	}, nil
}

func (c *StoreCoordinator) persistValidationCandidateAtExactEpoch(
	ctx context.Context,
	snapshot controller.MemoryBackendValidationSnapshot,
	authorityEpoch, routingEpoch int64,
) (controller.MemoryBackendValidationBinding, error) {
	digest := validationCandidateDigest(snapshot)
	outstanding, err := c.outstandingValidationCandidates(ctx, controller.MemoryBackendDeletionSnapshot{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, BackendUID: snapshot.BackendUID,
	})
	if err != nil {
		return controller.MemoryBackendValidationBinding{}, err
	}
	for _, candidate := range outstanding {
		if candidate.digest == digest && candidate.authorityEpoch == authorityEpoch && candidate.routingEpoch == routingEpoch {
			return controller.MemoryBackendValidationBinding{
				AuthorityEpoch: authorityEpoch, RoutingEpoch: routingEpoch, CandidateDigest: digest,
			}, nil
		}
	}
	if len(outstanding) > 0 {
		return controller.MemoryBackendValidationBinding{}, fmt.Errorf(
			"%w: a previously claimed validation candidate must be retired before route rotation", store.ErrConflict,
		)
	}
	found := false
	retired := false
	incorporated := false
	err = c.visitMemoryAudit(ctx, snapshot.NamespaceUID, func(record store.MemoryAuditRecord) bool {
		if record.RequestID != snapshot.BackendUID || record.AuthorityEpoch != authorityEpoch || record.RoutingEpoch != routingEpoch {
			return true
		}
		switch record.Action {
		case memoryValidationCandidateAuditAction:
			if record.RequestDigest == digest {
				found = true
			}
		case memoryValidationIncorporatedAuditAction:
			if record.RequestDigest == digest {
				incorporated = true
			}
		case memoryValidationRemovedAuditAction:
			if record.RequestDigest == "" || record.RequestDigest == digest {
				retired = true
			}
		}
		return !found || !retired
	})
	if err != nil {
		return controller.MemoryBackendValidationBinding{}, err
	}
	if incorporated {
		return controller.MemoryBackendValidationBinding{
			AuthorityEpoch: authorityEpoch, RoutingEpoch: routingEpoch, CandidateDigest: digest,
		}, nil
	}
	if retired {
		return controller.MemoryBackendValidationBinding{}, fmt.Errorf("%w: validation candidate was already retired", store.ErrConflict)
	}
	if !found {
		if err := c.Store.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
			Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, Actor: c.actor(),
			Action: memoryValidationCandidateAuditAction, Reason: "durable OMS validation candidate prepared",
			PreviousState: memoryValidationCandidateTrackingState, NewState: string(snapshot.RequestedLifecycle),
			AuthorityEpoch: authorityEpoch, RoutingEpoch: routingEpoch, RequestDigest: digest,
			RequestID: snapshot.BackendUID, CreatedAt: c.now(),
		}); err != nil {
			return controller.MemoryBackendValidationBinding{}, err
		}
	}
	return controller.MemoryBackendValidationBinding{
		AuthorityEpoch: authorityEpoch, RoutingEpoch: routingEpoch, CandidateDigest: digest,
	}, nil
}

func validationCandidateDigest(snapshot controller.MemoryBackendValidationSnapshot) string {
	parts := []string{
		snapshot.Namespace, snapshot.NamespaceUID, snapshot.BackendUID, fmt.Sprintf("%d", snapshot.BackendGeneration),
		snapshot.ClusterID, snapshot.TenantID, string(snapshot.RequestedLifecycle), snapshot.SpecDigest,
		snapshot.EndpointDigest, snapshot.ResolvedAddressDigest, snapshot.ServerCertificateDigest,
		snapshot.SecretName, snapshot.SecretKey, snapshot.SecretUID, snapshot.SecretResourceVersion,
		snapshot.StoreName, snapshot.StoreUUID, snapshot.Protocol,
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "sha256:" + hex.EncodeToString(sum[:])
}

type validationCandidateEpoch struct {
	authorityEpoch int64
	routingEpoch   int64
	digest         string
}

func (candidate validationCandidateEpoch) valid() bool {
	return candidate.authorityEpoch > 0 && candidate.routingEpoch > 0
}

// RecordMemoryBackendOwnershipClaimAttempt durably marks the crash window
// immediately before the controller may send an ownership-claim request.
func (c *StoreCoordinator) RecordMemoryBackendOwnershipClaimAttempt(
	ctx context.Context,
	snapshot controller.MemoryBackendOwnershipClaimAttemptSnapshot,
) error {
	if c == nil || c.Store == nil {
		return fmt.Errorf("memory backend store is not configured")
	}
	if snapshot.RequestedLifecycle != corev1alpha1.MemoryBackendLifecycleActive &&
		snapshot.RequestedLifecycle != corev1alpha1.MemoryBackendLifecycleReadOnly {
		return store.ValidationErrorf("ownership claim attempts require Active or ReadOnly lifecycle")
	}
	candidate := validationCandidateEpoch{
		authorityEpoch: snapshot.AuthorityEpoch, routingEpoch: snapshot.RoutingEpoch, digest: snapshot.CandidateDigest,
	}
	if strings.TrimSpace(snapshot.Namespace) == "" || strings.TrimSpace(snapshot.NamespaceUID) == "" ||
		strings.TrimSpace(snapshot.BackendUID) == "" || !candidate.valid() || strings.TrimSpace(snapshot.CandidateDigest) == "" {
		return store.ValidationErrorf("ownership claim attempt requires namespace, backend, candidate digest, and positive candidate epochs")
	}
	prepared := false
	alreadyRecorded := false
	removed := false
	err := c.visitMemoryAudit(ctx, snapshot.NamespaceUID, func(record store.MemoryAuditRecord) bool {
		if record.RequestID != snapshot.BackendUID || record.AuthorityEpoch != candidate.authorityEpoch ||
			record.RoutingEpoch != candidate.routingEpoch {
			return true
		}
		switch record.Action {
		case memoryValidationCandidateAuditAction:
			prepared = record.NewState == string(snapshot.RequestedLifecycle) && record.RequestDigest == snapshot.CandidateDigest
		case memoryValidationClaimAttemptAuditAction, memoryValidationClaimedAuditAction:
			alreadyRecorded = record.RequestDigest == snapshot.CandidateDigest
		case memoryValidationRemovedAuditAction:
			removed = record.RequestDigest == "" || record.RequestDigest == snapshot.CandidateDigest
		}
		return true
	})
	if err != nil {
		return err
	}
	if removed {
		return fmt.Errorf("%w: validation candidate was already retired", store.ErrConflict)
	}
	if alreadyRecorded {
		return nil
	}
	if !prepared {
		return fmt.Errorf("%w: validation candidate was not durably prepared", store.ErrConflict)
	}
	return c.Store.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, Actor: c.actor(),
		Action: memoryValidationClaimAttemptAuditAction, Reason: "OMS validation candidate ownership claim may be sent",
		PreviousState: string(store.MemoryBackendBindingValidating), NewState: string(snapshot.RequestedLifecycle),
		AuthorityEpoch: candidate.authorityEpoch, RoutingEpoch: candidate.routingEpoch,
		RequestDigest: snapshot.CandidateDigest, RequestID: snapshot.BackendUID, CreatedAt: c.now(),
	})
}

func (c *StoreCoordinator) recordValidationCandidateClaim(
	ctx context.Context,
	snapshot controller.MemoryBackendBindingSnapshot,
) error {
	if snapshot.RequestedLifecycle != corev1alpha1.MemoryBackendLifecycleActive &&
		snapshot.RequestedLifecycle != corev1alpha1.MemoryBackendLifecycleReadOnly {
		return nil
	}
	candidate := validationCandidateEpoch{
		authorityEpoch: snapshot.AuthorityEpoch, routingEpoch: snapshot.RoutingEpoch, digest: snapshot.CandidateDigest,
	}
	if !candidate.valid() || strings.TrimSpace(snapshot.OwnershipClaimIdentity) == "" || strings.TrimSpace(snapshot.CandidateDigest) == "" {
		return store.ValidationErrorf("claimed validation candidate identity, digest, and epochs are required")
	}
	found := false
	attemptFound := false
	err := c.visitMemoryAudit(ctx, snapshot.NamespaceUID, func(record store.MemoryAuditRecord) bool {
		if record.RequestID != snapshot.BackendUID || record.AuthorityEpoch != candidate.authorityEpoch ||
			record.RoutingEpoch != candidate.routingEpoch || record.RequestDigest != snapshot.CandidateDigest {
			return true
		}
		switch record.Action {
		case memoryValidationClaimAttemptAuditAction:
			attemptFound = true
		case memoryValidationClaimedAuditAction:
			found = true
		}
		return !found || !attemptFound
	})
	if err != nil || found {
		return err
	}
	if !attemptFound {
		return fmt.Errorf("%w: validation candidate claim lacks a durable send-attempt marker", store.ErrConflict)
	}
	return c.Store.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, Actor: c.actor(),
		Action: memoryValidationClaimedAuditAction, Reason: "OMS validation candidate ownership was claimed",
		PreviousState: string(store.MemoryBackendBindingValidating), NewState: "claimed",
		AuthorityEpoch: candidate.authorityEpoch, RoutingEpoch: candidate.routingEpoch,
		RequestDigest: snapshot.CandidateDigest, RequestID: snapshot.BackendUID, CreatedAt: c.now(),
	})
}

func (c *StoreCoordinator) outstandingValidationCandidates(
	ctx context.Context,
	snapshot controller.MemoryBackendDeletionSnapshot,
) ([]validationCandidateEpoch, error) {
	type epochKey struct {
		authorityEpoch int64
		routingEpoch   int64
	}
	prepared := make(map[epochKey]map[string]struct{})
	claimed := make(map[validationCandidateEpoch]struct{})
	legacyClaims := make(map[epochKey]struct{})
	removed := make(map[validationCandidateEpoch]struct{})
	removedEpochs := make(map[epochKey]struct{})
	err := c.visitMemoryAudit(ctx, snapshot.NamespaceUID, func(record store.MemoryAuditRecord) bool {
		if record.RequestID != snapshot.BackendUID {
			return true
		}
		key := epochKey{authorityEpoch: record.AuthorityEpoch, routingEpoch: record.RoutingEpoch}
		candidate := validationCandidateEpoch{
			authorityEpoch: record.AuthorityEpoch, routingEpoch: record.RoutingEpoch, digest: record.RequestDigest,
		}
		if !candidate.valid() {
			return true
		}
		switch record.Action {
		case memoryValidationCandidateAuditAction:
			if prepared[key] == nil {
				prepared[key] = make(map[string]struct{})
			}
			prepared[key][record.RequestDigest] = struct{}{}
			// Pre-claim-tracking releases could claim every non-Staged candidate
			// after this record was written. Preserve that history fail-closed.
			if record.PreviousState != memoryValidationCandidateTrackingState &&
				record.NewState != string(corev1alpha1.MemoryBackendLifecycleStaged) {
				legacyClaims[key] = struct{}{}
			}
		case memoryValidationClaimAttemptAuditAction:
			if record.NewState == string(corev1alpha1.MemoryBackendLifecycleActive) ||
				record.NewState == string(corev1alpha1.MemoryBackendLifecycleReadOnly) {
				if record.RequestDigest == "" {
					legacyClaims[key] = struct{}{}
				} else {
					claimed[candidate] = struct{}{}
				}
			}
		case memoryValidationClaimedAuditAction:
			if record.RequestDigest == "" {
				legacyClaims[key] = struct{}{}
			} else {
				claimed[candidate] = struct{}{}
			}
		case memoryValidationRemovedAuditAction, memoryValidationIncorporatedAuditAction:
			if record.RequestDigest == "" {
				removedEpochs[key] = struct{}{}
			} else {
				removed[candidate] = struct{}{}
			}
		}
		return true
	})
	if err != nil {
		return nil, err
	}
	for key := range legacyClaims {
		digests := prepared[key]
		if len(digests) == 0 {
			claimed[validationCandidateEpoch{authorityEpoch: key.authorityEpoch, routingEpoch: key.routingEpoch}] = struct{}{}
			continue
		}
		for digest := range digests {
			claimed[validationCandidateEpoch{authorityEpoch: key.authorityEpoch, routingEpoch: key.routingEpoch, digest: digest}] = struct{}{}
		}
	}
	fallback := validationCandidateEpoch{authorityEpoch: snapshot.AuthorityEpoch, routingEpoch: snapshot.RoutingEpoch}
	if strings.TrimSpace(snapshot.OwnershipClaimIdentity) != "" && fallback.valid() {
		claimed[fallback] = struct{}{}
	}
	for candidate := range claimed {
		key := epochKey{authorityEpoch: candidate.authorityEpoch, routingEpoch: candidate.routingEpoch}
		if _, removedAll := removedEpochs[key]; removedAll {
			delete(claimed, candidate)
			continue
		}
		if _, wasRemoved := removed[candidate]; wasRemoved {
			delete(claimed, candidate)
		}
	}
	candidates := make([]validationCandidateEpoch, 0, len(claimed))
	for candidate := range claimed {
		candidates = append(candidates, candidate)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].authorityEpoch != candidates[j].authorityEpoch {
			return candidates[i].authorityEpoch < candidates[j].authorityEpoch
		}
		if candidates[i].routingEpoch != candidates[j].routingEpoch {
			return candidates[i].routingEpoch < candidates[j].routingEpoch
		}
		return candidates[i].digest < candidates[j].digest
	})
	return candidates, nil
}

func (c *StoreCoordinator) visitMemoryAudit(
	ctx context.Context,
	namespaceUID string,
	visit func(store.MemoryAuditRecord) bool,
) error {
	var (
		cursorTime *time.Time
		cursorID   string
	)
	for {
		records, err := c.Store.ListMemoryAudit(ctx, store.MemoryAuditFilter{
			NamespaceUID: namespaceUID, BeforeCreatedAt: cursorTime, BeforeID: cursorID, Limit: 100,
		})
		if err != nil {
			return err
		}
		for _, record := range records {
			if !visit(record) {
				return nil
			}
		}
		if len(records) < 100 {
			return nil
		}
		last := records[len(records)-1]
		cursor := last.CreatedAt.UTC()
		cursorTime = &cursor
		cursorID = last.ID
	}
}

func (c *StoreCoordinator) recordValidationCandidateIncorporation(
	ctx context.Context,
	snapshot controller.MemoryBackendDeletionSnapshot,
	candidate validationCandidateEpoch,
) error {
	found := false
	err := c.visitMemoryAudit(ctx, snapshot.NamespaceUID, func(record store.MemoryAuditRecord) bool {
		if record.Action == memoryValidationIncorporatedAuditAction && record.RequestID == snapshot.BackendUID &&
			record.AuthorityEpoch == candidate.authorityEpoch && record.RoutingEpoch == candidate.routingEpoch &&
			record.RequestDigest == candidate.digest {
			found = true
			return false
		}
		return true
	})
	if err != nil || found {
		return err
	}
	return c.Store.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, Actor: c.actor(),
		Action: memoryValidationIncorporatedAuditAction, Reason: memoryValidationCandidateIncorporatedReason,
		PreviousState: string(store.MemoryBackendBindingValidating), NewState: "incorporated",
		AuthorityEpoch: candidate.authorityEpoch, RoutingEpoch: candidate.routingEpoch,
		RequestDigest: candidate.digest, RequestID: snapshot.BackendUID, CreatedAt: c.now(),
	})
}

func (c *StoreCoordinator) recordValidationCandidateRemoval(
	ctx context.Context,
	snapshot controller.MemoryBackendDeletionSnapshot,
	candidate validationCandidateEpoch,
	reason string,
) error {
	found := false
	err := c.visitMemoryAudit(ctx, snapshot.NamespaceUID, func(record store.MemoryAuditRecord) bool {
		if record.Action == memoryValidationRemovedAuditAction && record.RequestID == snapshot.BackendUID &&
			record.AuthorityEpoch == candidate.authorityEpoch && record.RoutingEpoch == candidate.routingEpoch &&
			(record.RequestDigest == candidate.digest || record.RequestDigest == "") {
			found = true
			return false
		}
		return true
	})
	if err != nil || found {
		return err
	}
	if strings.TrimSpace(reason) == "" {
		reason = "claimed validation candidate was remotely fenced before deletion"
	}
	newState := string(store.MemoryBackendBindingRemoved)
	if reason == memoryValidationCandidateIncorporatedReason {
		newState = "incorporated"
	}
	return c.Store.AppendMemoryAudit(ctx, store.MemoryAuditRecord{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, Actor: c.actor(),
		Action: memoryValidationRemovedAuditAction, Reason: reason,
		PreviousState: string(store.MemoryBackendBindingValidating), NewState: newState,
		AuthorityEpoch: candidate.authorityEpoch, RoutingEpoch: candidate.routingEpoch,
		RequestDigest: candidate.digest, RequestID: snapshot.BackendUID, CreatedAt: c.now(),
	})
}

// RetireMemoryBackendValidationCandidate retires an exact candidate only after
// the remote fence is acknowledged, unless namespace termination explicitly
// selects the irreversible local orphan path.
func (c *StoreCoordinator) RetireMemoryBackendValidationCandidate(
	ctx context.Context,
	retirement controller.MemoryBackendValidationCandidateRetirement,
) error {
	if c == nil || c.Store == nil {
		return fmt.Errorf("memory backend store is not configured")
	}
	candidate := validationCandidateEpoch{
		authorityEpoch: retirement.AuthorityEpoch,
		routingEpoch:   retirement.RoutingEpoch,
		digest:         strings.TrimSpace(retirement.CandidateDigest),
	}
	if strings.TrimSpace(retirement.Namespace) == "" || strings.TrimSpace(retirement.NamespaceUID) == "" ||
		strings.TrimSpace(retirement.BackendUID) == "" || !candidate.valid() {
		return store.ValidationErrorf("candidate retirement requires exact namespace, backend, and candidate epochs")
	}
	deletion := controller.MemoryBackendDeletionSnapshot{
		Namespace: retirement.Namespace, NamespaceUID: retirement.NamespaceUID, BackendUID: retirement.BackendUID,
	}
	candidates, err := c.outstandingValidationCandidates(ctx, deletion)
	if err != nil {
		return err
	}
	var matched *validationCandidateEpoch
	for index := range candidates {
		current := candidates[index]
		if current.authorityEpoch == candidate.authorityEpoch && current.routingEpoch == candidate.routingEpoch &&
			(candidate.digest == "" || current.digest == candidate.digest) {
			matched = &current
			break
		}
	}
	if matched == nil {
		return nil
	}
	if !retirement.RemoteFenceAcknowledged && !retirement.NamespaceTerminating {
		return fmt.Errorf("%w: claimed validation candidate requires exact remote fence acknowledgement", store.ErrConflict)
	}
	reason := "claimed validation candidate was remotely fenced and retired"
	if retirement.NamespaceTerminating {
		reason = "claimed validation candidate was irreversibly orphaned during namespace termination"
	}
	return c.recordValidationCandidateRemoval(ctx, deletion, *matched, reason)
}

func (c *StoreCoordinator) hasUnresolvedOperations(ctx context.Context, namespaceUID string) (bool, error) {
	operations, err := c.Store.ListMemoryOperations(ctx, store.MemoryOperationFilter{
		NamespaceUID: namespaceUID,
		States: []store.MemoryOperationState{
			store.MemoryOperationQueued, store.MemoryOperationLeased, store.MemoryOperationDispatching,
			store.MemoryOperationAmbiguous, store.MemoryOperationDeadLettered,
		},
		Limit: 1,
	})
	return len(operations) > 0, err
}

func bindingFromSnapshot(snapshot controller.MemoryBackendBindingSnapshot, state store.MemoryBackendBindingState) store.MemoryBackendBinding {
	return store.MemoryBackendBinding{
		Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, ClusterID: snapshot.ClusterID,
		Mode: store.MemoryBackendModeRemote, BackendUID: snapshot.BackendUID, BackendGeneration: snapshot.BackendGeneration,
		AuthorityEpoch: snapshot.AuthorityEpoch, RoutingEpoch: snapshot.RoutingEpoch,
		SpecDigest: snapshot.SpecDigest, EndpointDigest: snapshot.EndpointDigest,
		ResolvedAddressDigest: snapshot.ResolvedAddressDigest, ServerCertificateDigest: snapshot.ServerCertificateDigest,
		SecretName: snapshot.SecretName, SecretKey: snapshot.SecretKey,
		SecretUID: snapshot.SecretUID, SecretResourceVersion: snapshot.SecretResourceVersion,
		TenantID: snapshot.TenantID, StoreName: snapshot.StoreName, StoreUUID: snapshot.StoreUUID,
		OwnershipClaim: snapshot.OwnershipClaimIdentity, CapabilityRevision: snapshot.Capabilities.Revision,
		Protocol: snapshot.Protocol, State: state, ActivationEpoch: snapshot.AuthorityEpoch,
		MinimumFeatureEpoch: ActivationFeatureEpoch, ValidationExpiresAt: snapshot.ValidationExpiresAt,
	}
}

func validateExistingBinding(existing *store.MemoryBackendBinding, snapshot controller.MemoryBackendBindingSnapshot) error {
	if existing == nil || existing.Mode != store.MemoryBackendModeRemote || existing.Namespace != snapshot.Namespace ||
		existing.NamespaceUID != snapshot.NamespaceUID || existing.ClusterID != snapshot.ClusterID ||
		existing.BackendUID != snapshot.BackendUID || existing.AuthorityEpoch != snapshot.AuthorityEpoch ||
		existing.StoreName != snapshot.StoreName || existing.StoreUUID != snapshot.StoreUUID || existing.Protocol != snapshot.Protocol {
		return fmt.Errorf("%w: MemoryBackend no longer matches the durable authority", store.ErrConflict)
	}
	return nil
}

func validateLifecycleBinding(existing *store.MemoryBackendBinding, snapshot controller.MemoryBackendBindingSnapshot) error {
	if existing == nil || existing.Mode != store.MemoryBackendModeRemote || existing.Namespace != snapshot.Namespace ||
		existing.NamespaceUID != snapshot.NamespaceUID || existing.ClusterID != snapshot.ClusterID ||
		existing.BackendUID != snapshot.BackendUID || existing.AuthorityEpoch != snapshot.AuthorityEpoch ||
		existing.StoreName != snapshot.StoreName || existing.Protocol != snapshot.Protocol {
		return fmt.Errorf("%w: MemoryBackend no longer matches the durable authority", store.ErrConflict)
	}
	if snapshot.StoreUUID != "" && existing.StoreUUID != snapshot.StoreUUID {
		return fmt.Errorf("%w: MemoryBackend store identity changed", store.ErrConflict)
	}
	return nil
}

func validationRouteMetadataChanged(existing *store.MemoryBackendBinding, snapshot controller.MemoryBackendValidationSnapshot) bool {
	return existing.SpecDigest != snapshot.SpecDigest || existing.EndpointDigest != snapshot.EndpointDigest ||
		existing.ResolvedAddressDigest != snapshot.ResolvedAddressDigest ||
		existing.ServerCertificateDigest != snapshot.ServerCertificateDigest ||
		existing.SecretName != snapshot.SecretName || existing.SecretKey != snapshot.SecretKey ||
		existing.SecretUID != snapshot.SecretUID || existing.SecretResourceVersion != snapshot.SecretResourceVersion ||
		existing.TenantID != snapshot.TenantID
}

func bindingMetadataChanged(current, desired *store.MemoryBackendBinding) bool {
	return current.BackendGeneration != desired.BackendGeneration || bindingRouteMetadataChanged(current, desired) ||
		!current.ValidationExpiresAt.Equal(desired.ValidationExpiresAt) || current.State != desired.State
}

func bindingRouteMetadataChanged(current, desired *store.MemoryBackendBinding) bool {
	return current.SpecDigest != desired.SpecDigest || current.EndpointDigest != desired.EndpointDigest ||
		current.ResolvedAddressDigest != desired.ResolvedAddressDigest ||
		current.ServerCertificateDigest != desired.ServerCertificateDigest || current.SecretName != desired.SecretName ||
		current.SecretKey != desired.SecretKey || current.SecretUID != desired.SecretUID ||
		current.SecretResourceVersion != desired.SecretResourceVersion || current.TenantID != desired.TenantID ||
		current.StoreUUID != desired.StoreUUID || current.OwnershipClaim != desired.OwnershipClaim ||
		current.CapabilityRevision != desired.CapabilityRevision || current.Protocol != desired.Protocol
}

func (c *StoreCoordinator) bindingResultAfterCandidate(
	ctx context.Context,
	snapshot controller.MemoryBackendBindingSnapshot,
	binding store.MemoryBackendBinding,
	effective corev1alpha1.MemoryBackendEffectiveLifecycleState,
	reason, message string,
) (controller.MemoryBackendBindingResult, error) {
	if strings.TrimSpace(snapshot.CandidateDigest) != "" {
		if err := c.recordValidationCandidateIncorporation(ctx, controller.MemoryBackendDeletionSnapshot{
			Namespace: snapshot.Namespace, NamespaceUID: snapshot.NamespaceUID, BackendUID: snapshot.BackendUID,
		}, validationCandidateEpoch{
			authorityEpoch: snapshot.AuthorityEpoch, routingEpoch: snapshot.RoutingEpoch, digest: snapshot.CandidateDigest,
		}); err != nil {
			return controller.MemoryBackendBindingResult{}, err
		}
	}
	return bindingResult(binding, effective, true, reason, message), nil
}

func bindingResult(
	binding store.MemoryBackendBinding,
	effective corev1alpha1.MemoryBackendEffectiveLifecycleState,
	ready bool,
	reason, message string,
) controller.MemoryBackendBindingResult {
	return controller.MemoryBackendBindingResult{
		EffectiveLifecycleState: effective, AuthorityEpoch: binding.AuthorityEpoch,
		RoutingEpoch: binding.RoutingEpoch, Ready: ready, Reason: reason, Message: message,
		Route: durableRouteFromBinding(binding),
	}
}

func (c *StoreCoordinator) requiredFeatureEpoch() int64 {
	if c.RequiredFeatureEpoch > 0 {
		return c.RequiredFeatureEpoch
	}
	return ActivationFeatureEpoch
}

func (c *StoreCoordinator) actor() string {
	if c.Actor != "" {
		return c.Actor
	}
	return "orka-memory-backend-controller"
}

func (c *StoreCoordinator) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}
