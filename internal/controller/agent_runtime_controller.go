/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerpkg "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
	v1conformance "github.com/orka-agents/orka/internal/harness/conformance"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	v2conformance "github.com/orka-agents/orka/internal/harness/v2/conformance"
	"github.com/orka-agents/orka/internal/tools"
)

var agentRuntimeAllowInsecureLoopbackForTests bool

const (
	agentRuntimeReadyCondition = "Ready"
	agentRuntimeReasonReady    = "ConformancePassed"
	agentRuntimeReasonNotReady = "ConformanceFailed"
	agentRuntimeProbeTimeout   = 60 * time.Second
	agentRuntimeRequeue        = 30 * time.Second
	agentRuntimeDeleteRequeue  = time.Second
	agentRuntimeMinBearerBytes = 32
	agentRuntimeFinalizer      = "orka.ai/agent-runtime-cleanup"

	agentRuntimeAuthUseLabel           = "orka.ai/agent-runtime-auth"
	agentRuntimeAuthRefNameLabel       = "orka.ai/agent-runtime-name"
	agentRuntimeAuthEndpointAnnotation = "orka.ai/agent-runtime-endpoint"
)

// AgentRuntimeReconciler reconciles external harness v1 and v2 registry entries.
type AgentRuntimeReconciler struct {
	client.Client
	APIReader              client.Reader
	Scheme                 *k8sruntime.Scheme
	HarnessV1HTTPClient    *http.Client
	MCPRegistry            *tools.Registry
	ControllerEpochManager *ControllerEpochManager
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups=discovery.k8s.io,resources=endpointslices,verbs=get;list;watch

// Reconcile validates one exact external runtime and publishes condition-ready status.
func (r *AgentRuntimeReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)
	runtime := &corev1alpha1.AgentRuntime{}
	if err := r.Get(ctx, req.NamespacedName, runtime); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling AgentRuntime", "agentRuntime", runtime.Name, "mode", runtime.Spec.Deployment.Mode)
	if !runtime.DeletionTimestamp.IsZero() {
		return r.finalizeAgentRuntime(ctx, runtime)
	}
	observed, ready, controllerAuthVersion, capabilityAuthVersion, message := r.probeAgentRuntime(ctx, runtime)
	observed = retainedAgentRuntimeObservation(
		runtime, ready, observed, controllerAuthVersion, capabilityAuthVersion,
	)
	if runtime.RegisteredContractVersion() == corev1alpha1.AgentRuntimeContractHarnessV2 &&
		observed != nil && strings.TrimSpace(observed.RuntimePoolUID) != "" {
		managedOwner, err := r.conflictingManagedRuntimePoolIdentityOwner(ctx, runtime, observed)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("arbitrate managed RuntimePool identity: %w", err)
		}
		if managedOwner != nil {
			return r.rejectManagedRuntimePoolIdentity(ctx, runtime, managedOwner)
		}
		owner, err := r.conflictingAgentRuntimePoolIdentityOwner(ctx, runtime, observed)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("arbitrate AgentRuntime pool identity: %w", err)
		}
		if owner != nil {
			return r.rejectAgentRuntimePoolIdentity(ctx, runtime, owner)
		}
	}
	if runtime.RegisteredContractVersion() == corev1alpha1.AgentRuntimeContractHarnessV2 &&
		agentRuntimeObservedStatusIdentityComplete(observed) &&
		!controllerutil.ContainsFinalizer(runtime, agentRuntimeFinalizer) {
		base := runtime.DeepCopy()
		controllerutil.AddFinalizer(runtime, agentRuntimeFinalizer)
		if err := r.Patch(ctx, runtime, client.MergeFrom(base)); err != nil {
			return ctrl.Result{}, fmt.Errorf("add AgentRuntime cleanup finalizer: %w", err)
		}
	}
	return r.writeAgentRuntimeStatus(
		ctx, runtime, ready, observed, controllerAuthVersion, capabilityAuthVersion, message,
	)
}

type agentRuntimeDeletionAuthority struct {
	runtime        *corev1alpha1.AgentRuntime
	auth           agentRuntimeAuthMaterial
	backendPins    []string
	canonicalValue []byte
}

func (r *AgentRuntimeReconciler) finalizeAgentRuntime(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
) (ctrl.Result, error) {
	if runtime.RegisteredContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 ||
		!controllerutil.ContainsFinalizer(runtime, agentRuntimeFinalizer) {
		return ctrl.Result{}, nil
	}
	if runtime.Spec.Capabilities == nil || !runtime.Spec.Capabilities.SupportsDrain {
		return ctrl.Result{}, fmt.Errorf("AgentRuntime deletion requires supportsDrain=true; cleanup finalizer retained because harness v2 has no safe admission-closing fallback")
	}
	if !agentRuntimeObservedStatusIdentityComplete(runtime.Status.ObservedCapabilities) {
		return ctrl.Result{}, fmt.Errorf("AgentRuntime deletion requires a complete authenticated runtime identity; cleanup finalizer retained")
	}
	if r.ControllerEpochManager == nil {
		return ctrl.Result{}, fmt.Errorf("AgentRuntime deletion requires the current controller epoch manager")
	}
	controllerFence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolve current controller epoch for AgentRuntime deletion: %w", err)
	}
	runtimeClient, authority, err := r.agentRuntimeDeletionClient(ctx, runtime)
	if err != nil {
		return ctrl.Result{}, err
	}
	status, err := runtimeClient.Status(ctx)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("read authenticated AgentRuntime deletion status: %w", err)
	}
	if err := validateExternalRuntimeStatus(runtime, controllerFence, status, false); err != nil {
		return ctrl.Result{}, fmt.Errorf("validate authenticated AgentRuntime deletion status: %w", err)
	}
	if !status.Drain.Requested {
		request, requestErr := newAgentRuntimeDeletionDrainRequest(status.Fence, time.Now().UTC())
		if requestErr != nil {
			return ctrl.Result{}, requestErr
		}
		if _, drainErr := runtimeClient.Drain(ctx, request); drainErr != nil {
			return ctrl.Result{}, fmt.Errorf("request authenticated AgentRuntime deletion drain: %w", drainErr)
		}
		return ctrl.Result{RequeueAfter: agentRuntimeDeleteRequeue}, nil
	}
	if !upgradeDrainSupervisorIsQuiescent(*status) {
		return ctrl.Result{RequeueAfter: agentRuntimeDeleteRequeue}, nil
	}
	current, err := r.revalidateAgentRuntimeDeletionAuthority(ctx, authority)
	if err != nil {
		return ctrl.Result{}, err
	}
	base := current.DeepCopy()
	controllerutil.RemoveFinalizer(current, agentRuntimeFinalizer)
	if err := r.Patch(ctx, current, client.MergeFrom(base)); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("remove AgentRuntime cleanup finalizer: %w", err)
	}
	return ctrl.Result{}, nil
}

func (r *AgentRuntimeReconciler) agentRuntimeDeletionClient(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
) (*harnessv2.Client, agentRuntimeDeletionAuthority, error) {
	if err := validateAgentRuntimeSpec(runtime); err != nil {
		return nil, agentRuntimeDeletionAuthority{}, fmt.Errorf("validate AgentRuntime deletion authority: %w", err)
	}
	backendPins, err := r.AgentRuntimeServiceBackendPins(ctx, runtime)
	if err != nil {
		return nil, agentRuntimeDeletionAuthority{}, fmt.Errorf("validate AgentRuntime deletion endpoint: %w", err)
	}
	auth, err := r.agentRuntimeAuthMaterial(ctx, runtime)
	if err != nil {
		return nil, agentRuntimeDeletionAuthority{}, fmt.Errorf("resolve AgentRuntime deletion auth: %w", err)
	}
	if runtime.Status.ObservedControllerAuthRefResourceVersion != auth.controllerResourceVersion ||
		runtime.Status.ObservedOperationCapabilityRefResourceVersion != auth.capabilityResourceVersion {
		return nil, agentRuntimeDeletionAuthority{}, fmt.Errorf("AgentRuntime deletion auth changed after conformance; cleanup finalizer retained")
	}
	canonicalValue, err := canonicalExternalRuntimeMutationAuthority(runtime)
	if err != nil {
		return nil, agentRuntimeDeletionAuthority{}, fmt.Errorf("canonicalize AgentRuntime deletion authority: %w", err)
	}
	authority := agentRuntimeDeletionAuthority{
		runtime: runtime.DeepCopy(), auth: auth, backendPins: slices.Clone(backendPins), canonicalValue: canonicalValue,
	}
	options := []harnessv2.ClientOption{
		harnessv2.WithControlTimeout(agentRuntimeProbeTimeout),
		harnessv2.WithControllerBearerToken(auth.controllerBearerToken),
		harnessv2.WithOperationCapabilitySecret(auth.operationCapabilitySecret),
		harnessv2.WithStatusCapabilityBinding(harnessv2.StatusCapabilityBinding{
			RuntimeProfileDigest: harnessv2.ProfileDigest(runtime.Spec.Capabilities.Profile.Digest),
			RuntimeInstanceID:    harnessv2.RuntimeInstanceID(runtime.Spec.Capabilities.RuntimeInstanceID),
		}),
		harnessv2.WithBeforeMutation(func(validateCtx context.Context, operation string) error {
			if operation != "drain" {
				return fmt.Errorf("unsupported AgentRuntime deletion mutation %q", operation)
			}
			_, revalidateErr := r.revalidateAgentRuntimeDeletionAuthority(validateCtx, authority)
			return revalidateErr
		}),
	}
	switch {
	case len(backendPins) > 0:
		options = append(options, harnessv2.WithHTTPClient(externalRuntimeHTTPClient(PinnedBackendDialTransport(backendPins))))
	case agentRuntimeEndpointRequiresPublicDial(runtime.Spec.Deployment.Endpoint):
		options = append(options, harnessv2.WithHTTPClient(externalRuntimeHTTPClient(v2conformance.PublicAddressDialTransport())))
	}
	runtimeClient, err := harnessv2.NewClient(runtime.Spec.Deployment.Endpoint, options...)
	if err != nil {
		return nil, agentRuntimeDeletionAuthority{}, fmt.Errorf("construct AgentRuntime deletion client: %w", err)
	}
	return runtimeClient, authority, nil
}

