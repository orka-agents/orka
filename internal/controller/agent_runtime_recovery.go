package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

type agentRuntimeBootRetirement struct {
	SchemaVersion        int                              `json:"schemaVersion"`
	WitnessDigest        string                           `json:"witnessDigest"`
	Kind                 string                           `json:"kind"`
	ContainerTermination *corev1.ContainerStateTerminated `json:"containerTermination,omitempty"`
	DrainedStatus        *harnessv2.StatusResponse        `json:"drainedStatus,omitempty"`
}

func runtimeWitnessDigest(witness agentRuntimeBootWitness) (string, error) {
	encoded, err := harnessv2.CanonicalValue(witness)
	if err != nil {
		return "", err
	}
	return store.CanonicalBytesDigest(encoded), nil
}

func loadAgentRuntimeBootWitness(ctx context.Context, effects store.ExternalEffectStore, namespace string, uid types.UID, boot harnessv2.SupervisorBootID) (agentRuntimeBootWitness, error) {
	var witness agentRuntimeBootWitness
	effect, err := readAgentRuntimeRecoveryEffect(ctx, effects, agentRuntimeRecoveryIdentity(agentRuntimeBootWitnessKind, uid, namespace, string(boot)), &witness)
	if err != nil {
		return witness, err
	}
	return witness, validateAgentRuntimeBootWitness(witness, namespace, uid, boot, effect.RequestDigest)
}

//nolint:gocyclo // Verify the full immutable observation for both committed and prepared witnesses.
func validateAgentRuntimeBootWitness(witness agentRuntimeBootWitness, namespace string, uid types.UID, boot harnessv2.SupervisorBootID, requestDigest string) error {
	digest, err := runtimeWitnessDigest(witness)
	if err != nil {
		return err
	}
	if witness.SchemaVersion != 1 || witness.Namespace != namespace || witness.RuntimeUID != uid || witness.RuntimeName == "" ||
		witness.RuntimeGeneration < 1 || witness.Fence.SupervisorBootID != boot || witness.Fence.Validate(false) != nil ||
		witness.Spec.Deployment.KubernetesRecovery == nil || witness.PodUID == "" || witness.ContainerID == "" ||
		store.ValidateCanonicalDigest("Pod specification digest", witness.PodSpecDigest) != nil ||
		witness.ImageID == "" || witness.StartedAt.IsZero() || requestDigest != digest ||
		witness.ControllerAuthUID == "" || witness.CapabilityAuthUID == "" || witness.ControllerAuthVersion == "" || witness.CapabilityAuthVersion == "" {
		return fmt.Errorf("%w: enrolled AgentRuntime boot witness is invalid", store.ErrConflict)
	}
	ref, capabilities := witness.Spec.Deployment.KubernetesRecovery, witness.Spec.Capabilities
	if ref.DeploymentName != witness.DeploymentName || ref.DeploymentUID != string(witness.DeploymentUID) || ref.ContainerName != witness.ContainerName ||
		witness.PodName == "" || witness.ReplicaSetName == "" || witness.ReplicaSetUID == "" || witness.ServiceName == "" || witness.ServiceUID == "" || len(witness.Pins) != 1 ||
		store.ValidateCanonicalDigest("Deployment template digest", witness.TemplateDigest) != nil ||
		capabilities == nil || capabilities.Profile == nil || !capabilities.SupportsDrain ||
		capabilities.RuntimeInstanceID != string(witness.Fence.RuntimeInstanceID) || capabilities.Profile.Digest != string(witness.Fence.RuntimeProfileDigest) {
		return fmt.Errorf("%w: enrolled boot ownership and runtime fences are inconsistent", store.ErrConflict)
	}
	return nil
}

