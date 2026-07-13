package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"os"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/metrics"
	"github.com/orka-agents/orka/internal/workerenv"
	"github.com/orka-agents/orka/workers/common"
)

const cliwrapperLocalOutputRef = "cliwrapper-result-v1"

var deprecatedHarnessRuntimeAnnotationKeys = []string{
	"orka.ai/harness-wrapper-runtime-endpoint",
	"orka.ai/harness-wrapper-runtime-generation",
	"orka.ai/harness-wrapper-runtime-auth-secret-name",
	"orka.ai/harness-wrapper-runtime-auth-secret-key",
}

func clearDeprecatedHarnessRuntimeAnnotations(annotations map[string]string) {
	for _, key := range deprecatedHarnessRuntimeAnnotationKeys {
		delete(annotations, key)
	}
}

const (
	harnessWrapperEndpointEnv                 = "ORKA_HARNESS_WRAPPER_ENDPOINT"
	harnessWrapperAuthValueEnv                = "ORKA_HARNESS_WRAPPER_BEARER_TOKEN"
	harnessWrapperAuthValueFileEnv            = "ORKA_HARNESS_WRAPPER_BEARER_TOKEN_FILE"
	harnessWrapperTurnIDAnnotation            = "orka.ai/harness-wrapper-turn-id"
	harnessWrapperRuntimeAnnotation           = "orka.ai/harness-wrapper-runtime-session-id"
	harnessWrapperCorrelationIDAnno           = "orka.ai/harness-wrapper-correlation-id"
	harnessWrapperLastFrameSeqAnno            = "orka.ai/harness-wrapper-last-frame-seq"
	harnessWrapperStartedAnno                 = "orka.ai/harness-wrapper-started"
	harnessWrapperStartAttemptedAnno          = "orka.ai/harness-wrapper-start-attempted"
	harnessWrapperPlannedAtAnno               = "orka.ai/harness-wrapper-planned-at"
	harnessWrapperMetadataAnno                = "orka.ai/harness-wrapper-metadata"
	harnessWrapperRuntimeRefAnno              = "orka.ai/harness-wrapper-runtime-ref"
	harnessWrapperContractAnno                = "orka.ai/harness-wrapper-contract-version"
	harnessWrapperOutputFetchRetriesAnno      = "orka.ai/harness-wrapper-output-fetch-retries"
	harnessWrapperCancelDependencyRetriesAnno = "orka.ai/harness-wrapper-cancel-dependency-retries" // Deprecated: folded into harnessWrapperCancellationAnno.
	harnessWrapperCancellationAnno            = "orka.ai/harness-wrapper-cancellation"
	harnessWrapperAuthRetriesAnno             = "orka.ai/harness-wrapper-auth-retries"
	harnessWrapperMaxOutputFetchRetries       = 3
	harnessWrapperMaxAuthRetries              = 3
	harnessWrapperSkillsFilesMeta             = "skillsFiles"
	harnessWrapperStreamPollTimeout           = 2 * time.Second
	harnessWrapperCancelInitialBackoff        = time.Second
	harnessWrapperCancelMaxBackoff            = 30 * time.Second
	harnessWrapperNoTimeoutDuration           = time.Hour * 24 * 365 * 100
	harnessWrapperRuntimeGeneric              = "generic"
	harnessWrapperOutcomeUnknown              = "outcome_unknown"
	harnessWrapperOutcomeUnknownMessage       = "harness wrapper turn " + harnessWrapperOutcomeUnknown + ": runtime reported completion without durable frames; automatic retry suppressed"
)

func taskHasHarnessWrapperTurn(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	return strings.EqualFold(strings.TrimSpace(task.Annotations[harnessWrapperStartedAnno]), scheduledRunLabelValue) &&
		taskHasPlannedHarnessWrapperTurn(task)
}

func taskHasHarnessWrapperStartAttempt(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	turnID := strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation])
	return turnID != "" && strings.TrimSpace(task.Annotations[harnessWrapperStartAttemptedAnno]) == turnID
}

func taskHasPlannedHarnessWrapperTurn(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	return strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]) != "" &&
		strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]) != "" &&
		strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno]) != ""
}

func harnessWrapperTurnOutcomeUnknown(task *corev1alpha1.Task) bool {
	if task == nil || task.Annotations == nil {
		return false
	}
	turnID := strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation])
	return turnID != "" && strings.TrimSpace(task.Annotations[labels.AnnotationHarnessTurnOutcomeUnknown]) == turnID
}

func harnessWrapperEndpoint() string {
	return strings.TrimSpace(os.Getenv(harnessWrapperEndpointEnv))
}

func harnessWrapperAuthValue() string {
	if tokenPath := strings.TrimSpace(os.Getenv(harnessWrapperAuthValueFileEnv)); tokenPath != "" {
		data, err := os.ReadFile(tokenPath)
		if err == nil {
			return strings.TrimSpace(string(data))
		}
	}
	return strings.TrimSpace(os.Getenv(harnessWrapperAuthValueEnv))
}

type harnessRuntimeTarget struct {
	Endpoint               string
	TransportSecurity      corev1alpha1.AgentRuntimeTransportSecurity
	DialAddress            string
	BackendPodName         string
	BackendPodUID          string
	BearerToken            string
	RuntimeName            string
	RuntimeRefName         string
	ContractVersion        string
	Wrapper                string
	Generation             int64
	AuthRefName            string
	AuthRefField           string
	AuthRefResourceVersion string
	ToolExecutionModes     []corev1alpha1.AgentRuntimeToolExecutionMode
	BrokeredToolClasses    []corev1alpha1.AgentRuntimeBrokeredToolClass
	SupportsContinuation   bool
}

type harnessWrapperCancellationState struct {
	TurnID   string `json:"turnID"`
	Status   string `json:"status"`
	Reason   string `json:"reason,omitempty"`
	Attempts int    `json:"attempts,omitempty"`
	RetryAt  string `json:"retryAt,omitempty"`
}

type harnessWrapperCancelOutcome string

const (
	harnessWrapperCancelPending      harnessWrapperCancelOutcome = "pending"
	harnessWrapperCancelAcknowledged harnessWrapperCancelOutcome = "acknowledged"
	harnessWrapperCancelMissing      harnessWrapperCancelOutcome = "missing"
	harnessWrapperCancelNotNeeded    harnessWrapperCancelOutcome = "not_needed"
)

type agentRuntimeDependencyNotReadyError struct {
	message string
}

func (e agentRuntimeDependencyNotReadyError) Error() string { return e.message }

func isAgentRuntimeDependencyNotReady(err error) bool {
	var notReady agentRuntimeDependencyNotReadyError
	return errors.As(err, &notReady)
}

func agentHarnessRuntimeName(agent *corev1alpha1.Agent) string {
	if agent != nil && agent.Spec.Runtime != nil {
		if agent.Spec.Runtime.RuntimeRef != nil && strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name) != "" {
			return strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name)
		}
		if strings.TrimSpace(string(agent.Spec.Runtime.Type)) != "" {
			return string(agent.Spec.Runtime.Type)
		}
	}
	return string(corev1alpha1.AgentRuntimeClaude)
}

func agentHarnessRuntimeRefName(agent *corev1alpha1.Agent) string {
	if agent == nil || agent.Spec.Runtime == nil || agent.Spec.Runtime.RuntimeRef == nil {
		return ""
	}
	return strings.TrimSpace(agent.Spec.Runtime.RuntimeRef.Name)
}

func harnessRuntimeTargetFromStatus(task *corev1alpha1.Task) (harnessRuntimeTarget, bool) {
	if task == nil || task.Status.HarnessRuntime == nil {
		return harnessRuntimeTarget{}, false
	}
	status := task.Status.HarnessRuntime
	if strings.TrimSpace(status.RuntimeRefName) == "" || strings.TrimSpace(status.Endpoint) == "" ||
		strings.TrimSpace(status.AuthRefName) == "" || strings.TrimSpace(status.AuthRefField) == "" {
		return harnessRuntimeTarget{}, false
	}
	contract := strings.TrimSpace(status.ContractVersion)
	if contract == "" {
		contract = harness.ProtocolVersion
	}
	runtimeName := strings.TrimSpace(status.RuntimeName)
	if runtimeName == "" {
		runtimeName = strings.TrimSpace(status.RuntimeRefName)
	}
	return harnessRuntimeTarget{
		Endpoint:               strings.TrimSpace(status.Endpoint),
		TransportSecurity:      status.TransportSecurity,
		DialAddress:            strings.TrimSpace(status.BackendAddress),
		BackendPodName:         strings.TrimSpace(status.BackendPodName),
		BackendPodUID:          strings.TrimSpace(status.BackendPodUID),
		RuntimeName:            runtimeName,
		RuntimeRefName:         strings.TrimSpace(status.RuntimeRefName),
		ContractVersion:        contract,
		Wrapper:                "external-endpoint",
		Generation:             status.RuntimeGeneration,
		AuthRefName:            strings.TrimSpace(status.AuthRefName),
		AuthRefField:           strings.TrimSpace(status.AuthRefField),
		AuthRefResourceVersion: strings.TrimSpace(status.AuthRefResourceVersion),
	}, true
}

func harnessWrapperApplyRuntimeTargetMetadata(metadata map[string]string, target harnessRuntimeTarget) map[string]string {
	if metadata == nil {
		metadata = map[string]string{}
	}
	if strings.TrimSpace(target.RuntimeName) != "" {
		metadata["runtime"] = strings.TrimSpace(target.RuntimeName)
	}
	if target.RuntimeRefName == "" {
		return metadata
	}
	metadata["runtimeRef"] = target.RuntimeRefName
	metadata["wrapper"] = target.Wrapper
	metadata["contractVersion"] = target.ContractVersion
	return metadata
}

func harnessRuntimeStatusFromTarget(target harnessRuntimeTarget) *corev1alpha1.HarnessRuntimeStatus {
	if target.RuntimeRefName == "" {
		return nil
	}
	return &corev1alpha1.HarnessRuntimeStatus{
		RuntimeRefName:         target.RuntimeRefName,
		RuntimeName:            target.RuntimeName,
		ContractVersion:        target.ContractVersion,
		Endpoint:               target.Endpoint,
		TransportSecurity:      effectiveAgentRuntimeTransportSecurity(target.TransportSecurity),
		BackendPodName:         target.BackendPodName,
		BackendPodUID:          target.BackendPodUID,
		BackendAddress:         target.DialAddress,
		RuntimeGeneration:      target.Generation,
		AuthRefName:            target.AuthRefName,
		AuthRefField:           target.AuthRefField,
		AuthRefResourceVersion: target.AuthRefResourceVersion,
	}
}

func (r *TaskReconciler) patchHarnessRuntimeStatus(ctx context.Context, task *corev1alpha1.Task, target harnessRuntimeTarget) error {
	status := harnessRuntimeStatusFromTarget(target)
	if status == nil {
		return nil
	}
	return r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.HarnessRuntime = status
	})
}

func (r *TaskReconciler) resolveHarnessRuntimeTarget(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) (harnessRuntimeTarget, error) {
	if taskHasPlannedHarnessWrapperTurn(task) {
		if frozen, ok := harnessRuntimeTargetFromStatus(task); ok {
			// harnessRuntimeTargetFromStatus only returns registered AgentRuntime targets;
			// built-in CLI wrapper turns never freeze HarnessRuntime status.
			migrated, backfilled, err := r.resolveFrozenAgentRuntimeTarget(ctx, task, frozen)
			if err != nil {
				return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %v", frozen.RuntimeRefName, err)}
			}
			frozen = migrated
			requiresPinnedBackend := agentRuntimeTransportRequiresPinnedBackend(frozen.Endpoint, frozen.TransportSecurity)
			expectedBackend, hasBackend := agentRuntimeBackendFromTarget(frozen)
			if err := validateAgentRuntimeTransportPolicy(ctx, r.agentRuntimeDispatchReader(), task.Namespace, frozen.Endpoint, frozen.TransportSecurity); err != nil {
				if requiresPinnedBackend && hasBackend {
					if _, identityErr := validateAgentRuntimePinnedPodIdentity(
						ctx,
						r.agentRuntimeDispatchReader(),
						task.Namespace,
						expectedBackend,
					); errors.Is(identityErr, errAgentRuntimePinnedBackendLost) {
						return harnessRuntimeTarget{}, fmt.Errorf("AgentRuntime %q pinned backend was lost and the turn cannot be safely resumed: %w", frozen.RuntimeRefName, identityErr)
					}
				}
				return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %v", frozen.RuntimeRefName, err)}
			}
			token, authRefResourceVersion, err := r.resolveAgentRuntimeBearerTokenFromRef(ctx, task.Namespace, frozen.RuntimeRefName, frozen.Endpoint, frozen.AuthRefName, frozen.AuthRefField)
			if err != nil {
				return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %v", frozen.RuntimeRefName, err)}
			}
			if strings.TrimSpace(frozen.AuthRefResourceVersion) != authRefResourceVersion {
				if err := r.validateFrozenRuntimeAuthObserved(ctx, task, frozen.RuntimeRefName, authRefResourceVersion); err != nil {
					return harnessRuntimeTarget{}, err
				}
			}
			frozen.BearerToken = token
			frozen.AuthRefResourceVersion = authRefResourceVersion
			statusChanged := backfilled
			if requiresPinnedBackend {
				turnMayHaveStarted, startStateErr := r.harnessWrapperTurnMayHaveStarted(ctx, task)
				if startStateErr != nil {
					return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q start recovery state is unavailable: %v", frozen.RuntimeRefName, startStateErr)}
				}
				var backend agentRuntimeBackend
				if hasBackend {
					backend, err = validateAgentRuntimeInsecureBackend(
						ctx,
						r.agentRuntimeDispatchReader(),
						task.Namespace,
						frozen.Endpoint,
						frozen.TransportSecurity,
						expectedBackend,
						!turnMayHaveStarted,
					)
					if err != nil {
						if errors.Is(err, errAgentRuntimePinnedBackendLost) {
							return harnessRuntimeTarget{}, fmt.Errorf("AgentRuntime %q pinned backend was lost and the turn cannot be safely resumed: %w", frozen.RuntimeRefName, err)
						}
						return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %v", frozen.RuntimeRefName, err)}
					}
				} else {
					if turnMayHaveStarted {
						return harnessRuntimeTarget{}, fmt.Errorf("started legacy insecure AgentRuntime turn %q has no pinned backend identity and cannot be safely resumed", frozen.RuntimeRefName)
					}
					backend, err = resolveAgentRuntimeInsecureBackend(
						ctx,
						r.agentRuntimeDispatchReader(),
						task.Namespace,
						frozen.Endpoint,
						frozen.TransportSecurity,
						agentRuntimeTaskBackendSelectionKey(task, frozen.RuntimeRefName),
					)
					if err != nil {
						return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %v", frozen.RuntimeRefName, err)}
					}
					statusChanged = true
				}
				applyAgentRuntimeBackendToTarget(&frozen, backend)
			}
			if statusChanged {
				if err := r.patchHarnessRuntimeStatus(ctx, task, frozen); err != nil {
					return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q frozen runtime status could not be persisted: %v", frozen.RuntimeRefName, err)}
				}
			}
			return frozen, nil
		}
	}
	runtimeRefName := agentHarnessRuntimeRefName(agent)
	if runtimeRefName == "" {
		endpoint := harnessWrapperEndpoint()
		if endpoint == "" {
			return harnessRuntimeTarget{}, fmt.Errorf("%s is required when agent harness runtime is enabled", harnessWrapperEndpointEnv)
		}
		return harnessRuntimeTarget{
			Endpoint:        endpoint,
			BearerToken:     (harnessWrapperAuthValue()),
			RuntimeName:     agentHarnessRuntimeName(agent),
			ContractVersion: harness.ProtocolVersion,
			Wrapper:         "cli",
		}, nil
	}
	return r.resolveReadyAgentRuntimeTarget(ctx, task, runtimeRefName)
}