func (r *AgentRuntimeReconciler) revalidateAgentRuntimeDeletionAuthority(
	ctx context.Context,
	expected agentRuntimeDeletionAuthority,
) (*corev1alpha1.AgentRuntime, error) {
	if expected.runtime == nil {
		return nil, fmt.Errorf("AgentRuntime deletion authority is incomplete")
	}
	current := &corev1alpha1.AgentRuntime{}
	if err := r.endpointReader().Get(ctx, client.ObjectKeyFromObject(expected.runtime), current); err != nil {
		return nil, fmt.Errorf("re-read AgentRuntime deletion authority: %w", err)
	}
	if current.UID != expected.runtime.UID || current.DeletionTimestamp.IsZero() ||
		!controllerutil.ContainsFinalizer(current, agentRuntimeFinalizer) {
		return nil, fmt.Errorf("AgentRuntime deletion authority changed before cleanup mutation")
	}
	canonicalValue, err := canonicalExternalRuntimeMutationAuthority(current)
	if err != nil {
		return nil, fmt.Errorf("canonicalize current AgentRuntime deletion authority: %w", err)
	}
	if !bytes.Equal(canonicalValue, expected.canonicalValue) {
		return nil, fmt.Errorf("AgentRuntime registration or observed authority changed during deletion")
	}
	backendPins, err := r.AgentRuntimeServiceBackendPins(ctx, current)
	if err != nil {
		return nil, fmt.Errorf("revalidate AgentRuntime deletion endpoint: %w", err)
	}
	if !slices.Equal(backendPins, expected.backendPins) {
		return nil, fmt.Errorf("AgentRuntime verified backend set changed during deletion")
	}
	auth, err := r.agentRuntimeAuthMaterial(ctx, current)
	if err != nil {
		return nil, fmt.Errorf("revalidate AgentRuntime deletion auth: %w", err)
	}
	if auth.controllerSecretUID != expected.auth.controllerSecretUID ||
		auth.capabilitySecretUID != expected.auth.capabilitySecretUID ||
		auth.controllerResourceVersion != expected.auth.controllerResourceVersion ||
		auth.capabilityResourceVersion != expected.auth.capabilityResourceVersion ||
		auth.controllerBearerToken != expected.auth.controllerBearerToken ||
		!bytes.Equal(auth.operationCapabilitySecret, expected.auth.operationCapabilitySecret) {
		return nil, fmt.Errorf("AgentRuntime authentication authority changed during deletion")
	}
	return current, nil
}

func newAgentRuntimeDeletionDrainRequest(fence harnessv2.Fence, now time.Time) (harnessv2.DrainRequest, error) {
	request := harnessv2.DrainRequest{
		Protocol: harnessv2.ProtocolVersion,
		Metadata: harnessv2.MutationMetadata{
			Fence:                      fence,
			OperationID:                harnessv2.OperationID("agent-runtime-delete-drain-g" + strconv.FormatUint(fence.RuntimePoolGeneration, 10)),
			RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion,
			ExpiresAt:                  now.Add(time.Minute),
		},
		Reason: "agent_runtime_deletion",
	}
	if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
		return harnessv2.DrainRequest{}, fmt.Errorf("seal AgentRuntime deletion drain request: %w", err)
	}
	return request, nil
}

// conflictingManagedRuntimePoolIdentityOwner reserves every managed
// RuntimePool UID for that pool, regardless of its current lifecycle state.
// External registrations must never make shared authorization lookups
// ambiguous for an existing managed pool.
func (r *AgentRuntimeReconciler) conflictingManagedRuntimePoolIdentityOwner(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	observed *corev1alpha1.AgentRuntimeObservedCapabilities,
) (*corev1alpha1.RuntimePool, error) {
	if runtime == nil || observed == nil || strings.TrimSpace(observed.RuntimePoolUID) == "" {
		return nil, fmt.Errorf("authenticated runtime pool identity is incomplete")
	}
	var pools corev1alpha1.RuntimePoolList
	if err := r.endpointReader().List(ctx, &pools, client.InNamespace(runtime.Namespace)); err != nil {
		return nil, fmt.Errorf("list managed RuntimePool identity owners: %w", err)
	}
	poolUID := strings.TrimSpace(observed.RuntimePoolUID)
	for index := range pools.Items {
		if string(pools.Items[index].UID) == poolUID {
			return pools.Items[index].DeepCopy(), nil
		}
	}
	return nil, nil
}

// conflictingAgentRuntimePoolIdentityOwner returns the incumbent registration
// for an authenticated pool identity. A single published registration remains
// the owner even while NotReady because active sessions retain its broker
// authority. If an upgrade left several published owners, stable object
// identity ordering picks one so reconciling the others repairs the ambiguity.
func (r *AgentRuntimeReconciler) conflictingAgentRuntimePoolIdentityOwner(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	observed *corev1alpha1.AgentRuntimeObservedCapabilities,
) (*corev1alpha1.AgentRuntime, error) {
	if runtime == nil || observed == nil || strings.TrimSpace(observed.RuntimePoolUID) == "" {
		return nil, fmt.Errorf("authenticated runtime pool identity is incomplete")
	}
	var runtimes corev1alpha1.AgentRuntimeList
	if err := r.endpointReader().List(ctx, &runtimes, client.InNamespace(runtime.Namespace)); err != nil {
		return nil, fmt.Errorf("list AgentRuntime pool identity owners: %w", err)
	}
	candidates := make([]*corev1alpha1.AgentRuntime, 0, len(runtimes.Items))
	for index := range runtimes.Items {
		candidate := &runtimes.Items[index]
		candidateObserved := candidate.Status.ObservedCapabilities
		if candidateObserved == nil || candidateObserved.RuntimePoolUID != observed.RuntimePoolUID {
			continue
		}
		candidates = append(candidates, candidate)
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	sort.Slice(candidates, func(left, right int) bool {
		return agentRuntimeIdentityPrecedes(candidates[left], candidates[right])
	})
	owner := candidates[0]
	if sameAgentRuntimeIdentity(owner, runtime) {
		return nil, nil
	}
	return owner.DeepCopy(), nil
}

func agentRuntimeIdentityPrecedes(left, right *corev1alpha1.AgentRuntime) bool {
	if !left.CreationTimestamp.Equal(&right.CreationTimestamp) {
		return left.CreationTimestamp.Time.Before(right.CreationTimestamp.Time)
	}
	if left.UID != right.UID {
		return string(left.UID) < string(right.UID)
	}
	return left.Name < right.Name
}

func sameAgentRuntimeIdentity(left, right *corev1alpha1.AgentRuntime) bool {
	if left == nil || right == nil || left.Namespace != right.Namespace || left.Name != right.Name {
		return false
	}
	return left.UID == "" || right.UID == "" || left.UID == right.UID
}

func (r *AgentRuntimeReconciler) probeAgentRuntime(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
) (*corev1alpha1.AgentRuntimeObservedCapabilities, bool, string, string, string) {
	if err := validateAgentRuntimeSpec(runtime); err != nil {
		return nil, false, "", "", err.Error()
	}
	if err := r.validateAgentRuntimeEndpointPolicy(ctx, runtime); err != nil {
		return nil, false, "", "", err.Error()
	}
	// Resolve the verified Service backend pins now, right after the endpoint
	// policy passed, so every conformance dial below (v1 or v2) targets a proven
	// backend Pod rather than the mutable Service ClusterIP. Non-Service
	// endpoints return no pins and fall back to the public-address dial control.
	backendPins, err := r.serviceBackendPinsForValidatedEndpoint(ctx, runtime)
	if err != nil {
		return nil, false, "", "", err.Error()
	}
	if runtime.RegisteredContractVersion() == corev1alpha1.AgentRuntimeContractHarnessV1 {
		return r.probeHarnessV1AgentRuntime(ctx, runtime, backendPins)
	}
	if r.ControllerEpochManager == nil {
		return nil, false, "", "", "current controller epoch manager is unavailable"
	}
	controllerFence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return nil, false, "", "", fmt.Sprintf("resolve current controller epoch: %v", err)
	}
	if controllerFence.Epoch < 1 {
		return nil, false, "", "", "current controller epoch is invalid"
	}
	auth, err := r.agentRuntimeAuthMaterial(ctx, runtime)
	if err != nil {
		return nil, false, "", "", err.Error()
	}
	profile, err := agentRuntimeProfile(*runtime.Spec.Capabilities.Profile)
	if err != nil {
		return nil, false, auth.controllerResourceVersion, auth.capabilityResourceVersion, err.Error()
	}
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	registry := r.MCPRegistry
	if registry == nil {
		registry = tools.DefaultRegistry
	}
	mcpConfiguration, err := buildAgentRuntimeMCPConfigurationWithRegistry(ctx, reader, runtime, profile, registry)
	if err != nil {
		return nil, false, auth.controllerResourceVersion, auth.capabilityResourceVersion, err.Error()
	}
	limits, err := agentRuntimeProtocolLimits(*runtime.Spec.Capabilities.Limits)
	if err != nil {
		return nil, false, auth.controllerResourceVersion, auth.capabilityResourceVersion, err.Error()
	}
	governance, err := agentRuntimeWorkspaceGovernance(*runtime.Spec.Capabilities.WorkspaceGovernance)
	if err != nil {
		return nil, false, auth.controllerResourceVersion, auth.capabilityResourceVersion, err.Error()
	}
	observedDescriptorDigest := ""
	if runtime.Status.ObservedCapabilities != nil {
		observedDescriptorDigest = runtime.Status.ObservedCapabilities.MCPToolDescriptorDigest
	}
	deepProbe := runtime.Status.ObservedGeneration != runtime.Generation || !runtime.Status.Ready ||
		runtime.Status.ObservedControllerAuthRefResourceVersion != auth.controllerResourceVersion ||
		runtime.Status.ObservedOperationCapabilityRefResourceVersion != auth.capabilityResourceVersion ||
		observedDescriptorDigest != mcpConfiguration.ToolPolicy.DescriptorDigest
	probeCtx, cancel := context.WithTimeout(ctx, agentRuntimeProbeTimeout)
	defer cancel()
	target := v2conformance.Target{
		BaseURL:                         runtime.Spec.Deployment.Endpoint,
		ControllerBearerToken:           auth.controllerBearerToken,
		OperationCapabilitySecret:       auth.operationCapabilitySecret,
		ControlTimeout:                  agentRuntimeProbeTimeout,
		ExpectedRuntimeInstanceID:       harnessv2.RuntimeInstanceID(runtime.Spec.Capabilities.RuntimeInstanceID),
		ExpectedControllerEpoch:         uint64(controllerFence.Epoch),
		Profile:                         profile,
		ToolPolicy:                      mcpConfiguration.ToolPolicy,
		ApprovalPolicy:                  mcpConfiguration.ApprovalPolicy,
		Limits:                          limits,
		SupportsDrain:                   runtime.Spec.Capabilities.SupportsDrain,
		SupportsPublicationFinalization: runtime.Spec.Capabilities.SupportsPublicationFinalization,
		WorkspaceGovernance:             governance,
		ProbeLifecycle:                  deepProbe,
		RequirePublicAddresses:          agentRuntimeEndpointRequiresPublicDial(runtime.Spec.Deployment.Endpoint),
		PinnedBackendAddresses:          backendPins,
	}
	probe := v2conformance.Check(probeCtx, target)
	if !deepProbe && probe.Passed && agentRuntimeAuthenticatedIdentityChanged(runtime.Status.ObservedCapabilities, probe.ObservedStatus) {
		target.ProbeLifecycle = true
		probe = v2conformance.Check(probeCtx, target)
	}
	observed := observedCapabilitiesFromConformance(probe, profile)
	if observed != nil {
		observed.MCPToolDescriptorDigest = mcpConfiguration.ToolPolicy.DescriptorDigest
	}
	if !probe.Passed {
		return observed, false, auth.controllerResourceVersion, auth.capabilityResourceVersion,
			sanitizeAgentRuntimeStatusMessage(probe.Message)
	}
	if err := r.requireCurrentAgentRuntimeAuthMaterial(ctx, runtime, auth); err != nil {
		return observed, false, auth.controllerResourceVersion, auth.capabilityResourceVersion, err.Error()
	}
	return observed, true, auth.controllerResourceVersion, auth.capabilityResourceVersion,
		"authenticated orka.harness.v2 conformance passed"
}

