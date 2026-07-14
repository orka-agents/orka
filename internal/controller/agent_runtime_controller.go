/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"net"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/intstr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/harness/conformance"
)

var (
	agentRuntimeAllowInsecureLoopbackForTests bool
	agentRuntimeClusterDomainForTests         string
	agentRuntimeClusterDomainOnce             sync.Once
	agentRuntimeClusterDomainValue            string
	errAgentRuntimePinnedBackendLost          = errors.New("pinned AgentRuntime backend lost")
)

const (
	agentRuntimeReadyCondition = "Ready"
	agentRuntimeReasonReady    = "ConformancePassed"
	agentRuntimeReasonNotReady = "ConformanceFailed"
	agentRuntimeProbeTimeout   = 60 * time.Second
	agentRuntimeRequeue        = 30 * time.Second

	agentRuntimeAuthUseLabel           = "orka.ai/agent-runtime-auth"
	agentRuntimeAuthRefNameLabel       = "orka.ai/agent-runtime-name"
	agentRuntimeAuthEndpointAnnotation = "orka.ai/agent-runtime-endpoint"
	agentRuntimeTransportMigrationAnno = "orka.ai/transport-security-migration"
	agentRuntimeTransportMigrationV1   = "legacy-v1"
)

// AgentRuntimeReconciler reconciles AgentRuntime registry entries.
type AgentRuntimeReconciler struct {
	client.Client
	APIReader client.Reader
	Scheme    *k8sruntime.Scheme
}

// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.orka.ai,resources=agentruntimes/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

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
	backfilled, err := r.backfillAgentRuntimeTransportSecurity(ctx, runtime)
	if err != nil {
		return ctrl.Result{}, err
	}
	if backfilled {
		logger.Info("Backfilled AgentRuntime transport security",
			"agentRuntime", runtime.Name,
			"transportSecurity", runtime.Spec.Deployment.TransportSecurity,
		)
		return ctrl.Result{RequeueAfter: time.Nanosecond}, nil
	}
	observed, ready, authRefResourceVersion, message := r.probeAgentRuntime(ctx, runtime)
	return r.updateAgentRuntimeStatus(ctx, runtime, ready, observed, authRefResourceVersion, message)
}

