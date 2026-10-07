// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
)

const (
	runtimePoolDurableWorkspaceVolume          = "orka-workspace"
	runtimePoolDurableWorkspaceMountPath       = "/durable/orka-workspace"
	runtimePoolNativeProcessPort         int32 = 80
)

var errWorkspaceCredentialConflict = errors.New("workspace supervisor credential bootstrap conflict")

// Native processes use a fresh writable container filesystem for scratch.
// Keep the special durable-workspace mount separate from Pod volume declarations.
func runtimePoolNativeProcessTemplate(template corev1.PodTemplateSpec) corev1.PodTemplateSpec {
	result := *template.DeepCopy()
	// The pinned native runtime runs the supervisor as root under gVisor. Its
	// public API supports capabilities, but no Kubernetes seccomp or privilege-
	// escalation controls. Publish only the guarantees it can enforce.
	result.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsUser: new(int64(0)), RunAsGroup: new(int64(0)), RunAsNonRoot: new(false)}
	// Native worker placement is fixed by the provider's pinned Linux WorkerPool.
	result.Spec.NodeSelector = nil
	// Native Actor health and shutdown are provider-owned. Kubernetes probes,
	// lifecycle hooks and grace periods cannot describe those guarantees.
	result.Spec.TerminationGracePeriodSeconds = nil
	scratch := map[string]bool{}
	volumes := result.Spec.Volumes[:0]
	for _, volume := range result.Spec.Volumes {
		if volume.EmptyDir != nil {
			scratch[volume.Name] = true
			continue
		}
		volumes = append(volumes, volume)
	}
	result.Spec.Volumes = volumes
	for i := range result.Spec.Containers {
		container := &result.Spec.Containers[i]
		container.StartupProbe, container.ReadinessProbe, container.LivenessProbe = nil, nil, nil
		container.Lifecycle = nil
		for j := range container.Ports {
			if container.Ports[j].ContainerPort == runtimePoolPort {
				container.Ports[j].ContainerPort = runtimePoolNativeProcessPort
			}
		}
		env := container.Env[:0]
		for _, variable := range container.Env {
			switch variable.Name {
			case "ORKA_ACP_SESSION_BASE_DIR", "ORKA_ACP_MCP_BROKER_URL", "ORKA_ACP_POD_NAMESPACE":
				// The native adapter uses supervisor defaults and SystemInfo
				// identity instead of these Pod-specific overrides.
				continue
			case "ORKA_ACP_LISTEN_ADDRESS":
				variable = corev1.EnvVar{Name: variable.Name, Value: ":80"}
			}
			env = append(env, variable)
		}
		container.Env = env
		var capabilities *corev1.Capabilities
		if container.SecurityContext != nil && container.SecurityContext.Capabilities != nil {
			capabilities = container.SecurityContext.Capabilities.DeepCopy()
		}
		container.SecurityContext = &corev1.SecurityContext{RunAsUser: new(int64(0)), RunAsGroup: new(int64(0)), RunAsNonRoot: new(false), Privileged: new(false), ReadOnlyRootFilesystem: new(false), Capabilities: capabilities}
		mounts := container.VolumeMounts[:0]
		for _, mount := range container.VolumeMounts {
			if !scratch[mount.Name] {
				mounts = append(mounts, mount)
			}
		}
		container.VolumeMounts = mounts
	}
	return result
}