func agentRuntimeAuthenticatedIdentityChanged(
	previous *corev1alpha1.AgentRuntimeObservedCapabilities,
	current *harnessv2.StatusResponse,
) bool {
	if previous == nil || current == nil {
		return true
	}
	fence := current.Fence
	return previous.RuntimeInstanceID != string(fence.RuntimeInstanceID) ||
		previous.SupervisorBootID != string(fence.SupervisorBootID) ||
		previous.ControllerEpoch != int64(fence.ControllerEpoch) ||
		previous.RuntimePoolUID != string(fence.RuntimePoolUID) ||
		previous.RuntimePoolGeneration != int64(fence.RuntimePoolGeneration) ||
		previous.RuntimeProfileDigest != string(fence.RuntimeProfileDigest) ||
		previous.ProfileDigestSchemaVersion != int32(fence.ProfileDigestSchemaVersion)
}

func agentRuntimeObservedStatusIdentityComplete(observed *corev1alpha1.AgentRuntimeObservedCapabilities) bool {
	return observed != nil &&
		strings.TrimSpace(observed.RuntimeInstanceID) != "" &&
		strings.TrimSpace(observed.SupervisorBootID) != "" &&
		observed.ControllerEpoch > 0 &&
		strings.TrimSpace(observed.RuntimePoolUID) != "" &&
		observed.RuntimePoolGeneration > 0 &&
		strings.TrimSpace(observed.RuntimeProfileDigest) != "" &&
		observed.ProfileDigestSchemaVersion == int32(harnessv2.ProfileDigestSchemaVersion)
}

func agentRuntimeObservedStatusIdentityChanged(
	previous *corev1alpha1.AgentRuntimeObservedCapabilities,
	current *corev1alpha1.AgentRuntimeObservedCapabilities,
) bool {
	if previous == nil || current == nil {
		return true
	}
	return previous.RuntimeInstanceID != current.RuntimeInstanceID ||
		previous.SupervisorBootID != current.SupervisorBootID ||
		previous.ControllerEpoch != current.ControllerEpoch ||
		previous.RuntimePoolUID != current.RuntimePoolUID ||
		previous.RuntimePoolGeneration != current.RuntimePoolGeneration ||
		previous.RuntimeProfileDigest != current.RuntimeProfileDigest ||
		previous.ProfileDigestSchemaVersion != current.ProfileDigestSchemaVersion
}

func retainedAgentRuntimeObservation(
	runtime *corev1alpha1.AgentRuntime,
	ready bool,
	observed *corev1alpha1.AgentRuntimeObservedCapabilities,
	controllerAuthResourceVersion string,
	capabilityAuthResourceVersion string,
) *corev1alpha1.AgentRuntimeObservedCapabilities {
	if ready || runtime == nil || runtime.RegisteredContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return observed
	}
	previous := runtime.Status.ObservedCapabilities
	if agentRuntimeObservedStatusIdentityComplete(observed) &&
		agentRuntimeObservedStatusIdentityChanged(previous, observed) {
		return observed
	}
	if runtime.Status.ObservedGeneration != runtime.Generation ||
		!agentRuntimeObservedStatusIdentityComplete(previous) ||
		strings.TrimSpace(controllerAuthResourceVersion) == "" || strings.TrimSpace(capabilityAuthResourceVersion) == "" ||
		runtime.Status.ObservedControllerAuthRefResourceVersion != controllerAuthResourceVersion ||
		runtime.Status.ObservedOperationCapabilityRefResourceVersion != capabilityAuthResourceVersion {
		return observed
	}
	return previous.DeepCopy()
}

func (r *AgentRuntimeReconciler) probeHarnessV1AgentRuntime(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	backendPins []string,
) (*corev1alpha1.AgentRuntimeObservedCapabilities, bool, string, string, string) {
	auth, err := r.agentRuntimeV1BearerAuthMaterial(ctx, runtime)
	if err != nil {
		return nil, false, "", "", err.Error()
	}
	deepProbe := runtime.Status.ObservedGeneration != runtime.Generation || !runtime.Status.Ready ||
		runtime.Status.ObservedAuthRefResourceVersion != auth.secretResourceVersion
	probeCtx, cancel := context.WithTimeout(ctx, agentRuntimeProbeTimeout)
	defer cancel()
	target := v1conformance.Target{
		BaseURL:        runtime.Spec.Deployment.Endpoint,
		BearerToken:    auth.bearerToken,
		HTTPClient:     agentRuntimeV1DialControlledClient(r.HarnessV1HTTPClient, runtime.Spec.Deployment.Endpoint, backendPins),
		ControlTimeout: agentRuntimeProbeTimeout,
		RequireAuth:    true,
	}
	var probe v1conformance.Result
	if deepProbe {
		probe = v1conformance.CheckReadiness(probeCtx, target)
	} else {
		probe = v1conformance.Check(probeCtx, target)
	}
	observed := observedHarnessV1CapabilitiesFromConformance(probe.ObservedCapabilities)
	if !probe.Passed {
		return observed, false, auth.secretResourceVersion, "", sanitizeAgentRuntimeStatusMessage(probe.Message)
	}
	if err := validateHarnessV1AgentRuntimeRequiredCapabilities(runtime, probe.ObservedCapabilities); err != nil {
		return observed, false, auth.secretResourceVersion, "", err.Error()
	}
	if err := validateHarnessV1AgentRuntimeExecutableCapabilities(probe.ObservedCapabilities); err != nil {
		return observed, false, auth.secretResourceVersion, "", err.Error()
	}
	if err := r.requireCurrentAgentRuntimeV1BearerAuthMaterial(ctx, runtime, auth); err != nil {
		return observed, false, auth.secretResourceVersion, "", err.Error()
	}
	return observed, true, auth.secretResourceVersion, "", "authenticated orka.harness.v1 conformance passed"
}

func validateHarnessV1AgentRuntimeRequiredCapabilities(
	runtime *corev1alpha1.AgentRuntime,
	capabilities *harness.CapabilitiesResponse,
) error {
	if runtime == nil {
		return fmt.Errorf("AgentRuntime is required")
	}
	if capabilities == nil {
		return fmt.Errorf("observed harness v1 capabilities are missing")
	}
	required := runtime.Spec.Capabilities
	if required == nil {
		return nil
	}
	if required.SupportsCancel != nil && *required.SupportsCancel && !capabilities.SupportsCancel {
		return fmt.Errorf("runtime does not advertise required supportsCancel capability")
	}
	if required.SupportsRuntimeSessions != nil && *required.SupportsRuntimeSessions && !capabilities.SupportsRuntimeSessions {
		return fmt.Errorf("runtime does not advertise required supportsRuntimeSessions capability")
	}
	if required.SupportsContinuation != nil && *required.SupportsContinuation && !capabilities.SupportsContinuation {
		return fmt.Errorf("runtime does not advertise required supportsContinuation capability")
	}
	if required.SupportsArtifacts != nil && *required.SupportsArtifacts && !capabilities.SupportsArtifacts {
		return fmt.Errorf("runtime does not advertise required supportsArtifacts capability")
	}
	for _, requiredMode := range required.ToolExecutionModes {
		if !slices.ContainsFunc(capabilities.ToolExecutionModes, func(observed harness.ToolExecutionMode) bool {
			return string(observed) == string(requiredMode)
		}) {
			return fmt.Errorf("runtime does not advertise required toolExecutionMode %q", requiredMode)
		}
	}
	for _, requiredClass := range required.BrokeredToolClasses {
		if !slices.ContainsFunc(capabilities.BrokeredToolClasses, func(observed harness.BrokeredToolClass) bool {
			return string(observed) == string(requiredClass)
		}) {
			return fmt.Errorf("runtime does not advertise required brokeredToolClass %q", requiredClass)
		}
	}
	return nil
}

func validateHarnessV1AgentRuntimeExecutableCapabilities(capabilities *harness.CapabilitiesResponse) error {
	if capabilities == nil {
		return fmt.Errorf("observed harness v1 capabilities are missing")
	}
	if capabilities.RuntimeName != sanitizeAgentRuntimeCapabilityValue(capabilities.RuntimeName) {
		return fmt.Errorf("runtimeName contains unsafe text or exceeds status length limits")
	}
	for _, mode := range capabilities.ToolExecutionModes {
		if !harness.IsKnownToolExecutionMode(mode) {
			return fmt.Errorf("unsupported toolExecutionMode %q", mode)
		}
	}
	for _, class := range capabilities.BrokeredToolClasses {
		if !harness.IsKnownBrokeredToolClass(class) {
			return fmt.Errorf("unsupported brokeredToolClass %q", class)
		}
	}
	if !capabilities.SupportsRuntimeSessions {
		return fmt.Errorf("runtime does not advertise required supportsRuntimeSessions capability")
	}
	observed := slices.Contains(capabilities.ToolExecutionModes, harness.ToolExecutionModeObserved)
	brokered := slices.Contains(capabilities.ToolExecutionModes, harness.ToolExecutionModeBrokered)
	if !observed && !brokered {
		return fmt.Errorf("runtime must advertise toolExecutionMode %q or %q",
			corev1alpha1.AgentRuntimeToolExecutionModeObserved, corev1alpha1.AgentRuntimeToolExecutionModeBrokered)
	}
	if !capabilities.SupportsCancel {
		return fmt.Errorf("runtime does not advertise required supportsCancel capability")
	}
	if brokered && !capabilities.SupportsContinuation {
		return fmt.Errorf("runtime advertises brokered mode but not supportsContinuation")
	}
	if brokered && len(capabilities.BrokeredToolClasses) == 0 {
		return fmt.Errorf("runtime advertises brokered mode but no brokeredToolClasses")
	}
	if capabilities.MaxOutputBytes > harness.MaxFetchTurnOutputBytes {
		return fmt.Errorf(
			"runtime maxOutputBytes %d exceeds controller fetch limit %d",
			capabilities.MaxOutputBytes,
			harness.MaxFetchTurnOutputBytes,
		)
	}
	return nil
}

