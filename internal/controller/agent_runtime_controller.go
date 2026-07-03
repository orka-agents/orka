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
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/harness/conformance"
)

const (
	agentRuntimeReadyCondition = "Ready"
	agentRuntimeReasonReady    = "ConformancePassed"
	agentRuntimeReasonNotReady = "ConformanceFailed"
	agentRuntimeProbeTimeout   = 10 * time.Second
	agentRuntimeRequeue        = 30 * time.Second

	agentRuntimeAuthUseLabel           = "orka.ai/agent-runtime-auth"
	agentRuntimeAuthRefNameLabel       = "orka.ai/agent-runtime-name"
	agentRuntimeAuthEndpointAnnotation = "orka.ai/agent-runtime-endpoint"
)

// AgentRuntimeReconciler reconciles AgentRuntime registry entries.
type AgentRuntimeReconciler struct {
	client.Client
	Scheme *k8sruntime.Scheme
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// Reconcile validates an external Orka harness endpoint and publishes condition-ready status.
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
	observed, ready, authRefResourceVersion, message := r.probeAgentRuntime(ctx, runtime)
	return r.updateAgentRuntimeStatus(ctx, runtime, ready, observed, authRefResourceVersion, message)
}

func (r *AgentRuntimeReconciler) probeAgentRuntime(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
) (*corev1alpha1.AgentRuntimeObservedCapabilities, bool, string, string) {
	if err := validateAgentRuntimeSpec(runtime); err != nil {
		return nil, false, "", err.Error()
	}
	if err := r.validateAgentRuntimeEndpointPolicy(ctx, runtime); err != nil {
		return nil, false, "", err.Error()
	}
	token, authRefResourceVersion, err := r.agentRuntimeBearerToken(ctx, runtime)
	if err != nil {
		return nil, false, "", err.Error()
	}
	probeCtx, cancel := context.WithTimeout(ctx, agentRuntimeProbeTimeout)
	defer cancel()
	deepProbe := runtime.Status.ObservedGeneration != runtime.Generation || !runtime.Status.Ready ||
		runtime.Status.ObservedAuthRefResourceVersion != authRefResourceVersion
	preflight := conformance.Check(probeCtx, conformance.Target{
		BaseURL:        runtime.Spec.Deployment.Endpoint,
		BearerToken:    token,
		ControlTimeout: agentRuntimeProbeTimeout,
		RequireAuth:    true,
	})
	observed := observedCapabilitiesFromConformance(preflight.ObservedCapabilities)
	if !preflight.Passed {
		return observed, false, authRefResourceVersion, sanitizeAgentRuntimeStatusMessage(preflight.Message)
	}
	if err := validateAgentRuntimeRequiredCapabilities(runtime, preflight.ObservedCapabilities); err != nil {
		return observed, false, authRefResourceVersion, err.Error()
	}
	if err := validateAgentRuntimeExecutableCapabilities(preflight.ObservedCapabilities); err != nil {
		return observed, false, authRefResourceVersion, err.Error()
	}
	if deepProbe {
		for _, class := range brokeredClassesToProbe(preflight.ObservedCapabilities) {
			switch class {
			case corev1alpha1.AgentRuntimeBrokeredToolClassRead:
				brokeredReadProbe := conformance.Check(probeCtx, conformance.Target{
					BaseURL:           runtime.Spec.Deployment.Endpoint,
					BearerToken:       token,
					ControlTimeout:    agentRuntimeProbeTimeout,
					ProbeBrokeredRead: true,
					RequireAuth:       true,
				})
				if !brokeredReadProbe.Passed {
					return observed, false, authRefResourceVersion, sanitizeAgentRuntimeStatusMessage(brokeredReadProbe.Message)
				}
			case corev1alpha1.AgentRuntimeBrokeredToolClassWrite:
				brokeredWriteProbe := conformance.Check(probeCtx, conformance.Target{
					BaseURL:            runtime.Spec.Deployment.Endpoint,
					BearerToken:        token,
					ControlTimeout:     agentRuntimeProbeTimeout,
					ProbeBrokeredWrite: true,
					RequireAuth:        true,
				})
				if !brokeredWriteProbe.Passed {
					return observed, false, authRefResourceVersion, sanitizeAgentRuntimeStatusMessage(brokeredWriteProbe.Message)
				}
			case corev1alpha1.AgentRuntimeBrokeredToolClassCoordination:
				brokeredCoordinationProbe := conformance.Check(probeCtx, conformance.Target{
					BaseURL:                   runtime.Spec.Deployment.Endpoint,
					BearerToken:               token,
					ControlTimeout:            agentRuntimeProbeTimeout,
					ProbeBrokeredCoordination: true,
					RequireAuth:               true,
				})
				if !brokeredCoordinationProbe.Passed {
					return observed, false, authRefResourceVersion, sanitizeAgentRuntimeStatusMessage(brokeredCoordinationProbe.Message)
				}
			}
		}
	}
	if deepProbe && capabilityHasToolMode(preflight.ObservedCapabilities, corev1alpha1.AgentRuntimeToolExecutionModeObserved) {
		turnProbe := conformance.Check(probeCtx, conformance.Target{
			BaseURL:        runtime.Spec.Deployment.Endpoint,
			BearerToken:    token,
			ControlTimeout: agentRuntimeProbeTimeout,
			ProbeTurn:      true,
			RequireAuth:    true,
		})
		if turnProbe.ObservedCapabilities != nil {
			if err := validateAgentRuntimeRequiredCapabilities(runtime, turnProbe.ObservedCapabilities); err != nil {
				return observed, false, authRefResourceVersion, err.Error()
			}
			observed = observedCapabilitiesFromConformance(turnProbe.ObservedCapabilities)
		}
		if !turnProbe.Passed {
			return observed, false, authRefResourceVersion, sanitizeAgentRuntimeStatusMessage(turnProbe.Message)
		}
	}
	return observed, true, authRefResourceVersion, "AgentRuntime passed Orka harness readiness checks"
}