func (r *AgentRuntimeReconciler) backfillAgentRuntimeTransportSecurity(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
) (bool, error) {
	if runtime == nil || !runtime.DeletionTimestamp.IsZero() ||
		strings.TrimSpace(runtime.Annotations[agentRuntimeTransportMigrationAnno]) != agentRuntimeTransportMigrationV1 {
		return false, nil
	}
	before := runtime.DeepCopy()
	if runtime.Spec.Deployment.TransportSecurity != "" {
		delete(runtime.Annotations, agentRuntimeTransportMigrationAnno)
		if err := r.Patch(ctx, runtime, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, fmt.Errorf("clear AgentRuntime transportSecurity migration marker: %w", err)
		}
		return true, nil
	}
	security := agentRuntimeSpecTransportSecurity(runtime)
	_, _, err := validateAgentRuntimeTransportSecurity(runtime.Spec.Deployment.Endpoint, security)
	if err != nil {
		return false, nil
	}
	if security == corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP {
		if err := validateAgentRuntimeTransportPolicy(
			ctx,
			r.apiReader(),
			runtime.Namespace,
			runtime.Spec.Deployment.Endpoint,
			security,
		); err != nil {
			return false, nil
		}
	}

	runtime.Spec.Deployment.TransportSecurity = security
	delete(runtime.Annotations, agentRuntimeTransportMigrationAnno)
	if err := r.Patch(ctx, runtime, client.MergeFromWithOptions(before, client.MergeFromWithOptimisticLock{})); err != nil {
		return false, fmt.Errorf("backfill AgentRuntime transportSecurity: %w", err)
	}
	return true, nil
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
	transportSecurity := agentRuntimeSpecTransportSecurity(runtime)
	probeBaseURL, transportSecurity, err := agentRuntimeRequestEndpoint(
		runtime.Spec.Deployment.Endpoint,
		transportSecurity,
	)
	if err != nil {
		return nil, false, "", err.Error()
	}
	token, authRefResourceVersion, err := r.agentRuntimeBearerToken(ctx, runtime)
	if err != nil {
		return nil, false, "", err.Error()
	}
	insecureHTTP := transportSecurity == corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP
	dialAddress, err := resolveAgentRuntimeInsecureDialAddress(
		ctx,
		r.apiReader(),
		runtime.Namespace,
		runtime.Spec.Deployment.Endpoint,
		transportSecurity,
		runtime.Namespace+"/"+runtime.Name+"/"+strconv.FormatInt(runtime.Generation, 10),
	)
	if err != nil {
		return nil, false, authRefResourceVersion, err.Error()
	}
	probeHTTPClient := harness.NewAgentRuntimeHTTPClient(insecureHTTP)
	if insecureHTTP && dialAddress != "" {
		probeHTTPClient, err = harness.NewPinnedAgentRuntimeHTTPClient(dialAddress)
		if err != nil {
			return nil, false, authRefResourceVersion, err.Error()
		}
	}
	probeCtx, cancel := context.WithTimeout(ctx, agentRuntimeProbeTimeout)
	defer cancel()
	deepProbe := runtime.Status.ObservedGeneration != runtime.Generation || !runtime.Status.Ready ||
		runtime.Status.ObservedAuthRefResourceVersion != authRefResourceVersion
	// conformance.Check uses a tokenless client for health/capabilities and applies
	// BearerToken only to authenticated turn and turn-resource probes.
	preflight := conformance.Check(probeCtx, conformance.Target{
		BaseURL:        probeBaseURL,
		BearerToken:    token,
		HTTPClient:     probeHTTPClient,
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
					BaseURL:           probeBaseURL,
					BearerToken:       token,
					HTTPClient:        probeHTTPClient,
					ControlTimeout:    agentRuntimeProbeTimeout,
					ProbeBrokeredRead: true,
					RequireAuth:       true,
				})
				if !brokeredReadProbe.Passed {
					return observed, false, authRefResourceVersion, sanitizeAgentRuntimeStatusMessage(brokeredReadProbe.Message)
				}
			case corev1alpha1.AgentRuntimeBrokeredToolClassWrite:
				brokeredWriteProbe := conformance.Check(probeCtx, conformance.Target{
					BaseURL:            probeBaseURL,
					BearerToken:        token,
					HTTPClient:         probeHTTPClient,
					ControlTimeout:     agentRuntimeProbeTimeout,
					ProbeBrokeredWrite: true,
					RequireAuth:        true,
				})
				if !brokeredWriteProbe.Passed {
					return observed, false, authRefResourceVersion, sanitizeAgentRuntimeStatusMessage(brokeredWriteProbe.Message)
				}
			case corev1alpha1.AgentRuntimeBrokeredToolClassCoordination:
				brokeredCoordinationProbe := conformance.Check(probeCtx, conformance.Target{
					BaseURL:                   probeBaseURL,
					BearerToken:               token,
					HTTPClient:                probeHTTPClient,
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
			BaseURL:        probeBaseURL,
			BearerToken:    token,
			HTTPClient:     probeHTTPClient,
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
	parsed, _, err := validateAgentRuntimeTransportSecurity(endpoint, agentRuntimeSpecTransportSecurity(runtime))
	if err != nil {
		return err
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
	if runtime == nil {
		return fmt.Errorf("AgentRuntime is required")
	}
	return validateAgentRuntimeTransportPolicy(
		ctx,
		r.apiReader(),
		runtime.Namespace,
		runtime.Spec.Deployment.Endpoint,
		agentRuntimeSpecTransportSecurity(runtime),
	)
}

func (r *AgentRuntimeReconciler) apiReader() client.Reader {
	if r != nil && r.APIReader != nil {
		return r.APIReader
	}
	if r == nil {
		return nil
	}
	return r.Client
}

func effectiveAgentRuntimeTransportSecurity(security corev1alpha1.AgentRuntimeTransportSecurity) corev1alpha1.AgentRuntimeTransportSecurity {
	if security == "" {
		return corev1alpha1.AgentRuntimeTransportSecurityTLS
	}
	return security
}

func agentRuntimeNeedsTransportMigration(runtime *corev1alpha1.AgentRuntime) bool {
	return runtime != nil && runtime.Spec.Deployment.TransportSecurity == "" &&
		strings.TrimSpace(runtime.Annotations[agentRuntimeTransportMigrationAnno]) == agentRuntimeTransportMigrationV1
}

func agentRuntimeSpecTransportSecurity(runtime *corev1alpha1.AgentRuntime) corev1alpha1.AgentRuntimeTransportSecurity {
	if runtime == nil || runtime.Spec.Deployment.TransportSecurity != "" {
		if runtime == nil {
			return corev1alpha1.AgentRuntimeTransportSecurityTLS
		}
		return runtime.Spec.Deployment.TransportSecurity
	}
	if !agentRuntimeNeedsTransportMigration(runtime) {
		return corev1alpha1.AgentRuntimeTransportSecurityTLS
	}
	parsed, err := url.Parse(strings.TrimSpace(runtime.Spec.Deployment.Endpoint))
	if err == nil && parsed.Scheme == urlSchemeHTTP {
		return corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP
	}
	return corev1alpha1.AgentRuntimeTransportSecurityTLS
}

func validateAgentRuntimeTransportSecurity(
	endpoint string,
	security corev1alpha1.AgentRuntimeTransportSecurity,
) (*url.URL, corev1alpha1.AgentRuntimeTransportSecurity, error) {
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return nil, "", fmt.Errorf("deployment.endpoint must be an absolute http(s) URL")
	}
	if parsed.Scheme != urlSchemeHTTP && parsed.Scheme != urlSchemeHTTPS {
		return nil, "", fmt.Errorf("deployment.endpoint scheme %q is not supported", parsed.Scheme)
	}
	security = effectiveAgentRuntimeTransportSecurity(security)
	switch security {
	case corev1alpha1.AgentRuntimeTransportSecurityTLS:
		if parsed.Scheme != urlSchemeHTTPS {
			return nil, "", fmt.Errorf("deployment.endpoint must use https when deployment.transportSecurity is %q", security)
		}
	case corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP:
		if parsed.Scheme != urlSchemeHTTP {
			return nil, "", fmt.Errorf("deployment.endpoint must use http when deployment.transportSecurity is %q", security)
		}
	default:
		return nil, "", fmt.Errorf("unsupported deployment.transportSecurity %q", security)
	}
	return parsed, security, nil
}

func agentRuntimeTransportRequiresPinnedBackend(
	endpoint string,
	security corev1alpha1.AgentRuntimeTransportSecurity,
) bool {
	parsed, security, err := validateAgentRuntimeTransportSecurity(endpoint, security)
	if err != nil || security != corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP {
		return false
	}
	return !isLoopbackAgentRuntimeEndpoint(parsed.Hostname()) || !agentRuntimeAllowInsecureLoopbackForTests
}

func agentRuntimeRequestEndpoint(
	endpoint string,
	security corev1alpha1.AgentRuntimeTransportSecurity,
) (string, corev1alpha1.AgentRuntimeTransportSecurity, error) {
	parsed, security, err := validateAgentRuntimeTransportSecurity(endpoint, security)
	if err != nil {
		return "", "", err
	}
	if security == corev1alpha1.AgentRuntimeTransportSecurityTLS {
		return parsed.String(), security, nil
	}
	if isLoopbackAgentRuntimeEndpoint(parsed.Hostname()) && agentRuntimeAllowInsecureLoopbackForTests {
		return parsed.String(), security, nil
	}
	if _, _, ok := parseAgentRuntimeServiceNamespaceHost(parsed.Hostname()); !ok {
		return "", "", fmt.Errorf("deployment.transportSecurity %q requires deployment.endpoint to reference a Kubernetes Service", security)
	}
	// Preserve the configured authority for the HTTP Host header. In insecure
	// mode NewAgentRuntimeHTTPClient roots the DNS name in its DialContext, so
	// resolver search domains cannot redirect the validated Service hostname.
	return parsed.String(), security, nil
}

func validateAgentRuntimeTransportPolicy(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	endpoint string,
	security corev1alpha1.AgentRuntimeTransportSecurity,
) error {
	parsed, security, err := validateAgentRuntimeTransportSecurity(endpoint, security)
	if err != nil {
		return err
	}
	if security == corev1alpha1.AgentRuntimeTransportSecurityTLS {
		return nil
	}
	host := parsed.Hostname()
	if isLoopbackAgentRuntimeEndpoint(host) && agentRuntimeAllowInsecureLoopbackForTests {
		return nil
	}
	serviceName, serviceNamespace, ok := parseAgentRuntimeServiceNamespaceHost(host)
	if !ok || serviceNamespace != namespace {
		return fmt.Errorf("deployment.transportSecurity %q requires deployment.endpoint to reference a same-namespace Kubernetes Service", security)
	}
	if reader == nil {
		return fmt.Errorf("deployment.transportSecurity %q requires Kubernetes Service validation", security)
	}
	service := &corev1.Service{}
	if err := reader.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: serviceNamespace}, service); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("deployment.transportSecurity %q requires Service %q in namespace %q to exist", security, serviceName, serviceNamespace)
		}
		return fmt.Errorf("read Service %q in namespace %q for deployment.transportSecurity %q: %w", serviceName, serviceNamespace, security, err)
	}
	if service.Spec.Type == corev1.ServiceTypeExternalName {
		return fmt.Errorf("deployment.transportSecurity %q does not allow ExternalName Service %q", security, serviceName)
	}
	if len(service.Spec.Selector) == 0 {
		return fmt.Errorf("deployment.transportSecurity %q requires Service %q to have a non-empty selector", security, serviceName)
	}
	return nil
}

