package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	agentRuntimeRecoveryOwnerAnnotation = "orka.ai/agent-runtime-recovery-uid"
	agentRuntimeRecoveryPodFinalizer    = "orka.ai/agent-runtime-boot-retirement"
	agentRuntimeBootWitnessKind         = "agent-runtime-boot-witness"
	agentRuntimeBootRetirementKind      = "agent-runtime-boot-retirement"
	agentRuntimeExposureKind            = "agent-runtime-session-exposure"
	agentRuntimeEpochEnvironment        = "ORKA_ACP_CONTROLLER_EPOCH"
	agentRuntimeBootEnvironment         = "ORKA_ACP_SUPERVISOR_BOOT_ID"
)

// agentRuntimeBootWitness is an immutable observation made before runtime
// admission. The authenticated boot and Kubernetes container are observed in
// one bracketed read. Later endpoint or Pod absence cannot replace this proof.
type agentRuntimeBootWitness struct {
	SchemaVersion         int                                   `json:"schemaVersion"`
	Namespace             string                                `json:"namespace"`
	RuntimeName           string                                `json:"runtimeName"`
	RuntimeUID            types.UID                             `json:"runtimeUID"`
	RuntimeGeneration     int64                                 `json:"runtimeGeneration"`
	Spec                  corev1alpha1.AgentRuntimeRegistrySpec `json:"spec"`
	Fence                 harnessv2.Fence                       `json:"fence"`
	DeploymentName        string                                `json:"deploymentName"`
	DeploymentUID         types.UID                             `json:"deploymentUID"`
	TemplateDigest        string                                `json:"templateDigest"`
	ReplicaSetName        string                                `json:"replicaSetName"`
	ReplicaSetUID         types.UID                             `json:"replicaSetUID"`
	PodName               string                                `json:"podName"`
	PodUID                types.UID                             `json:"podUID"`
	PodSpecDigest         string                                `json:"podSpecDigest"`
	ContainerName         string                                `json:"containerName"`
	ContainerID           string                                `json:"containerID"`
	ImageID               string                                `json:"imageID"`
	RestartCount          int32                                 `json:"restartCount"`
	StartedAt             metav1.Time                           `json:"startedAt"`
	ServiceName           string                                `json:"serviceName"`
	ServiceUID            types.UID                             `json:"serviceUID"`
	Pins                  []string                              `json:"pins"`
	ControllerAuthUID     types.UID                             `json:"controllerAuthUID"`
	ControllerAuthVersion string                                `json:"controllerAuthVersion"`
	CapabilityAuthUID     types.UID                             `json:"capabilityAuthUID"`
	CapabilityAuthVersion string                                `json:"capabilityAuthVersion"`
}

type agentRuntimeRecoveryBackend struct {
	deployment *appsv1.Deployment
	pod        *corev1.Pod
	witness    agentRuntimeBootWitness
}

func agentRuntimeRecoveryIdentity(kind string, runtimeUID types.UID, namespace, operation string) store.ExternalEffectIdentity {
	return store.ExternalEffectIdentity{Kind: kind, Namespace: namespace, AggregateID: string(runtimeUID), OperationID: operation}
}

