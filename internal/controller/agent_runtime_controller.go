/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/netip"
	"net/url"
	"slices"

	"strings"
	"time"

	discoveryv1 "k8s.io/api/discovery/v1"

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
	"github.com/orka-agents/orka/internal/harness"
	v1conformance "github.com/orka-agents/orka/internal/harness/conformance"
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
	agentRuntimeMinBearerBytes = 32

	agentRuntimeAuthUseLabel           = "orka.ai/agent-runtime-auth"
	agentRuntimeAuthRefNameLabel       = "orka.ai/agent-runtime-name"
	agentRuntimeAuthEndpointAnnotation = "orka.ai/agent-runtime-endpoint"
)

// AgentRuntimeReconciler reconciles external harness v1 and v2 registry entries.
type AgentRuntimeReconciler struct {
	client.Client
	APIReader           client.Reader
	Scheme              *k8sruntime.Scheme
	HarnessV1HTTPClient *http.Client
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
	if runtime.RegisteredContractVersion() == corev1alpha1.AgentRuntimeContractHarnessV1 {
		return r.probeHarnessV1AgentRuntime(ctx, runtime)
	}
	auth, err := r.agentRuntimeAuthMaterial(ctx, runtime)
	if err != nil {
		return nil, false, "", "", err.Error()
	}
	profile, err := agentRuntimeProfile(*runtime.Spec.Capabilities.Profile)
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
		RequirePublicAddresses:          agentRuntimeEndpointRequiresPublicDial(runtime.Spec.Deployment.Endpoint),
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

func (r *AgentRuntimeReconciler) probeHarnessV1AgentRuntime(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
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
		HTTPClient:     r.HarnessV1HTTPClient,
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
		capabilities.Limits != nil || capabilities.WorkspaceGovernance != nil ||
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
	if capabilities.Profile == nil || capabilities.Limits == nil || capabilities.WorkspaceGovernance == nil {
		return fmt.Errorf("orka.harness.v2 AgentRuntime capabilities require profile, limits, and workspaceGovernance")
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
		return r.validateAgentRuntimeServiceBackends(ctx, serviceNamespace, serviceName)
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

// validateAgentRuntimeServiceBackends requires every EndpointSlice serving
// the endpoint Service to be managed by the Kubernetes endpoint controllers
// for selector-based Services and to target same-namespace Pods. Manually
// created slices (and legacy Endpoints, which are mirrored with a different
// managed-by value) can steer a ClusterIP at arbitrary internal addresses.
// This is re-validated on every reconcile immediately before probing.
func (r *AgentRuntimeReconciler) validateAgentRuntimeServiceBackends(ctx context.Context, serviceNamespace, serviceName string) error {
	var endpointSlices discoveryv1.EndpointSliceList
	if err := r.List(ctx, &endpointSlices, client.InNamespace(serviceNamespace), client.MatchingLabels{
		discoveryv1.LabelServiceName: serviceName,
	}); err != nil {
		return fmt.Errorf("list AgentRuntime endpoint Service %s/%s EndpointSlices: %w", serviceNamespace, serviceName, err)
	}
	for i := range endpointSlices.Items {
		slice := &endpointSlices.Items[i]
		if slice.Labels[discoveryv1.LabelManagedBy] != "endpointslice-controller.k8s.io" {
			return fmt.Errorf("AgentRuntime endpoint Service %s/%s has an EndpointSlice %q not managed by the Kubernetes endpoint controller; manually managed backends are not permitted", serviceNamespace, serviceName, slice.Name)
		}
		for _, endpoint := range slice.Endpoints {
			if endpoint.TargetRef == nil || endpoint.TargetRef.Kind != "Pod" ||
				(endpoint.TargetRef.Namespace != "" && endpoint.TargetRef.Namespace != serviceNamespace) {
				return fmt.Errorf("AgentRuntime endpoint Service %s/%s routes to a backend that is not a same-namespace Pod; only same-namespace Pod backends are permitted", serviceNamespace, serviceName)
			}
		}
	}
	return nil
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
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Named("agentruntime").
		Complete(r)
}