func (r *AgentRuntimeReconciler) recoveryWitnesses(ctx context.Context, runtime *corev1alpha1.AgentRuntime) ([]agentRuntimeBootWitness, error) {
	var effects corev1alpha1.ExternalEffectList
	if err := r.endpointReader().List(ctx, &effects, client.InNamespace(runtime.Namespace)); err != nil {
		return nil, err
	}
	var witnesses []agentRuntimeBootWitness
	for i := range effects.Items {
		effect := &effects.Items[i]
		if effect.Spec.Kind != agentRuntimeBootWitnessKind || effect.Spec.AggregateID != string(runtime.UID) {
			continue
		}
		// A pending observation cannot have admitted work. Its exact response
		// must commit before conformance or public Session creation can start.
		if (effect.Status.State == corev1alpha1.ExternalEffectControlState(store.ExternalEffectPending) || effect.Status.State == "" && effect.Status.Version == 0) && effect.Status.Response == nil && effect.Status.ResponseDigest == "" {
			continue
		}
		witness, err := loadAgentRuntimeBootWitness(ctx, r.ControlStore, runtime.Namespace, runtime.UID, harnessv2.SupervisorBootID(effect.Spec.OperationID))
		if err != nil {
			return nil, err
		}
		witnesses = append(witnesses, witness)
	}
	return witnesses, nil
}

func loadAgentRuntimeBootRetirement(ctx context.Context, effects store.ExternalEffectStore, witness agentRuntimeBootWitness) (bool, error) {
	var proof agentRuntimeBootRetirement
	effect, err := readAgentRuntimeRecoveryEffect(ctx, effects,
		agentRuntimeRecoveryIdentity(agentRuntimeBootRetirementKind, witness.RuntimeUID, witness.Namespace, string(witness.Fence.SupervisorBootID)), &proof)
	if errors.Is(err, store.ErrNotFound) || errors.Is(err, store.ErrNotReady) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	digest, err := runtimeWitnessDigest(witness)
	if err != nil {
		return false, err
	}
	if proof.SchemaVersion != 1 || proof.WitnessDigest != digest || effect.RequestDigest != digest {
		return false, fmt.Errorf("%w: retirement evidence does not match its enrolled boot", store.ErrConflict)
	}
	switch proof.Kind {
	case "authenticated-drain":
		if proof.ContainerTermination != nil || proof.DrainedStatus == nil ||
			harnessv2.CompareFence(witness.Fence, proof.DrainedStatus.Fence, false) != harnessv2.FenceMatch || !upgradeDrainSupervisorIsQuiescent(*proof.DrainedStatus) {
			return false, fmt.Errorf("%w: exact supervisor drain evidence is invalid", store.ErrConflict)
		}
	case "kubernetes-container-termination":
		if proof.DrainedStatus != nil || !validWitnessContainerTermination(witness, proof.ContainerTermination) ||
			witness.Spec.Capabilities == nil || witness.Spec.Capabilities.Profile == nil || !agentRuntimeLocalContainerRetirementAllowed(witness.Spec.Capabilities.Profile.ProviderKind) {
			return false, fmt.Errorf("%w: exact local container termination evidence is invalid", store.ErrConflict)
		}
	default:
		return false, fmt.Errorf("%w: unsupported AgentRuntime retirement proof", store.ErrConflict)
	}
	return true, nil
}

func validWitnessContainerTermination(witness agentRuntimeBootWitness, terminal *corev1.ContainerStateTerminated) bool {
	return terminal != nil && terminal.ContainerID == witness.ContainerID && !terminal.StartedAt.IsZero() && !terminal.FinishedAt.IsZero() &&
		terminal.StartedAt.Equal(&witness.StartedAt) && !terminal.FinishedAt.Before(&terminal.StartedAt)
}