func readAgentRuntimeRecoveryEffect(ctx context.Context, effects store.ExternalEffectStore, identity store.ExternalEffectIdentity, into any) (*store.ExternalEffect, error) {
	if effects == nil {
		return nil, errors.New("AgentRuntime recovery requires the durable control store")
	}
	var effect *store.ExternalEffect
	var err error
	if reader, ok := effects.(store.ExternalEffectIdentityReader); ok {
		effect, err = reader.GetExternalEffectByIdentity(ctx, identity)
	} else {
		id, idErr := identity.CanonicalID()
		if idErr != nil {
			return nil, idErr
		}
		effect, err = effects.GetExternalEffect(ctx, id)
	}
	if err != nil {
		return nil, err
	}
	if effect == nil || effect.Identity != identity {
		return nil, fmt.Errorf("%w: AgentRuntime recovery evidence identity is invalid", store.ErrConflict)
	}
	if (effect.State == store.ExternalEffectPending || effect.State == "" && effect.Version == 0) && len(effect.Response) == 0 && effect.ResponseDigest == "" {
		return effect, fmt.Errorf("%w: AgentRuntime recovery observation is pending", store.ErrNotReady)
	}
	if effect.State != store.ExternalEffectSucceeded ||
		effect.ResponseDigest != store.CanonicalBytesDigest(effect.Response) || len(effect.Response) == 0 {
		return nil, fmt.Errorf("%w: AgentRuntime recovery evidence is incomplete or corrupt", store.ErrConflict)
	}
	if err := json.Unmarshal(effect.Response, into); err != nil {
		return nil, fmt.Errorf("decode AgentRuntime recovery evidence: %w", err)
	}
	canonical, err := harnessv2.CanonicalValue(into)
	if err != nil || !bytes.Equal(canonical, effect.Response) {
		return nil, fmt.Errorf("%w: AgentRuntime recovery evidence is not canonical", store.ErrConflict)
	}
	return effect, nil
}