type agentRuntimeBackend struct {
	PodName     string
	PodUID      string
	DialAddress string
}

func resolveAgentRuntimeInsecureDialAddress(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	endpoint string,
	security corev1alpha1.AgentRuntimeTransportSecurity,
	selectionKey string,
) (string, error) {
	backend, err := resolveAgentRuntimeInsecureBackend(ctx, reader, namespace, endpoint, security, selectionKey)
	if err != nil {
		return "", err
	}
	return backend.DialAddress, nil
}

func resolveAgentRuntimeInsecureBackend(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	endpoint string,
	security corev1alpha1.AgentRuntimeTransportSecurity,
	selectionKey string,
) (agentRuntimeBackend, error) {
	candidates, err := agentRuntimeInsecureBackendCandidates(ctx, reader, namespace, endpoint, security)
	if err != nil {
		return agentRuntimeBackend{}, err
	}
	if len(candidates) == 0 {
		return agentRuntimeBackend{}, nil
	}
	slices.SortFunc(candidates, func(left, right agentRuntimeBackend) int {
		if comparison := strings.Compare(left.DialAddress, right.DialAddress); comparison != 0 {
			return comparison
		}
		if comparison := strings.Compare(left.PodName, right.PodName); comparison != 0 {
			return comparison
		}
		return strings.Compare(left.PodUID, right.PodUID)
	})
	if strings.TrimSpace(selectionKey) == "" {
		selectionKey = namespace + "/" + endpoint
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(selectionKey))
	return candidates[hash.Sum64()%uint64(len(candidates))], nil
}