func witnessedContainerTermination(witness agentRuntimeBootWitness, pod *corev1.Pod) (*corev1.ContainerStateTerminated, error) {
	if pod == nil || pod.Namespace != witness.Namespace || pod.Name != witness.PodName || pod.UID != witness.PodUID {
		return nil, errors.New("retirement observation does not match the witnessed Pod UID")
	}
	if err := validateAgentRuntimeRecoveryPodSpec(pod.Spec, witness.ContainerName, recoveryProviderKind(witness.Spec)); err != nil {
		return nil, err
	}
	digest, err := recoveryPodSpecDigest(pod)
	if err != nil || digest != witness.PodSpecDigest {
		return nil, errors.New("retirement observation Pod specification changed")
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != appsv1.SchemeGroupVersion.String() || owner.Kind != "ReplicaSet" || owner.Name != witness.ReplicaSetName || owner.UID != witness.ReplicaSetUID {
		return nil, errors.New("retirement observation Pod ownership changed")
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name != witness.ContainerName {
			continue
		}
		for _, terminal := range []*corev1.ContainerStateTerminated{status.State.Terminated, status.LastTerminationState.Terminated} {
			if validWitnessContainerTermination(witness, terminal) {
				return terminal.DeepCopy(), nil
			}
		}
	}
	return nil, nil
}

func (r *AgentRuntimeReconciler) persistBootRetirement(ctx context.Context, witness agentRuntimeBootWitness, proof agentRuntimeBootRetirement, fence store.ControllerEpochFence) error {
	digest, err := runtimeWitnessDigest(witness)
	if err != nil {
		return err
	}
	proof.SchemaVersion, proof.WitnessDigest = 1, digest
	return persistAgentRuntimeRecoveryEffect(ctx, r.ControlStore, fence,
		agentRuntimeRecoveryIdentity(agentRuntimeBootRetirementKind, witness.RuntimeUID, witness.Namespace, string(witness.Fence.SupervisorBootID)), digest, proof)
}

func (r *AgentRuntimeReconciler) observeContainerRetirement(ctx context.Context, witness agentRuntimeBootWitness, fence store.ControllerEpochFence) (bool, error) {
	if retired, err := loadAgentRuntimeBootRetirement(ctx, r.ControlStore, witness); retired || err != nil {
		return retired, err
	}
	if witness.Spec.Capabilities == nil || witness.Spec.Capabilities.Profile == nil || !agentRuntimeLocalContainerRetirementAllowed(witness.Spec.Capabilities.Profile.ProviderKind) {
		return false, nil
	}
	pod := &corev1.Pod{}
	err := r.endpointReader().Get(ctx, client.ObjectKey{Namespace: witness.Namespace, Name: witness.PodName}, pod)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	terminal, err := witnessedContainerTermination(witness, pod)
	if err != nil || terminal == nil {
		return false, err
	}
	if err := r.validateAgentRuntimeRecoveryPodSpec(ctx, witness.Namespace, pod.Spec, witness.ContainerName, recoveryProviderKind(witness.Spec)); err != nil {
		return false, err
	}
	if err := r.persistBootRetirement(ctx, witness, agentRuntimeBootRetirement{Kind: "kubernetes-container-termination", ContainerTermination: terminal}, fence); err != nil {
		return false, err
	}
	return true, nil
}

func (r *AgentRuntimeReconciler) recoveryBootClient(ctx context.Context, witness agentRuntimeBootWitness, fence store.ControllerEpochFence) (*harnessv2.Client, error) {
	runtime := &corev1alpha1.AgentRuntime{ObjectMeta: metav1.ObjectMeta{Namespace: witness.Namespace, Name: witness.RuntimeName, UID: witness.RuntimeUID}, Spec: witness.Spec}
	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: witness.Namespace, Name: recoveryBootSecretName(witness)}
	err := r.endpointReader().Get(ctx, secretKey, secret)
	if apierrors.IsNotFound(err) {
		if err := r.resumeRecoveryBootRetention(ctx, witness, fence); err != nil {
			return nil, err
		}
		err = r.endpointReader().Get(ctx, secretKey, secret)
	}
	if err != nil {
		return nil, err
	}
	// A pending DELETE retains the original immutable authority until its
	// cleanup finalizer can be released after proven boot retirement.
	if secret.Immutable == nil || !*secret.Immutable || !controllerutil.ContainsFinalizer(secret, agentRuntimeSecretFinalizer) {
		return nil, errors.New("retained boot authentication must be immutable and retain its cleanup finalizer")
	}
	frozen, auth, err := decodeAgentRuntimeDeletionSnapshot(runtime, secret)
	if err != nil {
		return nil, err
	}
	if frozen.Generation != witness.RuntimeGeneration || !reflect.DeepEqual(frozen.Spec, witness.Spec) ||
		frozen.Status.ObservedCapabilities.SupervisorBootID != string(witness.Fence.SupervisorBootID) ||
		auth.controllerSecretUID != witness.ControllerAuthUID || auth.controllerResourceVersion != witness.ControllerAuthVersion ||
		auth.capabilitySecretUID != witness.CapabilityAuthUID || auth.capabilityResourceVersion != witness.CapabilityAuthVersion {
		return nil, errors.New("retained boot authentication does not match its witness")
	}
	validate := func(checkCtx context.Context) error {
		current, err := r.ControllerEpochManager.CurrentFence(checkCtx)
		if err != nil || current != fence {
			return errors.New("AgentRuntime recovery controller ownership changed")
		}
		if err := requireAgentRuntimeRecoveryFence(checkCtx, r.ControlStore, fence); err != nil {
			return err
		}
		pod := &corev1.Pod{}
		if err := r.endpointReader().Get(checkCtx, client.ObjectKey{Namespace: witness.Namespace, Name: witness.PodName}, pod); err != nil {
			return err
		}
		if pod.UID != witness.PodUID {
			return errors.New("old supervisor Pod identity changed")
		}
		if err := r.validateAgentRuntimeRecoveryPodSpec(checkCtx, witness.Namespace, pod.Spec, witness.ContainerName, recoveryProviderKind(witness.Spec)); err != nil {
			return err
		}
		if _, err := witnessedContainerTermination(witness, pod); err != nil {
			return err
		}
		status, err := recoverySupervisorStatus(pod, witness.ContainerName)
		if err != nil {
			return err
		}
		if status.Name != witness.ContainerName || status.ContainerID != witness.ContainerID || status.State.Running == nil ||
			!status.State.Running.StartedAt.Equal(&witness.StartedAt) || status.RestartCount != witness.RestartCount {
			return errors.New("old supervisor container is no longer the witnessed live incarnation")
		}
		return nil
	}
	if err := validate(ctx); err != nil {
		return nil, err
	}
	return harnessv2.NewClient(witness.Spec.Deployment.Endpoint,
		harnessv2.WithControllerBearerToken(auth.controllerBearerToken), harnessv2.WithOperationCapabilitySecret(auth.operationCapabilitySecret),
		harnessv2.WithStatusCapabilityBinding(harnessv2.StatusCapabilityBinding{RuntimeInstanceID: witness.Fence.RuntimeInstanceID, RuntimeProfileDigest: witness.Fence.RuntimeProfileDigest}),
		harnessv2.WithHTTPClient(externalRuntimeHTTPClient(PinnedBackendDialTransport(witness.Pins))), harnessv2.WithControlTimeout(10*time.Second),
		harnessv2.WithBeforeMutation(func(checkCtx context.Context, operation string) error {
			if operation != externalRuntimeDrainOperation {
				return errors.New("old-boot recovery only authorizes drain")
			}
			return validate(checkCtx)
		}),
	)
}