func persistAgentRuntimeRecoveryEffect(ctx context.Context, effects store.ExternalEffectStore, fence store.ControllerEpochFence, identity store.ExternalEffectIdentity, requestDigest string, response any) error {
	if effects == nil {
		return errors.New("AgentRuntime recovery requires the durable control store")
	}
	body, err := harnessv2.CanonicalValue(response)
	if err != nil {
		return err
	}
	effect, err := effects.ReserveExternalEffect(ctx, store.ReserveExternalEffectRequest{
		Identity: identity, RequestDigest: requestDigest, Fence: fence, CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		return err
	}
	if effect.State == store.ExternalEffectSucceeded {
		if effect.RequestDigest != requestDigest || effect.ResponseDigest != store.CanonicalBytesDigest(body) || !bytes.Equal(effect.Response, body) {
			return fmt.Errorf("%w: AgentRuntime recovery observation cannot be replaced", store.ErrConflict)
		}
		return nil
	}
	if effect.State != store.ExternalEffectPending {
		return fmt.Errorf("%w: AgentRuntime recovery observation is not pending", store.ErrConflict)
	}
	_, err = effects.TransitionExternalEffect(ctx, store.ExternalEffectTransition{
		ID: effect.ID, Fence: fence, ExpectedVersion: effect.Version, ExpectedState: effect.State,
		NewState: store.ExternalEffectSucceeded, RequestDigest: requestDigest,
		ResponseDigest: store.CanonicalBytesDigest(body), Response: body, UpdatedAt: time.Now().UTC(),
	})
	return err
}

func agentRuntimeRecoveryGuard(ctx context.Context, control store.DurableControlStore, fence store.ControllerEpochFence, fn func(context.Context) error) error {
	guard, ok := control.(store.ControllerEpochMutationStore)
	if !ok {
		return errors.New("AgentRuntime recovery requires the authoritative epoch mutation guard")
	}
	return guard.WithControllerEpochMutation(ctx, fence, fn)
}

func requireAgentRuntimeRecoveryFence(ctx context.Context, control store.DurableControlStore, fence store.ControllerEpochFence) error {
	reader, ok := control.(interface {
		GetControllerEpochFence(context.Context, string) (store.ControllerEpochFence, error)
	})
	if !ok {
		return errors.New("AgentRuntime recovery requires authoritative epoch reads")
	}
	actual, err := reader.GetControllerEpochFence(ctx, fence.Name)
	if err != nil || actual != fence {
		return errors.New("AgentRuntime recovery controller lost its durable fence")
	}
	return nil
}

func agentRuntimeLocalContainerRetirementAllowed(provider string) bool {
	switch provider {
	case "codex", "claude", "copilot", "opencode", "agentkit":
		return true
	default:
		return false
	}
}

func validateAgentRuntimeKubernetesRecoverySpec(runtime *corev1alpha1.AgentRuntime) error {
	recovery := runtime.Spec.Deployment.KubernetesRecovery
	if recovery == nil {
		return nil
	}
	if runtime.RegisteredContractVersion() != corev1alpha1.AgentRuntimeContractHarnessV2 ||
		recovery.DeploymentName == "" || recovery.DeploymentUID == "" || recovery.ContainerName == "" {
		return errors.New("runtime recovery requires v2 and an exact Deployment UID and container name")
	}
	if runtime.Spec.Capabilities == nil || !runtime.Spec.Capabilities.SupportsDrain {
		return errors.New("runtime recovery requires supportsDrain")
	}
	return nil
}

// recoveryTemplateDigest excludes only the controller-owned epoch value.
// Any image, command, volume, auth source, selector or security change remains
// a distinct physical authority and cannot be silently adopted during recovery.
func recoveryTemplateDigest(template corev1.PodTemplateSpec, containerName string) (string, uint64, error) {
	copy := template.DeepCopy()
	var epoch uint64
	count := 0
	for i := range copy.Spec.Containers {
		if copy.Spec.Containers[i].Name != containerName {
			continue
		}
		for j := range copy.Spec.Containers[i].Env {
			env := &copy.Spec.Containers[i].Env[j]
			if env.Name == agentRuntimeEpochEnvironment {
				count++
				parsed, err := strconv.ParseUint(env.Value, 10, 64)
				if err != nil || parsed == 0 || env.ValueFrom != nil {
					return "", 0, errors.New("runtime recovery requires one literal positive controller epoch")
				}
				epoch, env.Value = parsed, "<controller-epoch>"
			}
		}
	}
	if count != 1 {
		return "", 0, errors.New("runtime recovery requires one literal controller epoch")
	}
	digest, err := acpDomainDigest("agent-runtime-deployment-template", copy)
	return digest, epoch, err
}

// validateRecoveryPodTemplate verifies the execution configuration as well as
// the owner chain. Kubernetes may assign scheduling fields and default the
// ServiceAccount name; it must not inject or replace executable configuration.
func validateRecoveryPodTemplate(deployment *appsv1.Deployment, rs *appsv1.ReplicaSet, pod *corev1.Pod) error {
	actualTemplate := rs.Spec.Template.DeepCopy()
	delete(actualTemplate.Labels, appsv1.DefaultDeploymentUniqueLabelKey)
	if !reflect.DeepEqual(*actualTemplate, deployment.Spec.Template) {
		return errors.New("runtime recovery ReplicaSet template differs from the opted-in Deployment")
	}
	expected, actual := deployment.Spec.Template.Spec, pod.Spec
	if !reflect.DeepEqual(expected.Containers, actual.Containers) || !reflect.DeepEqual(expected.Volumes, actual.Volumes) ||
		!reflect.DeepEqual(expected.SecurityContext, actual.SecurityContext) || !reflect.DeepEqual(expected.RuntimeClassName, actual.RuntimeClassName) ||
		!reflect.DeepEqual(expected.OS, actual.OS) || !reflect.DeepEqual(expected.HostUsers, actual.HostUsers) {
		return errors.New("runtime recovery Pod execution configuration differs from its Deployment")
	}
	serviceAccount := func(name string) string {
		if name == "" {
			return "default"
		}
		return name
	}
	if serviceAccount(expected.ServiceAccountName) != serviceAccount(actual.ServiceAccountName) {
		return errors.New("runtime recovery Pod ServiceAccount changed")
	}
	return nil
}

func recoveryPodSpecDigest(pod *corev1.Pod) (string, error) {
	return acpDomainDigest("agent-runtime-enrolled-pod-spec", pod.Spec)
}

func (r *AgentRuntimeReconciler) recoveryDeployment(ctx context.Context, runtime *corev1alpha1.AgentRuntime) (*appsv1.Deployment, string, uint64, error) {
	ref := runtime.Spec.Deployment.KubernetesRecovery
	if ref == nil {
		return nil, "", 0, errors.New("runtime recovery is not enabled")
	}
	deployment := &appsv1.Deployment{}
	if err := r.endpointReader().Get(ctx, client.ObjectKey{Namespace: runtime.Namespace, Name: ref.DeploymentName}, deployment); err != nil {
		return nil, "", 0, err
	}
	if deployment.UID != types.UID(ref.DeploymentUID) || !deployment.DeletionTimestamp.IsZero() ||
		deployment.Annotations[agentRuntimeRecoveryOwnerAnnotation] != string(runtime.UID) || deployment.Spec.Paused ||
		deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 1 || deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		return nil, "", 0, errors.New("runtime recovery Deployment consent, UID, single replica or Recreate strategy changed")
	}
	if err := r.validateAgentRuntimeRecoveryPodSpec(ctx, runtime.Namespace, deployment.Spec.Template.Spec, ref.ContainerName, recoveryProviderKind(runtime.Spec)); err != nil {
		return nil, "", 0, err
	}
	digest, epoch, err := recoveryTemplateDigest(deployment.Spec.Template, ref.ContainerName)
	return deployment, digest, epoch, err
}