func runtimePoolWorkspaceBootstrapTemplate(
	template corev1.PodTemplateSpec,
	nonce, publicKey string,
) corev1.PodTemplateSpec {
	result := *template.DeepCopy()
	if len(result.Spec.Containers) != 1 {
		return result
	}
	container := &result.Spec.Containers[0]
	env := make([]corev1.EnvVar, 0, len(container.Env)+2)
	for i := range container.Env {
		switch container.Env[i].Name {
		case runtimePoolControllerTokenFileEnv, runtimePoolCapabilitySecretFileEnv, runtimePoolProviderTokenFileEnv,
			"ORKA_ACP_CONTROLLER_TOKEN_BOOTSTRAP", "ORKA_ACP_CAPABILITY_SECRET_BOOTSTRAP", "ORKA_ACP_PROVIDER_TOKEN_BOOTSTRAP":
			continue
		default:
			env = append(env, container.Env[i])
		}
	}
	env = append(env,
		corev1.EnvVar{Name: runtimePoolBootstrapNonceEnv, Value: nonce},
		corev1.EnvVar{Name: harnessv2.CredentialBootstrapPublicKeyEnv, Value: publicKey},
	)
	container.Env = env
	container.EnvFrom = nil
	mounts := container.VolumeMounts[:0]
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name != runtimePoolAuthVolume && container.VolumeMounts[i].Name != runtimePoolProviderCapabilityVolume {
			mounts = append(mounts, container.VolumeMounts[i])
		}
	}
	container.VolumeMounts = mounts
	volumes := result.Spec.Volumes[:0]
	for i := range result.Spec.Volumes {
		if result.Spec.Volumes[i].Name != runtimePoolAuthVolume && result.Spec.Volumes[i].Name != runtimePoolProviderCapabilityVolume {
			volumes = append(volumes, result.Spec.Volumes[i])
		}
	}
	result.Spec.Volumes = volumes
	result.Annotations[runtimePoolTemplateRevisionAnnotation] = runtimePoolPodTemplateRevision(result)
	return result
}

func runtimePoolWorkspacePodSpecsMatch(expected, actual corev1.PodSpec, injectedDurableClaimName string) bool {
	expectedSpec := normalizeRuntimePoolWorkspacePodSpec(expected)
	actualSpec := normalizeRuntimePoolWorkspacePodSpec(actual)

	// The provider injects the durable workspace PVC volume from the claim's
	// volumeClaimTemplates; its per-sandbox claim name cannot be rendered into
	// the template, so the reserved-name PVC volume is compared by presence of
	// its mount rather than by claim identity. Any other unexpected volume
	// still fails the match.
	actualSpec.Volumes = stripInjectedDurableWorkspaceVolume(expectedSpec.Volumes, actualSpec.Volumes, injectedDurableClaimName)

	// These fields are derived from cluster scheduling/admission state rather
	// than from the provider-visible Sandbox template.
	expectedSpec.NodeName, actualSpec.NodeName = "", ""
	expectedSpec.Priority, actualSpec.Priority = nil, nil
	expectedSpec.PreemptionPolicy, actualSpec.PreemptionPolicy = nil, nil
	expectedSpec.Overhead, actualSpec.Overhead = nil, nil
	if len(expectedSpec.ImagePullSecrets) == 0 {
		actualSpec.ImagePullSecrets = nil
	}
	if expectedSpec.PriorityClassName == "" {
		actualSpec.PriorityClassName = ""
	}

	return apiequality.Semantic.DeepEqual(expectedSpec, actualSpec)
}

func normalizeRuntimePoolWorkspacePodSpec(spec corev1.PodSpec) corev1.PodSpec {
	result := *spec.DeepCopy()
	if result.DNSPolicy == "" {
		result.DNSPolicy = corev1.DNSClusterFirst
	}
	if result.RestartPolicy == "" {
		result.RestartPolicy = corev1.RestartPolicyAlways
	}
	if result.SecurityContext == nil {
		result.SecurityContext = &corev1.PodSecurityContext{}
	}
	if result.TerminationGracePeriodSeconds == nil {
		result.TerminationGracePeriodSeconds = new(int64)
		*result.TerminationGracePeriodSeconds = corev1.DefaultTerminationGracePeriodSeconds
	}
	if result.SchedulerName == "" {
		result.SchedulerName = corev1.DefaultSchedulerName
	}
	if result.EnableServiceLinks == nil {
		result.EnableServiceLinks = new(bool)
		*result.EnableServiceLinks = corev1.DefaultEnableServiceLinks
	}
	if result.ServiceAccountName == "" {
		result.ServiceAccountName = runtimePoolDefaultServiceAccountName
	}
	// The core API's internal-to-v1 conversion mirrors the effective service
	// account into this deprecated alias on Pods. Embedded PodSpecs in CRDs do
	// not receive that conversion, so compare the canonical field once here.
	result.DeprecatedServiceAccount = result.ServiceAccountName
	for i := range result.InitContainers {
		normalizeRuntimePoolWorkspaceContainer(&result.InitContainers[i])
	}
	for i := range result.Containers {
		normalizeRuntimePoolWorkspaceContainer(&result.Containers[i])
	}
	result.Tolerations = runtimePoolWorkspaceExplicitTolerations(result.Tolerations)
	return result
}