// A witness owns its boot before retention writes complete. Takeover and
// deletion must resume that interrupted enrollment before attempting drain.
// Only the originally observed credentials and physical backend may complete
// it; changed source auth cannot replace missing retained boot authority.
func (r *AgentRuntimeReconciler) resumeRecoveryBootRetention(ctx context.Context, witness agentRuntimeBootWitness, fence store.ControllerEpochFence) error {
	current := &corev1alpha1.AgentRuntime{}
	if err := r.endpointReader().Get(ctx, client.ObjectKey{Namespace: witness.Namespace, Name: witness.RuntimeName}, current); err != nil {
		return err
	}
	if current.UID != witness.RuntimeUID {
		return errors.New("AgentRuntime ownership changed before resuming boot retention")
	}
	runtime := &corev1alpha1.AgentRuntime{ObjectMeta: metav1.ObjectMeta{
		Namespace: witness.Namespace, Name: witness.RuntimeName, UID: witness.RuntimeUID, Generation: witness.RuntimeGeneration,
	}, Spec: witness.Spec}
	auth, err := r.agentRuntimeAuthMaterial(ctx, runtime)
	if err != nil {
		return err
	}
	if auth.controllerSecretUID != witness.ControllerAuthUID || auth.controllerResourceVersion != witness.ControllerAuthVersion ||
		auth.capabilitySecretUID != witness.CapabilityAuthUID || auth.capabilityResourceVersion != witness.CapabilityAuthVersion {
		return errors.New("original boot authentication is unavailable for retention recovery")
	}
	backend, err := r.recoveryBackend(ctx, runtime)
	if err != nil {
		return err
	}
	observed := backend.witness
	observed.Fence = witness.Fence
	observed.ControllerAuthUID, observed.ControllerAuthVersion = auth.controllerSecretUID, auth.controllerResourceVersion
	observed.CapabilityAuthUID, observed.CapabilityAuthVersion = auth.capabilitySecretUID, auth.capabilityResourceVersion
	if !reflect.DeepEqual(observed, witness) {
		return errors.New("original runtime backend changed before resuming boot retention")
	}
	if err := r.retainRecoveryPod(ctx, runtime, backend, witness, fence); err != nil {
		return err
	}
	return r.persistRecoveryBootAuth(ctx, runtime, witness, auth, fence)
}