func brokeredClassesToProbe(caps *harness.CapabilitiesResponse) []corev1alpha1.AgentRuntimeBrokeredToolClass {
	if caps == nil || len(caps.BrokeredToolClasses) == 0 {
		return nil
	}
	classes := make([]corev1alpha1.AgentRuntimeBrokeredToolClass, 0, len(caps.BrokeredToolClasses))
	seen := map[corev1alpha1.AgentRuntimeBrokeredToolClass]struct{}{}
	for _, class := range caps.BrokeredToolClasses {
		converted := corev1alpha1.AgentRuntimeBrokeredToolClass(class)
		if _, ok := seen[converted]; ok {
			continue
		}
		seen[converted] = struct{}{}
		classes = append(classes, converted)
	}
	return classes
}

func validateAgentRuntimeSpec(runtime *corev1alpha1.AgentRuntime) error {
	if runtime == nil {
		return fmt.Errorf("AgentRuntime is required")
	}
	if runtime.Spec.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 {
		return fmt.Errorf("unsupported contractVersion %q", runtime.Spec.ContractVersion)
	}
	if runtime.Spec.Deployment.Mode != corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint {
		return fmt.Errorf("unsupported deployment.mode %q", runtime.Spec.Deployment.Mode)
	}
	endpoint := strings.TrimSpace(runtime.Spec.Deployment.Endpoint)
	if endpoint == "" {
		return fmt.Errorf("deployment.endpoint is required for external-endpoint AgentRuntime")
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("deployment.endpoint must be an absolute http(s) URL")
	}
	if parsed.Scheme != urlSchemeHTTP && parsed.Scheme != urlSchemeHTTPS {
		return fmt.Errorf("deployment.endpoint scheme %q is not supported", parsed.Scheme)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("deployment.endpoint must not include credentials, query, or fragment components")
	}
	ref := runtime.Spec.ClientAuth.BearerAuthRef
	if strings.TrimSpace(ref.Name) == "" {
		return fmt.Errorf("clientAuth.bearerTokenSecretRef.name is required")
	}
	if strings.TrimSpace(ref.Key) == "" {
		return fmt.Errorf("clientAuth.bearerTokenSecretRef.key is required")
	}
	return nil
}