func normalizeRuntimePoolWorkspaceContainer(container *corev1.Container) {
	if container == nil {
		return
	}
	if container.TerminationMessagePath == "" {
		container.TerminationMessagePath = corev1.TerminationMessagePathDefault
	}
	if container.TerminationMessagePolicy == "" {
		container.TerminationMessagePolicy = corev1.TerminationMessageReadFile
	}
	for i := range container.Ports {
		if container.Ports[i].Protocol == "" {
			container.Ports[i].Protocol = corev1.ProtocolTCP
		}
	}
	for i := range container.Env {
		fieldRef := container.Env[i].ValueFrom
		if fieldRef != nil && fieldRef.FieldRef != nil && fieldRef.FieldRef.APIVersion == "" {
			fieldRef.FieldRef.APIVersion = "v1"
		}
	}
	normalizeRuntimePoolWorkspaceProbe(container.LivenessProbe)
	normalizeRuntimePoolWorkspaceProbe(container.ReadinessProbe)
	normalizeRuntimePoolWorkspaceProbe(container.StartupProbe)
	if container.Lifecycle != nil {
		if container.Lifecycle.PostStart != nil {
			normalizeRuntimePoolWorkspaceHTTPGet(container.Lifecycle.PostStart.HTTPGet)
		}
		if container.Lifecycle.PreStop != nil {
			normalizeRuntimePoolWorkspaceHTTPGet(container.Lifecycle.PreStop.HTTPGet)
		}
	}
}

func normalizeRuntimePoolWorkspaceProbe(probe *corev1.Probe) {
	if probe == nil {
		return
	}
	if probe.TimeoutSeconds == 0 {
		probe.TimeoutSeconds = 1
	}
	if probe.PeriodSeconds == 0 {
		probe.PeriodSeconds = 10
	}
	if probe.SuccessThreshold == 0 {
		probe.SuccessThreshold = 1
	}
	if probe.FailureThreshold == 0 {
		probe.FailureThreshold = 3
	}
	normalizeRuntimePoolWorkspaceHTTPGet(probe.HTTPGet)
	if probe.GRPC != nil && probe.GRPC.Service == nil {
		probe.GRPC.Service = new(string)
	}
}

func normalizeRuntimePoolWorkspaceHTTPGet(action *corev1.HTTPGetAction) {
	if action == nil {
		return
	}
	if action.Path == "" {
		action.Path = "/"
	}
	if action.Scheme == "" {
		action.Scheme = corev1.URISchemeHTTP
	}
	// Nil uses HTTP/1.1 whether or not H2CContainerProbe writes its default.
	if action.Protocol == nil {
		action.Protocol = new(corev1.HTTPProtocolHTTP1)
	}
}

func runtimePoolWorkspaceExplicitTolerations(tolerations []corev1.Toleration) []corev1.Toleration {
	result := make([]corev1.Toleration, 0, len(tolerations))
	for i := range tolerations {
		toleration := tolerations[i]
		if toleration.Operator == corev1.TolerationOpExists && toleration.Effect == corev1.TaintEffectNoExecute &&
			(toleration.Key == corev1.TaintNodeNotReady || toleration.Key == corev1.TaintNodeUnreachable) {
			continue
		}
		result = append(result, toleration)
	}
	return result
}