func (r *TaskReconciler) agentRuntimeDispatchReader() ctrlclient.Reader {
	if r != nil && r.APIReader != nil {
		return r.APIReader
	}
	if r == nil {
		return nil
	}
	return r.Client
}

func (r *TaskReconciler) resolveFrozenAgentRuntimeTarget(
	ctx context.Context,
	task *corev1alpha1.Task,
	target harnessRuntimeTarget,
) (harnessRuntimeTarget, bool, error) {
	if target.TransportSecurity != "" {
		target.TransportSecurity = effectiveAgentRuntimeTransportSecurity(target.TransportSecurity)
		return target, false, nil
	}
	if _, security, err := validateAgentRuntimeTransportSecurity(target.Endpoint, corev1alpha1.AgentRuntimeTransportSecurityTLS); err == nil {
		target.TransportSecurity = security
		return target, true, nil
	}
	if _, _, err := validateAgentRuntimeTransportSecurity(target.Endpoint, corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP); err != nil {
		return harnessRuntimeTarget{}, false, err
	}
	if task == nil {
		return harnessRuntimeTarget{}, false, fmt.Errorf("task is required to migrate legacy AgentRuntime transport security")
	}
	reader := r.agentRuntimeDispatchReader()
	if reader == nil {
		return harnessRuntimeTarget{}, false, fmt.Errorf("kubernetes API reader is required to migrate legacy AgentRuntime transport security")
	}
	runtime := &corev1alpha1.AgentRuntime{}
	if err := reader.Get(ctx, ctrlclient.ObjectKey{Namespace: task.Namespace, Name: target.RuntimeRefName}, runtime); err != nil {
		if apierrors.IsNotFound(err) {
			return harnessRuntimeTarget{}, false, fmt.Errorf("legacy HTTP runtime requires AgentRuntime %q to exist with explicit insecure-cluster-local-http transport opt-in", target.RuntimeRefName)
		}
		return harnessRuntimeTarget{}, false, fmt.Errorf("read AgentRuntime %q for legacy transport migration: %w", target.RuntimeRefName, err)
	}
	_, currentTransportSecurity, err := validateAgentRuntimeTransportSecurity(
		runtime.Spec.Deployment.Endpoint,
		agentRuntimeSpecTransportSecurity(runtime),
	)
	if err != nil {
		return harnessRuntimeTarget{}, false, fmt.Errorf("legacy HTTP runtime has invalid current AgentRuntime transport: %w", err)
	}
	ref := runtime.Spec.ClientAuth.BearerAuthRef
	if runtime.Spec.Deployment.Mode != corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint ||
		currentTransportSecurity != corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP ||
		strings.TrimSpace(ref.Name) != target.AuthRefName || strings.TrimSpace(ref.Key) != target.AuthRefField {
		return harnessRuntimeTarget{}, false, fmt.Errorf("legacy HTTP runtime requires current AgentRuntime %q to match the frozen endpoint, transport, and auth reference", target.RuntimeRefName)
	}
	if err := validateAgentRuntimeTransportPolicy(
		ctx,
		reader,
		runtime.Namespace,
		runtime.Spec.Deployment.Endpoint,
		currentTransportSecurity,
	); err != nil {
		return harnessRuntimeTarget{}, false, fmt.Errorf("legacy HTTP runtime requires a valid current AgentRuntime transport policy: %w", err)
	}
	if runtime.Status.ObservedGeneration != runtime.Generation || !runtime.Status.Ready {
		return harnessRuntimeTarget{}, false, fmt.Errorf("legacy HTTP runtime requires current AgentRuntime %q to be Ready at generation %d", target.RuntimeRefName, runtime.Generation)
	}
	currentEndpoint := strings.TrimSpace(runtime.Spec.Deployment.Endpoint)
	if !legacyFrozenAgentRuntimeEndpointMatches(target.Endpoint, currentEndpoint, task.Namespace) {
		return harnessRuntimeTarget{}, false, fmt.Errorf("legacy HTTP runtime requires current AgentRuntime %q to reference the same Service, port, and path as the frozen endpoint", target.RuntimeRefName)
	}
	target.Endpoint = currentEndpoint
	target.TransportSecurity = corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP
	return target, true, nil
}

func legacyFrozenAgentRuntimeEndpointMatches(frozenEndpoint, currentEndpoint, namespace string) bool {
	frozen, _, err := validateAgentRuntimeTransportSecurity(frozenEndpoint, corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP)
	if err != nil || frozen.User != nil || frozen.RawQuery != "" || frozen.Fragment != "" {
		return false
	}
	current, _, err := validateAgentRuntimeTransportSecurity(currentEndpoint, corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP)
	if err != nil || current.User != nil || current.RawQuery != "" || current.Fragment != "" {
		return false
	}
	frozenService, frozenNamespace, ok := parseLegacyAgentRuntimeServiceNamespaceHost(frozen.Hostname())
	if !ok || frozenNamespace != namespace {
		return false
	}
	currentService, currentNamespace, ok := parseAgentRuntimeServiceNamespaceHost(current.Hostname())
	return ok && currentNamespace == namespace && currentService == frozenService &&
		agentRuntimeEffectiveURLPort(current) == agentRuntimeEffectiveURLPort(frozen) &&
		agentRuntimeNormalizedURLPath(current) == agentRuntimeNormalizedURLPath(frozen)
}

func agentRuntimeEffectiveURLPort(endpoint *url.URL) string {
	if endpoint == nil {
		return ""
	}
	if port := endpoint.Port(); port != "" {
		return port
	}
	switch strings.ToLower(endpoint.Scheme) {
	case urlSchemeHTTP:
		return "80"
	case urlSchemeHTTPS:
		return "443"
	default:
		return ""
	}
}

func agentRuntimeNormalizedURLPath(endpoint *url.URL) string {
	if endpoint == nil {
		return ""
	}
	if escapedPath := endpoint.EscapedPath(); escapedPath != "" {
		return escapedPath
	}
	return "/"
}

func parseLegacyAgentRuntimeServiceNamespaceHost(host string) (serviceName, serviceNamespace string, ok bool) {
	if serviceName, serviceNamespace, ok = parseAgentRuntimeServiceNamespaceHost(host); ok {
		return serviceName, serviceNamespace, true
	}
	parts := strings.Split(strings.Trim(strings.ToLower(strings.TrimSpace(host)), "."), ".")
	switch {
	case len(parts) == 2:
		serviceName, serviceNamespace = parts[0], parts[1]
	case len(parts) == 3 && parts[2] == k8sServiceDNSLabel:
		serviceName, serviceNamespace = parts[0], parts[1]
	default:
		return "", "", false
	}
	return serviceName, serviceNamespace, serviceName != "" && serviceNamespace != ""
}

func (r *TaskReconciler) validateFrozenRuntimeAuthObserved(
	ctx context.Context,
	task *corev1alpha1.Task,
	runtimeRefName string,
	authRefResourceVersion string,
) error {
	if task == nil {
		return fmt.Errorf("task is required to validate runtimeRef %q", runtimeRefName)
	}
	runtime := &corev1alpha1.AgentRuntime{}
	reader := r.agentRuntimeDispatchReader()
	if reader == nil {
		return fmt.Errorf("kubernetes API reader is required to validate AgentRuntime %q", runtimeRefName)
	}
	if err := reader.Get(ctx, ctrlclient.ObjectKey{Namespace: task.Namespace, Name: runtimeRefName}, runtime); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("AgentRuntime %q not found in namespace %q", runtimeRefName, task.Namespace)
		}
		return fmt.Errorf("read AgentRuntime %q: %w", runtimeRefName, err)
	}
	if runtime.Status.ObservedGeneration != runtime.Generation {
		return agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: observedGeneration %d is stale for generation %d", runtimeRefName, runtime.Status.ObservedGeneration, runtime.Generation)}
	}
	if !runtime.Status.Ready {
		message := strings.TrimSpace(runtime.Status.Message)
		if message == "" {
			message = "runtime is not Ready"
		}
		return agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %s", runtimeRefName, message)}
	}
	if strings.TrimSpace(runtime.Status.ObservedAuthRefResourceVersion) != authRefResourceVersion {
		return agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: bearer token Secret version changed since runtime readiness", runtimeRefName)}
	}
	return nil
}

func (r *TaskReconciler) resolveReadyAgentRuntimeTarget(
	ctx context.Context,
	task *corev1alpha1.Task,
	runtimeRefName string,
) (harnessRuntimeTarget, error) {
	if task == nil {
		return harnessRuntimeTarget{}, fmt.Errorf("task is required to resolve runtimeRef %q", runtimeRefName)
	}
	runtime := &corev1alpha1.AgentRuntime{}
	reader := r.agentRuntimeDispatchReader()
	if reader == nil {
		return harnessRuntimeTarget{}, fmt.Errorf("kubernetes API reader is required to resolve AgentRuntime %q", runtimeRefName)
	}
	if err := reader.Get(ctx, ctrlclient.ObjectKey{Namespace: task.Namespace, Name: runtimeRefName}, runtime); err != nil {
		if apierrors.IsNotFound(err) {
			return harnessRuntimeTarget{}, fmt.Errorf("AgentRuntime %q not found in namespace %q", runtimeRefName, task.Namespace)
		}
		return harnessRuntimeTarget{}, fmt.Errorf("read AgentRuntime %q: %w", runtimeRefName, err)
	}
	if runtime.Status.ObservedGeneration != runtime.Generation {
		return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: observedGeneration %d is stale for generation %d", runtimeRefName, runtime.Status.ObservedGeneration, runtime.Generation)}
	}
	if !runtime.Status.Ready {
		message := strings.TrimSpace(runtime.Status.Message)
		if message == "" {
			message = "runtime is not Ready"
		}
		return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %s", runtimeRefName, message)}
	}
	if runtime.Spec.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV1 {
		return harnessRuntimeTarget{}, fmt.Errorf("AgentRuntime %q has unsupported contractVersion %q", runtimeRefName, runtime.Spec.ContractVersion)
	}
	if runtime.Spec.Deployment.Mode != corev1alpha1.AgentRuntimeDeploymentModeExternalEndpoint {
		return harnessRuntimeTarget{}, fmt.Errorf("AgentRuntime %q has unsupported deployment.mode %q", runtimeRefName, runtime.Spec.Deployment.Mode)
	}
	transportSecurity := agentRuntimeSpecTransportSecurity(runtime)
	if err := validateAgentRuntimeTransportPolicy(
		ctx,
		reader,
		runtime.Namespace,
		runtime.Spec.Deployment.Endpoint,
		transportSecurity,
	); err != nil {
		return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %v", runtimeRefName, err)}
	}
	_, transportSecurity, err := validateAgentRuntimeTransportSecurity(
		runtime.Spec.Deployment.Endpoint,
		transportSecurity,
	)
	if err != nil {
		return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %v", runtimeRefName, err)}
	}
	token, authRefResourceVersion, err := r.resolveAgentRuntimeBearerToken(ctx, runtime)
	if err != nil {
		return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %v", runtimeRefName, err)}
	}
	if strings.TrimSpace(runtime.Status.ObservedAuthRefResourceVersion) != authRefResourceVersion {
		return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: bearer token Secret version changed since runtime readiness", runtimeRefName)}
	}
	ref := runtime.Spec.ClientAuth.BearerAuthRef
	runtimeName := runtimeRefName
	if runtime.Status.ObservedCapabilities != nil && strings.TrimSpace(runtime.Status.ObservedCapabilities.RuntimeName) != "" {
		runtimeName = strings.TrimSpace(runtime.Status.ObservedCapabilities.RuntimeName)
	}
	target := harnessRuntimeTarget{
		Endpoint:               strings.TrimSpace(runtime.Spec.Deployment.Endpoint),
		TransportSecurity:      transportSecurity,
		BearerToken:            token,
		RuntimeName:            runtimeName,
		RuntimeRefName:         runtimeRefName,
		ContractVersion:        string(runtime.Spec.ContractVersion),
		Wrapper:                "external-endpoint",
		Generation:             runtime.Generation,
		AuthRefName:            strings.TrimSpace(ref.Name),
		AuthRefField:           strings.TrimSpace(ref.Key),
		AuthRefResourceVersion: authRefResourceVersion,
		ToolExecutionModes: func() []corev1alpha1.AgentRuntimeToolExecutionMode {
			if runtime.Status.ObservedCapabilities == nil {
				return nil
			}
			return append([]corev1alpha1.AgentRuntimeToolExecutionMode(nil), runtime.Status.ObservedCapabilities.ToolExecutionModes...)
		}(),
		BrokeredToolClasses: func() []corev1alpha1.AgentRuntimeBrokeredToolClass {
			if runtime.Status.ObservedCapabilities == nil {
				return nil
			}
			return append([]corev1alpha1.AgentRuntimeBrokeredToolClass(nil), runtime.Status.ObservedCapabilities.BrokeredToolClasses...)
		}(),
		SupportsContinuation: runtime.Status.ObservedCapabilities != nil && runtime.Status.ObservedCapabilities.SupportsContinuation,
	}
	backend, err := resolveAgentRuntimeInsecureBackend(
		ctx,
		reader,
		task.Namespace,
		target.Endpoint,
		target.TransportSecurity,
		agentRuntimeTaskBackendSelectionKey(task, runtimeRefName),
	)
	if err != nil {
		return harnessRuntimeTarget{}, agentRuntimeDependencyNotReadyError{message: fmt.Sprintf("AgentRuntime %q is not ready: %v", runtimeRefName, err)}
	}
	applyAgentRuntimeBackendToTarget(&target, backend)
	return target, nil
}

func agentRuntimeTaskBackendSelectionKey(task *corev1alpha1.Task, runtimeRefName string) string {
	if task == nil {
		return strings.TrimSpace(runtimeRefName)
	}
	if task.Annotations != nil {
		if runtimeSessionID := strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]); runtimeSessionID != "" {
			return runtimeSessionID
		}
	}
	return string(harnessWrapperRuntimeSessionID(task, strings.TrimSpace(runtimeRefName)))
}

func agentRuntimeBackendFromTarget(target harnessRuntimeTarget) (agentRuntimeBackend, bool) {
	backend := agentRuntimeBackend{
		PodName:     strings.TrimSpace(target.BackendPodName),
		PodUID:      strings.TrimSpace(target.BackendPodUID),
		DialAddress: strings.TrimSpace(target.DialAddress),
	}
	return backend, backend.PodName != "" && backend.PodUID != "" && backend.DialAddress != ""
}