//nolint:gocyclo // Enrollment verifies the full Service, EndpointSlice, Pod, ReplicaSet and opted-in Deployment owner chain before trusting a dial target.
func (r *AgentRuntimeReconciler) recoveryBackend(ctx context.Context, runtime *corev1alpha1.AgentRuntime) (*agentRuntimeRecoveryBackend, error) {
	deployment, templateDigest, _, err := r.recoveryDeployment(ctx, runtime)
	if err != nil {
		return nil, err
	}
	parsed, err := url.Parse(runtime.Spec.Deployment.Endpoint)
	if err != nil {
		return nil, err
	}
	serviceName, namespace, ok := parseAgentRuntimeServiceNamespaceHost(strings.TrimSuffix(strings.ToLower(parsed.Hostname()), "."))
	if !ok || namespace != runtime.Namespace {
		return nil, errors.New("runtime recovery requires an exact same-namespace Service endpoint")
	}
	service := &corev1.Service{}
	if err := r.endpointReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: serviceName}, service); err != nil {
		return nil, err
	}
	port, err := agentRuntimeEndpointServicePort(parsed)
	if err != nil {
		return nil, err
	}
	state, err := r.verifiedAgentRuntimeServiceBackendState(ctx, service, port)
	if err != nil {
		return nil, err
	}
	// Recovery witnesses retain one dial address. Reject unsupported address
	// sets before committing a witness that cannot be read or retired later.
	if state.endpointCount != 1 || len(state.pins) != 1 || service.UID == "" {
		return nil, errors.New("runtime recovery requires one exact Service backend address")
	}
	var pods corev1.PodList
	if err := r.endpointReader().List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels(service.Spec.Selector)); err != nil {
		return nil, err
	}
	var selected *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if pod.DeletionTimestamp != nil {
			continue
		}
		for _, pin := range state.pins {
			host, _, splitErr := net.SplitHostPort(pin)
			if splitErr != nil {
				return nil, splitErr
			}
			if host == pod.Status.PodIP || slices.ContainsFunc(pod.Status.PodIPs, func(ip corev1.PodIP) bool { return host == ip.IP }) {
				if selected != nil && selected.UID != pod.UID {
					return nil, errors.New("runtime recovery backend is ambiguous")
				}
				selected = pod
			}
		}
	}
	if selected == nil || selected.UID == "" {
		return nil, errors.New("runtime recovery backend Pod is unavailable")
	}
	ref := runtime.Spec.Deployment.KubernetesRecovery
	if err := r.validateAgentRuntimeRecoveryPodSpec(ctx, selected.Namespace, selected.Spec, ref.ContainerName, recoveryProviderKind(runtime.Spec)); err != nil {
		return nil, err
	}
	owner := metav1.GetControllerOf(selected)
	if owner == nil || owner.Kind != "ReplicaSet" || owner.APIVersion != appsv1.SchemeGroupVersion.String() {
		return nil, errors.New("runtime recovery Pod has no ReplicaSet owner")
	}
	rs := &appsv1.ReplicaSet{}
	if err := r.endpointReader().Get(ctx, client.ObjectKey{Namespace: namespace, Name: owner.Name}, rs); err != nil {
		return nil, err
	}
	if rs.UID != owner.UID || !metav1.IsControlledBy(rs, deployment) {
		return nil, errors.New("runtime recovery ReplicaSet does not belong to the opted-in Deployment")
	}
	if err := validateRecoveryPodTemplate(deployment, rs, selected); err != nil {
		return nil, err
	}
	if deployment.Spec.Selector == nil {
		return nil, errors.New("runtime recovery Deployment selector is missing")
	}
	selector, err := metav1.LabelSelectorAsSelector(deployment.Spec.Selector)
	if err != nil || !selector.Matches(labels.Set(selected.Labels)) {
		return nil, errors.New("runtime recovery Pod selector changed")
	}
	// EndpointSlice UID binding is stricter than the portable dialing policy.
	var endpointSlices discoveryv1.EndpointSliceList
	if err := r.endpointReader().List(ctx, &endpointSlices, client.InNamespace(namespace), client.MatchingLabels{discoveryv1.LabelServiceName: serviceName}); err != nil {
		return nil, err
	}
	for _, slice := range endpointSlices.Items {
		if !metav1.IsControlledBy(&slice, service) {
			return nil, errors.New("runtime recovery EndpointSlice is not owned by the exact Service")
		}
		for _, endpoint := range slice.Endpoints {
			if endpoint.TargetRef == nil || endpoint.TargetRef.Kind != agentRuntimeRecoveryPodKind || endpoint.TargetRef.Namespace != namespace ||
				endpoint.TargetRef.Name != selected.Name || endpoint.TargetRef.UID != selected.UID {
				return nil, errors.New("runtime recovery EndpointSlice lacks the exact Pod UID")
			}
		}
	}
	status, err := recoverySupervisorStatus(selected, ref.ContainerName)
	if err != nil {
		return nil, err
	}
	if status.Name != ref.ContainerName || status.ContainerID == "" || status.ImageID == "" || status.State.Running == nil || status.State.Running.StartedAt.IsZero() || !status.Ready {
		return nil, errors.New("runtime recovery requires a ready running container identity")
	}
	podDigest, err := recoveryPodSpecDigest(selected)
	if err != nil {
		return nil, err
	}
	witness := agentRuntimeBootWitness{
		SchemaVersion: 1, Namespace: runtime.Namespace, RuntimeName: runtime.Name, RuntimeUID: runtime.UID, RuntimeGeneration: runtime.Generation,
		Spec: runtime.Spec, DeploymentName: deployment.Name, DeploymentUID: deployment.UID, TemplateDigest: templateDigest,
		ReplicaSetName: rs.Name, ReplicaSetUID: rs.UID, PodName: selected.Name, PodUID: selected.UID, PodSpecDigest: podDigest,
		ContainerName: status.Name, ContainerID: status.ContainerID, ImageID: status.ImageID, RestartCount: status.RestartCount, StartedAt: status.State.Running.StartedAt,
		ServiceName: service.Name, ServiceUID: service.UID, Pins: state.pins,
	}
	return &agentRuntimeRecoveryBackend{deployment: deployment, pod: selected.DeepCopy(), witness: witness}, nil
}