func validateAgentRuntimeInsecureBackend(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	endpoint string,
	security corev1alpha1.AgentRuntimeTransportSecurity,
	expected agentRuntimeBackend,
	requireReady bool,
) (agentRuntimeBackend, error) {
	parsed, security, err := validateAgentRuntimeTransportSecurity(endpoint, security)
	if err != nil {
		return agentRuntimeBackend{}, err
	}
	if security != corev1alpha1.AgentRuntimeTransportSecurityInsecureClusterLocalHTTP ||
		(isLoopbackAgentRuntimeEndpoint(parsed.Hostname()) && agentRuntimeAllowInsecureLoopbackForTests) {
		return agentRuntimeBackend{}, nil
	}
	if reader == nil || expected.PodName == "" || expected.PodUID == "" || expected.DialAddress == "" {
		return agentRuntimeBackend{}, fmt.Errorf("pinned AgentRuntime backend identity is incomplete")
	}
	pod, err := validateAgentRuntimePinnedPodIdentity(ctx, reader, namespace, expected)
	if err != nil {
		return agentRuntimeBackend{}, err
	}
	serviceName, serviceNamespace, ok := parseAgentRuntimeServiceNamespaceHost(parsed.Hostname())
	if !ok || serviceNamespace != namespace {
		return agentRuntimeBackend{}, fmt.Errorf("deployment.endpoint no longer references the pinned backend Service")
	}
	service := &corev1.Service{}
	if err := reader.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: serviceNamespace}, service); err != nil {
		return agentRuntimeBackend{}, fmt.Errorf("read Service %q for pinned AgentRuntime backend: %w", serviceName, err)
	}
	if service.Spec.Type == corev1.ServiceTypeExternalName || len(service.Spec.Selector) == 0 {
		return agentRuntimeBackend{}, fmt.Errorf("service %q is no longer a selector-backed non-ExternalName Service", serviceName)
	}
	if !agentRuntimePodMatchesSelector(pod, service.Spec.Selector) {
		return agentRuntimeBackend{}, fmt.Errorf("pinned AgentRuntime Pod %q is no longer selected by the Service", expected.PodName)
	}
	if requireReady && !agentRuntimePodReady(pod) {
		return agentRuntimeBackend{}, fmt.Errorf("pinned AgentRuntime Pod %q is not Ready for turn start", expected.PodName)
	}
	if !requireReady && (pod.Spec.HostNetwork || pod.Status.Phase != corev1.PodRunning) {
		return agentRuntimeBackend{}, fmt.Errorf("pinned AgentRuntime Pod %q is not a Running selector-matched backend", expected.PodName)
	}
	servicePort, err := agentRuntimeServicePort(parsed, service)
	if err != nil {
		return agentRuntimeBackend{}, err
	}
	targetPort, ok := agentRuntimePodTargetPort(pod, servicePort)
	if !ok {
		return agentRuntimeBackend{}, fmt.Errorf("pinned AgentRuntime Pod %q no longer resolves the Service target port", expected.PodName)
	}
	podIP, ok := agentRuntimePodIPForService(pod, service)
	if !ok {
		return agentRuntimeBackend{}, fmt.Errorf("pinned AgentRuntime Pod %q has no IP matching the Service IP families", expected.PodName)
	}
	current := agentRuntimeBackend{
		PodName:     pod.Name,
		PodUID:      string(pod.UID),
		DialAddress: net.JoinHostPort(podIP.String(), strconv.Itoa(int(targetPort))),
	}
	if current.DialAddress != expected.DialAddress {
		return agentRuntimeBackend{}, fmt.Errorf("%w: Pod %q address changed from %q to %q", errAgentRuntimePinnedBackendLost, expected.PodName, expected.DialAddress, current.DialAddress)
	}
	return current, nil
}