func applyAgentRuntimeBackendToTarget(target *harnessRuntimeTarget, backend agentRuntimeBackend) {
	if target == nil {
		return
	}
	target.BackendPodName = backend.PodName
	target.BackendPodUID = backend.PodUID
	target.DialAddress = backend.DialAddress
}

func newHarnessClientForRuntimeTarget(target harnessRuntimeTarget) (*harness.Client, error) {
	opts := []harness.ClientOption{harness.WithBearerToken(target.BearerToken)}
	requestEndpoint := target.Endpoint
	if target.RuntimeRefName != "" {
		var err error
		requestEndpoint, target.TransportSecurity, err = agentRuntimeRequestEndpoint(target.Endpoint, target.TransportSecurity)
		if err != nil {
			return nil, err
		}
		opts = append(opts, harness.WithPublicDiscovery())
		insecureHTTP := target.TransportSecurity == corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP
		httpClient := harness.NewAgentRuntimeHTTPClient(insecureHTTP)
		if insecureHTTP && target.DialAddress != "" {
			httpClient, err = harness.NewPinnedAgentRuntimeHTTPClient(target.DialAddress)
			if err != nil {
				return nil, err
			}
		}
		opts = append(opts, harness.WithHTTPClient(httpClient))
	}
	return harness.NewClient(requestEndpoint, opts...)
}

func (r *TaskReconciler) resolveAgentRuntimeBearerToken(ctx context.Context, runtime *corev1alpha1.AgentRuntime) (string, string, error) {
	if runtime == nil {
		return "", "", fmt.Errorf("AgentRuntime is required")
	}
	ref := runtime.Spec.ClientAuth.BearerAuthRef
	return r.resolveAgentRuntimeBearerTokenFromRef(ctx, runtime.Namespace, runtime.Name, runtime.Spec.Deployment.Endpoint, ref.Name, ref.Key)
}

func (r *TaskReconciler) resolveAgentRuntimeBearerTokenFromRef(
	ctx context.Context,
	namespace string,
	runtimeName string,
	endpoint string,
	refName string,
	refField string,
) (string, string, error) {
	refName = strings.TrimSpace(refName)
	refField = strings.TrimSpace(refField)
	secret := &corev1.Secret{}
	reader := r.agentRuntimeDispatchReader()
	if reader == nil {
		return "", "", fmt.Errorf("kubernetes API reader is required to resolve AgentRuntime %q bearer token", runtimeName)
	}
	if err := reader.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: refName}, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return "", "", fmt.Errorf("AgentRuntime %q bearer token Secret %q not found", runtimeName, refName)
		}
		return "", "", fmt.Errorf("read AgentRuntime %q bearer token Secret %q: %w", runtimeName, refName, err)
	}
	if err := validateAgentRuntimeBearerSecretUse(runtimeName, endpoint, secret); err != nil {
		return "", "", err
	}
	value := strings.TrimSpace(string(secret.Data[refField]))
	if value == "" {
		return "", "", fmt.Errorf("AgentRuntime %q bearer token Secret %q key %q is empty or missing", runtimeName, refName, refField)
	}
	return value, strings.TrimSpace(secret.ResourceVersion), nil
}

//nolint:gocyclo // Coordinates idempotent turn planning, wrapper start, and Running transition.
func (r *TaskReconciler) runHarnessWrapperTask(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent) (ctrl.Result, error) {
	// Agent execution planning owns path compatibility and rejection decisions.
	// This method starts or resumes an already-approved harness-wrapper turn.
	now := metav1.Now()
	attempts := task.Status.Attempts + 1
	if taskHasPlannedHarnessWrapperTurn(task) && !harnessWrapperPlannedTurnMatchesTask(task, agent, attempts) {
		if err := r.clearHarnessWrapperTurnState(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}
	if harnessWrapperTurnOutcomeUnknown(task) {
		return r.failTask(ctx, task, harnessWrapperOutcomeUnknownMessage)
	}

	if r.ExecutionEventStore == nil {
		return r.failTask(ctx, task, "execution event store is required for harness wrapper mode")
	}
	var request harness.StartTurnRequest
	var target harnessRuntimeTarget
	var err error
	startedPlannedTurn := false
	if taskHasPlannedHarnessWrapperTurn(task) {
		if taskHasHarnessWrapperTurn(task) {
			startedPlannedTurn = true
		}
		request, err = r.plannedHarnessWrapperStartTurnRequest(ctx, task, agent, now.Time)
		if err != nil {
			return r.failTask(ctx, task, err.Error())
		}
		targetAgent := agent
		if strings.TrimSpace(request.Metadata["runtimeRef"]) == "" {
			plannedRuntime := corev1alpha1.AgentRuntimeType(strings.TrimSpace(request.Metadata["runtime"]))
			if plannedRuntime == "" {
				plannedRuntime = corev1alpha1.AgentRuntimeClaude
			}
			targetAgent = &corev1alpha1.Agent{Spec: corev1alpha1.AgentSpec{Runtime: &corev1alpha1.AgentCLIRuntime{Type: plannedRuntime}}}
		}
		target, err = r.resolveHarnessRuntimeTarget(ctx, task, targetAgent)
		if err != nil {
			if isAgentRuntimeDependencyNotReady(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return r.failTask(ctx, task, events.RedactExecutionEventText(err.Error()))
		}
		if target.RuntimeRefName != "" {
			if _, frozen := harnessRuntimeTargetFromStatus(task); !frozen {
				if err := r.patchHarnessRuntimeStatus(ctx, task, target); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
			}
		}
	} else {
		request, err = r.harnessWrapperStartTurnRequest(ctx, task, agent, now.Time, attempts)
		if err != nil {
			return r.failTask(ctx, task, err.Error())
		}
		target, err = r.resolveHarnessRuntimeTarget(ctx, task, agent)
		if err != nil {
			if isAgentRuntimeDependencyNotReady(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return r.failTask(ctx, task, events.RedactExecutionEventText(err.Error()))
		}
		request.Metadata = harnessWrapperApplyRuntimeTargetMetadata(request.Metadata, target)
		if err := applyHarnessRuntimeToolExecutionMode(&request, target); err != nil {
			return r.failTask(ctx, task, events.RedactExecutionEventText(err.Error()))
		}
		if err := r.patchHarnessWrapperPlannedTurn(ctx, task, request); err != nil {
			return ctrl.Result{}, err
		}
		if err := r.patchHarnessRuntimeStatus(ctx, task, target); err != nil {
			return ctrl.Result{}, err
		}
		// Persist the deterministic turn identity in a separate reconcile before
		// accepting the turn. The Running path requires the planned identity
		// annotations/status to be durable; keeping planning and start in one
		// reconcile can leave a flaky observed state where started=true is visible
		// but the identity annotations are not, causing handleRunning to fail the
		// task as missing its harness runtime turn identity.
		return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
	}
	turnAccepted := startedPlannedTurn
	if !startedPlannedTurn {
		latest := &corev1alpha1.Task{}
		if err := r.Get(ctx, ctrlclient.ObjectKey{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
			return ctrl.Result{}, err
		}
		if latest.Status.Phase != corev1alpha1.TaskPhasePending {
			task.Status = latest.Status
			return ctrl.Result{}, nil
		}
		if strings.TrimSpace(target.Endpoint) == "" {
			target, err = r.resolveHarnessRuntimeTarget(ctx, task, agent)
			if err != nil {
				if isAgentRuntimeDependencyNotReady(err) {
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}
				return r.failTask(ctx, task, events.RedactExecutionEventText(err.Error()))
			}
			request.Metadata = harnessWrapperApplyRuntimeTargetMetadata(request.Metadata, target)
		}
		client, err := newHarnessClientForRuntimeTarget(target)
		if err != nil {
			return r.failTask(ctx, task, fmt.Sprintf("invalid harness runtime endpoint: %v", err))
		}
		// Cross-restart idempotency backstop: if this deterministic turn ID already
		// produced persisted frames, the turn was already accepted and ran. Do NOT
		// re-issue StartTurn (which would duplicate external side effects after a
		// wrapper pod restart wiped its in-memory turn map); recover by treating the
		// turn as accepted and proceeding to the Running transition. Check persisted
		// frames before live capabilities so accepted turns remain recoverable if the
		// runtime rolls after emitting frames but before started=true is persisted.
		journal := r.harnessWrapperTurnJournal(ctx, task)
		hasFrames, framesErr := journal.HasPersistedFrames(ctx, request.TurnID)
		if framesErr != nil {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		if !hasFrames {
			if err := r.validateHarnessWrapperCapabilities(ctx, client, request); err != nil {
				if target.RuntimeRefName != "" && harnessWrapperAuthError(err) {
					if shouldWait, waitErr := r.waitForHarnessWrapperAuthRetry(ctx, task); waitErr != nil {
						return ctrl.Result{}, waitErr
					} else if shouldWait {
						return ctrl.Result{RequeueAfter: time.Second}, nil
					}
				}
				if harnessWrapperCapabilitiesErrorIsRetryable(err) {
					return ctrl.Result{RequeueAfter: time.Second}, nil
				}
				return r.failTask(ctx, task, err.Error())
			}
			if !taskHasHarnessWrapperStartAttempt(task) {
				if err := r.patchHarnessWrapperStartAttempt(ctx, task, request.TurnID); err != nil {
					return ctrl.Result{}, err
				}
			}
			if _, err := client.StartTurn(ctx, request); err != nil {
				message := err.Error()
				switch {
				case strings.Contains(message, "turn already exists"):
					// Treat a duplicate turn ID as idempotent recovery after the wrapper
					// accepted the planned turn before Running status was persisted.
				case strings.Contains(message, "turn already completed"):
					// A completed-turn tombstone proves this deterministic turn already ran.
					// With no durable frames, its terminal result is unknowable. Persist a
					// turn-bound fence before terminalizing so a controller restart cannot
					// issue StartTurn again or advance to a new automatic retry attempt.
					marked, markErr := r.patchHarnessWrapperTurnOutcomeUnknown(ctx, task, request.TurnID)
					if markErr != nil {
						return ctrl.Result{}, markErr
					}
					if !marked {
						return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
					}
					return r.failTask(ctx, task, harnessWrapperOutcomeUnknownMessage)
				case strings.Contains(message, "maximum concurrent turns"):
					if clearErr := r.clearHarnessWrapperTurnState(ctx, task); clearErr != nil {
						return ctrl.Result{}, clearErr
					}
					return ctrl.Result{RequeueAfter: time.Second}, nil
				case target.RuntimeRefName != "" && harnessWrapperAuthError(err):
					if wait, waitErr := r.waitForHarnessWrapperAuthRetry(ctx, task); waitErr != nil {
						return ctrl.Result{}, waitErr
					} else if wait {
						return ctrl.Result{RequeueAfter: time.Second}, nil
					}
					return r.failTask(ctx, task, events.RedactExecutionEventText(message))
				case harnessWrapperStartTurnErrorIsRetryable(err):
					return ctrl.Result{RequeueAfter: time.Second}, nil
				default:
					return r.failTask(ctx, task, events.RedactExecutionEventText(message))
				}
			}
		}
		turnAccepted = true
		if err := r.patchHarnessWrapperStarted(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
	}

	if strings.TrimSpace(target.Endpoint) == "" {
		target, err = r.resolveHarnessRuntimeTarget(ctx, task, agent)
		if err != nil {
			if isAgentRuntimeDependencyNotReady(err) {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			return r.failTask(ctx, task, events.RedactExecutionEventText(err.Error()))
		}
	}

	transitionedToRunning := false
	if err := r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		if t.Status.Phase != corev1alpha1.TaskPhasePending && t.Status.Phase != corev1alpha1.TaskPhaseRunning {
			return
		}
		t.Status.Phase = corev1alpha1.TaskPhaseRunning
		t.Status.StartTime = &now
		t.Status.Attempts = attempts
		t.Status.JobName = ""
		t.Status.HarnessRuntime = harnessRuntimeStatusFromTarget(target)
		t.Status.Message = "harness wrapper turn running"
		transitionedToRunning = true
	}); err != nil {
		return ctrl.Result{}, err
	}
	if !transitionedToRunning {
		if turnAccepted {
			cancelled, retryAfter, cancelErr := r.ensureHarnessWrapperTurnCancelled(ctx, task, "task left pending before running transition")
			if cancelErr != nil {
				return ctrl.Result{}, cancelErr
			}
			if !cancelled {
				return ctrl.Result{RequeueAfter: retryAfter}, nil
			}
		}
		return ctrl.Result{}, nil
	}
	// TaskStarted is best-effort, mirroring the general-worker Job path
	// (task_controller.go). The status has already transitioned to Running above;
	// returning an error here would route the next reconcile to
	// finishHarnessWrapperTask (which does not append TaskStarted), permanently
	// dropping the start event on a transient store failure. Log and continue.
	if err := r.recordTaskLifecycleEvent(
		ctx,
		task,
		events.ExecutionEventTypeTaskStarted,
		events.ExecutionEventSeverityInfo,
		"harness wrapper task started",
	); err != nil {
		logf.FromContext(ctx).Info("best-effort harness TaskStarted event append failed", "error", err, "task", task.Name)
	}
	return ctrl.Result{RequeueAfter: 100 * time.Millisecond}, nil
}

func (r *TaskReconciler) harnessWrapperTurnJournal(ctx context.Context, task *corev1alpha1.Task) harness.TurnJournal {
	journal := harness.TurnJournal{EventStore: r.ExecutionEventStore}
	if task == nil {
		return journal
	}
	journal.MapContext = harness.EventMapContext{
		Namespace: task.Namespace,
		TaskName:  task.Name,
		// Use the real-session-only helper here (not harnessWrapperSessionName,
		// which falls back to task.Name): this SessionName is PERSISTED as the
		// execution event's session key, so a task-name fallback would collide a
		// SessionRef-less task's events into any real Session of the same name.
		// The StartTurn/CancelTurn request sites intentionally keep
		// harnessWrapperSessionName because the protocol requires a non-empty
		// identifier there (it is not a stored timeline key).
		SessionName: r.executionEventSessionName(ctx, task),
		AgentName:   harnessWrapperTaskAgentName(task),
	}
	return journal
}

func (r *TaskReconciler) harnessWrapperTurnMayHaveStarted(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if taskHasHarnessWrapperTurn(task) || taskHasHarnessWrapperStartAttempt(task) {
		return true, nil
	}
	if r == nil || r.ExecutionEventStore == nil || task == nil || task.Annotations == nil {
		return false, nil
	}
	turnID := harness.HarnessTurnID(strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]))
	if turnID == "" {
		return false, nil
	}
	return r.harnessWrapperTurnJournal(ctx, task).HasPersistedFrames(ctx, turnID)
}

//nolint:gocyclo // Handles stream polling, event mapping, and terminal task classification in one reconcile step.
func (r *TaskReconciler) finishHarnessWrapperTask(ctx context.Context, task *corev1alpha1.Task) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	if harnessWrapperTurnOutcomeUnknown(task) {
		return r.failTask(ctx, task, harnessWrapperOutcomeUnknownMessage)
	}
	target, targetErr := r.resolveHarnessRuntimeTarget(ctx, task, nil)
	if targetErr != nil {
		if isAgentRuntimeDependencyNotReady(targetErr) {
			return ctrl.Result{RequeueAfter: time.Second}, nil
		}
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, events.RedactExecutionEventText(targetErr.Error()))
	}
	if r.ExecutionEventStore == nil {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "execution event store is required for harness wrapper mode")
	}
	agent, agentErr := r.resolveAgent(ctx, task)
	if agentErr != nil && harnessWrapperPlannedToolExecutionMode(task) == harness.ToolExecutionModeBrokered {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, events.RedactExecutionEventText(agentErr.Error()))
	}
	turnID := harness.HarnessTurnID(strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]))
	runtimeSessionID := harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]))
	correlationID := strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno])
	if turnID == "" || runtimeSessionID == "" || correlationID == "" {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "harness wrapper turn identity is missing")
	}
	if !harnessWrapperTurnAnnotationsMatchTaskAttempt(task, harnessWrapperCurrentAttempt(task)) {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "harness wrapper turn identity does not match task")
	}
	client, err := newHarnessClientForRuntimeTarget(target)
	if err != nil {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, fmt.Sprintf("invalid harness runtime endpoint: %v", err))
	}
	result := harness.TurnRunResult{}
	journal := r.harnessWrapperTurnJournal(ctx, task)
	afterSeq := harnessWrapperLastFrameSeq(task)
	lastFrameSeq := afterSeq
	journalState, err := journal.Open(ctx)
	if err != nil {
		return ctrl.Result{}, err
	}
	streamCtx, cancel := context.WithTimeout(ctx, harnessWrapperStreamPollTimeout)
	defer cancel()
	err = client.StreamFrames(streamCtx, turnID, afterSeq, func(frame harness.HarnessEventFrame) error {
		if frame.RuntimeSessionID != runtimeSessionID || frame.TurnID != turnID || frame.CorrelationID != correlationID {
			return fmt.Errorf("harness frame identity does not match running turn")
		}
		result.Frames = append(result.Frames, frame)
		appended, appendedNew, err := journalState.AppendFrameIfNew(streamCtx, frame)
		if err != nil {
			return err
		}
		if appendedNew {
			result.Events = append(result.Events, *appended)
		}
		if frame.Type == harness.FrameToolCallRequested && harnessWrapperPlannedToolExecutionMode(task) == harness.ToolExecutionModeBrokered {
			if err := r.continueHarnessBrokeredToolCall(ctx, client, task, agent, frame); err != nil {
				if approvalID, toolName, ok := harnessBrokeredPendingApproval(err); ok || errors.Is(err, errHarnessBrokeredApprovalPending) {
					if statusErr := r.markHarnessBrokeredApprovalWaiting(ctx, task, approvalID, toolName); statusErr != nil {
						return statusErr
					}
				}
				return err
			}
		}
		if frame.Seq > lastFrameSeq {
			lastFrameSeq = frame.Seq
		}
		switch frame.Type {
		case harness.FrameTurnCompleted:
			result.Completed = frame.Completed
		case harness.FrameTurnFailed:
			result.Failed = frame.Failed
		case harness.FrameTurnCancelled:
			result.Cancelled = true
		}
		return nil
	})
	terminalFrameSeen := result.Completed != nil || result.Failed != nil || result.Cancelled
	if lastFrameSeq > afterSeq && !terminalFrameSeen && !harnessWrapperStreamErrorIsBrokeredPause(err) {
		if patchErr := r.patchHarnessWrapperLastFrameSeq(ctx, task, lastFrameSeq); patchErr != nil {
			return ctrl.Result{}, patchErr
		}
	}
	if err != nil && result.Completed == nil && result.Failed == nil && !result.Cancelled {
		if target.RuntimeRefName != "" && harnessWrapperAuthError(err) {
			if wait, waitErr := r.waitForHarnessWrapperAuthRetry(ctx, task); waitErr != nil {
				return ctrl.Result{}, waitErr
			} else if wait {
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
		}
		if harnessWrapperStreamErrorIsMissingTurn(err) && r.shouldRetry(task) {
			if clearErr := r.clearHarnessWrapperTurnState(ctx, task); clearErr != nil {
				return ctrl.Result{}, clearErr
			}
			return r.retryTask(ctx, task)
		}
		if harnessWrapperStreamErrorIsTerminal(err) {
			return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, events.RedactExecutionEventText(err.Error()))
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if result.Completed != nil && r.ResultStore != nil {
		resultBytes := harnessWrapperCompletedResultBytes(result.Completed)
		if outputRef := strings.TrimSpace(result.Completed.OutputRef); outputRef == cliwrapperLocalOutputRef {
			fetched, fetchErr := client.FetchTurnOutput(ctx, turnID, outputRef)
			if fetchErr != nil {
				log.Error(fetchErr, "failed to fetch harness wrapper result")
				retries := harnessWrapperOutputFetchRetries(task)
				if retries >= harnessWrapperMaxOutputFetchRetries {
					return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, fmt.Sprintf("failed to fetch harness wrapper result: %v", fetchErr))
				}
				if patchErr := r.patchHarnessWrapperOutputFetchRetries(ctx, task, retries+1); patchErr != nil {
					return ctrl.Result{}, patchErr
				}
				return ctrl.Result{RequeueAfter: time.Second}, nil
			}
			resultBytes = fetched
		}
		if saveErr := r.ResultStore.SaveResult(ctx, task.Namespace, task.Name, resultBytes); saveErr != nil {
			log.Error(saveErr, "failed to save harness wrapper result")
			return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, fmt.Sprintf("failed to save harness wrapper result: %v", saveErr))
		}
		task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
	}
	if result.Failed != nil && r.ResultStore != nil && (!result.Failed.Retryable || !r.shouldRetry(task)) {
		resultBytes := harnessWrapperFailedResultBytes(result.Failed)
		if outputRef := strings.TrimSpace(result.Failed.OutputRef); outputRef == cliwrapperLocalOutputRef {
			fetched, fetchErr := client.FetchTurnOutput(ctx, turnID, outputRef)
			if fetchErr != nil {
				log.Error(fetchErr, "failed to fetch failed harness wrapper result")
			} else {
				resultBytes = fetched
			}
		}
		if len(resultBytes) > 0 {
			if saveErr := r.ResultStore.SaveResult(ctx, task.Namespace, task.Name, resultBytes); saveErr != nil {
				log.Error(saveErr, "failed to save failed harness wrapper result")
			} else {
				task.Status.ResultRef = &corev1alpha1.ResultReference{Available: true}
			}
		}
	}
	if result.Cancelled {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseCancelled, "harness wrapper turn cancelled")
	}
	if result.Failed != nil {
		if result.Failed.Retryable && r.shouldRetry(task) {
			if clearErr := r.clearHarnessWrapperTurnState(ctx, task); clearErr != nil {
				return ctrl.Result{}, clearErr
			}
			return r.retryTask(ctx, task)
		}
		message := strings.TrimSpace(result.Failed.Message)
		if message == "" {
			message = strings.TrimSpace(result.Failed.Reason)
		}
		if message == "" && err != nil {
			message = err.Error()
		}
		if message == "" {
			message = "harness wrapper turn failed"
		}
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, events.RedactExecutionEventText(message))
	}
	if result.Completed == nil {
		return r.completeTask(ctx, task, corev1alpha1.TaskPhaseFailed, "harness wrapper turn ended without result")
	}
	return r.completeTask(ctx, task, corev1alpha1.TaskPhaseSucceeded, "harness wrapper task completed successfully")
}