func observedHarnessV1CapabilitiesFromConformance(
	capabilities *harness.CapabilitiesResponse,
) *corev1alpha1.AgentRuntimeObservedCapabilities {
	if capabilities == nil {
		return nil
	}
	modes := make([]corev1alpha1.AgentRuntimeToolExecutionMode, 0, len(capabilities.ToolExecutionModes))
	seenModes := make(map[corev1alpha1.AgentRuntimeToolExecutionMode]struct{}, len(capabilities.ToolExecutionModes))
	for _, mode := range capabilities.ToolExecutionModes {
		converted := corev1alpha1.AgentRuntimeToolExecutionMode(mode)
		if _, duplicate := seenModes[converted]; duplicate || !harness.IsKnownToolExecutionMode(mode) {
			continue
		}
		seenModes[converted] = struct{}{}
		modes = append(modes, converted)
	}
	classes := make([]corev1alpha1.AgentRuntimeBrokeredToolClass, 0, len(capabilities.BrokeredToolClasses))
	seenClasses := make(map[corev1alpha1.AgentRuntimeBrokeredToolClass]struct{}, len(capabilities.BrokeredToolClasses))
	for _, class := range capabilities.BrokeredToolClasses {
		converted := corev1alpha1.AgentRuntimeBrokeredToolClass(class)
		if _, duplicate := seenClasses[converted]; duplicate || !harness.IsKnownBrokeredToolClass(class) {
			continue
		}
		seenClasses[converted] = struct{}{}
		classes = append(classes, converted)
	}
	return &corev1alpha1.AgentRuntimeObservedCapabilities{
		ProtocolVersion:           sanitizeAgentRuntimeCapabilityValue(capabilities.ProtocolVersion),
		Transport:                 sanitizeAgentRuntimeCapabilityValue(capabilities.Transport),
		RuntimeName:               sanitizeAgentRuntimeCapabilityValue(capabilities.RuntimeName),
		RuntimeVersion:            sanitizeAgentRuntimeCapabilityValue(capabilities.RuntimeVersion),
		ProviderKind:              sanitizeAgentRuntimeCapabilityValue(string(capabilities.ProviderKind)),
		ToolExecutionModes:        modes,
		BrokeredToolClasses:       classes,
		SupportsCancel:            capabilities.SupportsCancel,
		SupportsRuntimeSessions:   capabilities.SupportsRuntimeSessions,
		SupportsContinuation:      capabilities.SupportsContinuation,
		SupportsArtifacts:         capabilities.SupportsArtifacts,
		SupportsSuspend:           capabilities.SupportsSuspend,
		SupportsWorkspaceSnapshot: capabilities.SupportsWorkspaceSnapshot,
		MaxConcurrentTurns:        capabilities.MaxConcurrentTurns,
		MaxTurnSeconds:            capabilities.MaxTurnSeconds,
		MaxOutputBytes:            capabilities.MaxOutputBytes,
	}
}

type agentRuntimeV1AuthMaterial struct {
	bearerToken           string
	secretUID             types.UID
	secretResourceVersion string
}

func (r *AgentRuntimeReconciler) agentRuntimeV1BearerAuthMaterial(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
) (agentRuntimeV1AuthMaterial, error) {
	if runtime == nil || runtime.Spec.ClientAuth.BearerAuthRef == nil {
		return agentRuntimeV1AuthMaterial{}, fmt.Errorf("AgentRuntime v1 bearerTokenSecretRef is required")
	}
	if r.APIReader == nil {
		return agentRuntimeV1AuthMaterial{}, fmt.Errorf("uncached APIReader is required for exact AgentRuntime v1 bearer Secret validation")
	}
	ref := *runtime.Spec.ClientAuth.BearerAuthRef
	var secret corev1.Secret
	if err := r.APIReader.Get(ctx, types.NamespacedName{Namespace: runtime.Namespace, Name: ref.Name}, &secret); err != nil {
		if apierrors.IsNotFound(err) {
			return agentRuntimeV1AuthMaterial{}, fmt.Errorf("AgentRuntime bearer token Secret %s/%s not found", runtime.Namespace, ref.Name)
		}
		return agentRuntimeV1AuthMaterial{}, fmt.Errorf("get AgentRuntime bearer token Secret %s/%s: %w", runtime.Namespace, ref.Name, err)
	}
	if err := validateAgentRuntimeAuthSecretUse(runtime.Name, runtime.Spec.Deployment.Endpoint, &secret); err != nil {
		return agentRuntimeV1AuthMaterial{}, err
	}
	if secret.UID == "" {
		return agentRuntimeV1AuthMaterial{}, fmt.Errorf("AgentRuntime bearer token Secret %s/%s UID is required", secret.Namespace, secret.Name)
	}
	resourceVersion := strings.TrimSpace(secret.ResourceVersion)
	if resourceVersion == "" {
		return agentRuntimeV1AuthMaterial{}, fmt.Errorf("AgentRuntime bearer token Secret %s/%s resourceVersion is required", secret.Namespace, secret.Name)
	}
	token := strings.TrimSpace(string(secret.Data[ref.Key]))
	if token == "" {
		return agentRuntimeV1AuthMaterial{}, fmt.Errorf("AgentRuntime bearer token Secret %s/%s key %q is empty or missing", secret.Namespace, secret.Name, ref.Key)
	}
	if len(token) < agentRuntimeMinBearerBytes {
		return agentRuntimeV1AuthMaterial{}, fmt.Errorf("AgentRuntime bearer token Secret %s/%s key %q must contain at least %d bytes", secret.Namespace, secret.Name, ref.Key, agentRuntimeMinBearerBytes)
	}
	if !agentRuntimeBearerTokenHeaderSafe(token) {
		return agentRuntimeV1AuthMaterial{}, fmt.Errorf("AgentRuntime bearer token Secret %s/%s key %q contains invalid HTTP header bytes", secret.Namespace, secret.Name, ref.Key)
	}
	return agentRuntimeV1AuthMaterial{
		bearerToken: token, secretUID: secret.UID, secretResourceVersion: resourceVersion,
	}, nil
}

func agentRuntimeBearerTokenHeaderSafe(token string) bool {
	for i := 0; i < len(token); i++ {
		if token[i] <= 0x20 || token[i] >= 0x7f {
			return false
		}
	}
	return true
}

func (r *AgentRuntimeReconciler) requireCurrentAgentRuntimeV1BearerAuthMaterial(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	expected agentRuntimeV1AuthMaterial,
) error {
	current, err := r.agentRuntimeV1BearerAuthMaterial(ctx, runtime)
	if err != nil {
		return fmt.Errorf("revalidate AgentRuntime v1 bearer auth after conformance: %w", err)
	}
	if current.secretUID != expected.secretUID {
		return fmt.Errorf("AgentRuntime v1 bearer token Secret was replaced during conformance; readiness fails closed")
	}
	if current.secretResourceVersion != expected.secretResourceVersion {
		return fmt.Errorf("AgentRuntime v1 bearer token Secret changed during conformance; readiness fails closed")
	}
	return nil
}

type agentRuntimeAuthMaterial struct {
	controllerBearerToken     string
	operationCapabilitySecret []byte
	controllerSecretUID       types.UID
	capabilitySecretUID       types.UID
	controllerResourceVersion string
	capabilityResourceVersion string
}

func validateAgentRuntimeSpec(runtime *corev1alpha1.AgentRuntime) error {
	if runtime == nil {
		return fmt.Errorf("AgentRuntime is required")
	}
	contract := runtime.RegisteredContractVersion()
	switch contract {
	case corev1alpha1.AgentRuntimeContractHarnessV1, corev1alpha1.AgentRuntimeContractHarnessV2:
	default:
		return fmt.Errorf("AgentRuntime contractVersion is unclassified; explicit %q or %q classification is required and omission is never protocol evidence",
			corev1alpha1.AgentRuntimeContractHarnessV1, corev1alpha1.AgentRuntimeContractHarnessV2)
	}
	if runtime.Spec.Deployment.Mode != corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint {
		return fmt.Errorf("unsupported AgentRuntime deployment mode %q", runtime.Spec.Deployment.Mode)
	}
	switch contract {
	case corev1alpha1.AgentRuntimeContractHarnessV1:
		if err := validateHarnessV1AgentRuntimeEndpointSpec(runtime.Spec.Deployment.Endpoint); err != nil {
			return err
		}
		if err := validateHarnessV1AgentRuntimeClientAuthSpec(runtime.Spec.ClientAuth); err != nil {
			return err
		}
		return validateHarnessV1AgentRuntimeCapabilitiesSpec(runtime.Spec.Capabilities)
	case corev1alpha1.AgentRuntimeContractHarnessV2:
		if err := validateAgentRuntimeEndpointSpec(runtime.Spec.Deployment.Endpoint); err != nil {
			return err
		}
		if err := validateAgentRuntimeClientAuthSpec(runtime.Spec.ClientAuth); err != nil {
			return err
		}
		return validateAgentRuntimeCapabilitiesSpec(runtime.Spec.Capabilities)
	}
	return nil
}

func validateHarnessV1AgentRuntimeEndpointSpec(endpoint string) error {
	if _, err := harness.NewClient(endpoint); err != nil {
		return fmt.Errorf("AgentRuntime endpoint is invalid: %w", err)
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return fmt.Errorf("AgentRuntime endpoint is invalid: %w", err)
	}
	if parsed.Scheme != urlSchemeHTTPS {
		host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
		if !agentRuntimeAllowInsecureLoopbackForTests || !isLoopbackAgentRuntimeEndpoint(host) {
			return fmt.Errorf("authenticated orka.harness.v1 AgentRuntime endpoints must use https")
		}
	}
	return nil
}

func validateAgentRuntimeEndpointSpec(endpoint string) error {
	if _, err := harnessv2.NewClient(endpoint); err != nil {
		return fmt.Errorf("AgentRuntime endpoint is invalid: %w", err)
	}
	return nil
}

func validateAgentRuntimeClientAuthSpec(auth corev1alpha1.AgentRuntimeClientAuth) error {
	if auth.BearerAuthRef != nil {
		return fmt.Errorf("orka.harness.v2 AgentRuntime must not carry the legacy v1 bearerTokenSecretRef auth shape")
	}
	if auth.ControllerBearerTokenSecretRef == nil || auth.ControllerBearerTokenSecretRef.Name == "" || auth.ControllerBearerTokenSecretRef.Key == "" {
		return fmt.Errorf("AgentRuntime controllerBearerTokenSecretRef name and key are required")
	}
	if auth.OperationCapabilitySecretRef == nil || auth.OperationCapabilitySecretRef.Name == "" || auth.OperationCapabilitySecretRef.Key == "" {
		return fmt.Errorf("AgentRuntime operationCapabilitySecretRef name and key are required")
	}
	if *auth.ControllerBearerTokenSecretRef == *auth.OperationCapabilitySecretRef {
		return fmt.Errorf("controller bearer token and operation capability must use distinct Secret keys")
	}
	return nil
}

