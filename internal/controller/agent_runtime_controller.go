/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"net/netip"
	"net/url"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	v2conformance "github.com/orka-agents/orka/internal/harness/v2/conformance"
)

var agentRuntimeAllowInsecureLoopbackForTests bool

const (
	agentRuntimeReadyCondition = "Ready"
	agentRuntimeReasonReady    = "ConformancePassed"
	agentRuntimeReasonNotReady = "ConformanceFailed"
	agentRuntimeProbeTimeout   = 60 * time.Second
	agentRuntimeRequeue        = 30 * time.Second

	agentRuntimeAuthUseLabel           = "orka.ai/agent-runtime-auth"
	agentRuntimeAuthRefNameLabel       = "orka.ai/agent-runtime-name"
	agentRuntimeAuthEndpointAnnotation = "orka.ai/agent-runtime-endpoint"
)

// AgentRuntimeReconciler reconciles external orka.harness.v2 registry entries.
type AgentRuntimeReconciler struct {
	client.Client
	Scheme *k8sruntime.Scheme
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch

// Reconcile validates one exact external v2 runtime and publishes condition-ready status.
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
	observed, ready, controllerAuthVersion, capabilityAuthVersion, message := r.probeAgentRuntime(ctx, runtime)
	return r.updateAgentRuntimeStatus(ctx, runtime, ready, observed, controllerAuthVersion, capabilityAuthVersion, message)
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
	auth, err := r.agentRuntimeAuthMaterial(ctx, runtime)
	if err != nil {
		return nil, false, "", "", err.Error()
	}
	profile, err := agentRuntimeProfile(runtime.Spec.Capabilities.Profile)
	if err != nil {
		return nil, false, auth.controllerResourceVersion, auth.capabilityResourceVersion, err.Error()
	}
	limits, err := agentRuntimeProtocolLimits(runtime.Spec.Capabilities.Limits)
	if err != nil {
		return nil, false, auth.controllerResourceVersion, auth.capabilityResourceVersion, err.Error()
	}
	governance, err := agentRuntimeWorkspaceGovernance(runtime.Spec.Capabilities.WorkspaceGovernance)
	if err != nil {
		return nil, false, auth.controllerResourceVersion, auth.capabilityResourceVersion, err.Error()
	}
	deepProbe := runtime.Status.ObservedGeneration != runtime.Generation || !runtime.Status.Ready ||
		runtime.Status.ObservedControllerAuthRefResourceVersion != auth.controllerResourceVersion ||
		runtime.Status.ObservedOperationCapabilityRefResourceVersion != auth.capabilityResourceVersion
	probeCtx, cancel := context.WithTimeout(ctx, agentRuntimeProbeTimeout)
	defer cancel()
	target := v2conformance.Target{
		BaseURL:                         runtime.Spec.Deployment.Endpoint,
		ControllerBearerToken:           auth.controllerBearerToken,
		OperationCapabilitySecret:       auth.operationCapabilitySecret,
		ControlTimeout:                  agentRuntimeProbeTimeout,
		ExpectedRuntimeInstanceID:       harnessv2.RuntimeInstanceID(runtime.Spec.Capabilities.RuntimeInstanceID),
		Profile:                         profile,
		Limits:                          limits,
		SupportsDrain:                   runtime.Spec.Capabilities.SupportsDrain,
		SupportsPublicationFinalization: runtime.Spec.Capabilities.SupportsPublicationFinalization,
		WorkspaceGovernance:             governance,
		ProbeLifecycle:                  deepProbe,
	}
	probe := v2conformance.Check(probeCtx, target)
	if !deepProbe && probe.Passed && agentRuntimeAuthenticatedIdentityChanged(runtime.Status.ObservedCapabilities, probe.ObservedStatus) {
		target.ProbeLifecycle = true
		probe = v2conformance.Check(probeCtx, target)
	}
	observed := observedCapabilitiesFromConformance(probe)
	if !probe.Passed {
		return observed, false, auth.controllerResourceVersion, auth.capabilityResourceVersion,
			sanitizeAgentRuntimeStatusMessage(probe.Message)
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

type agentRuntimeAuthMaterial struct {
	controllerBearerToken     string
	operationCapabilitySecret []byte
	controllerResourceVersion string
	capabilityResourceVersion string
}

func validateAgentRuntimeSpec(runtime *corev1alpha1.AgentRuntime) error {
	if runtime == nil {
		return fmt.Errorf("AgentRuntime is required")
	}
	if runtime.Spec.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return fmt.Errorf("unsupported AgentRuntime contractVersion %q; want %q", runtime.Spec.ContractVersion, corev1alpha1.AgentRuntimeContractHarnessV2)
	}
	if runtime.Spec.Deployment.Mode != corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint {
		return fmt.Errorf("unsupported AgentRuntime deployment mode %q", runtime.Spec.Deployment.Mode)
	}
	if err := validateAgentRuntimeEndpointSpec(runtime.Spec.Deployment.Endpoint); err != nil {
		return err
	}
	if err := validateAgentRuntimeClientAuthSpec(runtime.Spec.ClientAuth); err != nil {
		return err
	}
	return validateAgentRuntimeCapabilitiesSpec(runtime.Spec.Capabilities)
}

func validateAgentRuntimeEndpointSpec(endpoint string) error {
	if _, err := harnessv2.NewClient(endpoint); err != nil {
		return fmt.Errorf("AgentRuntime endpoint is invalid: %w", err)
	}
	return nil
}

func validateAgentRuntimeClientAuthSpec(auth corev1alpha1.AgentRuntimeClientAuth) error {
	if auth.ControllerBearerTokenSecretRef.Name == "" || auth.ControllerBearerTokenSecretRef.Key == "" {
		return fmt.Errorf("AgentRuntime controllerBearerTokenSecretRef name and key are required")
	}
	if auth.OperationCapabilitySecretRef.Name == "" || auth.OperationCapabilitySecretRef.Key == "" {
		return fmt.Errorf("AgentRuntime operationCapabilitySecretRef name and key are required")
	}
	if auth.ControllerBearerTokenSecretRef == auth.OperationCapabilitySecretRef {
		return fmt.Errorf("controller bearer token and operation capability must use distinct Secret keys")
	}
	return nil
}

func validateAgentRuntimeCapabilitiesSpec(capabilities *corev1alpha1.AgentRuntimeCapabilitiesSpec) error {
	if capabilities == nil {
		return fmt.Errorf("AgentRuntime capabilities are required")
	}
	if _, err := harnessv2.PathSegment("runtime instance ID", capabilities.RuntimeInstanceID); err != nil {
		return fmt.Errorf("AgentRuntime capabilities.runtimeInstanceID: %w", err)
	}
	profile, err := agentRuntimeProfile(capabilities.Profile)
	if err != nil {
		return err
	}
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		return fmt.Errorf("canonicalize AgentRuntime profile: %w", err)
	}
	if string(digest) != capabilities.Profile.Digest {
		return fmt.Errorf("AgentRuntime profile digest %q does not match canonical digest %q", capabilities.Profile.Digest, digest)
	}
	if _, err := agentRuntimeProtocolLimits(capabilities.Limits); err != nil {
		return err
	}
	governance, err := agentRuntimeWorkspaceGovernance(capabilities.WorkspaceGovernance)
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
		if err := r.Get(ctx, types.NamespacedName{Namespace: serviceNamespace, Name: serviceName}, &service); err != nil {
			return fmt.Errorf("get AgentRuntime endpoint Service %s/%s: %w", serviceNamespace, serviceName, err)
		}
		return nil
	}
	if parsed.Scheme != urlSchemeHTTPS {
		return fmt.Errorf("external AgentRuntime endpoints must use https")
	}
	return nil
}