func (r *TaskReconciler) markHarnessBrokeredApprovalWaiting(ctx context.Context, task *corev1alpha1.Task, approvalID, toolName string) error {
	if r == nil || task == nil {
		return nil
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		toolName = "brokered tool"
	}
	approvalID = strings.TrimSpace(approvalID)
	if approvalID == "" {
		approvalID = "pending"
	}
	message := fmt.Sprintf("waiting for brokered approval %s for %s", approvalID, toolName)
	now := metav1.Now()
	return r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		if t.Status.Phase != corev1alpha1.TaskPhaseRunning {
			return
		}
		t.Status.Message = message
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeWaitingForApproval,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "BrokeredToolApprovalPending",
			Message:            message,
		})
	})
}

func (r *TaskReconciler) clearHarnessBrokeredApprovalWaiting(ctx context.Context, task *corev1alpha1.Task, toolName string) error {
	if r == nil || task == nil {
		return nil
	}
	toolName = strings.TrimSpace(toolName)
	if toolName == "" {
		toolName = "brokered tool"
	}
	now := metav1.Now()
	return r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		if t.Status.Phase != corev1alpha1.TaskPhaseRunning {
			return
		}
		cond := meta.FindStatusCondition(t.Status.Conditions, ConditionTypeWaitingForApproval)
		if cond == nil || cond.Status != metav1.ConditionTrue {
			return
		}
		if strings.HasPrefix(t.Status.Message, "waiting for brokered approval ") {
			t.Status.Message = "harness wrapper turn running"
		}
		meta.SetStatusCondition(&t.Status.Conditions, metav1.Condition{
			Type:               ConditionTypeWaitingForApproval,
			Status:             metav1.ConditionFalse,
			LastTransitionTime: now,
			Reason:             "BrokeredToolContinued",
			Message:            fmt.Sprintf("brokered tool %s continued", toolName),
		})
	})
}

func harnessWrapperCompletedResultBytes(completed *harness.TurnCompleted) []byte {
	if completed == nil {
		return nil
	}
	if len(completed.Data) == 0 && len(completed.Artifacts) == 0 {
		return []byte(completed.Result)
	}
	encoded, err := common.FormatStructuredResult(&common.StructuredResult{
		Version:   1,
		Summary:   completed.Result,
		Data:      completed.Data,
		Artifacts: harnessArtifactRefsToStructured(completed.Artifacts),
	})
	if err != nil {
		return []byte(completed.Result)
	}
	return encoded
}

func harnessWrapperFailedResultBytes(failed *harness.TurnFailed) []byte {
	if failed == nil {
		return nil
	}
	if len(failed.Data) == 0 && len(failed.Artifacts) == 0 {
		return []byte(failed.Result)
	}
	summary := strings.TrimSpace(failed.Result)
	if summary == "" {
		summary = strings.TrimSpace(failed.Message)
	}
	encoded, err := common.FormatStructuredResult(&common.StructuredResult{
		Version:   1,
		Summary:   summary,
		Data:      failed.Data,
		Artifacts: harnessArtifactRefsToStructured(failed.Artifacts),
	})
	if err != nil {
		return []byte(failed.Result)
	}
	return encoded
}

func harnessArtifactRefsToStructured(refs []harness.ArtifactRef) []common.ArtifactRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]common.ArtifactRef, 0, len(refs))
	for _, ref := range refs {
		filename := strings.TrimSpace(ref.Filename)
		if filename == "" {
			continue
		}
		out = append(out, common.ArtifactRef{
			Filename:    filename,
			ContentType: strings.TrimSpace(ref.ContentType),
			Size:        ref.Size,
			Description: strings.TrimSpace(ref.Description),
		})
	}
	return out
}

func harnessWrapperAuthRetries(task *corev1alpha1.Task) int {
	if task == nil || task.Annotations == nil {
		return 0
	}
	retries, err := strconv.Atoi(strings.TrimSpace(task.Annotations[harnessWrapperAuthRetriesAnno]))
	if err != nil || retries < 0 {
		return 0
	}
	return retries
}

func (r *TaskReconciler) waitForHarnessWrapperAuthRetry(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	retries := harnessWrapperAuthRetries(task)
	if retries >= harnessWrapperMaxAuthRetries {
		return false, nil
	}
	patch := ctrlclient.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[harnessWrapperAuthRetriesAnno] = strconv.Itoa(retries + 1)
	if err := r.Patch(ctx, task, patch); err != nil {
		return false, err
	}
	return true, nil
}

func harnessWrapperCancellation(task *corev1alpha1.Task) (harnessWrapperCancellationState, bool) {
	if task == nil || task.Annotations == nil {
		return harnessWrapperCancellationState{}, false
	}
	raw := strings.TrimSpace(task.Annotations[harnessWrapperCancellationAnno])
	if raw == "" {
		return harnessWrapperCancellationState{}, false
	}
	var state harnessWrapperCancellationState
	if err := json.Unmarshal([]byte(raw), &state); err != nil {
		return harnessWrapperCancellationState{}, false
	}
	state.TurnID = strings.TrimSpace(state.TurnID)
	state.Status = strings.TrimSpace(state.Status)
	state.Reason = strings.TrimSpace(state.Reason)
	state.RetryAt = strings.TrimSpace(state.RetryAt)
	return state, state.TurnID != "" && state.Status != ""
}

func taskHasHarnessWrapperCancellationState(task *corev1alpha1.Task) bool {
	return task != nil && task.Annotations != nil && strings.TrimSpace(task.Annotations[harnessWrapperCancellationAnno]) != ""
}

func (r *TaskReconciler) patchHarnessWrapperCancellation(
	ctx context.Context,
	task *corev1alpha1.Task,
	state harnessWrapperCancellationState,
) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	latest := &corev1alpha1.Task{}
	if err := reader.Get(ctx, ctrlclient.ObjectKey{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
		return err
	}
	latestTurnID := strings.TrimSpace(latest.Annotations[harnessWrapperTurnIDAnnotation])
	if latestTurnID == "" || latestTurnID != strings.TrimSpace(state.TurnID) ||
		!harnessWrapperTurnAnnotationsMatchTaskAttempt(latest, harnessWrapperCurrentAttempt(latest)) {
		latest.DeepCopyInto(task)
		return fmt.Errorf("harness cancellation turn changed while persisting state")
	}
	if existing, ok := harnessWrapperCancellation(latest); ok &&
		existing.TurnID == state.TurnID && harnessWrapperCancellationFinished(existing) && !harnessWrapperCancellationFinished(state) {
		latest.DeepCopyInto(task)
		return nil
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return err
	}
	patch := ctrlclient.MergeFromWithOptions(latest.DeepCopy(), ctrlclient.MergeFromWithOptimisticLock{})
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[harnessWrapperCancellationAnno] = string(encoded)
	delete(latest.Annotations, harnessWrapperCancelDependencyRetriesAnno)
	if err := r.Patch(ctx, latest, patch); err != nil {
		return err
	}
	latest.DeepCopyInto(task)
	return nil
}

func harnessWrapperCancellationFinished(state harnessWrapperCancellationState) bool {
	return state.Status == string(harnessWrapperCancelAcknowledged) || state.Status == string(harnessWrapperCancelMissing)
}

func harnessWrapperCancelRetryDelay(attempts int) time.Duration {
	if attempts <= 1 {
		return harnessWrapperCancelInitialBackoff
	}
	delay := harnessWrapperCancelInitialBackoff
	for range attempts - 1 {
		if delay >= harnessWrapperCancelMaxBackoff/2 {
			return harnessWrapperCancelMaxBackoff
		}
		delay *= 2
	}
	if delay > harnessWrapperCancelMaxBackoff {
		return harnessWrapperCancelMaxBackoff
	}
	return delay
}

func harnessWrapperCancellationRetryAfter(state harnessWrapperCancellationState, now time.Time) time.Duration {
	if state.RetryAt == "" {
		return 0
	}
	retryAt, err := time.Parse(time.RFC3339Nano, state.RetryAt)
	if err != nil || !retryAt.After(now) {
		return 0
	}
	return min(retryAt.Sub(now), harnessWrapperCancelMaxBackoff)
}