func validateAgentRuntimePinnedPodIdentity(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	expected agentRuntimeBackend,
) (*corev1.Pod, error) {
	if reader == nil || expected.PodName == "" || expected.PodUID == "" || expected.DialAddress == "" {
		return nil, fmt.Errorf("pinned AgentRuntime backend identity is incomplete")
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKey{Name: expected.PodName, Namespace: namespace}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: Pod %q no longer exists", errAgentRuntimePinnedBackendLost, expected.PodName)
		}
		return nil, fmt.Errorf("read pinned AgentRuntime Pod %q: %w", expected.PodName, err)
	}
	if string(pod.UID) != expected.PodUID {
		return nil, fmt.Errorf("%w: Pod %q UID changed from %q to %q", errAgentRuntimePinnedBackendLost, expected.PodName, expected.PodUID, pod.UID)
	}
	if !pod.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("%w: Pod %q is terminating", errAgentRuntimePinnedBackendLost, expected.PodName)
	}
	if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
		return nil, fmt.Errorf("%w: Pod %q reached terminal phase %q", errAgentRuntimePinnedBackendLost, expected.PodName, pod.Status.Phase)
	}
	expectedHost, _, err := net.SplitHostPort(expected.DialAddress)
	if err != nil {
		return nil, fmt.Errorf("pinned AgentRuntime backend address %q is invalid: %w", expected.DialAddress, err)
	}
	expectedIP := net.ParseIP(strings.Trim(expectedHost, "[]"))
	if expectedIP == nil || !agentRuntimePodHasIP(pod, expectedIP) {
		return nil, fmt.Errorf("%w: Pod %q no longer has pinned IP %q", errAgentRuntimePinnedBackendLost, expected.PodName, expectedHost)
	}
	return pod, nil
}