func (r *AgentRuntimeReconciler) drainRecoveryBoot(ctx context.Context, witness agentRuntimeBootWitness, fence store.ControllerEpochFence) (bool, error) {
	if retired, err := r.observeContainerRetirement(ctx, witness, fence); retired || err != nil {
		return retired, err
	}
	runtimeClient, err := r.recoveryBootClient(ctx, witness, fence)
	if err != nil {
		return false, err
	}
	status, err := runtimeClient.Status(ctx)
	if err != nil {
		return false, err
	}
	if harnessv2.CompareFence(witness.Fence, status.Fence, false) != harnessv2.FenceMatch {
		return false, errors.New("old supervisor status does not match enrolled boot")
	}
	if !status.Drain.Requested {
		digest, err := runtimeWitnessDigest(witness)
		if err != nil {
			return false, err
		}
		if _, err := r.ControlStore.ReserveExternalEffect(ctx, store.ReserveExternalEffectRequest{
			Identity:      agentRuntimeRecoveryIdentity(agentRuntimeBootRetirementKind, witness.RuntimeUID, witness.Namespace, string(witness.Fence.SupervisorBootID)),
			RequestDigest: digest, Fence: fence, CreatedAt: time.Now().UTC(),
		}); err != nil {
			return false, err
		}
		request, err := newAgentRuntimeDeletionDrainRequest(witness.Fence, time.Now().UTC())
		if err != nil {
			return false, err
		}
		request.Reason = "controller_epoch_recovery"
		if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
			return false, err
		}
		_, err = runtimeClient.Drain(ctx, request)
		return false, err
	}
	if !upgradeDrainSupervisorIsQuiescent(*status) {
		return false, nil
	}
	if err := r.persistBootRetirement(ctx, witness, agentRuntimeBootRetirement{Kind: "authenticated-drain", DrainedStatus: status}, fence); err != nil {
		return false, err
	}
	return true, nil
}

func runtimeStatusIdle(status *harnessv2.StatusResponse) bool {
	return status != nil && status.Pressure.ResidentSessions == 0 && status.Pressure.ActivePrompts == 0 &&
		status.Pressure.QueuedAdmissions == 0 && status.Pressure.PendingPermissions == 0 && status.Pressure.LiveDescendants == 0 &&
		len(status.Sessions) == 0 && len(status.ActivePrompts) == 0 && len(status.PendingPermissions) == 0
}