func (r *RuntimePoolReconciler) seedWorkspaceSupervisorCredentials(
	ctx context.Context,
	endpoint, nonce string,
	authSecret, providerSecret *corev1.Secret,
) (bool, error) {
	request := harnessv2.CredentialBootstrapRequest{
		ControllerToken:  strings.TrimSpace(string(authSecret.Data[runtimePoolControllerTokenKey])),
		CapabilitySecret: strings.TrimSpace(string(authSecret.Data[runtimePoolCapabilitySecretKey])),
		ProviderToken:    strings.TrimSpace(string(providerSecret.Data[runtimePoolProviderTokenKey])),
	}
	if err := request.Validate(); err != nil {
		return false, fmt.Errorf("pool credentials are incomplete: %w", err)
	}
	if r.WorkspaceCredentialSeeder != nil {
		return r.WorkspaceCredentialSeeder(ctx, endpoint, nonce, authSecret.Data[runtimePoolBootstrapSigningSeedKey], request)
	}
	payload, err := json.Marshal(request)
	if err != nil {
		return false, err
	}
	seedCtx, cancel := context.WithTimeout(ctx, runtimePoolProbeTimeout)
	defer cancel()
	httpRequest, err := http.NewRequestWithContext(
		seedCtx, http.MethodPut, strings.TrimRight(endpoint, "/")+harnessv2.CredentialBootstrapPath, bytes.NewReader(payload),
	)
	if err != nil {
		return false, err
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set(harnessv2.CredentialBootstrapNonceHeader, nonce)
	signature, err := harnessv2.SignCredentialBootstrap(authSecret.Data[runtimePoolBootstrapSigningSeedKey], nonce, payload)
	if err != nil {
		return false, fmt.Errorf("sign credential bootstrap request: %w", err)
	}
	httpRequest.Header.Set(harnessv2.CredentialBootstrapSignatureHeader, signature)
	httpClient := r.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{Timeout: runtimePoolProbeTimeout, Transport: harnessv2.NewProxylessTransport()}
	}
	isolationClient := *httpClient
	isolationClient.CheckRedirect = func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }
	response, err := isolationClient.Do(httpRequest)
	if err != nil {
		return false, err
	}
	defer response.Body.Close() //nolint:errcheck // response body is unused
	switch response.StatusCode {
	case http.StatusCreated, http.StatusOK:
		return false, nil
	case http.StatusConflict:
		return false, errWorkspaceCredentialConflict
	case http.StatusNotFound:
		return true, nil
	default:
		return false, fmt.Errorf("credential bootstrap returned status %d", response.StatusCode)
	}
}

func runtimePoolPodTemplateValidationTarget(
	pool *corev1alpha1.RuntimePool,
	template corev1.PodTemplateSpec,
) (*corev1alpha1.RuntimePool, runtimePoolConfig, error) {
	if pool == nil || len(template.Spec.Containers) != 1 {
		return nil, runtimePoolConfig{}, fmt.Errorf("deployed RuntimePool template is invalid")
	}
	return runtimePoolValidationTargetFromTemplate(pool, template)
}

func (r *RuntimePoolReconciler) runtimePoolPodTemplateAuthSecret(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	namespace string,
	podSpec corev1.PodSpec,
) (*corev1.Secret, error) {
	secretName := ""
	for i := range podSpec.Volumes {
		volume := podSpec.Volumes[i]
		if volume.Name == runtimePoolAuthVolume && volume.Secret != nil {
			secretName = strings.TrimSpace(volume.Secret.SecretName)
			break
		}
	}
	var secret *corev1.Secret
	if secretName != "" {
		secret = &corev1.Secret{}
		if err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: namespace, Name: secretName}, secret); err != nil {
			return nil, fmt.Errorf("get deployed RuntimePool auth Secret: %w", err)
		}
	} else {
		epochText := ""
		if len(podSpec.Containers) == 1 {
			epochText = strings.TrimSpace(runtimePoolLiteralEnvironment(podSpec.Containers[0].Env)["ORKA_ACP_CONTROLLER_EPOCH"])
		}
		epoch, err := strconv.ParseInt(epochText, 10, 64)
		if err != nil || epoch <= 0 {
			return nil, fmt.Errorf("deployed RuntimePool auth Secret reference is missing")
		}
		secret, err = resolveRuntimePoolAuthSecret(ctx, uncachedReader(r.APIReader, r.Client), pool, namespace, epoch)
		if err != nil {
			return nil, err
		}
	}
	if strings.TrimSpace(string(secret.Data[runtimePoolControllerTokenKey])) == "" ||
		len(secret.Data[runtimePoolCapabilitySecretKey]) == 0 {
		return nil, fmt.Errorf("deployed RuntimePool auth Secret is incomplete")
	}
	return secret, nil
}