func (r *TaskReconciler) ensureHarnessWrapperTurnCancelled(
	ctx context.Context,
	task *corev1alpha1.Task,
	reason string,
) (bool, time.Duration, error) {
	if task == nil {
		return true, 0, nil
	}
	if harnessWrapperTurnOutcomeUnknown(task) {
		return true, 0, nil
	}
	state, stateValid := harnessWrapperCancellation(task)
	if !taskHasPlannedHarnessWrapperTurn(task) {
		if taskHasHarnessWrapperCancellationState(task) {
			return false, 0, fmt.Errorf("harness cancellation state exists but turn identity is missing")
		}
		return true, 0, nil
	}
	if !harnessWrapperTurnAnnotationsMatchTaskAttempt(task, harnessWrapperCurrentAttempt(task)) {
		return false, 0, fmt.Errorf("harness cancellation turn identity does not match task")
	}

	turnID := strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation])
	if !stateValid || state.TurnID != turnID {
		state = harnessWrapperCancellationState{
			TurnID: turnID,
			Status: string(harnessWrapperCancelPending),
			Reason: strings.TrimSpace(reason),
		}
		// The durable owner must exist before issuing the external cancellation.
		if err := r.patchHarnessWrapperCancellation(ctx, task, state); err != nil {
			return false, 0, err
		}
	}
	if harnessWrapperCancellationFinished(state) {
		return true, 0, nil
	}
	state.Status = string(harnessWrapperCancelPending)
	if state.Reason == "" {
		state.Reason = strings.TrimSpace(reason)
	}
	if retryAfter := harnessWrapperCancellationRetryAfter(state, time.Now()); retryAfter > 0 {
		return false, retryAfter, nil
	}

	outcome, err := r.cancelHarnessWrapperTurnOnce(ctx, task, state.Reason)
	if err != nil {
		if state.Attempts < 1_000_000 {
			state.Attempts++
		}
		retryAfter := harnessWrapperCancelRetryDelay(state.Attempts)
		state.RetryAt = time.Now().UTC().Add(retryAfter).Format(time.RFC3339Nano)
		if patchErr := r.patchHarnessWrapperCancellation(ctx, task, state); patchErr != nil {
			return false, 0, errors.Join(err, patchErr)
		}
		return false, retryAfter, nil
	}
	state.Status = string(outcome)
	state.RetryAt = ""
	if err := r.patchHarnessWrapperCancellation(ctx, task, state); err != nil {
		return false, 0, err
	}
	return true, 0, nil
}

func harnessWrapperOutputFetchRetries(task *corev1alpha1.Task) int {
	if task == nil || task.Annotations == nil {
		return 0
	}
	retries, err := strconv.Atoi(strings.TrimSpace(task.Annotations[harnessWrapperOutputFetchRetriesAnno]))
	if err != nil || retries < 0 {
		return 0
	}
	return retries
}

func (r *TaskReconciler) patchHarnessWrapperOutputFetchRetries(
	ctx context.Context,
	task *corev1alpha1.Task,
	retries int,
) error {
	patch := ctrlclient.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[harnessWrapperOutputFetchRetriesAnno] = strconv.Itoa(retries)
	return r.Patch(ctx, task, patch)
}

func (r *TaskReconciler) patchHarnessWrapperPlannedTurn(
	ctx context.Context,
	task *corev1alpha1.Task,
	request harness.StartTurnRequest,
) error {
	patch := ctrlclient.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[harnessWrapperTurnIDAnnotation] = string(request.TurnID)
	task.Annotations[harnessWrapperRuntimeAnnotation] = string(request.RuntimeSessionID)
	task.Annotations[harnessWrapperCorrelationIDAnno] = request.CorrelationID
	task.Annotations[harnessWrapperLastFrameSeqAnno] = "0"
	task.Annotations[harnessWrapperStartedAnno] = "false"
	task.Annotations[harnessWrapperPlannedAtAnno] = time.Now().UTC().Format(time.RFC3339Nano)
	delete(task.Annotations, harnessWrapperStartAttemptedAnno)
	delete(task.Annotations, harnessWrapperCancellationAnno)
	delete(task.Annotations, harnessWrapperCancelDependencyRetriesAnno)
	delete(task.Annotations, labels.AnnotationHarnessTurnOutcomeUnknown)
	if runtimeRefName := strings.TrimSpace(request.Metadata["runtimeRef"]); runtimeRefName != "" {
		task.Annotations[harnessWrapperRuntimeRefAnno] = runtimeRefName
	} else {
		delete(task.Annotations, harnessWrapperRuntimeRefAnno)
	}
	if contractVersion := strings.TrimSpace(request.Metadata["contractVersion"]); contractVersion != "" {
		task.Annotations[harnessWrapperContractAnno] = contractVersion
	} else {
		task.Annotations[harnessWrapperContractAnno] = harness.ProtocolVersion
	}
	clearDeprecatedHarnessRuntimeAnnotations(task.Annotations)

	plannedMetadata := make(map[string]string, len(request.Metadata))
	for key, value := range request.Metadata {
		if key == "systemPrompt" || key == harnessWrapperSkillsFilesMeta {
			continue
		}
		plannedMetadata[key] = value
	}
	metadata, err := json.Marshal(plannedMetadata)
	if err != nil {
		return err
	}
	task.Annotations[harnessWrapperMetadataAnno] = string(metadata)
	return r.Patch(ctx, task, patch)
}

func (r *TaskReconciler) patchHarnessWrapperStartAttempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	turnID harness.HarnessTurnID,
) error {
	if task == nil {
		return fmt.Errorf("task is required to persist harness start attempt")
	}
	value := strings.TrimSpace(string(turnID))
	if value == "" {
		return fmt.Errorf("turn ID is required to persist harness start attempt")
	}
	if taskHasHarnessWrapperStartAttempt(task) {
		return nil
	}
	if task.Annotations == nil || strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]) != value {
		return fmt.Errorf("harness start attempt does not match planned turn")
	}
	patch := ctrlclient.MergeFrom(task.DeepCopy())
	task.Annotations[harnessWrapperStartAttemptedAnno] = value
	return r.Patch(ctx, task, patch)
}

func (r *TaskReconciler) patchHarnessWrapperTurnOutcomeUnknown(
	ctx context.Context,
	task *corev1alpha1.Task,
	turnID harness.HarnessTurnID,
) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	latest := &corev1alpha1.Task{}
	if err := reader.Get(ctx, ctrlclient.ObjectKey{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
		return false, err
	}
	if latest.Status.Phase != corev1alpha1.TaskPhasePending {
		latest.DeepCopyInto(task)
		return false, nil
	}
	if strings.TrimSpace(latest.Annotations[harnessWrapperTurnIDAnnotation]) != strings.TrimSpace(string(turnID)) ||
		!harnessWrapperTurnAnnotationsMatchTaskAttempt(latest, harnessWrapperCurrentAttempt(latest)) {
		latest.DeepCopyInto(task)
		return false, nil
	}
	patch := ctrlclient.MergeFromWithOptions(latest.DeepCopy(), ctrlclient.MergeFromWithOptimisticLock{})
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	latest.Annotations[labels.AnnotationHarnessTurnOutcomeUnknown] = strings.TrimSpace(string(turnID))
	if err := r.Patch(ctx, latest, patch); err != nil {
		return false, err
	}
	latest.DeepCopyInto(task)
	return true, nil
}

func (r *TaskReconciler) patchHarnessWrapperStarted(ctx context.Context, task *corev1alpha1.Task) error {
	latest := &corev1alpha1.Task{}
	if err := r.Get(ctx, ctrlclient.ObjectKey{Name: task.Name, Namespace: task.Namespace}, latest); err != nil {
		return err
	}
	patch := ctrlclient.MergeFrom(latest.DeepCopy())
	if latest.Annotations == nil {
		latest.Annotations = map[string]string{}
	}
	for _, key := range []string{
		harnessWrapperTurnIDAnnotation,
		harnessWrapperRuntimeAnnotation,
		harnessWrapperCorrelationIDAnno,
		harnessWrapperLastFrameSeqAnno,
		harnessWrapperPlannedAtAnno,
		harnessWrapperMetadataAnno,
	} {
		if strings.TrimSpace(latest.Annotations[key]) == "" && task != nil && task.Annotations != nil {
			if value := task.Annotations[key]; strings.TrimSpace(value) != "" {
				latest.Annotations[key] = value
			}
		}
	}
	latest.Annotations[harnessWrapperStartedAnno] = scheduledRunLabelValue
	delete(latest.Annotations, harnessWrapperStartAttemptedAnno)
	if err := r.Patch(ctx, latest, patch); err != nil {
		return err
	}
	latest.DeepCopyInto(task)
	return nil
}

func harnessWrapperPlannedMetadata(task *corev1alpha1.Task, runtimeName string) map[string]string {
	metadata := map[string]string{}
	if task != nil && task.Annotations != nil {
		_ = json.Unmarshal([]byte(task.Annotations[harnessWrapperMetadataAnno]), &metadata)
	}
	if metadata == nil {
		metadata = map[string]string{}
	}
	if strings.TrimSpace(metadata["runtime"]) == "" {
		metadata["runtime"] = runtimeName
	}
	if strings.TrimSpace(metadata["wrapper"]) == "" {
		metadata["wrapper"] = "cli"
	}
	return metadata
}

func harnessWrapperPlannedToolExecutionMode(task *corev1alpha1.Task) harness.ToolExecutionMode {
	metadata := harnessWrapperPlannedMetadata(task, "")
	return harness.ToolExecutionMode(strings.TrimSpace(metadata["toolExecutionMode"]))
}

func harnessWrapperCurrentAttempt(task *corev1alpha1.Task) int32 {
	if task == nil || task.Status.Attempts <= 0 {
		return 1
	}
	return task.Status.Attempts
}

func harnessWrapperTurnAnnotationsMatchTaskAttempt(task *corev1alpha1.Task, attempts int32) bool {
	if !taskHasPlannedHarnessWrapperTurn(task) {
		return false
	}
	correlationID := ""
	if task != nil {
		correlationID = string(task.UID)
		if strings.TrimSpace(correlationID) == "" {
			correlationID = task.Namespace + "/" + task.Name
		}
	}
	return strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]) == string(harnessWrapperTurnID(task, attempts)) &&
		strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno]) == correlationID
}

func harnessWrapperPlannedTurnMatchesTask(task *corev1alpha1.Task, agent *corev1alpha1.Agent, attempts int32) bool {
	if !taskHasPlannedHarnessWrapperTurn(task) {
		return false
	}
	runtimeName := agentHarnessRuntimeName(agent)
	if plannedRuntime := strings.TrimSpace(harnessWrapperPlannedMetadata(task, "")["runtime"]); plannedRuntime != "" {
		runtimeName = plannedRuntime
	}
	if task != nil && task.Status.HarnessRuntime != nil && strings.TrimSpace(task.Status.HarnessRuntime.RuntimeRefName) != "" {
		runtimeName = strings.TrimSpace(task.Status.HarnessRuntime.RuntimeRefName)
	}
	correlationID := string(task.UID)
	if strings.TrimSpace(correlationID) == "" {
		correlationID = task.Namespace + "/" + task.Name
	}
	expectedRuntimeSessionID := string(harnessWrapperRuntimeSessionID(task, runtimeName))
	return strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]) == string(harnessWrapperTurnID(task, attempts)) &&
		strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]) == expectedRuntimeSessionID &&
		strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno]) == correlationID
}

func (r *TaskReconciler) validateHarnessWrapperCapabilities(
	ctx context.Context,
	client *harness.Client,
	request harness.StartTurnRequest,
) error {
	capabilities, err := client.Capabilities(ctx)
	if err != nil {
		return fmt.Errorf("read harness runtime capabilities: %w", err)
	}
	wantRuntime := strings.TrimSpace(request.Metadata["runtime"])
	runtimeMatches := wantRuntime == "" || capabilities.RuntimeName == wantRuntime
	if !runtimeMatches {
		for runtime := range strings.SplitSeq(capabilities.Metadata["supportedRuntimes"], ",") {
			if strings.TrimSpace(runtime) == wantRuntime {
				runtimeMatches = true
				break
			}
		}
	}
	if !runtimeMatches {
		return fmt.Errorf("harness runtime %q does not match task runtime %q", sanitizeAgentRuntimeCapabilityValue(capabilities.RuntimeName), sanitizeAgentRuntimeCapabilityValue(wantRuntime))
	}
	if strings.TrimSpace(request.Metadata["runtimeRef"]) != "" {
		if request.ToolExecutionMode == harness.ToolExecutionModeBrokered {
			if err := validateAgentRuntimeExecutableCapabilities(capabilities); err != nil {
				return err
			}
		} else if err := validateObservedHarnessCapabilities(capabilities); err != nil {
			return err
		}
	}
	if request.ToolExecutionMode == harness.ToolExecutionModeBrokered {
		if !capabilityHasToolMode(capabilities, corev1alpha1.AgentRuntimeToolExecutionModeBrokered) {
			return fmt.Errorf("runtime does not advertise required toolExecutionMode %q", corev1alpha1.AgentRuntimeToolExecutionModeBrokered)
		}
		for _, requiredClass := range harnessWrapperRequiredBrokeredClassesFromTurnRequest(request) {
			if !capabilityHasBrokeredToolClass(capabilities, requiredClass) {
				return fmt.Errorf("runtime does not advertise required brokeredToolClass %q", requiredClass)
			}
		}
		if !capabilities.SupportsContinuation {
			return fmt.Errorf("runtime does not advertise required supportsContinuation capability")
		}
	}
	return nil
}

func applyHarnessRuntimeToolExecutionMode(request *harness.StartTurnRequest, target harnessRuntimeTarget) error {
	if request == nil || strings.TrimSpace(target.RuntimeRefName) == "" {
		return nil
	}
	observed := slices.Contains(target.ToolExecutionModes, corev1alpha1.AgentRuntimeToolExecutionModeObserved)
	brokered := slices.Contains(target.ToolExecutionModes, corev1alpha1.AgentRuntimeToolExecutionModeBrokered)
	requestedClasses := harnessWrapperRequiredBrokeredClassesFromTurnRequest(*request)
	if len(request.Input.Tools) > 0 || len(requestedClasses) > 0 {
		if !brokered {
			return fmt.Errorf("AgentRuntime %q does not advertise brokered tool execution", target.RuntimeRefName)
		}
		if !target.SupportsContinuation {
			return fmt.Errorf("AgentRuntime %q does not advertise brokered continuation", target.RuntimeRefName)
		}
		for _, class := range requestedClasses {
			if !slices.Contains(target.BrokeredToolClasses, class) {
				return fmt.Errorf("AgentRuntime %q does not advertise brokeredToolClass %q", target.RuntimeRefName, class)
			}
		}
		request.ToolExecutionMode = harness.ToolExecutionModeBrokered
		if request.Metadata == nil {
			request.Metadata = map[string]string{}
		}
		request.Metadata["toolExecutionMode"] = string(harness.ToolExecutionModeBrokered)
		return nil
	}
	if !observed {
		return fmt.Errorf("AgentRuntime %q does not advertise observed mode and task exposes no brokered tools", target.RuntimeRefName)
	}
	request.ToolExecutionMode = harness.ToolExecutionModeObserved
	if request.Metadata == nil {
		request.Metadata = map[string]string{}
	}
	request.Metadata["toolExecutionMode"] = string(harness.ToolExecutionModeObserved)
	return nil
}