func agentRuntimePodMatchesSelector(pod *corev1.Pod, selector map[string]string) bool {
	if pod == nil || len(selector) == 0 {
		return false
	}
	for key, value := range selector {
		if pod.Labels[key] != value {
			return false
		}
	}
	return true
}

func agentRuntimeInsecureBackendCandidates(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	endpoint string,
	security corev1alpha1.AgentRuntimeTransportSecurity,
) ([]agentRuntimeBackend, error) {
	parsed, security, err := validateAgentRuntimeTransportSecurity(endpoint, security)
	if err != nil {
		return nil, err
	}
	if security == corev1alpha1.AgentRuntimeTransportSecurityTLS {
		return nil, nil
	}
	if isLoopbackAgentRuntimeEndpoint(parsed.Hostname()) && agentRuntimeAllowInsecureLoopbackForTests {
		return nil, nil
	}
	serviceName, serviceNamespace, ok := parseAgentRuntimeServiceNamespaceHost(parsed.Hostname())
	if !ok || serviceNamespace != namespace || reader == nil {
		return nil, fmt.Errorf("deployment.transportSecurity %q requires a validated same-namespace Kubernetes Service", security)
	}
	service := &corev1.Service{}
	if err := reader.Get(ctx, client.ObjectKey{Name: serviceName, Namespace: serviceNamespace}, service); err != nil {
		return nil, fmt.Errorf("read Service %q in namespace %q for pinned AgentRuntime dial: %w", serviceName, serviceNamespace, err)
	}
	if service.Spec.Type == corev1.ServiceTypeExternalName || len(service.Spec.Selector) == 0 {
		return nil, fmt.Errorf("service %q is no longer a selector-backed non-ExternalName Service", serviceName)
	}
	servicePort, err := agentRuntimeServicePort(parsed, service)
	if err != nil {
		return nil, err
	}
	var pods corev1.PodList
	if err := reader.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels(service.Spec.Selector)); err != nil {
		return nil, fmt.Errorf("list Pods selected by AgentRuntime Service %q: %w", serviceName, err)
	}
	candidates := make([]agentRuntimeBackend, 0, len(pods.Items))
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !agentRuntimePodReady(pod) || pod.UID == "" || strings.TrimSpace(pod.Name) == "" {
			continue
		}
		podIP, ok := agentRuntimePodIPForService(pod, service)
		if !ok {
			continue
		}
		targetPort, ok := agentRuntimePodTargetPort(pod, servicePort)
		if !ok {
			continue
		}
		candidates = append(candidates, agentRuntimeBackend{
			PodName:     pod.Name,
			PodUID:      string(pod.UID),
			DialAddress: net.JoinHostPort(podIP.String(), strconv.Itoa(int(targetPort))),
		})
	}
	if len(candidates) == 0 {
		return nil, fmt.Errorf("AgentRuntime Service %q has no Ready selector-matched Pod with a resolvable target port", serviceName)
	}
	return candidates, nil
}