func runtimePoolDurableWorkspaceTemplate(template corev1.PodTemplateSpec) corev1.PodTemplateSpec {
	result := *template.DeepCopy()
	if len(result.Spec.Containers) != 1 {
		return result
	}
	container := &result.Spec.Containers[0]
	container.VolumeMounts = append(container.VolumeMounts, corev1.VolumeMount{
		Name: runtimePoolDurableWorkspaceVolume, MountPath: runtimePoolDurableWorkspaceMountPath,
	})
	container.Env = append(container.Env, corev1.EnvVar{
		Name: "ORKA_ACP_DURABLE_WORKSPACE_DIR", Value: runtimePoolDurableWorkspaceMountPath,
	})
	return result
}

func stripInjectedDurableWorkspaceVolume(expected, actual []corev1.Volume, injectedClaimName string) []corev1.Volume {
	for _, volume := range expected {
		if volume.Name == runtimePoolDurableWorkspaceVolume {
			return actual
		}
	}
	result := make([]corev1.Volume, 0, len(actual))
	for _, volume := range actual {
		// A read-only PVC source would mount the active repository workspace
		// read-only despite the writable mount the template declares; it is
		// retained so the spec comparison fails instead of Serving a
		// workspace whose clone/edit/commit operations cannot work.
		if volume.Name == runtimePoolDurableWorkspaceVolume && volume.PersistentVolumeClaim != nil &&
			!volume.PersistentVolumeClaim.ReadOnly &&
			injectedClaimName != "" && volume.PersistentVolumeClaim.ClaimName == injectedClaimName {
			continue
		}
		result = append(result, volume)
	}
	return result
}

func (r *RuntimePoolReconciler) recycleRuntimePoolInstance(
	ctx context.Context,
	pool *corev1alpha1.RuntimePool,
	pod *corev1.Pod,
) error {
	if runtimePoolHasExternalWorkspace(pool) {
		workspace, err := r.externalPoolWorkspace(ctx, pool)
		if err != nil {
			return err
		}
		if workspace.Spec.Workload == nil || workspace.Status.Allocation == nil || pod == nil {
			return fmt.Errorf("external runtime has no exact retirement identity")
		}
		a := workspace.Status.Allocation
		request := workspace.Spec.Workload
		if a.Sequence != request.Sequence || a.Key != request.Key || a.Identity.RequestRevision != request.Revision {
			return fmt.Errorf("external runtime retirement refers to a different instance")
		}
		if request.Runtime != nil && slices.Contains(request.Runtime.RequiredFeatures, workspacev1alpha1.WorkspaceFeatureNativeProcess) {
			if err := r.validateExternalNativeRetirement(ctx, pool, workspace, pod); err != nil {
				return err
			}
		} else if a.Identity.InstanceID != string(pod.UID) {
			return fmt.Errorf("external runtime retirement refers to a different instance")
		}
		// Runtime settlement must finish before authorizing provider termination.
		// The normal retirement reconcile performs authenticated drain and keeps
		// durable lineage while the replacement is pending.
		return r.patchRuntimePoolAnnotation(ctx, pool, "orka.ai/external-runtime-retirement-requested", "true")
	}
	if pool.Spec.ExecutionWorkspace == nil {
		if err := r.Delete(ctx, pod, deleteCurrentObjectPreconditions(pod)...); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		return nil
	}
	return fmt.Errorf("legacy workspace RuntimePool must retire under its original provider before upgrade")
}