func harnessWrapperRequiredBrokeredClassesFromTurnRequest(request harness.StartTurnRequest) []corev1alpha1.AgentRuntimeBrokeredToolClass {
	seen := map[corev1alpha1.AgentRuntimeBrokeredToolClass]struct{}{}
	out := []corev1alpha1.AgentRuntimeBrokeredToolClass{}
	for _, definition := range request.Input.Tools {
		class := corev1alpha1.AgentRuntimeBrokeredToolClass(strings.TrimSpace(string(definition.BrokeredClass)))
		if class == "" {
			continue
		}
		if _, ok := seen[class]; ok {
			continue
		}
		seen[class] = struct{}{}
		out = append(out, class)
	}
	if len(out) > 0 {
		return out
	}
	return harnessWrapperRequiredBrokeredClassesFromMetadata(request.Metadata)
}

func harnessWrapperRequiredBrokeredClassesFromMetadata(metadata map[string]string) []corev1alpha1.AgentRuntimeBrokeredToolClass {
	if len(metadata) == 0 {
		return nil
	}
	return parseBrokeredToolClasses(metadata["brokeredToolClasses"])
}

func parseBrokeredToolClasses(value string) []corev1alpha1.AgentRuntimeBrokeredToolClass {
	seen := map[corev1alpha1.AgentRuntimeBrokeredToolClass]struct{}{}
	out := []corev1alpha1.AgentRuntimeBrokeredToolClass{}
	for raw := range strings.SplitSeq(value, ",") {
		class := corev1alpha1.AgentRuntimeBrokeredToolClass(strings.TrimSpace(raw))
		if class == "" {
			continue
		}
		if _, ok := seen[class]; ok {
			continue
		}
		seen[class] = struct{}{}
		out = append(out, class)
	}
	return out
}

func harnessWrapperPlannedBrokeredToolClassMap(task *corev1alpha1.Task) (map[string]corev1alpha1.AgentRuntimeBrokeredToolClass, error) {
	metadata := harnessWrapperPlannedMetadata(task, "")
	raw := strings.TrimSpace(metadata["brokeredToolClassMap"])
	if raw == "" {
		return nil, nil
	}
	decoded := map[string]corev1alpha1.AgentRuntimeBrokeredToolClass{}
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil {
		return nil, fmt.Errorf("parse planned brokered tool class map: %w", err)
	}
	return decoded, nil
}

func (r *TaskReconciler) harnessWrapperBrokeredToolClassMap(
	ctx context.Context,
	task *corev1alpha1.Task,
) (map[string]corev1alpha1.AgentRuntimeBrokeredToolClass, error) {
	out := map[string]corev1alpha1.AgentRuntimeBrokeredToolClass{}
	for _, toolName := range harnessWrapperBrokeredToolNames(task) {
		if isHarnessBrokeredCoordinationToolName(toolName) {
			out[toolName] = corev1alpha1.AgentRuntimeBrokeredToolClassCoordination
			continue
		}
		tool := &corev1alpha1.Tool{}
		if err := r.Get(ctx, ctrlclient.ObjectKey{Namespace: task.Namespace, Name: toolName}, tool); err != nil {
			return nil, fmt.Errorf("read brokered tool %q: %w", toolName, err)
		}
		if tool.Spec.BrokeredToolClass == "" {
			return nil, fmt.Errorf("brokered tool %q must set spec.brokeredToolClass", toolName)
		}
		out[toolName] = tool.Spec.BrokeredToolClass
	}
	return out, nil
}

func (r *TaskReconciler) harnessWrapperRequiredBrokeredToolClasses(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) ([]string, error) {
	if harnessWrapperToolExecutionMode(task, agent) != harness.ToolExecutionModeBrokered {
		return nil, nil
	}
	classMap, err := r.harnessWrapperBrokeredToolClassMap(ctx, task)
	if err != nil {
		return nil, err
	}
	seen := map[corev1alpha1.AgentRuntimeBrokeredToolClass]struct{}{}
	classes := []string{}
	for _, class := range classMap {
		if _, ok := seen[class]; ok {
			continue
		}
		seen[class] = struct{}{}
		classes = append(classes, string(class))
	}
	sort.Strings(classes)
	return classes, nil
}

func (r *TaskReconciler) harnessWrapperBrokeredToolDefinitions(
	ctx context.Context,
	task *corev1alpha1.Task,
) ([]harness.ToolDefinition, error) {
	toolNames := harnessWrapperBrokeredToolNames(task)
	if len(toolNames) == 0 {
		return nil, nil
	}
	definitions := make([]harness.ToolDefinition, 0, len(toolNames))
	for _, toolName := range toolNames {
		if isHarnessBrokeredCoordinationToolName(toolName) {
			definitions = append(definitions, harnessBrokeredCoordinationToolDefinition(toolName))
			continue
		}
		tool := &corev1alpha1.Tool{}
		if err := r.Get(ctx, ctrlclient.ObjectKey{Namespace: task.Namespace, Name: toolName}, tool); err != nil {
			return nil, fmt.Errorf("read brokered tool %q: %w", toolName, err)
		}
		if tool.Spec.BrokeredToolClass == "" {
			return nil, fmt.Errorf("brokered tool %q must set spec.brokeredToolClass", toolName)
		}
		definition := harness.ToolDefinition{
			Name:          tool.Name,
			Description:   strings.TrimSpace(tool.Spec.Description),
			BrokeredClass: harness.BrokeredToolClass(tool.Spec.BrokeredToolClass),
		}
		if tool.Spec.Parameters != nil && len(tool.Spec.Parameters.Raw) > 0 {
			definition.Parameters = append(json.RawMessage(nil), tool.Spec.Parameters.Raw...)
		}
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

func harnessBrokeredCoordinationToolDefinition(name string) harness.ToolDefinition {
	definition := harness.ToolDefinition{
		Name:          strings.TrimSpace(name),
		BrokeredClass: harness.BrokeredToolClassCoordination,
		Parameters:    json.RawMessage(`{"type":"object","additionalProperties":true}`),
	}
	switch definition.Name {
	case "delegate_task":
		definition.Description = "Create a governed child Orka agent task."
	case "wait_for_tasks":
		definition.Description = "Wait for delegated child tasks and return bounded result summaries."
	case "cancel_task":
		definition.Description = "Cancel a governed child Orka task."
	case "send_message":
		definition.Description = "Send a coordination message to another task."
	case "check_messages":
		definition.Description = "Check coordination messages for this task."
	default:
		definition.Description = "Orka coordination tool."
	}
	return definition
}

func harnessWrapperAuthError(err error) bool {
	if err == nil {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, marker := range []string{"(401)", "(403)", "unauthorized", "forbidden"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func harnessWrapperStartTurnErrorIsRetryable(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, marker := range []string{"(400)", "(401)", "(403)", "unsupported version", "harness did not accept"} {
		if strings.Contains(message, marker) {
			return false
		}
	}
	return true
}

func harnessWrapperCapabilitiesErrorIsRetryable(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	if !strings.Contains(message, "read harness runtime capabilities") {
		return false
	}
	for _, marker := range []string{"(400)", "(401)", "(403)", "(404)", "unsupported version"} {
		if strings.Contains(message, marker) {
			return false
		}
	}
	return true
}

func (r *TaskReconciler) clearHarnessWrapperTurnState(ctx context.Context, task *corev1alpha1.Task) error {
	patch := ctrlclient.MergeFrom(task.DeepCopy())
	if task.Annotations != nil {
		delete(task.Annotations, harnessWrapperTurnIDAnnotation)
		delete(task.Annotations, harnessWrapperRuntimeAnnotation)
		delete(task.Annotations, harnessWrapperCorrelationIDAnno)
		delete(task.Annotations, harnessWrapperLastFrameSeqAnno)
		delete(task.Annotations, harnessWrapperStartedAnno)
		delete(task.Annotations, harnessWrapperStartAttemptedAnno)
		delete(task.Annotations, harnessWrapperPlannedAtAnno)
		delete(task.Annotations, harnessWrapperMetadataAnno)
		delete(task.Annotations, harnessWrapperRuntimeRefAnno)
		delete(task.Annotations, harnessWrapperContractAnno)
		clearDeprecatedHarnessRuntimeAnnotations(task.Annotations)
		delete(task.Annotations, harnessWrapperOutputFetchRetriesAnno)
		delete(task.Annotations, harnessWrapperCancelDependencyRetriesAnno)
		delete(task.Annotations, harnessWrapperCancellationAnno)
		delete(task.Annotations, labels.AnnotationHarnessTurnOutcomeUnknown)
		delete(task.Annotations, harnessWrapperAuthRetriesAnno)
	}
	if err := r.Patch(ctx, task, patch); err != nil {
		return err
	}
	return r.updateStatusWithRetry(ctx, task, func(t *corev1alpha1.Task) {
		t.Status.HarnessRuntime = nil
	})
}

func harnessWrapperStreamErrorIsMissingTurn(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, marker := range []string{"(404)", "(410)", "turn not found"} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func harnessWrapperStreamErrorIsTerminal(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	for _, marker := range []string{
		"(401)", "(403)", "(404)", "(410)", "turn not found", "unauthorized",
		"harness frame identity does not match", "invalid harness frame", "invalid harness frame content JSON",
		"decode harness frame",
	} {
		if strings.Contains(message, marker) {
			return true
		}
	}
	return false
}

func harnessWrapperStreamErrorIsBrokeredPause(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), "continue brokered tool call") || errors.Is(err, errHarnessBrokeredApprovalPending)
}

func harnessWrapperCancelErrorIsMissing(err error) bool {
	if err == nil {
		return false
	}
	var clientErr harness.ClientError
	if errors.As(err, &clientErr) && (clientErr.StatusCode == 404 || clientErr.StatusCode == 410) {
		return true
	}
	return harnessWrapperStreamErrorIsMissingTurn(err)
}

func (r *TaskReconciler) cancelHarnessWrapperTurnOnce(
	ctx context.Context,
	task *corev1alpha1.Task,
	reason string,
) (harnessWrapperCancelOutcome, error) {
	if !taskHasPlannedHarnessWrapperTurn(task) {
		return harnessWrapperCancelNotNeeded, nil
	}
	if !harnessWrapperTurnAnnotationsMatchTaskAttempt(task, harnessWrapperCurrentAttempt(task)) {
		return harnessWrapperCancelNotNeeded, nil
	}
	target, err := r.resolveHarnessRuntimeTarget(ctx, task, nil)
	if err != nil {
		return harnessWrapperCancelPending, err
	}
	client, err := newHarnessClientForRuntimeTarget(target)
	if err != nil {
		return harnessWrapperCancelPending, err
	}
	_, err = client.CancelTurn(ctx, harness.CancelTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        task.Namespace,
		TaskName:         task.Name,
		SessionName:      harnessWrapperSessionName(task),
		RuntimeSessionID: harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation])),
		TurnID:           harness.HarnessTurnID(strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation])),
		CorrelationID:    strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno]),
		Reason:           reason,
	})
	if err != nil {
		if harnessWrapperCancelErrorIsMissing(err) {
			return harnessWrapperCancelMissing, nil
		}
		return harnessWrapperCancelPending, err
	}
	return harnessWrapperCancelAcknowledged, nil
}

func (r *TaskReconciler) cancelHarnessWrapperTurn(ctx context.Context, task *corev1alpha1.Task, reason string) error {
	_, err := r.cancelHarnessWrapperTurnOnce(ctx, task, reason)
	return err
}

func (r *TaskReconciler) plannedHarnessWrapperStartTurnRequest(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	now time.Time,
) (harness.StartTurnRequest, error) {
	deadline := now.Add(harnessWrapperNoTimeoutDuration)
	if task.Spec.Timeout != nil {
		deadline = now.Add(task.Spec.Timeout.Duration)
	}
	plannedMetadata := harnessWrapperPlannedMetadata(task, agentHarnessRuntimeName(agent))
	attempts := harnessWrapperCurrentAttempt(task)
	request, err := r.harnessWrapperStartTurnRequest(ctx, task, agent, now, attempts)
	if err != nil {
		return harness.StartTurnRequest{}, err
	}
	request.RuntimeSessionID = harness.RuntimeSessionID(strings.TrimSpace(task.Annotations[harnessWrapperRuntimeAnnotation]))
	request.TurnID = harness.HarnessTurnID(strings.TrimSpace(task.Annotations[harnessWrapperTurnIDAnnotation]))
	request.CorrelationID = strings.TrimSpace(task.Annotations[harnessWrapperCorrelationIDAnno])
	request.Deadline = deadline.UTC()
	if request.Metadata == nil {
		request.Metadata = map[string]string{}
	}
	maps.Copy(request.Metadata, plannedMetadata)
	return request, nil
}

func (r *TaskReconciler) harnessWrapperStartTurnRequest(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	now time.Time,
	attempts int32,
) (harness.StartTurnRequest, error) {
	deadline := now.Add(harnessWrapperNoTimeoutDuration)
	if task.Spec.Timeout != nil {
		deadline = now.Add(task.Spec.Timeout.Duration)
	}
	runtimeName := agentHarnessRuntimeName(agent)
	turnID := harnessWrapperTurnID(task, attempts)
	correlationID := string(task.UID)
	if strings.TrimSpace(correlationID) == "" {
		correlationID = task.Namespace + "/" + task.Name
	}
	prompt := task.Spec.Prompt
	if prompt == "" && task.Spec.AI != nil {
		prompt = task.Spec.AI.Prompt
	}
	metadata, err := r.harnessWrapperTurnMetadata(ctx, task, agent, runtimeName)
	if err != nil {
		return harness.StartTurnRequest{}, err
	}
	brokeredClasses, err := r.harnessWrapperRequiredBrokeredToolClasses(ctx, task, agent)
	if err != nil {
		return harness.StartTurnRequest{}, err
	}
	if len(brokeredClasses) > 0 {
		metadata["brokeredToolClasses"] = strings.Join(brokeredClasses, ",")
		classMap, err := r.harnessWrapperBrokeredToolClassMap(ctx, task)
		if err != nil {
			return harness.StartTurnRequest{}, err
		}
		encoded, err := json.Marshal(classMap)
		if err != nil {
			return harness.StartTurnRequest{}, fmt.Errorf("marshal brokered tool class map: %w", err)
		}
		metadata["brokeredToolClassMap"] = string(encoded)
	}
	toolExecutionMode := harnessWrapperToolExecutionMode(task, agent)
	if toolExecutionMode != "" {
		metadata["toolExecutionMode"] = string(toolExecutionMode)
	}
	turnEnv, err := r.harnessWrapperTurnEnv(ctx, task, agent)
	if err != nil {
		return harness.StartTurnRequest{}, err
	}
	var toolDefinitions []harness.ToolDefinition
	if toolExecutionMode == harness.ToolExecutionModeBrokered {
		toolDefinitions, err = r.harnessWrapperBrokeredToolDefinitions(ctx, task)
		if err != nil {
			return harness.StartTurnRequest{}, err
		}
	}
	runtimeIdentity := harnessWrapperRuntimeSessionIdentity(task, agent, runtimeName)
	return harness.StartTurnRequest{
		Version:          harness.ProtocolVersion,
		Namespace:        task.Namespace,
		TaskName:         task.Name,
		SessionName:      harnessWrapperSessionName(task),
		RuntimeSessionID: runtimeIdentity.ID,
		TurnID:           turnID,
		CorrelationID:    correlationID,
		Deadline:         deadline.UTC(),
		AuthIdentity: harness.AuthIdentity{
			Subject: "task:" + task.Namespace + "/" + task.Name,
		},
		Input:             harness.TurnInput{Prompt: prompt, Env: turnEnv, Tools: toolDefinitions},
		ToolExecutionMode: toolExecutionMode,
		Metadata:          metadata,
	}, nil
}