func (r *AgentRuntimeReconciler) validateAgentRuntimeEndpointPolicy(ctx context.Context, runtime *corev1alpha1.AgentRuntime) error {
	parsed, err := url.Parse(strings.TrimSpace(runtime.Spec.Deployment.Endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("deployment.endpoint must be an absolute http(s) URL")
	}
	if parsed.Scheme != urlSchemeHTTP {
		return nil
	}
	host := parsed.Hostname()
	if isLoopbackAgentRuntimeEndpoint(host) {
		return nil
	}
	if serviceName, serviceNamespace, ok := parseAgentRuntimeServiceNamespaceHost(host); ok && serviceNamespace == runtime.Namespace {
		if r != nil && r.Client != nil {
			service := &corev1.Service{}
			if err := r.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: serviceNamespace}, service); err == nil && service.Spec.Type != corev1.ServiceTypeExternalName {
				return nil
			}
		}
	}
	return fmt.Errorf("deployment.endpoint must use https for non-local AgentRuntime endpoints")
}

func isLoopbackAgentRuntimeEndpoint(host string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" {
		return false
	}
	if addr, err := netip.ParseAddr(host); err == nil {
		return addr.IsLoopback()
	}
	return host == "localhost"
}

func parseAgentRuntimeServiceNamespaceHost(host string) (serviceName, serviceNamespace string, ok bool) {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), ".")
	if host == "" {
		return "", "", false
	}
	parts := strings.Split(host, ".")
	switch {
	case len(parts) == 2:
		serviceName, serviceNamespace = parts[0], parts[1]
	case len(parts) >= 3 && parts[2] == k8sServiceDNSLabel:
		serviceName, serviceNamespace = parts[0], parts[1]
	default:
		return "", "", false
	}
	if serviceName == "" || serviceNamespace == "" {
		return "", "", false
	}
	return serviceName, serviceNamespace, true
}

func validateAgentRuntimeBearerSecretUse(runtimeName string, endpoint string, secret *corev1.Secret) error {
	runtimeName = strings.TrimSpace(runtimeName)
	endpoint = strings.TrimSpace(endpoint)
	if secret == nil {
		return fmt.Errorf("bearer token Secret is required")
	}
	if secret.Labels[agentRuntimeAuthUseLabel] != scheduledRunLabelValue {
		return fmt.Errorf("bearer token Secret %q must be labeled %s=%s before an AgentRuntime can use it", secret.Name, agentRuntimeAuthUseLabel, scheduledRunLabelValue)
	}
	if allowed := strings.TrimSpace(secret.Labels[agentRuntimeAuthRefNameLabel]); allowed != "" && allowed != runtimeName {
		return fmt.Errorf("bearer token Secret %q is labeled for AgentRuntime %q, not %q", secret.Name, allowed, runtimeName)
	}
	boundEndpoint := strings.TrimSpace(secret.Annotations[agentRuntimeAuthEndpointAnnotation])
	if boundEndpoint == "" {
		return fmt.Errorf("bearer token Secret %q must be annotated %s=<deployment.endpoint> before an AgentRuntime can use it", secret.Name, agentRuntimeAuthEndpointAnnotation)
	}
	if boundEndpoint != endpoint {
		return fmt.Errorf("bearer token Secret %q is annotated for endpoint %q, not %q", secret.Name, sanitizeAgentRuntimeEndpointForStatus(boundEndpoint), sanitizeAgentRuntimeEndpointForStatus(endpoint))
	}
	return nil
}

func (r *AgentRuntimeReconciler) agentRuntimeBearerToken(ctx context.Context, runtime *corev1alpha1.AgentRuntime) (string, string, error) {
	ref := runtime.Spec.ClientAuth.BearerAuthRef
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: runtime.Namespace, Name: ref.Name}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("bearer token Secret %q not found", ref.Name)
		}
		return "", "", fmt.Errorf("read bearer token Secret %q: %w", ref.Name, err)
	}
	if err := validateAgentRuntimeBearerSecretUse(runtime.Name, runtime.Spec.Deployment.Endpoint, secret); err != nil {
		return "", "", err
	}
	value := strings.TrimSpace(string(secret.Data[ref.Key]))
	if value == "" {
		return "", "", fmt.Errorf("bearer token Secret %q key %q is empty or missing", ref.Name, ref.Key)
	}
	return value, strings.TrimSpace(secret.ResourceVersion), nil
}