func validateHarnessV1AgentRuntimeClientAuthSpec(auth corev1alpha1.AgentRuntimeClientAuth) error {
	if auth.ControllerBearerTokenSecretRef != nil || auth.OperationCapabilitySecretRef != nil {
		return fmt.Errorf("orka.harness.v1 AgentRuntime must not carry the v2 controller bearer or operation capability auth shape")
	}
	if auth.BearerAuthRef == nil || strings.TrimSpace(auth.BearerAuthRef.Name) == "" || strings.TrimSpace(auth.BearerAuthRef.Key) == "" {
		return fmt.Errorf("AgentRuntime bearerTokenSecretRef name and key are required")
	}
	return nil
}

func validateHarnessV1AgentRuntimeCapabilitiesSpec(capabilities *corev1alpha1.AgentRuntimeCapabilitiesSpec) error {
	if capabilities == nil {
		return nil
	}
	if strings.TrimSpace(capabilities.RuntimeInstanceID) != "" || capabilities.Profile != nil ||
		capabilities.MCPPolicy != nil || capabilities.Limits != nil || capabilities.WorkspaceGovernance != nil ||
		capabilities.SupportsDrain || capabilities.SupportsPublicationFinalization {
		return fmt.Errorf("orka.harness.v1 AgentRuntime capabilities must not carry harness v2 capability fields")
	}
	seenModes := make(map[corev1alpha1.AgentRuntimeToolExecutionMode]struct{}, len(capabilities.ToolExecutionModes))
	for _, mode := range capabilities.ToolExecutionModes {
		switch mode {
		case corev1alpha1.AgentRuntimeToolExecutionModeObserved, corev1alpha1.AgentRuntimeToolExecutionModeBrokered:
		default:
			return fmt.Errorf("orka.harness.v1 AgentRuntime tool execution mode %q is unsupported", mode)
		}
		if _, duplicate := seenModes[mode]; duplicate {
			return fmt.Errorf("orka.harness.v1 AgentRuntime tool execution mode %q is duplicated", mode)
		}
		seenModes[mode] = struct{}{}
	}
	seenClasses := make(map[corev1alpha1.AgentRuntimeBrokeredToolClass]struct{}, len(capabilities.BrokeredToolClasses))
	for _, class := range capabilities.BrokeredToolClasses {
		switch class {
		case corev1alpha1.AgentRuntimeBrokeredToolClassRead,
			corev1alpha1.AgentRuntimeBrokeredToolClassWrite,
			corev1alpha1.AgentRuntimeBrokeredToolClassCoordination:
		default:
			return fmt.Errorf("orka.harness.v1 AgentRuntime brokered tool class %q is unsupported", class)
		}
		if _, duplicate := seenClasses[class]; duplicate {
			return fmt.Errorf("orka.harness.v1 AgentRuntime brokered tool class %q is duplicated", class)
		}
		seenClasses[class] = struct{}{}
	}
	return nil
}

func validateAgentRuntimeCapabilitiesSpec(capabilities *corev1alpha1.AgentRuntimeCapabilitiesSpec) error {
	if capabilities == nil {
		return fmt.Errorf("AgentRuntime capabilities are required")
	}
	if capabilities.Profile == nil || capabilities.MCPPolicy == nil || capabilities.Limits == nil || capabilities.WorkspaceGovernance == nil {
		return fmt.Errorf("orka.harness.v2 AgentRuntime capabilities require profile, mcpPolicy, limits, and workspaceGovernance")
	}
	if len(capabilities.ToolExecutionModes) > 0 || len(capabilities.BrokeredToolClasses) > 0 ||
		capabilities.SupportsCancel != nil || capabilities.SupportsRuntimeSessions != nil ||
		capabilities.SupportsContinuation != nil || capabilities.SupportsArtifacts != nil {
		return fmt.Errorf("orka.harness.v2 AgentRuntime capabilities must not carry harness v1 capability fields")
	}
	if _, err := harnessv2.PathSegment("runtime instance ID", capabilities.RuntimeInstanceID); err != nil {
		return fmt.Errorf("AgentRuntime capabilities.runtimeInstanceID: %w", err)
	}
	profile, err := agentRuntimeProfile(*capabilities.Profile)
	if err != nil {
		return err
	}
	if err := validateAgentRuntimeMCPPolicyClaims(capabilities.MCPPolicy, profile); err != nil {
		return err
	}
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		return fmt.Errorf("canonicalize AgentRuntime profile: %w", err)
	}
	if string(digest) != capabilities.Profile.Digest {
		return fmt.Errorf("AgentRuntime profile digest %q does not match canonical digest %q", capabilities.Profile.Digest, digest)
	}
	if _, err := agentRuntimeProtocolLimits(*capabilities.Limits); err != nil {
		return err
	}
	governance, err := agentRuntimeWorkspaceGovernance(*capabilities.WorkspaceGovernance)
	if err != nil {
		return err
	}
	if governance.Mode == v2conformance.WorkspaceGovernanceTrusted && hasStrictWorkspaceGovernanceClaim(governance) {
		return fmt.Errorf("trusted-non-governed runtime must not claim strict workspace guarantees")
	}
	return nil
}

func hasStrictWorkspaceGovernanceClaim(governance v2conformance.WorkspaceGovernanceClaims) bool {
	return governance.OrkaOwnedWorkspaceDeltas || governance.PromptScopedBrokerAuthorization ||
		governance.NoDirectSCMPublication || governance.OrkaOwnedCleanRoomPublication ||
		governance.ExactInstanceFencing || governance.DuplicateSafeMutations || governance.CancellationSettlement
}

func agentRuntimeProfile(spec corev1alpha1.AgentRuntimeProfileSpec) (harnessv2.RuntimeProfile, error) {
	var modelLimits *harnessv2.ModelTokenLimits
	if spec.ModelLimits != nil {
		modelLimits = &harnessv2.ModelTokenLimits{Context: spec.ModelLimits.Context, Output: spec.ModelLimits.Output}
	}
	profile := harnessv2.RuntimeProfile{
		ACPProfile:               spec.ACPProfile,
		AdapterDigests:           map[string]string{spec.AdapterName: spec.AdapterDigest},
		ProviderKind:             spec.ProviderKind,
		Model:                    spec.Model,
		ModelLimits:              modelLimits,
		AgentConfigurationDigest: spec.AgentConfigurationDigest,
		ToolPolicyDigest:         spec.ToolPolicyDigest,
		ApprovalPolicyDigest:     spec.ApprovalPolicyDigest,
		MCPConfigurationDigest:   spec.MCPConfigurationDigest,
		WorkspaceIntent:          harnessv2.WorkspaceIntent(spec.WorkspaceIntent),
		ProxyCredentialRole:      spec.ProxyCredentialRole,
		ProxyCredentialScope:     spec.ProxyCredentialScope,
		ResourceClass:            spec.ResourceClass,
	}
	if spec.DigestSchemaVersion != int32(harnessv2.ProfileDigestSchemaVersion) {
		return harnessv2.RuntimeProfile{}, fmt.Errorf("AgentRuntime profile digest schema version %d is unsupported; want %d", spec.DigestSchemaVersion, harnessv2.ProfileDigestSchemaVersion)
	}
	if err := profile.Validate(); err != nil {
		return harnessv2.RuntimeProfile{}, fmt.Errorf("AgentRuntime profile: %w", err)
	}
	return profile, nil
}

func agentRuntimeProtocolLimits(spec corev1alpha1.AgentRuntimeProtocolLimits) (harnessv2.ProtocolLimits, error) {
	limits := harnessv2.ProtocolLimits{
		MaxResidentSessions:      uint32(spec.MaxResidentSessions),
		MaxConcurrentPrompts:     uint32(spec.MaxConcurrentPrompts),
		MaxRequestBytes:          int(spec.MaxRequestBytes),
		MaxEventLineBytes:        int(spec.MaxEventLineBytes),
		MaxTerminalResultBytes:   int(spec.MaxTerminalResultBytes),
		MaxBufferedEvents:        int(spec.MaxBufferedEvents),
		MaxUpdateEventsPerSecond: int(spec.MaxUpdateEventsPerSecond),
		MinPromptLeaseMillis:     spec.MinPromptLeaseMillis,
		MaxPromptLeaseMillis:     spec.MaxPromptLeaseMillis,
		MaxPendingPermissions:    uint32(spec.MaxPendingPermissions),
		MaxWorkspaceDeltaBytes:   spec.MaxWorkspaceDeltaBytes,
	}
	if spec.MaxResidentSessions <= 0 || spec.MaxConcurrentPrompts <= 0 || spec.MaxRequestBytes <= 0 ||
		spec.MaxEventLineBytes <= 0 || spec.MaxTerminalResultBytes <= 0 || spec.MaxBufferedEvents <= 0 ||
		spec.MaxUpdateEventsPerSecond <= 0 || spec.MinPromptLeaseMillis <= 0 || spec.MaxPromptLeaseMillis <= 0 ||
		spec.MaxPendingPermissions <= 0 || spec.MaxWorkspaceDeltaBytes <= 0 {
		return harnessv2.ProtocolLimits{}, fmt.Errorf("AgentRuntime protocol limits must all be positive")
	}
	if err := limits.Validate(); err != nil {
		return harnessv2.ProtocolLimits{}, fmt.Errorf("AgentRuntime protocol limits: %w", err)
	}
	return limits, nil
}

func agentRuntimeWorkspaceGovernance(spec corev1alpha1.AgentRuntimeWorkspaceGovernanceCapabilities) (v2conformance.WorkspaceGovernanceClaims, error) {
	claims := v2conformance.WorkspaceGovernanceClaims{
		Mode:                            v2conformance.WorkspaceGovernanceMode(spec.Mode),
		Trusted:                         spec.Trusted,
		OrkaOwnedWorkspaceDeltas:        spec.OrkaOwnedWorkspaceDeltas,
		PromptScopedBrokerAuthorization: spec.PromptScopedBrokerAuthorization,
		NoDirectSCMPublication:          spec.NoDirectSCMPublication,
		OrkaOwnedCleanRoomPublication:   spec.OrkaOwnedCleanRoomPublication,
		ExactInstanceFencing:            spec.ExactInstanceFencing,
		DuplicateSafeMutations:          spec.DuplicateSafeMutations,
		CancellationSettlement:          spec.CancellationSettlement,
	}
	if err := claims.Validate(); err != nil {
		return v2conformance.WorkspaceGovernanceClaims{}, fmt.Errorf("AgentRuntime workspace governance: %w", err)
	}
	return claims, nil
}