func isLoopbackAgentRuntimeEndpoint(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	address, err := netip.ParseAddr(host)
	return err == nil && address.IsLoopback()
}

func parseAgentRuntimeServiceNamespaceHost(host string) (serviceName, serviceNamespace string, ok bool) {
	labels := strings.Split(strings.TrimSuffix(strings.ToLower(host), "."), ".")
	switch {
	case len(labels) == 3 && labels[2] == k8sServiceDNSLabel:
		return labels[0], labels[1], labels[0] != "" && labels[1] != ""
	case len(labels) == 4 && labels[2] == k8sServiceDNSLabel && labels[3] == k8sClusterDNSLabel:
		return labels[0], labels[1], labels[0] != "" && labels[1] != ""
	case len(labels) == 5 && labels[2] == "svc" && labels[3] == "cluster" && labels[4] == "local":
		return labels[0], labels[1], labels[0] != "" && labels[1] != ""
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
	controllerRef := runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef
	capabilityRef := runtime.Spec.ClientAuth.OperationCapabilitySecretRef
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
	return agentRuntimeAuthMaterial{
		controllerBearerToken:     string(controllerToken),
		operationCapabilitySecret: append([]byte(nil), capabilityKey...),
		controllerResourceVersion: controllerSecret.ResourceVersion,
		capabilityResourceVersion: capabilitySecret.ResourceVersion,
	}, nil
}

func (r *AgentRuntimeReconciler) getAgentRuntimeAuthSecret(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	ref corev1alpha1.AgentRuntimeSecretKeyReference,
) (*corev1.Secret, error) {
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: runtime.Namespace, Name: ref.Name}, &secret); err != nil {
		return nil, fmt.Errorf("get AgentRuntime auth Secret %s/%s: %w", runtime.Namespace, ref.Name, err)
	}
	if err := validateAgentRuntimeAuthSecretUse(runtime.Name, runtime.Spec.Deployment.Endpoint, &secret); err != nil {
		return nil, err
	}
	return &secret, nil
}

func observedCapabilitiesFromConformance(probe v2conformance.Result) *corev1alpha1.AgentRuntimeObservedCapabilities {
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
		observed.Limits = agentRuntimeObservedProtocolLimits(base.Limits)
		observed.SupportsDrain = base.SupportsDrain
		observed.SupportsPublicationFinalization = base.SupportsPublicationFinalization
		observed.WorkspaceGovernance = corev1alpha1.AgentRuntimeWorkspaceGovernanceCapabilities{
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
		if len(base.Provider.ProviderKinds) == 1 {
			observed.ProviderKind = sanitizeAgentRuntimeCapabilityValue(base.Provider.ProviderKinds[0])
		}
		if len(base.Provider.Models) == 1 {
			observed.Model = sanitizeAgentRuntimeCapabilityValue(base.Provider.Models[0])
		}
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

func (r *AgentRuntimeReconciler) updateAgentRuntimeStatus(
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
	runtime.Status.ObservedControllerAuthRefResourceVersion = controllerAuthResourceVersion
	runtime.Status.ObservedOperationCapabilityRefResourceVersion = capabilityAuthResourceVersion
	runtime.Status.ObservedAuthRefResourceVersion = controllerAuthResourceVersion
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
	if len(message) > 1024 {
		return message[:1024]
	}
	return message
}

func sanitizeAgentRuntimeCapabilityValue(value string) string {
	value = events.RedactExecutionEventText(strings.TrimSpace(value))
	if len(value) > 512 {
		return value[:512]
	}
	return value
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentRuntimeReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.AgentRuntime{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Named("agentruntime").
		Complete(r)
}