func validateBaseHarnessCapabilities(caps *harness.CapabilitiesResponse) error {
	if caps == nil {
		return fmt.Errorf("observed capabilities are missing")
	}
	if caps.RuntimeName != sanitizeAgentRuntimeCapabilityValue(caps.RuntimeName) {
		return fmt.Errorf("runtimeName contains unsafe text or exceeds status length limits")
	}
	for _, class := range caps.BrokeredToolClasses {
		if !harness.IsKnownBrokeredToolClass(class) {
			return fmt.Errorf("unsupported brokeredToolClass %q", class)
		}
	}
	return nil
}

func validateAgentRuntimeExecutableCapabilities(caps *harness.CapabilitiesResponse) error {
	if err := validateBaseHarnessCapabilities(caps); err != nil {
		return err
	}
	if !caps.SupportsRuntimeSessions {
		return fmt.Errorf("runtime does not advertise required supportsRuntimeSessions capability")
	}
	observed := capabilityHasToolMode(caps, corev1alpha1.AgentRuntimeToolExecutionModeObserved)
	brokered := capabilityHasToolMode(caps, corev1alpha1.AgentRuntimeToolExecutionModeBrokered)
	if observed && !caps.SupportsCancel {
		return fmt.Errorf("runtime advertises observed mode but not supportsCancel")
	}
	if !observed && !brokered {
		return fmt.Errorf("runtime must advertise toolExecutionMode %q or %q", corev1alpha1.AgentRuntimeToolExecutionModeObserved, corev1alpha1.AgentRuntimeToolExecutionModeBrokered)
	}
	if brokered {
		if !caps.SupportsContinuation {
			return fmt.Errorf("runtime advertises brokered mode but not supportsContinuation")
		}
		if len(caps.BrokeredToolClasses) == 0 {
			return fmt.Errorf("runtime advertises brokered mode but no brokeredToolClasses")
		}
	}
	return nil
}

func validateObservedHarnessCapabilities(caps *harness.CapabilitiesResponse) error {
	if err := validateBaseHarnessCapabilities(caps); err != nil {
		return err
	}
	if !capabilityHasToolMode(caps, corev1alpha1.AgentRuntimeToolExecutionModeObserved) {
		return fmt.Errorf("runtime does not advertise required toolExecutionMode %q", corev1alpha1.AgentRuntimeToolExecutionModeObserved)
	}
	if !caps.SupportsCancel {
		return fmt.Errorf("runtime does not advertise required supportsCancel capability")
	}
	if !caps.SupportsRuntimeSessions {
		return fmt.Errorf("runtime does not advertise required supportsRuntimeSessions capability")
	}
	return nil
}

func validateAgentRuntimeRequiredCapabilities(
	runtime *corev1alpha1.AgentRuntime,
	caps *harness.CapabilitiesResponse,
) error {
	if err := validateBaseHarnessCapabilities(caps); err != nil {
		return err
	}
	required := runtime.Spec.Capabilities
	if required == nil {
		return nil
	}
	if required.SupportsCancel != nil && *required.SupportsCancel && !caps.SupportsCancel {
		return fmt.Errorf("runtime does not advertise required supportsCancel capability")
	}
	if required.SupportsRuntimeSessions != nil && *required.SupportsRuntimeSessions && !caps.SupportsRuntimeSessions {
		return fmt.Errorf("runtime does not advertise required supportsRuntimeSessions capability")
	}
	if required.SupportsContinuation != nil && *required.SupportsContinuation && !caps.SupportsContinuation {
		return fmt.Errorf("runtime does not advertise required supportsContinuation capability")
	}
	if required.SupportsArtifacts != nil && *required.SupportsArtifacts && !caps.SupportsArtifacts {
		return fmt.Errorf("runtime does not advertise required supportsArtifacts capability")
	}
	for _, requiredMode := range required.ToolExecutionModes {
		if !capabilityHasToolMode(caps, requiredMode) {
			return fmt.Errorf("runtime does not advertise required toolExecutionMode %q", requiredMode)
		}
	}
	for _, requiredClass := range required.BrokeredToolClasses {
		if !capabilityHasBrokeredToolClass(caps, requiredClass) {
			return fmt.Errorf("runtime does not advertise required brokeredToolClass %q", requiredClass)
		}
	}
	return nil
}