func agentRuntimePodIPForService(pod *corev1.Pod, service *corev1.Service) (net.IP, bool) {
	ips := agentRuntimePodIPs(pod)
	if len(ips) == 0 {
		return nil, false
	}
	if service == nil || len(service.Spec.IPFamilies) == 0 {
		return ips[0], true
	}
	for _, family := range service.Spec.IPFamilies {
		for _, ip := range ips {
			switch family {
			case corev1.IPv4Protocol:
				if ip.To4() != nil {
					return ip, true
				}
			case corev1.IPv6Protocol:
				if ip.To4() == nil {
					return ip, true
				}
			}
		}
	}
	return nil, false
}

func agentRuntimePodHasIP(pod *corev1.Pod, expected net.IP) bool {
	if expected == nil {
		return false
	}
	for _, ip := range agentRuntimePodIPs(pod) {
		if ip.Equal(expected) {
			return true
		}
	}
	return false
}

func agentRuntimePodIPs(pod *corev1.Pod) []net.IP {
	if pod == nil {
		return nil
	}
	values := make([]string, 0, len(pod.Status.PodIPs)+1)
	for _, podIP := range pod.Status.PodIPs {
		values = append(values, podIP.IP)
	}
	if len(values) == 0 && strings.TrimSpace(pod.Status.PodIP) != "" {
		values = append(values, pod.Status.PodIP)
	}
	ips := make([]net.IP, 0, len(values))
	for _, value := range values {
		if ip := net.ParseIP(strings.TrimSpace(value)); ip != nil {
			ips = append(ips, ip)
		}
	}
	return ips
}

func agentRuntimeServicePort(endpoint *url.URL, service *corev1.Service) (*corev1.ServicePort, error) {
	portNumber := 80
	if endpoint.Port() != "" {
		parsedPort, err := strconv.Atoi(endpoint.Port())
		if err != nil || parsedPort < 1 || parsedPort > 65535 {
			return nil, fmt.Errorf("deployment.endpoint has invalid port")
		}
		portNumber = parsedPort
	}
	for i := range service.Spec.Ports {
		servicePort := &service.Spec.Ports[i]
		if int(servicePort.Port) == portNumber && (servicePort.Protocol == "" || servicePort.Protocol == corev1.ProtocolTCP) {
			return servicePort, nil
		}
	}
	return nil, fmt.Errorf("AgentRuntime Service %q does not expose endpoint port %d over TCP", service.Name, portNumber)
}

func agentRuntimePodReady(pod *corev1.Pod) bool {
	if pod == nil || !pod.DeletionTimestamp.IsZero() || pod.Spec.HostNetwork || pod.Status.Phase != corev1.PodRunning {
		return false
	}
	for _, condition := range pod.Status.Conditions {
		if condition.Type == corev1.PodReady {
			return condition.Status == corev1.ConditionTrue
		}
	}
	return false
}

func agentRuntimePodTargetPort(pod *corev1.Pod, servicePort *corev1.ServicePort) (int32, bool) {
	if pod == nil || servicePort == nil {
		return 0, false
	}
	targetPort := servicePort.TargetPort
	if targetPort.Type == intstr.Int {
		if targetPort.IntVal > 0 {
			return targetPort.IntVal, true
		}
		return servicePort.Port, servicePort.Port > 0
	}
	name := strings.TrimSpace(targetPort.StrVal)
	if name == "" {
		return servicePort.Port, servicePort.Port > 0
	}
	for _, container := range pod.Spec.Containers {
		if port, ok := agentRuntimeNamedContainerPort(container, name); ok {
			return port, true
		}
	}
	for _, container := range pod.Spec.InitContainers {
		if container.RestartPolicy == nil || *container.RestartPolicy != corev1.ContainerRestartPolicyAlways {
			continue
		}
		if port, ok := agentRuntimeNamedContainerPort(container, name); ok {
			return port, true
		}
	}
	return 0, false
}