func (r *AgentRuntimeReconciler) observeRecoveryBoot(ctx context.Context, runtime *corev1alpha1.AgentRuntime, backend *agentRuntimeRecoveryBackend, fence store.ControllerEpochFence) (agentRuntimeBootWitness, error) {
	witness := backend.witness
	auth, err := r.agentRuntimeAuthMaterial(ctx, runtime)
	if err != nil {
		return witness, err
	}
	probe, err := harnessv2.NewClient(runtime.Spec.Deployment.Endpoint,
		harnessv2.WithControllerBearerToken(auth.controllerBearerToken), harnessv2.WithOperationCapabilitySecret(auth.operationCapabilitySecret),
		harnessv2.WithStatusCapabilityBinding(harnessv2.StatusCapabilityBinding{RuntimeInstanceID: harnessv2.RuntimeInstanceID(runtime.Spec.Capabilities.RuntimeInstanceID), RuntimeProfileDigest: harnessv2.ProfileDigest(runtime.Spec.Capabilities.Profile.Digest)}),
		harnessv2.WithHTTPClient(externalRuntimeHTTPClient(PinnedBackendDialTransport(witness.Pins))), harnessv2.WithControlTimeout(10*time.Second))
	if err != nil {
		return witness, err
	}
	status, err := probe.Status(ctx)
	if err != nil {
		return witness, err
	}
	if err := r.revalidateRecoveryBackend(ctx, runtime, backend); err != nil {
		return witness, err
	}
	if err := r.requireCurrentAgentRuntimeAuthMaterial(ctx, runtime, auth); err != nil {
		return witness, err
	}
	_, templateEpoch, err := recoveryTemplateDigest(backend.deployment.Spec.Template, witness.ContainerName)
	if err != nil {
		return witness, err
	}
	if status.Fence.Validate(false) != nil || status.Fence.ControllerEpoch != templateEpoch ||
		string(status.Fence.RuntimeInstanceID) != runtime.Spec.Capabilities.RuntimeInstanceID || string(status.Fence.RuntimeProfileDigest) != runtime.Spec.Capabilities.Profile.Digest {
		return witness, errors.New("authenticated runtime does not match its Deployment epoch and registration")
	}
	witness.Fence = status.Fence
	witness.ControllerAuthUID, witness.ControllerAuthVersion = auth.controllerSecretUID, auth.controllerResourceVersion
	witness.CapabilityAuthUID, witness.CapabilityAuthVersion = auth.capabilitySecretUID, auth.capabilityResourceVersion
	existing, err := loadAgentRuntimeBootWitness(ctx, r.ControlStore, runtime.Namespace, runtime.UID, status.Fence.SupervisorBootID)
	witnessPublished := err == nil
	if err == nil {
		if !reflect.DeepEqual(existing, witness) {
			return witness, errors.New("enrolled supervisor boot authority changed")
		}
	} else if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrNotReady) {
		return witness, err
	} else if !runtimeStatusIdle(status) {
		return witness, errors.New("cannot enroll a supervisor boot after runtime admission")
	}
	prior, err := r.recoveryWitnesses(ctx, runtime)
	if err != nil {
		return witness, err
	}
	for _, old := range prior {
		if old.Fence.SupervisorBootID == witness.Fence.SupervisorBootID {
			continue
		}
		if retired, err := loadAgentRuntimeBootRetirement(ctx, r.ControlStore, old); err != nil || !retired {
			return witness, errors.Join(errors.New("prior supervisor boot lacks exact retirement evidence"), err)
		}
	}
	if err := r.retainRecoveryRuntime(ctx, runtime, fence); err != nil {
		return witness, err
	}
	digest, err := runtimeWitnessDigest(witness)
	if err != nil {
		return witness, err
	}
	identity := agentRuntimeRecoveryIdentity(agentRuntimeBootWitnessKind, runtime.UID, runtime.Namespace, string(witness.Fence.SupervisorBootID))
	if !witnessPublished {
		// Preparation retains the original Pod identity before an ambiguous
		// retention PATCH. An existing witness already records that identity;
		// adding a preparation would hide its legacy retention format.
		if _, err := r.ControlStore.ReserveExternalEffect(ctx, store.ReserveExternalEffectRequest{
			Identity: identity, RequestDigest: digest, Fence: fence, CreatedAt: time.Now().UTC(),
		}); err != nil {
			return witness, err
		}
		if err := persistAgentRuntimeRecoveryEffect(ctx, r.ControlStore, fence,
			agentRuntimeRecoveryIdentity(agentRuntimeBootPreparationKind, runtime.UID, runtime.Namespace, string(witness.Fence.SupervisorBootID)), digest, witness); err != nil {
			return witness, err
		}
	}
	if err := r.retainRecoveryPod(ctx, runtime, backend, witness, fence); err != nil {
		return witness, err
	}
	if err := persistAgentRuntimeRecoveryEffect(ctx, r.ControlStore, fence, identity, digest, witness); err != nil {
		return witness, err
	}
	return witness, r.persistRecoveryBootAuth(ctx, runtime, witness, auth, fence)
}