func (r *AgentRuntimeReconciler) retainRecoveryRuntime(ctx context.Context, runtime *corev1alpha1.AgentRuntime, fence store.ControllerEpochFence) error {
	return agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		current := &corev1alpha1.AgentRuntime{}
		if err := r.endpointReader().Get(writeCtx, client.ObjectKeyFromObject(runtime), current); err != nil {
			return err
		}
		if current.UID != runtime.UID || current.Generation != runtime.Generation || !reflect.DeepEqual(current.Spec, runtime.Spec) || current.DeletionTimestamp != nil {
			return errors.New("AgentRuntime ownership changed before boot retention")
		}
		base := current.DeepCopy()
		changed := controllerutil.AddFinalizer(current, agentRuntimeFinalizer)
		changed = controllerutil.AddFinalizer(current, agentRuntimeSecretGCFinalizer) || changed
		if changed {
			if err := r.Patch(writeCtx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
		*runtime = *current
		return nil
	})
}

func (r *AgentRuntimeReconciler) revalidateRecoveryBackend(ctx context.Context, runtime *corev1alpha1.AgentRuntime, expected *agentRuntimeRecoveryBackend) error {
	current, err := r.recoveryBackend(ctx, runtime)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(expected.witness, current.witness) {
		return errors.New("runtime container identity changed during authenticated observation")
	}
	return nil
}