// endpointReader prefers the uncached APIReader for endpoint-policy reads so
// a Service, EndpointSlice, or backend Pod mutated just before dispatch is
// observed exactly, not at manager-cache latency. It falls back to the cached
// client when no APIReader is configured.
func (r *AgentRuntimeReconciler) endpointReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *AgentRuntimeReconciler) validateAgentRuntimeEndpointPolicy(ctx context.Context, runtime *corev1alpha1.AgentRuntime) error {
	parsed, err := url.Parse(strings.TrimSpace(runtime.Spec.Deployment.Endpoint))
	if err != nil {
		return fmt.Errorf("parse AgentRuntime endpoint: %w", err)
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if host == "" {
		return fmt.Errorf("AgentRuntime endpoint host is required")
	}
	if isLoopbackAgentRuntimeEndpoint(host) {
		if agentRuntimeAllowInsecureLoopbackForTests {
			return nil
		}
		return fmt.Errorf("AgentRuntime endpoint loopback addresses are not permitted")
	}
	serviceName, serviceNamespace, serviceEndpoint := parseAgentRuntimeServiceNamespaceHost(host)
	if serviceEndpoint {
		if serviceNamespace != runtime.Namespace {
			return fmt.Errorf("AgentRuntime service endpoint namespace %q must match AgentRuntime namespace %q", serviceNamespace, runtime.Namespace)
		}
		var service corev1.Service
		if err := r.endpointReader().Get(ctx, types.NamespacedName{Namespace: serviceNamespace, Name: serviceName}, &service); err != nil {
			return fmt.Errorf("get AgentRuntime endpoint Service %s/%s: %w", serviceNamespace, serviceName, err)
		}
		// An ExternalName Service is a CNAME alias to an arbitrary hostname:
		// it would let a namespace-scoped caller steer conformance traffic —
		// which exempts recognized .svc hostnames from the public-address
		// dial policy — at cross-namespace or internal targets.
		if service.Spec.Type == corev1.ServiceTypeExternalName {
			return fmt.Errorf("AgentRuntime endpoint Service %s/%s is an ExternalName alias; only cluster-backed Services are permitted", serviceNamespace, serviceName)
		}
		// The same-namespace exemption from the public-address dial policy is
		// justified only when the Service routes to same-namespace Pods that
		// Kubernetes selected. A selectorless Service or a manually managed
		// EndpointSlice/Endpoints backend can route the ClusterIP anywhere.
		if len(service.Spec.Selector) == 0 {
			return fmt.Errorf("AgentRuntime endpoint Service %s/%s has no selector; only Services selecting same-namespace Pods are permitted", serviceNamespace, serviceName)
		}
		servicePort, err := agentRuntimeEndpointServicePort(parsed)
		if err != nil {
			return err
		}
		return r.validateAgentRuntimeServiceBackends(ctx, &service, servicePort)
	}
	if parsed.Scheme != urlSchemeHTTPS {
		return fmt.Errorf("external AgentRuntime endpoints must use https")
	}
	// A non-service endpoint is probed and conformance-tested from the
	// controller's privileged network position. A private, link-local, or
	// otherwise non-public IP literal would let a namespace-scoped caller
	// bypass the same-namespace Service restriction (e.g. by naming a
	// ClusterIP directly) and aim controller traffic at internal addresses.
	if address, err := netip.ParseAddr(host); err == nil && !isPublicAgentRuntimeAddress(address) {
		return fmt.Errorf("external AgentRuntime endpoints must not use non-public IP literals")
	}
	return nil
}

// isPublicAgentRuntimeAddress permits only public global unicast addresses
// outside every special-use range (CGNAT, benchmarking, TEST-NETs, relays)
// for external AgentRuntime endpoints, sharing the conformance dial policy.
func isPublicAgentRuntimeAddress(address netip.Addr) bool {
	return v2conformance.PublicAddressAllowed(address)
}

// validateAgentRuntimeServiceBackends resolves every EndpointSlice address
// serving the endpoint Service back to a live same-namespace Pod that the
// Service selector actually selects and whose PodIPs contain the advertised
// address. EndpointSlice fields (managed-by label, TargetRef, Addresses) are
// all writable by a namespace caller, so none of them are trusted as proof of
// ownership; only the Pod's own status and labels are. This runs on every
// reconcile immediately before probing.
func (r *AgentRuntimeReconciler) validateAgentRuntimeServiceBackends(ctx context.Context, service *corev1.Service, targetPort int32) error {
	_, err := r.verifiedAgentRuntimeServiceBackends(ctx, service, targetPort)
	return err
}

// agentRuntimeEndpointServicePort resolves the Service port the endpoint URL
// targets: the explicit URL port, or the scheme default. Only the EndpointSlice
// port matching this Service port is pinned, so a Service exposing extra ports
// (metrics, sidecars) never diverts controller bearer/capability traffic.
func agentRuntimeEndpointServicePort(parsed *url.URL) (int32, error) {
	if portText := parsed.Port(); portText != "" {
		port, err := strconv.Atoi(portText)
		if err != nil || port <= 0 || port > 65535 {
			return 0, fmt.Errorf("AgentRuntime endpoint has an invalid port %q", portText)
		}
		return int32(port), nil
	}
	switch parsed.Scheme {
	case urlSchemeHTTPS:
		return 443, nil
	case urlSchemeHTTP:
		return 80, nil
	default:
		return 0, fmt.Errorf("AgentRuntime endpoint scheme %q has no default port", parsed.Scheme)
	}
}

// agentRuntimeServicePortName returns the name of the ServicePort matching the
// target port and whether the Service exposes it. The name (empty for a single
// unnamed port) keys the corresponding EndpointSlice port.
func agentRuntimeServicePortName(service *corev1.Service, targetPort int32) (string, bool) {
	for i := range service.Spec.Ports {
		if service.Spec.Ports[i].Port == targetPort {
			return service.Spec.Ports[i].Name, true
		}
	}
	return "", false
}

// verifiedAgentRuntimeServiceBackends validates the Service's backends and
// returns the set of verified backend Pod IPs. Dispatch pins the authenticated
// connection to one of these IPs so a Service or EndpointSlice swapped between
// this check and the dial cannot route the request to an arbitrary address
// (the validate-then-dial TOCTOU cannot be closed while routing through the
// still-mutable Service ClusterIP).
func (r *AgentRuntimeReconciler) verifiedAgentRuntimeServiceBackends(ctx context.Context, service *corev1.Service, targetPort int32) ([]string, error) {
	serviceNamespace, serviceName := service.Namespace, service.Name
	selector := labels.SelectorFromSet(service.Spec.Selector)
	deny := func(detail string) error {
		return fmt.Errorf("AgentRuntime endpoint Service %s/%s %s; only same-namespace Pods selected by the Service are permitted", serviceNamespace, serviceName, detail)
	}
	// Resolve the ServicePort the endpoint URL targets so only its matching
	// EndpointSlice port is pinned. A Service exposing extra ports (metrics,
	// sidecars) must never receive controller bearer/capability traffic, and an
	// endpoint naming a port the Service does not expose is rejected.
	servicePortName, ok := agentRuntimeServicePortName(service, targetPort)
	if !ok {
		return nil, deny(fmt.Sprintf("does not expose port %d", targetPort))
	}
	reader := r.endpointReader()
	var endpointSlices discoveryv1.EndpointSliceList
	if err := reader.List(ctx, &endpointSlices, client.InNamespace(serviceNamespace), client.MatchingLabels{
		discoveryv1.LabelServiceName: serviceName,
	}); err != nil {
		return nil, fmt.Errorf("list AgentRuntime endpoint Service %s/%s EndpointSlices: %w", serviceNamespace, serviceName, err)
	}
	verified := map[string]struct{}{}
	for i := range endpointSlices.Items {
		slice := &endpointSlices.Items[i]
		for _, endpoint := range slice.Endpoints {
			ref := endpoint.TargetRef
			if ref == nil || ref.Kind != "Pod" || (ref.Namespace != "" && ref.Namespace != serviceNamespace) {
				return nil, deny("routes to a backend that is not a same-namespace Pod")
			}
			var pod corev1.Pod
			if err := reader.Get(ctx, types.NamespacedName{Namespace: serviceNamespace, Name: ref.Name}, &pod); err != nil {
				if apierrors.IsNotFound(err) {
					return nil, deny(fmt.Sprintf("references a backend Pod %q that does not exist", ref.Name))
				}
				return nil, fmt.Errorf("get AgentRuntime endpoint backend Pod %s/%s: %w", serviceNamespace, ref.Name, err)
			}
			if !selector.Matches(labels.Set(pod.Labels)) {
				return nil, deny(fmt.Sprintf("references a backend Pod %q that the Service selector does not select", ref.Name))
			}
			podIPs := map[string]struct{}{}
			for _, podIP := range pod.Status.PodIPs {
				podIPs[podIP.IP] = struct{}{}
			}
			if pod.Status.PodIP != "" {
				podIPs[pod.Status.PodIP] = struct{}{}
			}
			// Every endpoint's advertised address is still validated against the
			// backing Pod, but only a currently serving backend enters the pinned
			// set: ApplyPinnedBackendDial round-robins the pins without a health
			// fallback, so an explicitly unready or terminating endpoint (or a Pod
			// being deleted) would make dispatch fail even though healthy backends
			// remain.
			pinnable := agentRuntimeEndpointPinnable(endpoint, &pod)
			for _, address := range endpoint.Addresses {
				if _, ok := podIPs[address]; !ok {
					return nil, deny(fmt.Sprintf("advertises address %q that is not an IP of backend Pod %q", address, ref.Name))
				}
				if !pinnable {
					continue
				}
				for _, port := range slice.Ports {
					if port.Port == nil || *port.Port <= 0 {
						continue
					}
					// EndpointSlice port names mirror the ServicePort name, so
					// pin only the port serving the endpoint's Service port.
					if agentRuntimeEndpointPortName(port.Name) != servicePortName {
						continue
					}
					verified[net.JoinHostPort(address, strconv.Itoa(int(*port.Port)))] = struct{}{}
				}
			}
		}
	}
	// Fail closed: a Service endpoint with no verified backend for the selected
	// port (a rollout gap, or a caller's validation race) must not degrade to an
	// unpinned Service ClusterIP dial, which recognized .svc endpoints also
	// exempt from the public-address control. Treating zero pins as "not pinned"
	// would let an EndpointSlice added after this check divert authenticated
	// status or mutation traffic to an unverified address.
	if len(verified) == 0 {
		return nil, deny(fmt.Sprintf("has no verified backend endpoint for port %d", targetPort))
	}
	addresses := make([]string, 0, len(verified))
	for address := range verified {
		addresses = append(addresses, address)
	}
	sort.Strings(addresses)
	return addresses, nil
}

// agentRuntimeEndpointPinnable reports whether an EndpointSlice endpoint is
// currently serving and may be pinned. A nil EndpointSlice Ready condition is
// tolerated for older controllers, but the backing Pod must independently be
// Ready so publishNotReadyAddresses or a forged slice cannot put an unready Pod
// in the authenticated dial set.
func agentRuntimeEndpointPinnable(endpoint discoveryv1.Endpoint, pod *corev1.Pod) bool {
	if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
		return false
	}
	if endpoint.Conditions.Terminating != nil && *endpoint.Conditions.Terminating {
		return false
	}
	if pod == nil || pod.DeletionTimestamp != nil {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

// agentRuntimeEndpointPortName dereferences an EndpointSlice port name, treating
// a nil name as the empty name of a single unnamed ServicePort.
func agentRuntimeEndpointPortName(name *string) string {
	if name == nil {
		return ""
	}
	return *name
}

// AgentRuntimeServiceBackendPins validates the endpoint policy and, for a
// same-namespace Service endpoint, returns the verified backend addresses to
// pin the dial to. It returns nil for non-Service endpoints (which the
// public-address dial control governs instead). The reader must be uncached
// for a dispatch-time revalidation.
func (r *AgentRuntimeReconciler) AgentRuntimeServiceBackendPins(ctx context.Context, runtime *corev1alpha1.AgentRuntime) ([]string, error) {
	if err := r.validateAgentRuntimeEndpointPolicy(ctx, runtime); err != nil {
		return nil, err
	}
	return r.serviceBackendPinsForValidatedEndpoint(ctx, runtime)
}

// serviceBackendPinsForValidatedEndpoint returns the verified Service backend
// pins for an endpoint whose policy the caller has already validated. It skips
// the endpoint-policy revalidation AgentRuntimeServiceBackendPins performs so a
// reconcile probe that validated the policy once does not repeat it.
func (r *AgentRuntimeReconciler) serviceBackendPinsForValidatedEndpoint(ctx context.Context, runtime *corev1alpha1.AgentRuntime) ([]string, error) {
	parsed, err := url.Parse(strings.TrimSpace(runtime.Spec.Deployment.Endpoint))
	if err != nil {
		return nil, fmt.Errorf("parse AgentRuntime endpoint: %w", err)
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	serviceName, serviceNamespace, serviceEndpoint := parseAgentRuntimeServiceNamespaceHost(host)
	if !serviceEndpoint {
		return nil, nil
	}
	servicePort, err := agentRuntimeEndpointServicePort(parsed)
	if err != nil {
		return nil, err
	}
	var service corev1.Service
	if err := r.endpointReader().Get(ctx, types.NamespacedName{Namespace: serviceNamespace, Name: serviceName}, &service); err != nil {
		return nil, fmt.Errorf("get AgentRuntime endpoint Service %s/%s: %w", serviceNamespace, serviceName, err)
	}
	return r.verifiedAgentRuntimeServiceBackends(ctx, &service, servicePort)
}

// PinnedBackendDialTransport returns a proxy-disabled transport that dials only
// the given verified backend addresses, ignoring the Service ClusterIP the URL
// would otherwise resolve to. Pinning the authenticated connection to a
// verified Pod backend closes the validate-then-dial TOCTOU that routing
// through the still-mutable Service would leave open. It shares the conformance
// package's dial implementation so dispatch and conformance pin identically.
func PinnedBackendDialTransport(addresses []string) *http.Transport {
	return v2conformance.PinnedBackendDialTransport(addresses)
}

// agentRuntimeV1DialControlledClient returns a copy of the configured harness v1
// TLS client whose transport dials only verified Service backends (when pins are
// present) or rejects any dial to a non-public address (for a non-Service
// endpoint), while preserving the client's TLS roots. The v1 client would
// otherwise dial the mutable Service ClusterIP or follow an attacker-controlled
// hostname that resolves — or rebinds — to a private, link-local, or
// cross-namespace address, so applying the same per-dial control here brings v1
// readiness probes to parity with the v2 conformance dial guarantees.
func agentRuntimeV1DialControlledClient(base *http.Client, endpoint string, backendPins []string) *http.Client {
	transport := &http.Transport{}
	clientCopy := http.Client{}
	if base != nil {
		clientCopy = *base
		if baseTransport, ok := base.Transport.(*http.Transport); ok && baseTransport != nil {
			transport = baseTransport.Clone()
		}
	}
	switch {
	case len(backendPins) > 0:
		v2conformance.ApplyPinnedBackendDial(transport, backendPins)
	case agentRuntimeEndpointRequiresPublicDial(endpoint):
		v2conformance.ApplyPublicAddressDialControl(transport)
	}
	clientCopy.Transport = transport
	return &clientCopy
}

// agentRuntimeEndpointRequiresPublicDial reports whether conformance dials to
// the endpoint must be restricted to public addresses: everything except
// recognized same-namespace Service DNS forms (which legitimately resolve to
// cluster-internal addresses) and the loopback test escape hatch.
func agentRuntimeEndpointRequiresPublicDial(endpoint string) bool {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return true
	}
	host := strings.TrimSuffix(strings.ToLower(parsed.Hostname()), ".")
	if isLoopbackAgentRuntimeEndpoint(host) && agentRuntimeAllowInsecureLoopbackForTests {
		return false
	}
	_, _, serviceEndpoint := parseAgentRuntimeServiceNamespaceHost(host)
	return !serviceEndpoint
}

func isLoopbackAgentRuntimeEndpoint(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func parseAgentRuntimeServiceNamespaceHost(host string) (serviceName, serviceNamespace string, ok bool) {
	segments := strings.Split(strings.TrimSuffix(strings.ToLower(host), "."), ".")
	switch {
	case len(segments) == 3 && segments[2] == k8sServiceDNSLabel:
		return segments[0], segments[1], segments[0] != "" && segments[1] != ""
	case len(segments) == 4 && segments[2] == k8sServiceDNSLabel && segments[3] == k8sClusterDNSLabel:
		return segments[0], segments[1], segments[0] != "" && segments[1] != ""
	case len(segments) == 5 && segments[2] == "svc" && segments[3] == "cluster" && segments[4] == "local":
		return segments[0], segments[1], segments[0] != "" && segments[1] != ""
	default:
		return "", "", false
	}
}

func validateAgentRuntimeAuthSecretUse(runtimeName string, endpoint string, secret *corev1.Secret) error {
	if secret == nil {
		return fmt.Errorf("AgentRuntime auth Secret is required")
	}
	if secret.Labels[agentRuntimeAuthUseLabel] != scheduledRunLabelValue {
		return fmt.Errorf("AgentRuntime auth Secret %s/%s must set label %s=true", secret.Namespace, secret.Name, agentRuntimeAuthUseLabel)
	}
	if boundRuntime := strings.TrimSpace(secret.Labels[agentRuntimeAuthRefNameLabel]); boundRuntime != "" && boundRuntime != runtimeName {
		return fmt.Errorf("AgentRuntime auth Secret %s/%s is bound to AgentRuntime %q", secret.Namespace, secret.Name, boundRuntime)
	}
	boundEndpoint := strings.TrimSpace(secret.Annotations[agentRuntimeAuthEndpointAnnotation])
	if boundEndpoint == "" {
		return fmt.Errorf("AgentRuntime auth Secret %s/%s must set annotation %s", secret.Namespace, secret.Name, agentRuntimeAuthEndpointAnnotation)
	}
	if boundEndpoint != strings.TrimSpace(endpoint) {
		return fmt.Errorf("AgentRuntime auth Secret %s/%s endpoint binding does not match AgentRuntime endpoint %q", secret.Namespace, secret.Name, sanitizeAgentRuntimeEndpointForStatus(endpoint))
	}
	return nil
}

func (r *AgentRuntimeReconciler) agentRuntimeAuthMaterial(ctx context.Context, runtime *corev1alpha1.AgentRuntime) (agentRuntimeAuthMaterial, error) {
	if runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef == nil || runtime.Spec.ClientAuth.OperationCapabilitySecretRef == nil {
		return agentRuntimeAuthMaterial{}, fmt.Errorf("AgentRuntime v2 client auth references are required")
	}
	controllerRef := *runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef
	capabilityRef := *runtime.Spec.ClientAuth.OperationCapabilitySecretRef
	controllerSecret, err := r.getAgentRuntimeAuthSecret(ctx, runtime, controllerRef)
	if err != nil {
		return agentRuntimeAuthMaterial{}, fmt.Errorf("controller bearer token: %w", err)
	}
	capabilitySecret, err := r.getAgentRuntimeAuthSecret(ctx, runtime, capabilityRef)
	if err != nil {
		return agentRuntimeAuthMaterial{}, fmt.Errorf("operation capability secret: %w", err)
	}
	controllerToken, ok := controllerSecret.Data[controllerRef.Key]
	if !ok || len(controllerToken) < 32 {
		return agentRuntimeAuthMaterial{}, fmt.Errorf("AgentRuntime controller bearer token Secret %s/%s key %q must contain at least 32 bytes", runtime.Namespace, controllerRef.Name, controllerRef.Key)
	}
	capabilityKey, ok := capabilitySecret.Data[capabilityRef.Key]
	if !ok || len(capabilityKey) < harnessv2.MinCapabilitySecretBytes {
		return agentRuntimeAuthMaterial{}, fmt.Errorf("AgentRuntime operation capability Secret %s/%s key %q must contain at least %d bytes", runtime.Namespace, capabilityRef.Name, capabilityRef.Key, harnessv2.MinCapabilitySecretBytes)
	}
	// Runtimes that project these Secrets as files trim surrounding
	// whitespace before verifying, while the controller signs with the raw
	// bytes; a trailing newline would make every signed capability invalid
	// and leave the runtime permanently unready. Fail closed with a clear
	// message instead.
	if !bytes.Equal(controllerToken, bytes.TrimSpace(controllerToken)) {
		return agentRuntimeAuthMaterial{}, fmt.Errorf("AgentRuntime controller bearer token Secret %s/%s key %q must not contain surrounding whitespace", runtime.Namespace, controllerRef.Name, controllerRef.Key)
	}
	if !bytes.Equal(capabilityKey, bytes.TrimSpace(capabilityKey)) {
		return agentRuntimeAuthMaterial{}, fmt.Errorf("AgentRuntime operation capability Secret %s/%s key %q must not contain surrounding whitespace", runtime.Namespace, capabilityRef.Name, capabilityRef.Key)
	}
	// The bearer is transmitted on every request; the capability secret is a
	// signing key that must never transit. Identical resolved bytes would let
	// a bearer holder mint valid status and exact-fence capabilities.
	if bytes.Equal(bytes.TrimSpace(controllerToken), bytes.TrimSpace(capabilityKey)) {
		return agentRuntimeAuthMaterial{}, fmt.Errorf("AgentRuntime controller bearer token and operation capability secret must resolve to distinct values")
	}
	return agentRuntimeAuthMaterial{
		controllerBearerToken:     string(controllerToken),
		operationCapabilitySecret: append([]byte(nil), capabilityKey...),
		controllerSecretUID:       controllerSecret.UID,
		capabilitySecretUID:       capabilitySecret.UID,
		controllerResourceVersion: controllerSecret.ResourceVersion,
		capabilityResourceVersion: capabilitySecret.ResourceVersion,
	}, nil
}

// requireCurrentAgentRuntimeAuthMaterial revalidates both v2 auth Secrets
// after conformance so readiness fails closed when either was replaced or
// rotated while the probe ran, mirroring the v1 bearer recheck.
func (r *AgentRuntimeReconciler) requireCurrentAgentRuntimeAuthMaterial(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	expected agentRuntimeAuthMaterial,
) error {
	current, err := r.agentRuntimeAuthMaterial(ctx, runtime)
	if err != nil {
		return fmt.Errorf("revalidate AgentRuntime v2 auth material after conformance: %w", err)
	}
	if current.controllerSecretUID != expected.controllerSecretUID || current.capabilitySecretUID != expected.capabilitySecretUID {
		return fmt.Errorf("AgentRuntime v2 auth Secret was replaced during conformance; readiness fails closed")
	}
	if current.controllerResourceVersion != expected.controllerResourceVersion || current.capabilityResourceVersion != expected.capabilityResourceVersion {
		return fmt.Errorf("AgentRuntime v2 auth Secret changed during conformance; readiness fails closed")
	}
	return nil
}

func (r *AgentRuntimeReconciler) getAgentRuntimeAuthSecret(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	ref corev1alpha1.AgentRuntimeSecretKeyReference,
) (*corev1.Secret, error) {
	// Prefer the uncached APIReader so rotation is observed exactly, not at
	// informer-cache latency; the dispatcher constructs this reconciler
	// without one and falls back to the cached client.
	reader := client.Reader(r.Client)
	if r.APIReader != nil {
		reader = r.APIReader
	}
	var secret corev1.Secret
	if err := reader.Get(ctx, types.NamespacedName{Namespace: runtime.Namespace, Name: ref.Name}, &secret); err != nil {
		return nil, fmt.Errorf("get AgentRuntime auth Secret %s/%s: %w", runtime.Namespace, ref.Name, err)
	}
	if err := validateAgentRuntimeAuthSecretUse(runtime.Name, runtime.Spec.Deployment.Endpoint, &secret); err != nil {
		return nil, err
	}
	return &secret, nil
}

func observedCapabilitiesFromConformance(
	probe v2conformance.Result,
	registeredProfile harnessv2.RuntimeProfile,
) *corev1alpha1.AgentRuntimeObservedCapabilities {
	if probe.ObservedCapabilities == nil && probe.ObservedStatus == nil {
		return nil
	}
	observed := &corev1alpha1.AgentRuntimeObservedCapabilities{}
	if capabilities := probe.ObservedCapabilities; capabilities != nil {
		base := capabilities.CapabilitiesResponse
		observed.ProtocolVersion = sanitizeAgentRuntimeCapabilityValue(base.Protocol)
		observed.Transport = sanitizeAgentRuntimeCapabilityValue(base.Transport)
		observed.ACPVersion = sanitizeAgentRuntimeCapabilityValue(base.ACPVersion)
		observed.RuntimeProfileDigest = sanitizeAgentRuntimeCapabilityValue(string(base.RuntimeProfileDigest))
		observed.ProfileDigestSchemaVersion = int32(base.ProfileDigestSchemaVersion)
		limits := agentRuntimeObservedProtocolLimits(base.Limits)
		observed.Limits = &limits
		observed.SupportsDrain = base.SupportsDrain
		observed.SupportsPublicationFinalization = base.SupportsPublicationFinalization
		observed.WorkspaceGovernance = &corev1alpha1.AgentRuntimeWorkspaceGovernanceCapabilities{
			Mode:                            corev1alpha1.AgentRuntimeWorkspaceGovernanceMode(capabilities.WorkspaceGovernance.Mode),
			Trusted:                         capabilities.WorkspaceGovernance.Trusted,
			OrkaOwnedWorkspaceDeltas:        capabilities.WorkspaceGovernance.OrkaOwnedWorkspaceDeltas,
			PromptScopedBrokerAuthorization: capabilities.WorkspaceGovernance.PromptScopedBrokerAuthorization,
			NoDirectSCMPublication:          capabilities.WorkspaceGovernance.NoDirectSCMPublication,
			OrkaOwnedCleanRoomPublication:   capabilities.WorkspaceGovernance.OrkaOwnedCleanRoomPublication,
			ExactInstanceFencing:            capabilities.WorkspaceGovernance.ExactInstanceFencing,
			DuplicateSafeMutations:          capabilities.WorkspaceGovernance.DuplicateSafeMutations,
			CancellationSettlement:          capabilities.WorkspaceGovernance.CancellationSettlement,
		}
		if len(base.AdapterDigests) == 1 {
			for name, digest := range base.AdapterDigests {
				observed.AdapterName = sanitizeAgentRuntimeCapabilityValue(name)
				observed.AdapterDigest = sanitizeAgentRuntimeCapabilityValue(digest)
			}
		}
		// Conformance already proved that the registered provider and model are
		// members of the advertised capability sets. Persist that exact selected
		// identity even when the runtime advertises additional supported values.
		observed.ProviderKind = sanitizeAgentRuntimeCapabilityValue(registeredProfile.ProviderKind)
		observed.Model = sanitizeAgentRuntimeCapabilityValue(registeredProfile.Model)
	}
	if status := probe.ObservedStatus; status != nil {
		observed.RuntimeInstanceID = sanitizeAgentRuntimeCapabilityValue(string(status.Fence.RuntimeInstanceID))
		observed.SupervisorBootID = sanitizeAgentRuntimeCapabilityValue(string(status.Fence.SupervisorBootID))
		observed.ControllerEpoch = int64(status.Fence.ControllerEpoch)
		observed.RuntimePoolUID = sanitizeAgentRuntimeCapabilityValue(string(status.Fence.RuntimePoolUID))
		observed.RuntimePoolGeneration = int64(status.Fence.RuntimePoolGeneration)
		observed.Lifecycle = sanitizeAgentRuntimeCapabilityValue(string(status.Lifecycle))
	}
	return observed
}

func agentRuntimeObservedProtocolLimits(limits harnessv2.ProtocolLimits) corev1alpha1.AgentRuntimeProtocolLimits {
	return corev1alpha1.AgentRuntimeProtocolLimits{
		MaxResidentSessions:      int32(limits.MaxResidentSessions),
		MaxConcurrentPrompts:     int32(limits.MaxConcurrentPrompts),
		MaxRequestBytes:          int32(limits.MaxRequestBytes),
		MaxEventLineBytes:        int32(limits.MaxEventLineBytes),
		MaxTerminalResultBytes:   int32(limits.MaxTerminalResultBytes),
		MaxBufferedEvents:        int32(limits.MaxBufferedEvents),
		MaxUpdateEventsPerSecond: int32(limits.MaxUpdateEventsPerSecond),
		MinPromptLeaseMillis:     limits.MinPromptLeaseMillis,
		MaxPromptLeaseMillis:     limits.MaxPromptLeaseMillis,
		MaxPendingPermissions:    int32(limits.MaxPendingPermissions),
		MaxWorkspaceDeltaBytes:   limits.MaxWorkspaceDeltaBytes,
	}
}

func (r *AgentRuntimeReconciler) rejectAgentRuntimePoolIdentity(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	owner *corev1alpha1.AgentRuntime,
) (ctrl.Result, error) {
	message := "observed runtime pool identity is already owned by another AgentRuntime"
	if owner != nil {
		message = fmt.Sprintf("observed runtime pool identity is already owned by AgentRuntime %q", owner.Name)
	}
	return r.writeAgentRuntimeStatus(ctx, runtime, false, nil, "", "", message)
}

func (r *AgentRuntimeReconciler) rejectManagedRuntimePoolIdentity(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	owner *corev1alpha1.RuntimePool,
) (ctrl.Result, error) {
	message := "observed runtime pool identity is already owned by a managed RuntimePool"
	if owner != nil {
		message = fmt.Sprintf("observed runtime pool identity is already owned by managed RuntimePool %q", owner.Name)
	}
	return r.writeAgentRuntimeStatus(ctx, runtime, false, nil, "", "", message)
}

func (r *AgentRuntimeReconciler) writeAgentRuntimeStatus(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	ready bool,
	observed *corev1alpha1.AgentRuntimeObservedCapabilities,
	controllerAuthResourceVersion string,
	capabilityAuthResourceVersion string,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	runtime.Status.Ready = ready
	runtime.Status.ObservedGeneration = runtime.Generation
	runtime.Status.ObservedCapabilities = observed
	runtime.Status.ObservedControllerAuthRefResourceVersion = ""
	runtime.Status.ObservedOperationCapabilityRefResourceVersion = ""
	runtime.Status.ObservedAuthRefResourceVersion = ""
	switch runtime.RegisteredContractVersion() {
	case corev1alpha1.AgentRuntimeContractHarnessV1:
		runtime.Status.ObservedAuthRefResourceVersion = controllerAuthResourceVersion
	case corev1alpha1.AgentRuntimeContractHarnessV2:
		runtime.Status.ObservedControllerAuthRefResourceVersion = controllerAuthResourceVersion
		runtime.Status.ObservedOperationCapabilityRefResourceVersion = capabilityAuthResourceVersion
		// Preserve the historical v2 status alias during coexistence. V1 never
		// writes the two v2-specific auth version fields.
		runtime.Status.ObservedAuthRefResourceVersion = controllerAuthResourceVersion
	}
	runtime.Status.LastValidated = &now
	runtime.Status.Message = sanitizeAgentRuntimeStatusMessage(message)
	condition := metav1.Condition{
		Type:               agentRuntimeReadyCondition,
		ObservedGeneration: runtime.Generation,
		LastTransitionTime: now,
		Message:            runtime.Status.Message,
	}
	if ready {
		condition.Status = metav1.ConditionTrue
		condition.Reason = agentRuntimeReasonReady
	} else {
		condition.Status = metav1.ConditionFalse
		condition.Reason = agentRuntimeReasonNotReady
	}
	meta.SetStatusCondition(&runtime.Status.Conditions, condition)
	if err := r.Status().Update(ctx, runtime); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: agentRuntimeRequeue}, nil
}

func sanitizeAgentRuntimeEndpointForStatus(endpoint string) string {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return events.RedactExecutionEventText(strings.TrimSpace(endpoint))
	}
	return parsed.Scheme + "://" + parsed.Host
}

func sanitizeAgentRuntimeStatusMessage(message string) string {
	message = events.RedactExecutionEventText(strings.TrimSpace(message))
	return truncateUTF8(strings.ToValidUTF8(message, "�"), 1024)
}

func sanitizeAgentRuntimeCapabilityValue(value string) string {
	value = events.RedactExecutionEventText(strings.TrimSpace(value))
	return truncateUTF8(strings.ToValidUTF8(value, "�"), 512)
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentRuntimeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.AgentRuntime{}).
		WithEventFilter(agentRuntimeEventPredicate()).
		WithOptions(controllerpkg.Options{MaxConcurrentReconciles: 1}).
		Named("agentruntime").
		Complete(r)
}

func agentRuntimeEventPredicate() predicate.Predicate {
	return predicate.Or(
		predicate.GenerationChangedPredicate{},
		predicate.Funcs{UpdateFunc: func(update event.UpdateEvent) bool {
			if update.ObjectOld == nil || update.ObjectNew == nil {
				return false
			}
			return update.ObjectOld.GetDeletionTimestamp().IsZero() != update.ObjectNew.GetDeletionTimestamp().IsZero()
		}},
	)
}