// reconcileKubernetesRuntimeRecovery runs before conformance. It never adopts a
// new boot until every older enrolled boot has positive retirement evidence.
//
//nolint:gocyclo // The ordered retirement, observation and epoch-replacement decisions share one fail-closed state boundary.
func (r *AgentRuntimeReconciler) reconcileKubernetesRuntimeRecovery(ctx context.Context, runtime *corev1alpha1.AgentRuntime) (bool, error) {
	if runtime.Spec.Deployment.KubernetesRecovery == nil {
		return false, nil
	}
	if err := validateAgentRuntimeSpec(runtime); err != nil {
		return true, err
	}
	if r.ControllerEpochManager == nil || r.ControlStore == nil {
		return true, errors.New("runtime recovery is not configured")
	}
	fence, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil {
		return true, err
	}
	if err := r.resumePreparedRecoveryPods(ctx, runtime, fence); err != nil {
		return true, err
	}
	deployment, templateDigest, templateEpoch, err := r.recoveryDeployment(ctx, runtime)
	if err != nil {
		return true, err
	}
	witnesses, err := r.recoveryWitnesses(ctx, runtime)
	if err != nil {
		return true, err
	}
	for _, witness := range witnesses {
		if witness.DeploymentUID != deployment.UID {
			return true, errors.New("runtime recovery Deployment ownership changed")
		}
		retired, err := r.observeContainerRetirement(ctx, witness, fence)
		if err != nil {
			return true, err
		}
		if !retired && witness.Fence.ControllerEpoch < uint64(fence.Epoch) {
			retired, err = r.drainRecoveryBoot(ctx, witness, fence)
			if err != nil || !retired {
				return true, err
			}
		}
		if retired {
			if err := r.releaseRetiredRecoveryPod(ctx, witness, fence, false); err != nil {
				return true, err
			}
		}
	}
	backend, backendErr := r.recoveryBackend(ctx, runtime)
	if backendErr != nil {
		if len(witnesses) > 0 && templateEpoch < uint64(fence.Epoch) {
			if err := r.requireRetiredRecoveryWitnesses(ctx, witnesses); err == nil {
				return true, r.patchRecoveryEpoch(ctx, runtime, deployment, templateDigest, fence)
			}
		}
		return true, backendErr
	}
	for _, witness := range witnesses {
		if witness.PodUID == backend.witness.PodUID && witness.ContainerID == backend.witness.ContainerID {
			continue
		}
		retired, err := r.observeContainerRetirement(ctx, witness, fence)
		if err != nil {
			return true, err
		}
		if !retired {
			return true, errors.New("prior supervisor boot lacks exact container retirement evidence")
		}
	}
	witness, err := r.observeRecoveryBoot(ctx, runtime, backend, fence)
	if err != nil {
		return true, err
	}
	if witness.Fence.ControllerEpoch > uint64(fence.Epoch) {
		return true, errors.New("supervisor epoch is newer than the current controller")
	}
	if witness.Fence.ControllerEpoch < uint64(fence.Epoch) {
		retired, err := r.drainRecoveryBoot(ctx, witness, fence)
		if err != nil || !retired {
			return true, err
		}
		if err := r.requireRetiredRecoveryWitnesses(ctx, witnesses); err != nil {
			return true, err
		}
		return true, r.patchRecoveryEpoch(ctx, runtime, deployment, templateDigest, fence)
	}
	if retired, err := loadAgentRuntimeBootRetirement(ctx, r.ControlStore, witness); retired || err != nil {
		if err == nil {
			err = errors.New("retired supervisor boot cannot admit new work")
		}
		return true, err
	}
	return false, nil
}

func (r *AgentRuntimeReconciler) requireRetiredRecoveryWitnesses(ctx context.Context, witnesses []agentRuntimeBootWitness) error {
	for _, witness := range witnesses {
		retired, err := loadAgentRuntimeBootRetirement(ctx, r.ControlStore, witness)
		if err != nil {
			return err
		}
		if !retired {
			return errors.New("prior supervisor boot is not retired")
		}
	}
	return nil
}