func capabilityHasToolMode(caps *harness.CapabilitiesResponse, want corev1alpha1.AgentRuntimeToolExecutionMode) bool {
	if caps == nil {
		return false
	}
	return slices.ContainsFunc(caps.ToolExecutionModes, func(got harness.ToolExecutionMode) bool {
		return string(got) == string(want)
	})
}

func capabilityHasBrokeredToolClass(caps *harness.CapabilitiesResponse, want corev1alpha1.AgentRuntimeBrokeredToolClass) bool {
	if caps == nil {
		return false
	}
	return slices.ContainsFunc(caps.BrokeredToolClasses, func(got harness.BrokeredToolClass) bool {
		return string(got) == string(want)
	})
}

func observedCapabilitiesFromConformance(caps *harness.CapabilitiesResponse) *corev1alpha1.AgentRuntimeObservedCapabilities {
	if caps == nil {
		return nil
	}
	modes := make([]corev1alpha1.AgentRuntimeToolExecutionMode, 0, len(caps.ToolExecutionModes))
	seenModes := map[corev1alpha1.AgentRuntimeToolExecutionMode]struct{}{}
	for _, mode := range caps.ToolExecutionModes {
		converted := corev1alpha1.AgentRuntimeToolExecutionMode(mode)
		if _, seen := seenModes[converted]; seen {
			continue
		}
		seenModes[converted] = struct{}{}
		modes = append(modes, converted)
	}
	classes := make([]corev1alpha1.AgentRuntimeBrokeredToolClass, 0, len(caps.BrokeredToolClasses))
	seenClasses := map[corev1alpha1.AgentRuntimeBrokeredToolClass]struct{}{}
	for _, class := range caps.BrokeredToolClasses {
		converted := corev1alpha1.AgentRuntimeBrokeredToolClass(class)
		if _, seen := seenClasses[converted]; seen {
			continue
		}
		seenClasses[converted] = struct{}{}
		classes = append(classes, converted)
	}
	return &corev1alpha1.AgentRuntimeObservedCapabilities{
		ProtocolVersion:           sanitizeAgentRuntimeCapabilityValue(caps.ProtocolVersion),
		Transport:                 sanitizeAgentRuntimeCapabilityValue(caps.Transport),
		RuntimeName:               sanitizeAgentRuntimeCapabilityValue(caps.RuntimeName),
		RuntimeVersion:            sanitizeAgentRuntimeCapabilityValue(caps.RuntimeVersion),
		ProviderKind:              sanitizeAgentRuntimeCapabilityValue(string(caps.ProviderKind)),
		ToolExecutionModes:        modes,
		BrokeredToolClasses:       classes,
		SupportsCancel:            caps.SupportsCancel,
		SupportsRuntimeSessions:   caps.SupportsRuntimeSessions,
		SupportsContinuation:      caps.SupportsContinuation,
		SupportsArtifacts:         caps.SupportsArtifacts,
		SupportsSuspend:           caps.SupportsSuspend,
		SupportsWorkspaceSnapshot: caps.SupportsWorkspaceSnapshot,
		MaxConcurrentTurns:        caps.MaxConcurrentTurns,
		MaxTurnSeconds:            caps.MaxTurnSeconds,
		MaxOutputBytes:            caps.MaxOutputBytes,
	}
}

func (r *AgentRuntimeReconciler) updateAgentRuntimeStatus(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	ready bool,
	observed *corev1alpha1.AgentRuntimeObservedCapabilities,
	authRefResourceVersion string,
	message string,
) (ctrl.Result, error) {
	now := metav1.Now()
	runtime.Status.Ready = ready
	runtime.Status.ObservedGeneration = runtime.Generation
	runtime.Status.ObservedCapabilities = observed
	runtime.Status.ObservedAuthRefResourceVersion = authRefResourceVersion
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
	if len(value) > 256 {
		return value[:256]
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