func (r *RuntimePoolReconciler) validateExternalNativeRetirement(ctx context.Context, pool *corev1alpha1.RuntimePool, workspace *workspacev1alpha1.ExecutionWorkspace, pod *corev1.Pod) error {
	a, request := workspace.Status.Allocation, workspace.Spec.Workload
	binding, err := externalRuntimeEvidence(pool)
	if err != nil || binding == nil || !binding.NativeProcess || binding.WorkspaceUID != workspace.UID ||
		binding.Sequence != a.Sequence || binding.Identity != a.Identity || binding.RuntimeUID != pod.UID ||
		a.Startup == nil || a.Startup.Process == nil || binding.Pod != a.Startup.Process.Worker || binding.Endpoint != a.Startup.Endpoint ||
		workspace.Status.ObservedGeneration != workspace.Generation {
		return fmt.Errorf("external native runtime retirement differs from the independently bound instance")
	}
	if err := workspacev1alpha1.ValidateStartup(*request, *a); err != nil {
		return fmt.Errorf("external native runtime retirement has invalid startup evidence: %w", err)
	}
	observed, err := r.attestExternalWorkspaceStartup(ctx, request, a.Startup)
	if err != nil || observed.UID != pod.UID || observed.Namespace != pod.Namespace || observed.Name != pod.Name {
		return fmt.Errorf("external native runtime retirement failed independent materialization verification")
	}
	return nil
}

// validateRuntimePoolExecutionWorkspace rejects legacy shapes even after the API
// server prunes their removed backend settings. Plain Deployment pools remain valid.
func validateRuntimePoolExecutionWorkspace(pool *corev1alpha1.RuntimePool) error {
	if pool.Spec.ExecutionWorkspace == nil {
		return nil
	}
	if !runtimePoolHasExternalWorkspace(pool) {
		return fmt.Errorf("legacy workspace RuntimePool must retire under its original provider before upgrade")
	}
	w := pool.Spec.ExecutionWorkspace
	if w.Workload.ContractVersion != workspacev1alpha1.LifecycleContractV1 || w.Workload.ProtocolVersion != corev1alpha1.RuntimePoolProtocolHarnessV2 {
		return fmt.Errorf("external workspace requires the supported generic lifecycle and harness v2 contract")
	}
	if w.Provider == "" || w.WorkspaceRef.Name == "" || w.WorkspaceRef.UID == "" || !validSHA256Digest(w.BindingDigest) {
		return fmt.Errorf("external workspace requires exact identity and binding digest")
	}
	if w.ParametersRef == nil || w.ParametersBinding == nil || w.ParametersRef.Group == "" || w.ParametersRef.Kind == "" || w.ParametersRef.Name != w.ParametersBinding.Name || w.ParametersBinding.UID == "" || w.ParametersBinding.Generation < 1 || !validSHA256Digest(w.ParametersBinding.ProfileHash) {
		return fmt.Errorf("external workspace requires exact immutable parameter pins")
	}
	if pool.Spec.Capacity == nil || pool.Spec.Capacity.MaxResidentSessions != 1 || pool.Spec.Capacity.MaxRunningPrompts != 1 {
		return fmt.Errorf("workspace-backed RuntimePools host exactly one resident RuntimeSession; spec.capacity must be 1/1")
	}
	return nil
}

const runtimePoolWorkspaceResumeLostAnnotation = "orka.ai/workspace-resume-lost"

// External allocation defaults cover resources represented by the lifecycle
// contract. Kubernetes ephemeral-storage quotas cannot bound native processes;
// the ordinary Deployment defaults retain their separate storage quotas.
func externalRuntimePoolResourceRequirements(resourceClass string) corev1.ResourceRequirements {
	if resourceClass != runtimePoolResourceClassStandard {
		return corev1.ResourceRequirements{}
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m"), corev1.ResourceMemory: resource.MustParse("512Mi")},
		Limits:   corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2"), corev1.ResourceMemory: resource.MustParse("4Gi")},
	}
}