//nolint:gocyclo // Collects runtime metadata from Agent and Task defaults in one place.
func (r *TaskReconciler) harnessWrapperTurnMetadata(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
	runtimeName string,
) (map[string]string, error) {
	metadata := map[string]string{
		"runtime":         runtimeName,
		"wrapper":         "cli",
		"contractVersion": harness.ProtocolVersion,
		"maxTurns":        "50",
	}
	if runtimeRefName := agentHarnessRuntimeRefName(agent); runtimeRefName != "" {
		metadata["runtimeRef"] = runtimeRefName
		metadata["wrapper"] = "external-endpoint"
	}
	if task != nil && task.Annotations != nil {
		if traceparent := strings.TrimSpace(task.Annotations[labels.AnnotationTraceParent]); traceparent != "" {
			metadata["traceparent"] = traceparent
		}
		if tracestate := strings.TrimSpace(task.Annotations[labels.AnnotationTraceState]); tracestate != "" {
			metadata["tracestate"] = tracestate
		}
	}
	if agent != nil {
		metadata["agentName"] = agent.Name
		if agent.Spec.Model != nil && strings.TrimSpace(agent.Spec.Model.Name) != "" {
			metadata["model"] = strings.TrimSpace(agent.Spec.Model.Name)
		}
		if strings.TrimSpace(metadata["model"]) == "" {
			if model := r.harnessWrapperDefaultProviderModel(ctx, agent.Namespace); model != "" {
				metadata["model"] = model
			}
		}
		if agent.Spec.SystemPrompt != nil {
			systemPrompt := strings.TrimSpace(agent.Spec.SystemPrompt.Inline)
			if systemPrompt == "" && agent.Spec.SystemPrompt.ConfigMapRef != nil {
				resolved, err := r.resolveHarnessWrapperConfigMapValue(
					ctx,
					agent.Namespace,
					agent.Spec.SystemPrompt.ConfigMapRef,
				)
				if err != nil {
					return nil, err
				}
				systemPrompt = strings.TrimSpace(resolved)
			}
			if systemPrompt != "" {
				metadata["systemPrompt"] = systemPrompt
			}
		}
	}
	skillsPrompt, skillsFiles, err := r.harnessWrapperSkillsPrompt(ctx, task, agent)
	if err != nil {
		return nil, err
	}
	if skillsPrompt != "" {
		metadata["systemPrompt"] = strings.TrimSpace(strings.Join(
			[]string{skillsPrompt, strings.TrimSpace(metadata["systemPrompt"])},
			"\n\n",
		))
	}
	if skillsFiles != "" {
		metadata[harnessWrapperSkillsFilesMeta] = skillsFiles
	}
	if agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultMaxTurns != nil {
		metadata["maxTurns"] = strconv.FormatInt(int64(*agent.Spec.Runtime.DefaultMaxTurns), 10)
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.MaxTurns != nil {
		metadata["maxTurns"] = strconv.FormatInt(int64(*task.Spec.AgentRuntime.MaxTurns), 10)
	}
	if task.Spec.Timeout != nil && task.Spec.Timeout.Duration > 0 {
		metadata["timeoutSeconds"] = strconv.FormatInt(int64(task.Spec.Timeout.Duration/time.Second), 10)
	}
	allowedTools := []string(nil)
	if agent != nil && agent.Spec.Runtime != nil {
		allowedTools = agent.Spec.Runtime.DefaultAllowedTools
	}
	if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.AllowedTools) > 0 {
		allowedTools = task.Spec.AgentRuntime.AllowedTools
	}
	disallowedTools := []string(nil)
	if task.Spec.AgentRuntime != nil && len(task.Spec.AgentRuntime.DisallowedTools) > 0 {
		disallowedTools = append(disallowedTools, task.Spec.AgentRuntime.DisallowedTools...)
	}
	if taskRequestsReadOnlyAgent(task) {
		metadata["readOnly"] = scheduledRunLabelValue
		allowedTools = readOnlyAgentAllowedTools()
		disallowedTools = append(disallowedTools, readOnlyAgentDisallowedTools()...)
		metadata["claudeBare"] = scheduledRunLabelValue
		metadata["claudeDisableSettingSources"] = scheduledRunLabelValue
		metadata["claudePermissionMode"] = "dontAsk"
	}
	if len(allowedTools) > 0 {
		metadata["allowedTools"] = strings.Join(allowedTools, ",")
	}
	if len(disallowedTools) > 0 {
		metadata["disallowedTools"] = strings.Join(disallowedTools, ",")
	}
	allowBash := true
	if agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.DefaultAllowBash != nil {
		allowBash = *agent.Spec.Runtime.DefaultAllowBash
	}
	if task.Spec.AgentRuntime != nil && task.Spec.AgentRuntime.AllowBash != nil {
		allowBash = *task.Spec.AgentRuntime.AllowBash
	}
	if taskRequestsReadOnlyAgent(task) {
		allowBash = false
	}
	metadata["allowBash"] = strconv.FormatBool(allowBash)
	if ws := effectiveWorkspace(task); ws != nil {
		if strings.TrimSpace(ws.GitRepo) != "" {
			metadata["gitRepo"] = strings.TrimSpace(ws.GitRepo)
		}
		if strings.TrimSpace(ws.Branch) != "" {
			metadata["gitBranch"] = strings.TrimSpace(ws.Branch)
		}
		if strings.TrimSpace(ws.Ref) != "" {
			metadata["gitRef"] = strings.TrimSpace(ws.Ref)
		}
		if strings.TrimSpace(ws.SubPath) != "" {
			metadata["workspaceSubPath"] = strings.TrimSpace(ws.SubPath)
		}
		if strings.TrimSpace(ws.ForkRepo) != "" {
			metadata["forkRepo"] = strings.TrimSpace(ws.ForkRepo)
		}
		if strings.TrimSpace(ws.PRBaseBranch) != "" {
			metadata["prBaseBranch"] = strings.TrimSpace(ws.PRBaseBranch)
		}
		if strings.TrimSpace(ws.PushBranch) != "" {
			metadata["pushBranch"] = strings.TrimSpace(ws.PushBranch)
		}
	}
	for _, env := range task.Spec.Env {
		switch env.Name {
		case workerenv.PRBaseRepo:
			metadata["prBaseRepo"] = strings.TrimSpace(env.Value)
		case workerenv.PRBaseSHA:
			metadata["prBaseSHA"] = strings.TrimSpace(env.Value)
		}
	}
	return metadata, nil
}

func (r *TaskReconciler) harnessWrapperDefaultProviderModel(ctx context.Context, namespace string) string {
	if r == nil || r.Client == nil || strings.TrimSpace(namespace) == "" {
		return ""
	}
	provider := &corev1alpha1.Provider{}
	if err := r.Get(ctx, ctrlclient.ObjectKey{Namespace: namespace, Name: "default"}, provider); err != nil {
		return ""
	}
	return strings.TrimSpace(provider.Spec.DefaultModel)
}

func (r *TaskReconciler) harnessWrapperSkillsPrompt(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) (string, string, error) {
	if task == nil {
		return "", "", nil
	}
	skillRefs := harnessWrapperSkillReferences(task, agent)
	if len(skillRefs) == 0 {
		return "", "", nil
	}
	promptParts := make([]string, 0, len(skillRefs))
	files := map[string]string{}
	for _, ref := range skillRefs {
		switch {
		case ref.Name != "":
			skillName := strings.TrimSpace(ref.Name)
			skill := &corev1alpha1.Skill{}
			if err := r.Get(ctx, ctrlclient.ObjectKey{Name: skillName, Namespace: task.Namespace}, skill); err != nil {
				return "", "", fmt.Errorf("failed to get Skill %q: %w", skillName, err)
			}
			metrics.SkillsLoaded.WithLabelValues(skill.Name, task.Namespace).Inc()
			if content := strings.TrimSpace(skill.Spec.Content.Inline); content != "" {
				promptParts = append(promptParts, content)
				files[path.Join(skillName, "SKILL.md")] = skill.Spec.Content.Inline
			}
			filePaths := make([]string, 0, len(skill.Spec.Content.Files))
			for filePath := range skill.Spec.Content.Files {
				filePaths = append(filePaths, filePath)
			}
			sort.Strings(filePaths)
			for _, filePath := range filePaths {
				files[path.Join(skillName, filePath)] = skill.Spec.Content.Files[filePath]
			}
		case ref.ConfigMapRef != nil:
			cmName := strings.TrimSpace(ref.ConfigMapRef.Name)
			cmKey := strings.TrimSpace(ref.ConfigMapRef.Key)
			cm := &corev1.ConfigMap{}
			if err := r.Get(ctx, ctrlclient.ObjectKey{Name: cmName, Namespace: task.Namespace}, cm); err != nil {
				return "", "", fmt.Errorf("failed to get skill ConfigMap %q: %w", cmName, err)
			}
			content, ok := cm.Data[cmKey]
			if !ok {
				return "", "", fmt.Errorf("key %q not found in skill ConfigMap %q", cmKey, cmName)
			}
			metrics.SkillsLoaded.WithLabelValues(cmName, task.Namespace).Inc()
			if content = strings.TrimSpace(content); content != "" {
				promptParts = append(promptParts, content)
				files[path.Join(cmName+"-"+cmKey, "SKILL.md")] = content
			}
		default:
			return "", "", fmt.Errorf("skill reference must set either name or configMapRef")
		}
	}
	filesJSON := ""
	if len(files) > 0 {
		data, err := json.Marshal(files)
		if err != nil {
			return "", "", err
		}
		filesJSON = string(data)
	}
	return strings.TrimSpace(strings.Join(promptParts, "\n\n")), filesJSON, nil
}

func harnessWrapperSkillReferences(
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) []corev1alpha1.SkillReference {
	var skillRefs []corev1alpha1.SkillReference
	if agent != nil {
		skillRefs = append(skillRefs, agent.Spec.Skills...)
	}
	if task != nil && task.Spec.AI != nil {
		skillRefs = append(skillRefs, task.Spec.AI.Skills...)
	}
	if len(skillRefs) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(skillRefs))
	deduped := make([]corev1alpha1.SkillReference, 0, len(skillRefs))
	for _, ref := range skillRefs {
		key := ""
		switch {
		case strings.TrimSpace(ref.Name) != "":
			name := strings.TrimSpace(ref.Name)
			ref.Name = name
			key = "skill:" + name
		case ref.ConfigMapRef != nil:
			cmName := strings.TrimSpace(ref.ConfigMapRef.Name)
			cmKey := strings.TrimSpace(ref.ConfigMapRef.Key)
			ref.ConfigMapRef = &corev1alpha1.ConfigMapKeySelector{Name: cmName, Key: cmKey}
			key = "configmap:" + cmName + "/" + cmKey
		}
		if key == "" {
			continue
		}
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		deduped = append(deduped, ref)
	}
	return deduped
}

// harnessWrapperBaseTurnEnv copies literal Task env and resolves supported valueFrom
// entries after validateTaskAgentCompatibility has rejected secret-looking task env
// names. Runtime credentials are resolved separately from Agent/Task SecretRefs and
// are never persisted in annotations.
func (r *TaskReconciler) harnessWrapperBaseTurnEnv(ctx context.Context, task *corev1alpha1.Task) ([]harness.TurnEnvVar, error) {
	if task == nil {
		return nil, nil
	}
	env := make([]harness.TurnEnvVar, 0, len(task.Spec.Env)+4)
	for _, item := range task.Spec.Env {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			continue
		}
		if taskRequestsReadOnlyAgent(task) && harnessWrapperReadOnlyEnvBlocked(name) {
			continue
		}
		value := item.Value
		if item.ValueFrom != nil {
			resolved, present, err := r.resolveHarnessWrapperTaskEnvValueFrom(ctx, task, item)
			if err != nil {
				return nil, err
			}
			if !present {
				continue
			}
			value = resolved
		}
		env = append(env, harness.TurnEnvVar{Name: item.Name, Value: value})
	}
	if r != nil && r.JobBuilder != nil && strings.TrimSpace(r.JobBuilder.ControllerURL) != "" {
		controllerURL := strings.TrimSpace(r.JobBuilder.ControllerURL)
		env = append(env,
			harness.TurnEnvVar{Name: workerenv.ControllerURL, Value: controllerURL},
			harness.TurnEnvVar{
				Name:  workerenv.ResultEndpoint,
				Value: fmt.Sprintf("%s/internal/v1/results/%s/%s", controllerURL, task.Namespace, task.Name),
			},
		)
	}
	if task.Spec.PriorTaskRef != nil {
		env = append(env, harness.TurnEnvVar{Name: workerenv.PriorTask, Value: task.Spec.PriorTaskRef.Name})
		priorNS := task.Spec.PriorTaskRef.Namespace
		if strings.TrimSpace(priorNS) == "" {
			priorNS = task.Namespace
		}
		env = append(env, harness.TurnEnvVar{Name: workerenv.PriorTaskNamespace, Value: priorNS})
	}
	if task.Annotations != nil {
		if traceparent := strings.TrimSpace(task.Annotations[labels.AnnotationTraceParent]); traceparent != "" {
			env = setHarnessTurnEnv(env, workerenv.TraceParent, traceparent)
		}
	}
	if parentTask := labels.ParentTaskName(task.Labels, task.Annotations); parentTask != "" {
		env = append(env, harness.TurnEnvVar{Name: workerenv.ParentTask, Value: parentTask})
	}
	if taskRequestsReadOnlyAgent(task) {
		env = setHarnessTurnEnv(env, workerenv.AgentReadOnly, scheduledRunLabelValue)
		env = setHarnessTurnEnv(env, workerenv.ResultStdout, scheduledRunLabelValue)
	}
	return env, nil
}

// harnessWrapperTurnEnv intentionally does not accept a Provider: type: agent tasks with
// Agent.providerRef are rejected by validateTaskAgentCompatibility. CLI runtime
// credentials come from Agent.spec.secretRef and Task.spec.secretRef.
func (r *TaskReconciler) harnessWrapperTurnEnv(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) ([]harness.TurnEnvVar, error) {
	env, err := r.harnessWrapperBaseTurnEnv(ctx, task)
	if err != nil {
		return nil, err
	}
	workspaceGitEnv, err := r.harnessWrapperWorkspaceGitSecretEnv(ctx, task)
	if err != nil {
		return nil, err
	}
	agentSecretEnv, err := r.harnessWrapperAgentSecretEnv(ctx, task, agent)
	if err != nil {
		return nil, err
	}
	env = append(env, agentSecretEnv...)
	if !taskRequestsReadOnlyAgent(task) {
		taskSecretEnv, err := r.harnessWrapperTaskSecretEnv(ctx, task)
		if err != nil {
			return nil, err
		}
		env = append(env, taskSecretEnv...)
	}
	// Workspace credentials are used by root-side clone/fetch/push preparation and
	// must remain authoritative over broad runtime Secret keys such as GIT_TOKEN.
	env = append(env, workspaceGitEnv...)
	return env, nil
}