func (r *AgentRuntimeReconciler) patchRecoveryEpoch(ctx context.Context, runtime *corev1alpha1.AgentRuntime, expected *appsv1.Deployment, templateDigest string, fence store.ControllerEpochFence) error {
	return agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		registration := &corev1alpha1.AgentRuntime{}
		if err := r.endpointReader().Get(writeCtx, client.ObjectKeyFromObject(runtime), registration); err != nil {
			return err
		}
		if registration.UID != runtime.UID || registration.Generation != runtime.Generation || registration.DeletionTimestamp != nil || !reflect.DeepEqual(registration.Spec, runtime.Spec) {
			return errors.New("AgentRuntime ownership changed before epoch replacement")
		}
		witnesses, err := r.recoveryWitnesses(writeCtx, registration)
		if err != nil {
			return err
		}
		if len(witnesses) == 0 {
			return errors.New("epoch replacement requires enrolled boot evidence")
		}
		if err := r.requireRetiredRecoveryWitnesses(writeCtx, witnesses); err != nil {
			return err
		}
		current, digest, epoch, err := r.recoveryDeployment(writeCtx, runtime)
		if err != nil {
			return err
		}
		if current.UID != expected.UID || digest != templateDigest || epoch > uint64(fence.Epoch) {
			return errors.New("runtime Deployment recovery authority changed before epoch replacement")
		}
		if epoch == uint64(fence.Epoch) {
			return nil
		}
		base := current.DeepCopy()
		container, err := recoverySupervisorContainer(&current.Spec.Template.Spec, runtime.Spec.Deployment.KubernetesRecovery.ContainerName)
		if err != nil {
			return err
		}
		for i := range container.Env {
			if container.Env[i].Name == agentRuntimeEpochEnvironment {
				container.Env[i].Value = strconv.FormatInt(fence.Epoch, 10)
			}
		}
		return r.Patch(writeCtx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}

func (r *AgentRuntimeReconciler) releaseRetiredRecoveryPod(ctx context.Context, witness agentRuntimeBootWitness, fence store.ControllerEpochFence, deletingRuntime bool) error {
	return agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		if retired, err := loadAgentRuntimeBootRetirement(writeCtx, r.ControlStore, witness); err != nil || !retired {
			return errors.Join(errors.New("runtime Pod retention requires exact boot retirement"), err)
		}
		pod := &corev1.Pod{}
		err := r.endpointReader().Get(writeCtx, client.ObjectKey{Namespace: witness.Namespace, Name: witness.PodName}, pod)
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if pod.UID != witness.PodUID {
			return errors.New("retired Pod name was reused")
		}
		if !deletingRuntime {
			terminal, err := witnessedContainerTermination(witness, pod)
			if err != nil {
				return err
			}
			if pod.DeletionTimestamp == nil || terminal == nil {
				return nil
			}
		}
		if !controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) {
			return nil
		}
		runtime := &corev1alpha1.AgentRuntime{ObjectMeta: metav1.ObjectMeta{Namespace: witness.Namespace, UID: witness.RuntimeUID}}
		witnesses, err := r.recoveryWitnesses(writeCtx, runtime)
		if err != nil {
			return err
		}
		for _, other := range witnesses {
			if other.PodUID != witness.PodUID {
				continue
			}
			if retired, err := loadAgentRuntimeBootRetirement(writeCtx, r.ControlStore, other); err != nil || !retired {
				return errors.Join(errors.New("runtime Pod has another unretired supervisor boot"), err)
			}
		}
		base := pod.DeepCopy()
		controllerutil.RemoveFinalizer(pod, agentRuntimeRecoveryPodFinalizer)
		delete(pod.Annotations, agentRuntimePreparedWitnessAnnotation)
		if pod.Annotations[agentRuntimePreparedOwnerAnnotation] == string(witness.RuntimeUID) {
			delete(pod.Annotations, agentRuntimePreparedOwnerAnnotation)
		}
		return r.Patch(writeCtx, pod, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}))
	})
}