func (r *AgentRuntimeReconciler) retainRecoveryPod(ctx context.Context, runtime *corev1alpha1.AgentRuntime, backend *agentRuntimeRecoveryBackend, witness agentRuntimeBootWitness, fence store.ControllerEpochFence) error {
	return agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		if err := r.revalidateRecoveryBackend(writeCtx, runtime, backend); err != nil {
			return err
		}
		return r.writeRecoveryPodRetention(writeCtx, runtime, witness)
	})
}

func (r *AgentRuntimeReconciler) writeRecoveryPodRetention(ctx context.Context, runtime *corev1alpha1.AgentRuntime, witness agentRuntimeBootWitness) error {
	current := &corev1alpha1.AgentRuntime{}
	if err := r.endpointReader().Get(ctx, client.ObjectKeyFromObject(runtime), current); err != nil {
		return err
	}
	if current.UID != witness.RuntimeUID || !controllerutil.ContainsFinalizer(current, agentRuntimeFinalizer) {
		return errors.New("original runtime owner is not retained before Pod retention")
	}
	body, err := harnessv2.CanonicalValue(witness)
	if err != nil {
		return err
	}
	pod, err := r.originalPreparedRecoveryPod(ctx, witness)
	if err != nil || pod == nil {
		return errors.Join(errors.New("original runtime Pod changed before retention"), err)
	}
	if !pod.DeletionTimestamp.IsZero() && !controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
		return fmt.Errorf("%w: unretained original Pod deletion is pending", store.ErrNotReady)
	}
	if owner := pod.Annotations[agentRuntimePreparedOwnerAnnotation]; owner != "" && owner != string(runtime.UID) {
		return errors.New("runtime recovery Pod retention owner changed")
	}
	if previous := pod.Annotations[agentRuntimePreparedWitnessAnnotation]; previous != "" && previous != string(body) {
		old, err := r.preparedRecoveryPodWitness(ctx, runtime, pod)
		if err != nil {
			return err
		}
		if retired, err := loadAgentRuntimeBootRetirement(ctx, r.ControlStore, old); err != nil || !retired {
			return errors.Join(errors.New("retained Pod has another unresolved prepared boot"), err)
		}
	}
	if controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) &&
		pod.Annotations[agentRuntimePreparedWitnessAnnotation] == string(body) &&
		pod.Annotations[agentRuntimePreparedOwnerAnnotation] == string(runtime.UID) {
		return nil
	}
	// A timed-out owner-removal PATCH may still be in flight after its epoch
	// guard returns. Change the owner's resourceVersion before retaining the
	// Pod, so either this write or that original removal must conflict.
	if err := r.fenceRecoveryPodRetention(ctx, current); err != nil {
		return err
	}
	runtime.ResourceVersion = current.ResourceVersion
	base := pod.DeepCopy()
	if pod.Annotations == nil {
		pod.Annotations = make(map[string]string)
	}
	pod.Annotations[agentRuntimePreparedWitnessAnnotation] = string(body)
	pod.Annotations[agentRuntimePreparedOwnerAnnotation] = string(runtime.UID)
	controllerutil.AddFinalizer(pod, agentRuntimeRecoveryPodFinalizer)
	return r.Patch(ctx, pod, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func (r *AgentRuntimeReconciler) fenceRecoveryPodRetention(ctx context.Context, runtime *corev1alpha1.AgentRuntime) error {
	if runtime.ResourceVersion == "" || runtime.Annotations[agentRuntimeRetentionVersionAnnotation] == runtime.ResourceVersion {
		return errors.New("runtime retention requires an advancing owner resourceVersion")
	}
	base := runtime.DeepCopy()
	if runtime.Annotations == nil {
		runtime.Annotations = make(map[string]string)
	}
	runtime.Annotations[agentRuntimeRetentionVersionAnnotation] = runtime.ResourceVersion
	return r.Patch(ctx, runtime, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
}

func recoveryBootSecretName(witness agentRuntimeBootWitness) string {
	digest := store.CanonicalControlID("runtime-boot-authority", witness.Namespace, string(witness.RuntimeUID), string(witness.Fence.SupervisorBootID))
	return "agent-runtime-boot-" + strings.TrimPrefix(store.CanonicalBytesDigest([]byte(digest)), "sha256:")[:40]
}

func (r *AgentRuntimeReconciler) persistRecoveryBootAuth(ctx context.Context, runtime *corev1alpha1.AgentRuntime, witness agentRuntimeBootWitness, auth agentRuntimeAuthMaterial, fence store.ControllerEpochFence) error {
	observed := &corev1alpha1.AgentRuntimeObservedCapabilities{
		RuntimeInstanceID: string(witness.Fence.RuntimeInstanceID), SupervisorBootID: string(witness.Fence.SupervisorBootID), ControllerEpoch: int64(witness.Fence.ControllerEpoch),
		RuntimePoolUID: string(witness.Fence.RuntimePoolUID), RuntimePoolGeneration: int64(witness.Fence.RuntimePoolGeneration),
		RuntimeProfileDigest: string(witness.Fence.RuntimeProfileDigest), ProfileDigestSchemaVersion: int32(witness.Fence.ProfileDigestSchemaVersion), SupportsDrain: runtime.Spec.Capabilities.SupportsDrain,
	}
	snapshot := agentRuntimeDeletionSnapshot{
		SchemaVersion: 1, Namespace: runtime.Namespace, Name: runtime.Name, UID: runtime.UID, Generation: runtime.Generation,
		Spec: runtime.Spec, ObservedCapabilities: observed, ControllerAuthSecretUID: auth.controllerSecretUID, CapabilityAuthSecretUID: auth.capabilitySecretUID,
		ControllerAuthResourceVersion: auth.controllerResourceVersion, CapabilityAuthResourceVersion: auth.capabilityResourceVersion,
	}
	authority, err := harnessv2.CanonicalValue(snapshot)
	if err != nil {
		return err
	}
	immutable := true
	desired := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{
		Namespace: runtime.Namespace, Name: recoveryBootSecretName(witness), Labels: map[string]string{agentRuntimeCleanupSecretLabel: scheduledRunLabelValue},
		Finalizers: []string{agentRuntimeSecretFinalizer}, OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(runtime, corev1alpha1.GroupVersion.WithKind("AgentRuntime"))},
	}, Immutable: &immutable, Type: agentRuntimeCleanupSecretType, Data: map[string][]byte{
		agentRuntimeCleanupSecretAuthorityKey: authority, agentRuntimeCleanupSecretControllerAuthKey: []byte(auth.controllerBearerToken), agentRuntimeCleanupSecretCapabilityKey: slices.Clone(auth.operationCapabilitySecret),
	}}
	return agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		current := &corev1.Secret{}
		err := r.endpointReader().Get(writeCtx, client.ObjectKeyFromObject(desired), current)
		if apierrors.IsNotFound(err) {
			return r.Create(writeCtx, desired)
		}
		if err != nil {
			return err
		}
		if current.Immutable == nil || !*current.Immutable || current.DeletionTimestamp != nil || !agentRuntimeCleanupSecretMatches(current, desired) || !metav1.IsControlledBy(current, runtime) {
			return errors.New("immutable AgentRuntime boot cleanup authority changed")
		}
		return nil
	})
}