func agentRuntimeNamedContainerPort(container corev1.Container, name string) (int32, bool) {
	for _, port := range container.Ports {
		if port.Name == name && (port.Protocol == "" || port.Protocol == corev1.ProtocolTCP) && port.ContainerPort > 0 {
			return port.ContainerPort, true
		}
	}
	return 0, false
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
	clusterDomain := agentRuntimeClusterDomain()
	if clusterDomain == "" {
		return "", "", false
	}
	clusterDomainParts := strings.Split(clusterDomain, ".")
	if len(parts) != 3+len(clusterDomainParts) || parts[2] != k8sServiceDNSLabel || !slices.Equal(parts[3:], clusterDomainParts) {
		return "", "", false
	}
	serviceName, serviceNamespace = parts[0], parts[1]
	if serviceName == "" || serviceNamespace == "" {
		return "", "", false
	}
	return serviceName, serviceNamespace, true
}

func agentRuntimeClusterDomain() string {
	if override := strings.Trim(strings.ToLower(strings.TrimSpace(agentRuntimeClusterDomainForTests)), "."); override != "" {
		return override
	}
	agentRuntimeClusterDomainOnce.Do(func() {
		agentRuntimeClusterDomainValue = discoverAgentRuntimeClusterDomain("/etc/resolv.conf")
	})
	return agentRuntimeClusterDomainValue
}

func discoverAgentRuntimeClusterDomain(resolvConfPath string) string {
	data, err := os.ReadFile(resolvConfPath)
	if err == nil {
		matches := map[string]struct{}{}
		for line := range strings.SplitSeq(string(data), "\n") {
			fields := strings.Fields(line)
			if len(fields) < 2 || fields[0] != "search" {
				continue
			}
			searchDomains := make([]string, 0, len(fields)-1)
			for _, domain := range fields[1:] {
				domain = strings.Trim(strings.ToLower(strings.TrimSpace(domain)), ".")
				if domain != "" {
					searchDomains = append(searchDomains, domain)
				}
			}
			for clusterDomain := range clusterDomainsFromSearchDomains(searchDomains) {
				matches[clusterDomain] = struct{}{}
			}
		}
		if len(matches) == 1 {
			for clusterDomain := range matches {
				return clusterDomain
			}
		}
	}
	return ""
}

func clusterDomainsFromSearchDomains(searchDomains []string) map[string]struct{} {
	matches := map[string]struct{}{}
	for i := 0; i+2 < len(searchDomains); i++ {
		namespaceDomain := searchDomains[i]
		serviceDomain := searchDomains[i+1]
		clusterDomain := searchDomains[i+2]
		if serviceDomain != k8sServiceDNSLabel+"."+clusterDomain {
			continue
		}
		namespace, ok := strings.CutSuffix(namespaceDomain, "."+serviceDomain)
		if !ok || namespace == "" || strings.Contains(namespace, ".") {
			continue
		}
		matches[clusterDomain] = struct{}{}
	}
	return matches
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
	reader := r.apiReader()
	if reader == nil {
		return "", "", fmt.Errorf("kubernetes API reader is required to resolve AgentRuntime %q bearer token", runtime.Name)
	}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: runtime.Namespace, Name: ref.Name}, secret); err != nil {
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
		if !harness.IsKnownBrokeredToolClass(class) {
			continue
		}
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
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.AgentRuntime{}).
		WithEventFilter(predicate.GenerationChangedPredicate{}).
		Named("agentruntime").
		Complete(r)
}