func (r *TaskReconciler) harnessWrapperWorkspaceGitSecretEnv(
	ctx context.Context,
	task *corev1alpha1.Task,
) ([]harness.TurnEnvVar, error) {
	ws := effectiveWorkspace(task)
	if ws == nil || ws.GitSecretRef == nil || strings.TrimSpace(ws.GitSecretRef.Name) == "" {
		return nil, nil
	}
	secret := &corev1.Secret{}
	key := ctrlclient.ObjectKey{Name: ws.GitSecretRef.Name, Namespace: task.Namespace}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("resolve harness runtime git credential Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	var token string
	for _, key := range []string{"token", "password", workerenv.GitHubToken} {
		if value := strings.TrimSpace(string(secret.Data[key])); value != "" {
			token = value
			break
		}
	}
	if token == "" {
		return nil, fmt.Errorf("workspace git secret %q must contain token, password, or %s", ws.GitSecretRef.Name, workerenv.GitHubToken)
	}
	env := []harness.TurnEnvVar{
		{Name: workerenv.GitToken, Value: token},
		{Name: workerenv.GitAskpass, Value: "/bin/echo-token"},
	}
	if username := strings.TrimSpace(string(secret.Data["username"])); username != "" {
		env = append(env, harness.TurnEnvVar{Name: workerenv.GitUsername, Value: username})
	}
	return env, nil
}

func (r *TaskReconciler) harnessWrapperAgentSecretEnv(
	ctx context.Context,
	task *corev1alpha1.Task,
	agent *corev1alpha1.Agent,
) ([]harness.TurnEnvVar, error) {
	if agent == nil || agent.Spec.SecretRef == nil || strings.TrimSpace(agent.Spec.SecretRef.Name) == "" {
		return nil, nil
	}
	env, err := r.harnessWrapperSecretEnv(ctx, ctrlclient.ObjectKey{Name: agent.Spec.SecretRef.Name, Namespace: task.Namespace})
	if err != nil {
		return nil, err
	}
	if !taskRequestsReadOnlyAgent(task) {
		return env, nil
	}
	allowedKeys, err := readOnlyAgentRuntimeSecretKeys(agent)
	if err != nil {
		return nil, err
	}
	return filterHarnessTurnEnv(env, allowedKeys), nil
}

func (r *TaskReconciler) harnessWrapperSecretEnv(
	ctx context.Context,
	key ctrlclient.ObjectKey,
) ([]harness.TurnEnvVar, error) {
	secret := &corev1.Secret{}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, fmt.Errorf("resolve harness runtime credential Secret %s/%s: %w", key.Namespace, key.Name, err)
	}
	env := make([]harness.TurnEnvVar, 0, len(secret.Data))
	for name, raw := range secret.Data {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !harnessWrapperEnvNameValid(name) {
			continue
		}
		if harnessWrapperPrivateEnvName(name) {
			return nil, fmt.Errorf(
				"harness runtime credential Secret %s/%s key %q is reserved for wrapper configuration",
				key.Namespace,
				key.Name,
				name,
			)
		}
		if harnessWrapperRootEnvNameBlocked(name) && !harnessWrapperRuntimeSecretEnvAllowed(name) {
			return nil, fmt.Errorf(
				"harness runtime credential Secret %s/%s key %q is reserved for controller-managed runtime configuration",
				key.Namespace,
				key.Name,
				name,
			)
		}
		if len(raw) == 0 {
			continue
		}
		env = append(env, harness.TurnEnvVar{Name: name, Value: string(raw)})
	}
	return env, nil
}

func (r *TaskReconciler) harnessWrapperTaskSecretEnv(
	ctx context.Context,
	task *corev1alpha1.Task,
) ([]harness.TurnEnvVar, error) {
	if task == nil || task.Spec.SecretRef == nil || strings.TrimSpace(task.Spec.SecretRef.Name) == "" {
		return nil, nil
	}
	namespace := strings.TrimSpace(task.Spec.SecretRef.Namespace)
	if namespace == "" {
		namespace = task.Namespace
	}
	if namespace != task.Namespace {
		return nil, fmt.Errorf("task secretRef namespace %q does not match task namespace %q", namespace, task.Namespace)
	}
	return r.harnessWrapperSecretEnv(ctx, ctrlclient.ObjectKey{Name: task.Spec.SecretRef.Name, Namespace: namespace})
}

func filterHarnessTurnEnv(env []harness.TurnEnvVar, allowedKeys []string) []harness.TurnEnvVar {
	if len(env) == 0 || len(allowedKeys) == 0 {
		return nil
	}
	allowed := make(map[string]struct{}, len(allowedKeys))
	for _, key := range allowedKeys {
		allowed[strings.TrimSpace(key)] = struct{}{}
	}
	out := make([]harness.TurnEnvVar, 0, len(env))
	for _, item := range env {
		if _, ok := allowed[item.Name]; ok {
			out = append(out, item)
		}
	}
	return out
}

func setHarnessTurnEnv(env []harness.TurnEnvVar, name, value string) []harness.TurnEnvVar {
	for i, item := range env {
		if item.Name == name {
			env[i].Value = value
			return env
		}
	}
	return append(env, harness.TurnEnvVar{Name: name, Value: value})
}

func harnessWrapperReadOnlyEnvBlocked(name string) bool {
	switch strings.TrimSpace(name) {
	case workerenv.AgentReadOnly,
		workerenv.ResultStdout,
		workerenv.AllowBash,
		workerenv.AllowedTools,
		workerenv.DisallowedTools,
		workerenv.ClaudeBare,
		workerenv.ClaudeDisableSettingSources,
		workerenv.ClaudePermissionMode,
		workerenv.CodexDisableSandbox,
		workerenv.CodexSandboxMode:
		return true
	default:
		return false
	}
}

func (r *TaskReconciler) resolveHarnessWrapperTaskEnvValueFrom(
	ctx context.Context,
	task *corev1alpha1.Task,
	item corev1.EnvVar,
) (string, bool, error) {
	if item.ValueFrom == nil {
		return item.Value, true, nil
	}
	name := strings.TrimSpace(item.Name)
	source := item.ValueFrom
	switch {
	case source.FieldRef != nil:
		switch strings.TrimSpace(source.FieldRef.FieldPath) {
		case "metadata.name":
			return task.Name, true, nil
		case "metadata.namespace":
			return task.Namespace, true, nil
		case "metadata.uid":
			return string(task.UID), true, nil
		default:
			return "", false, fmt.Errorf("task env %q fieldRef %q is not supported by harness runtime", name, source.FieldRef.FieldPath)
		}
	case source.ConfigMapKeyRef != nil:
		return r.resolveHarnessWrapperConfigMapEnv(ctx, task, name, source.ConfigMapKeyRef)
	default:
		return "", false, fmt.Errorf("task env %q uses unsupported valueFrom source for harness runtime", name)
	}
}

func (r *TaskReconciler) resolveHarnessWrapperConfigMapEnv(
	ctx context.Context,
	task *corev1alpha1.Task,
	envName string,
	ref *corev1.ConfigMapKeySelector,
) (string, bool, error) {
	if ref == nil || strings.TrimSpace(ref.Name) == "" || strings.TrimSpace(ref.Key) == "" {
		return "", false, fmt.Errorf("task env %q configMapKeyRef name and key are required", envName)
	}
	cm := &corev1.ConfigMap{}
	key := ctrlclient.ObjectKey{Name: strings.TrimSpace(ref.Name), Namespace: task.Namespace}
	if err := r.Get(ctx, key, cm); err != nil {
		if ref.Optional != nil && *ref.Optional && apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolve task env %q ConfigMap %s/%s: %w", envName, key.Namespace, key.Name, err)
	}
	value, ok := cm.Data[ref.Key]
	if !ok {
		if ref.Optional != nil && *ref.Optional {
			return "", false, nil
		}
		return "", false, fmt.Errorf("resolve task env %q ConfigMap %s/%s key %q: not found", envName, key.Namespace, key.Name, ref.Key)
	}
	return value, true, nil
}

func harnessWrapperTaskEnvValueFromSupported(source *corev1.EnvVarSource) bool {
	if source == nil {
		return true
	}
	if source.SecretKeyRef != nil || source.ResourceFieldRef != nil {
		return false
	}
	count := 0
	if source.FieldRef != nil {
		count++
	}
	if source.ConfigMapKeyRef != nil {
		count++
	}
	return count == 1
}

func validateHarnessWrapperTaskEnv(env []corev1.EnvVar) error {
	for i, item := range env {
		name := strings.TrimSpace(item.Name)
		if name == "" {
			return fmt.Errorf("task env %d name is required", i)
		}
		if !harnessWrapperEnvNameValid(name) {
			return fmt.Errorf("task env %q has an invalid name", name)
		}
		if item.ValueFrom != nil && !harnessWrapperTaskEnvValueFromSupported(item.ValueFrom) {
			return fmt.Errorf("task env %q uses unsupported valueFrom source for harness runtime", name)
		}
		if harnessWrapperPrivateEnvName(name) || harnessWrapperRootEnvNameBlocked(name) || harnessWrapperEnvNameLooksSecret(name) {
			return fmt.Errorf("task env %q is not supported by harness runtime", name)
		}
	}
	return nil
}

func harnessWrapperPrivateEnvName(name string) bool {
	return strings.HasPrefix(strings.TrimSpace(name), "ORKA_HARNESS_WRAPPER_")
}

func harnessWrapperRootEnvNameBlocked(name string) bool {
	switch strings.ToUpper(strings.TrimSpace(name)) {
	case workerenv.ControllerURL,
		workerenv.ResultEndpoint,
		workerenv.GitAskpass,
		workerenv.OpenAIBaseURL,
		workerenv.ServiceAccountToken,
		workerenv.ServiceAccountTokenPath,
		"ORKA_ARTIFACTS_DIR",
		"HTTP_PROXY",
		"HTTPS_PROXY",
		"ALL_PROXY",
		"NO_PROXY":
		return true
	default:
		return false
	}
}

func harnessWrapperEnvNameValid(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r == '_', r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
		case i > 0 && r >= '0' && r <= '9':
		default:
			return false
		}
	}
	return true
}

func harnessWrapperRuntimeSecretEnvAllowed(name string) bool {
	switch strings.TrimSpace(name) {
	case workerenv.OpenAIBaseURL, workerenv.AnthropicBaseURL, workerenv.AIBaseURL:
		return true
	default:
		return false
	}
}

func harnessWrapperEnvNameLooksSecret(name string) bool {
	upper := strings.ToUpper(strings.TrimSpace(name))
	for _, marker := range []string{"TOKEN", "SECRET", "PASSWORD", "PASSWD", "API_KEY", "ACCESS_KEY", "PRIVATE_KEY", "CREDENTIAL", "AUTHORIZATION"} {
		if strings.Contains(upper, marker) {
			return true
		}
	}
	return false
}

func (r *TaskReconciler) resolveHarnessWrapperConfigMapValue(
	ctx context.Context,
	namespace string,
	ref *corev1alpha1.ConfigMapKeySelector,
) (string, error) {
	if ref == nil {
		return "", nil
	}
	cm := &corev1.ConfigMap{}
	if err := r.Get(ctx, ctrlclient.ObjectKey{Name: ref.Name, Namespace: namespace}, cm); err != nil {
		return "", fmt.Errorf("resolve harness runtime system prompt ConfigMap %s/%s: %w", namespace, ref.Name, err)
	}
	value, ok := cm.Data[ref.Key]
	if !ok {
		return "", fmt.Errorf("resolve harness runtime system prompt ConfigMap %s/%s key %q: key not found", namespace, ref.Name, ref.Key)
	}
	return value, nil
}

func harnessWrapperTurnID(task *corev1alpha1.Task, attempts int32) harness.HarnessTurnID {
	identity := fmt.Sprintf("%s/%s/%s/%d", task.Namespace, task.Name, task.UID, attempts)
	sum := sha256.Sum256([]byte(identity))
	prefix := harnessWrapperTurnIDPrefix(task.Name)
	return harness.HarnessTurnID(fmt.Sprintf("%s-%s-%d", prefix, hex.EncodeToString(sum[:])[:12], attempts))
}

func harnessWrapperTurnIDPrefix(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '-', r == '_', r == '.':
			out.WriteRune(r)
		default:
			out.WriteByte('-')
		}
		if out.Len() >= 40 {
			break
		}
	}
	prefix := strings.Trim(out.String(), "-_.")
	if prefix == "" {
		return "turn"
	}
	return prefix
}

func harnessWrapperRuntimeSessionIdentity(task *corev1alpha1.Task, agent *corev1alpha1.Agent, runtimeName string) harness.RuntimeSessionIdentity {
	input := harness.RuntimeSessionIdentityInput{
		Namespace:   "default",
		SessionName: "default",
		RuntimeName: runtimeName,
		Provider:    harness.ProviderKindKubernetesService,
	}
	if task != nil {
		input.Namespace = task.Namespace
		input.TaskName = task.Name
		input.TaskUID = string(task.UID)
		input.ActiveTask = task.Name
		input.SessionName = ""
		if task.Spec.SessionRef != nil {
			input.SessionName = task.Spec.SessionRef.Name
		}
	}
	if agent != nil {
		input.AgentName = agent.Name
	}
	return harness.ResolveRuntimeSessionIdentity(input)
}

func harnessWrapperRuntimeSessionID(task *corev1alpha1.Task, runtimeName string) harness.RuntimeSessionID {
	return harnessWrapperRuntimeSessionIdentity(task, nil, runtimeName).ID
}

func harnessWrapperSessionName(task *corev1alpha1.Task) string {
	if task != nil && task.Spec.SessionRef != nil && strings.TrimSpace(task.Spec.SessionRef.Name) != "" {
		return strings.TrimSpace(task.Spec.SessionRef.Name)
	}
	if task != nil {
		return task.Name
	}
	return "default"
}

func harnessWrapperTaskAgentName(task *corev1alpha1.Task) string {
	if task != nil && task.Spec.AgentRef != nil {
		return task.Spec.AgentRef.Name
	}
	return ""
}

func harnessWrapperLastFrameSeq(task *corev1alpha1.Task) int64 {
	if task == nil || task.Annotations == nil {
		return 0
	}
	seq, err := strconv.ParseInt(strings.TrimSpace(task.Annotations[harnessWrapperLastFrameSeqAnno]), 10, 64)
	if err != nil || seq < 0 {
		return 0
	}
	return seq
}

func (r *TaskReconciler) patchHarnessWrapperLastFrameSeq(ctx context.Context, task *corev1alpha1.Task, seq int64) error {
	patch := ctrlclient.MergeFrom(task.DeepCopy())
	if task.Annotations == nil {
		task.Annotations = map[string]string{}
	}
	task.Annotations[harnessWrapperLastFrameSeqAnno] = strconv.FormatInt(seq, 10)
	return r.Patch(ctx, task, patch)
}
