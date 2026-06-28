/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
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

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/harness"
	"github.com/sozercan/orka/internal/harness/conformance"
)

const (
	agentRuntimeReadyCondition = "Ready"
	agentRuntimeReasonReady    = "ConformancePassed"
	agentRuntimeReasonNotReady = "ConformanceFailed"
	agentRuntimeProbeTimeout   = 10 * time.Second
	agentRuntimeRequeue        = 30 * time.Second

	agentRuntimeAuthUseLabel     = "orka.ai/agent-runtime-auth"
	agentRuntimeAuthRefNameLabel = "orka.ai/agent-runtime-name"
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
	})
	observed := observedCapabilitiesFromConformance(preflight.ObservedCapabilities)
	if !preflight.Passed {
		return observed, false, authRefResourceVersion, sanitizeAgentRuntimeStatusMessage(preflight.Message)
	}
	if err := validateAgentRuntimeRequiredCapabilities(runtime, preflight.ObservedCapabilities); err != nil {
		return observed, false, authRefResourceVersion, err.Error()
	}
	if deepProbe {
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
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
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

func validateAgentRuntimeBearerSecretUse(runtimeName string, secret *corev1.Secret) error {
	runtimeName = strings.TrimSpace(runtimeName)
	if secret == nil {
		return fmt.Errorf("bearer token Secret is required")
	}
	if secret.Labels[agentRuntimeAuthUseLabel] != scheduledRunLabelValue {
		return fmt.Errorf("bearer token Secret %q must be labeled %s=%s before an AgentRuntime can use it", secret.Name, agentRuntimeAuthUseLabel, scheduledRunLabelValue)
	}
	if allowed := strings.TrimSpace(secret.Labels[agentRuntimeAuthRefNameLabel]); allowed != "" && allowed != runtimeName {
		return fmt.Errorf("bearer token Secret %q is labeled for AgentRuntime %q, not %q", secret.Name, allowed, runtimeName)
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
	if err := validateAgentRuntimeBearerSecretUse(runtime.Name, secret); err != nil {
		return "", "", err
	}
	value := strings.TrimSpace(string(secret.Data[ref.Key]))
	if value == "" {
		return "", "", fmt.Errorf("bearer token Secret %q key %q is empty or missing", ref.Name, ref.Key)
	}
	return value, strings.TrimSpace(secret.ResourceVersion), nil
}

func validateObservedHarnessCapabilities(caps *harness.CapabilitiesResponse) error {
	if caps == nil {
		return fmt.Errorf("observed capabilities are missing")
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
	if err := validateObservedHarnessCapabilities(caps); err != nil {
		return err
	}
	if strings.TrimSpace(caps.RuntimeName) != sanitizeAgentRuntimeCapabilityValue(caps.RuntimeName) {
		return fmt.Errorf("runtimeName contains unsafe text or exceeds status length limits")
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
	for _, requiredMode := range required.ToolExecutionModes {
		if !capabilityHasToolMode(caps, requiredMode) {
			return fmt.Errorf("runtime does not advertise required toolExecutionMode %q", requiredMode)
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

func observedCapabilitiesFromConformance(caps *harness.CapabilitiesResponse) *corev1alpha1.AgentRuntimeObservedCapabilities {
	if caps == nil {
		return nil
	}
	modes := make([]corev1alpha1.AgentRuntimeToolExecutionMode, 0, len(caps.ToolExecutionModes))
	for _, mode := range caps.ToolExecutionModes {
		modes = append(modes, corev1alpha1.AgentRuntimeToolExecutionMode(mode))
	}
	return &corev1alpha1.AgentRuntimeObservedCapabilities{
		ProtocolVersion:           sanitizeAgentRuntimeCapabilityValue(caps.ProtocolVersion),
		Transport:                 sanitizeAgentRuntimeCapabilityValue(caps.Transport),
		RuntimeName:               sanitizeAgentRuntimeCapabilityValue(caps.RuntimeName),
		RuntimeVersion:            sanitizeAgentRuntimeCapabilityValue(caps.RuntimeVersion),
		ProviderKind:              sanitizeAgentRuntimeCapabilityValue(string(caps.ProviderKind)),
		ToolExecutionModes:        modes,
		SupportsCancel:            caps.SupportsCancel,
		SupportsRuntimeSessions:   caps.SupportsRuntimeSessions,
		SupportsSuspend:           caps.SupportsSuspend,
		SupportsWorkspaceSnapshot: caps.SupportsWorkspaceSnapshot,
		MaxConcurrentTurns:        caps.MaxConcurrentTurns,
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
