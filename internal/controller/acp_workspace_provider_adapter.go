/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	acpworkspacev1alpha1 "github.com/orka-agents/orka/api/acp.workspace/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
	"github.com/orka-agents/orka/pkg/workspaceprovider"
)

const (
	acpWorkspaceAdapterVersion         = "1.0.0"
	acpWorkspaceProviderConfigKind     = "RuntimeProviderConfig"
	acpWorkspaceProviderHeartbeat      = 20 * time.Second
	acpWorkspaceProviderProfileKind    = "RuntimeWorkspaceProfile"
	acpWorkspaceProviderContractV1Only = workspacev1alpha1.ContractVersionV1
)

// ACPWorkspaceProviderAdapterReconciler advertises the in-tree ACP RuntimePool
// execution-workspace adapter on ExecutionWorkspaceProvider objects carrying
// the reserved ACP controllerName. Advertisement is fail-closed: a provider
// whose backend flags are disabled, or whose RuntimeProviderConfig is missing
// or invalid, loses its advertisement and can no longer admit classes.
//
// The advertised exec, files, and reset features name the ACP runtime data
// plane: RuntimeSession prompt execution and the brokered repository
// workspace, not the generic workspace-agent protocol. The suspend feature
// names data-only cold suspension, whose class-profile policy decides whether
// a given class may actually request it.
type ACPWorkspaceProviderAdapterReconciler struct {
	client.Client
	AgentSandboxEnabled         bool
	SubstrateEnabled            bool
	ACPWorkspaceDispatchEnabled bool
	WorkspaceProviderAPIEnabled bool
	Now                         func() time.Time
}

func (r *ACPWorkspaceProviderAdapterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	provider := &workspacev1alpha1.ExecutionWorkspaceProvider{}
	if err := r.Get(ctx, req.NamespacedName, provider); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if provider.Spec.ControllerName != acpWorkspaceProviderControllerName || !provider.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	backend, reason, err := r.servableBackend(ctx, provider)
	if err != nil {
		return ctrl.Result{}, err
	}
	if backend == "" {
		if err := r.clearProviderAdvertisement(ctx, provider, reason); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: acpWorkspaceProviderHeartbeat}, nil
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	before := provider.DeepCopy()
	provider.Status.ObservedGeneration = provider.Generation
	provider.Status.Adapter = &workspacev1alpha1.ExecutionWorkspaceAdapterStatus{Version: acpWorkspaceAdapterVersion}
	provider.Status.Backend = &workspacev1alpha1.ExecutionWorkspaceBackendStatus{Version: string(backend)}
	provider.Status.SupportedContracts = []string{acpWorkspaceProviderContractV1Only}
	provider.Status.SupportedFeatures = []workspacev1alpha1.ExecutionWorkspaceFeature{
		workspacev1alpha1.WorkspaceFeatureExec,
		workspacev1alpha1.WorkspaceFeatureFiles,
		workspacev1alpha1.WorkspaceFeatureReset,
		workspacev1alpha1.WorkspaceFeatureTLS,
	}
	// Both backends support operator-governed data-only cold suspension:
	// Substrate through the derived template's DurableDir snapshot policy and
	// Agent Sandbox through a dedicated durable workspace PVC with
	// operatingMode-based Pod termination. Neither can capture process memory.
	provider.Status.SupportedFeatures = append(
		provider.Status.SupportedFeatures, workspacev1alpha1.WorkspaceFeatureSuspend,
	)
	heartbeat := metav1.NewTime(now)
	provider.Status.LastHeartbeat = &heartbeat
	// The core provider controller owns the Compatible, Heartbeat, and Ready
	// conditions and computes them from the advertised contracts, so the
	// adapter writes only its own advertisement fields. The optimistic lock
	// keeps a stale heartbeat snapshot from replacing the core-owned
	// conditions array; a conflict simply retries on the next pass.
	if err := r.Status().Patch(ctx, provider, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: acpWorkspaceProviderHeartbeat}, nil
}

// servableBackend resolves the provider's RuntimeProviderConfig and returns
// the backend this installation may serve, or empty with a reason when
// advertisement must be withheld.
func (r *ACPWorkspaceProviderAdapterReconciler) servableBackend(
	ctx context.Context,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
) (acpworkspacev1alpha1.RuntimeProviderBackend, string, error) {
	ref := provider.Spec.ParametersRef
	if ref.Group != acpworkspacev1alpha1.GroupVersion.Group || ref.Kind != acpWorkspaceProviderConfigKind || ref.Name == "" {
		return "", "provider parametersRef is not an ACP RuntimeProviderConfig", nil
	}
	// Older adapter versions kept the config UID only in the mirror
	// annotation. Move that pin into protected status before looking up the
	// config, so a delete-and-recreate cannot race the migration. If a
	// previously advertised provider has already lost both pins, its original
	// config identity cannot be recovered safely and the provider must remain
	// unavailable.
	statusPinned := strings.TrimSpace(provider.Status.PinnedParametersUID)
	annotationPinned := strings.TrimSpace(provider.Annotations[acpWorkspaceProviderConfigUIDAnnotation])
	pinned := statusPinned
	if pinned == "" {
		pinned = annotationPinned
	}
	if statusPinned == "" && pinned != "" {
		base := provider.DeepCopy()
		provider.Status.PinnedParametersUID = pinned
		if err := r.Status().Patch(ctx, provider, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				provider.Status.PinnedParametersUID = statusPinned
				return "", "provider config identity pin conflicted; retrying", nil
			}
			return "", "", fmt.Errorf("promote ACP runtime provider config identity pin: %w", err)
		}
	}
	if pinned == "" && provider.Status.Adapter != nil {
		return "", "previously advertised provider has no protected RuntimeProviderConfig UID pin; create a new provider", nil
	}
	config := &acpworkspacev1alpha1.RuntimeProviderConfig{}
	if err := r.Get(ctx, types.NamespacedName{Name: ref.Name}, config); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "referenced ACP RuntimeProviderConfig does not exist", nil
		}
		return "", "", fmt.Errorf("get ACP runtime provider config %q: %w", ref.Name, err)
	}
	if config.DeletionTimestamp != nil {
		return "", "referenced ACP RuntimeProviderConfig is deleting", nil
	}
	// The config is immutable, but a delete-and-recreate under the same name
	// (possibly switching backends) preserves the provider identity and the
	// class profile hash. The adapter pins the first advertised config UID on
	// the provider and refuses to advertise a replacement, so class
	// resolution fails closed instead of silently dispatching new Tasks onto
	// the replacement backend.
	// The pin lives in STATUS (controller-owned through the status
	// subresource): ordinary metadata writers cannot strip it, so a config
	// deleted and recreated under the same name keeps failing closed. The
	// annotation stays as a human-visible mirror only and is never the
	// authority.
	if pinned == "" {
		base := provider.DeepCopy()
		provider.Status.PinnedParametersUID = string(config.UID)
		if err := r.Status().Patch(ctx, provider, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			if apierrors.IsConflict(err) {
				return "", "provider config identity pin conflicted; retrying", nil
			}
			return "", "", fmt.Errorf("pin ACP runtime provider config identity: %w", err)
		}
	} else if pinned != string(config.UID) {
		return "", "referenced ACP RuntimeProviderConfig was replaced; create a new provider", nil
	}
	if annotationPinned == "" {
		base := provider.DeepCopy()
		if provider.Annotations == nil {
			provider.Annotations = map[string]string{}
		}
		provider.Annotations[acpWorkspaceProviderConfigUIDAnnotation] = string(config.UID)
		if err := r.Patch(ctx, provider, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil &&
			!apierrors.IsConflict(err) {
			return "", "", fmt.Errorf("mirror ACP runtime provider config identity pin: %w", err)
		}
	}
	if !r.WorkspaceProviderAPIEnabled {
		// Cleanup-only installations keep the adapter registered so existing
		// workspaces converge, but a provider must never stay advertised as
		// accepting allocations while class-backed Tasks are rejected.
		return "", "workspace provider API is disabled", nil
	}
	if !r.ACPWorkspaceDispatchEnabled {
		return "", "workspace-provider-backed RuntimeSession dispatch is disabled", nil
	}
	switch config.Spec.Backend {
	case acpworkspacev1alpha1.RuntimeProviderBackendAgentSandbox:
		if !r.AgentSandboxEnabled {
			return "", "agent-sandbox backend is disabled", nil
		}
	case acpworkspacev1alpha1.RuntimeProviderBackendSubstrate:
		if !r.SubstrateEnabled {
			return "", "substrate backend is disabled", nil
		}
	default:
		return "", fmt.Sprintf("backend %q is not supported", config.Spec.Backend), nil
	}
	return config.Spec.Backend, "", nil
}

func (r *ACPWorkspaceProviderAdapterReconciler) clearProviderAdvertisement(
	ctx context.Context,
	provider *workspacev1alpha1.ExecutionWorkspaceProvider,
	reason string,
) error {
	before := provider.DeepCopy()
	// Adapter identity is protected historical evidence for a legacy provider
	// that was advertised before the status pin existed. Preserve it while the
	// pin is missing so later reconciles cannot mistake the provider for a new
	// one and initialize the pin from a replacement config. The readiness-
	// bearing fields are still cleared below.
	preserveAdapterIdentity := provider.Status.Adapter != nil &&
		strings.TrimSpace(provider.Status.PinnedParametersUID) == ""
	provider.Status.ObservedGeneration = 0
	if !preserveAdapterIdentity {
		provider.Status.Adapter = nil
	}
	provider.Status.Backend = nil
	provider.Status.SupportedContracts = nil
	provider.Status.SupportedFeatures = nil
	provider.Status.LastHeartbeat = nil
	// Withdrawing the advertisement is enough: the core provider controller
	// recomputes Compatible/Heartbeat/Ready from the cleared fields, and the
	// adapter never writes core-owned conditions. The optimistic lock keeps a
	// stale snapshot from clobbering concurrent core condition writes.
	logf.FromContext(ctx).Info("withholding ACP workspace provider advertisement", "provider", provider.Name, "reason", reason)
	if err := r.Status().Patch(ctx, provider, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			return nil
		}
		return err
	}
	return nil
}

// SetupWithManager registers the ACP provider advertisement controller.
func (r *ACPWorkspaceProviderAdapterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&workspacev1alpha1.ExecutionWorkspaceProvider{}).
		// Every reconcile writes a fresh LastHeartbeat. Without the generation
		// filter that status write would feed back as an update event and turn
		// the timed heartbeat into a continuous reconcile loop.
		WithEventFilter(predicate.And(
			workspaceprovider.ControllerNamePredicate(acpWorkspaceProviderControllerName),
			predicate.GenerationChangedPredicate{},
		)).
		Named("acp-workspace-provider-adapter").
		Complete(r)
}
