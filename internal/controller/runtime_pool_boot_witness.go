package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

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

const (
	runtimePoolBootEnrollmentAnnotation   = "orka.ai/runtime-pool-boot-enrollment"
	runtimePoolBootEnrollmentVersion      = "v1"
	runtimePoolBootPodFinalizer           = "orka.ai/runtime-pool-boot-retirement"
	runtimePoolBootRetentionAnnotation    = "orka.ai/runtime-pool-boot-witness"
	runtimePoolBootOwnerVersionAnnotation = "orka.ai/runtime-pool-retention-version"
	runtimePoolBootPreparationKind        = "runtime-pool-boot-preparation"
	runtimePoolBootWitnessKind            = "runtime-pool-boot-witness"
	runtimePoolBootRetirementKind         = "runtime-pool-boot-retirement"
	runtimePoolBootContainerName          = "runtime"
	runtimePoolBootReplicaSetKind         = "ReplicaSet"
	runtimePoolBootDrainedProof           = "authenticated-drain"
	runtimePoolBootTerminatedProof        = "kubernetes-container-termination"
)

// runtimePoolBootWitness contains no credentials or Pod environment payloads.
// Its immutable preparation precedes Pod retention; only a committed witness
// may admit work. Evidence lives in the pool namespace, independently of its
// cross-namespace Pod and independently of the mutable ActiveInstance pointer.
type runtimePoolBootWitness struct {
	SchemaVersion  int             `json:"schemaVersion"`
	PoolNamespace  string          `json:"poolNamespace"`
	PoolName       string          `json:"poolName"`
	PoolUID        types.UID       `json:"poolUID"`
	Provider       string          `json:"provider"`
	Fence          harnessv2.Fence `json:"fence"`
	PodNamespace   string          `json:"podNamespace"`
	PodName        string          `json:"podName"`
	PodUID         types.UID       `json:"podUID"`
	PodAddress     string          `json:"podAddress"`
	PodSpecDigest  string          `json:"podSpecDigest"`
	DeploymentName string          `json:"deploymentName"`
	DeploymentUID  types.UID       `json:"deploymentUID"`
	ReplicaSetName string          `json:"replicaSetName"`
	ReplicaSetUID  types.UID       `json:"replicaSetUID"`
	TemplateDigest string          `json:"templateDigest"`
	ContainerID    string          `json:"containerID"`
	ImageID        string          `json:"imageID"`
	RestartCount   int32           `json:"restartCount"`
	StartedAt      metav1.Time     `json:"startedAt"`
}

func (w runtimePoolBootWitness) identity(kind string) store.ExternalEffectIdentity {
	return agentRuntimeRecoveryIdentity(kind, w.PoolUID, w.PoolNamespace, string(w.Fence.RuntimeInstanceID))
}

func runtimePoolBootWitnessDigest(w runtimePoolBootWitness) (string, error) {
	body, err := harnessv2.CanonicalValue(w)
	if err != nil {
		return "", err
	}
	return store.CanonicalBytesDigest(body), nil
}

// Only the common physical-lifetime fields are projected into the existing
// external-runtime validator. This does not create an AgentRuntime registration.
func (w runtimePoolBootWitness) physicalWitness() agentRuntimeBootWitness {
	return agentRuntimeBootWitness{
		Namespace: w.PodNamespace, PodName: w.PodName, PodUID: w.PodUID, PodSpecDigest: w.PodSpecDigest,
		ReplicaSetName: w.ReplicaSetName, ReplicaSetUID: w.ReplicaSetUID, ContainerName: runtimePoolBootContainerName,
		ContainerID: w.ContainerID, ImageID: w.ImageID, RestartCount: w.RestartCount, StartedAt: w.StartedAt,
		Spec: corev1alpha1.AgentRuntimeRegistrySpec{Capabilities: &corev1alpha1.AgentRuntimeCapabilitiesSpec{
			Profile: &corev1alpha1.AgentRuntimeProfileSpec{ProviderKind: w.Provider},
		}},
	}
}

func nativeRuntimePoolProvider(provider string) bool {
	switch provider {
	case runtimePoolProviderCodex, runtimePoolProviderClaude, runtimePoolProviderCopilot, runtimePoolProviderOpencode:
		return true
	default:
		return false
	}
}

func validateRuntimePoolBootWitness(w runtimePoolBootWitness, identity store.ExternalEffectIdentity, requestDigest string) error {
	digest, err := runtimePoolBootWitnessDigest(w)
	if err != nil {
		return err
	}
	if w.SchemaVersion != 1 || w.identity(identity.Kind) != identity || w.PoolNamespace == "" || w.PoolName == "" || w.PoolUID == "" ||
		!nativeRuntimePoolProvider(w.Provider) || w.Fence.Validate(false) != nil || string(w.Fence.RuntimePoolUID) != string(w.PoolUID) ||
		w.Fence.RuntimeSessionUID != "" || w.Fence.RuntimeSessionGeneration != 0 || w.Fence.ProfileDigestSchemaVersion != harnessv2.ProfileDigestSchemaVersion ||
		string(w.Fence.RuntimeInstanceID) != runtimePoolRuntimeInstanceID(w.PodUID, w.Fence.SupervisorBootID) ||
		w.PodNamespace == "" || w.PodName == "" || w.PodUID == "" || w.PodAddress == "" ||
		w.DeploymentName == "" || w.DeploymentUID == "" || w.ReplicaSetName == "" || w.ReplicaSetUID == "" ||
		w.ContainerID == "" || w.ImageID == "" || w.RestartCount < 0 || w.StartedAt.IsZero() ||
		store.ValidateCanonicalDigest("Pod specification", w.PodSpecDigest) != nil ||
		store.ValidateCanonicalDigest("template", w.TemplateDigest) != nil || digest != requestDigest {
		return fmt.Errorf("%w: native RuntimePool boot witness is invalid", store.ErrConflict)
	}
	return nil
}

func loadRuntimePoolBootWitness(ctx context.Context, effects store.ExternalEffectStore, identity store.ExternalEffectIdentity) (runtimePoolBootWitness, error) {
	var w runtimePoolBootWitness
	effect, err := readAgentRuntimeRecoveryEffect(ctx, effects, identity, &w)
	if err != nil {
		return w, err
	}
	return w, validateRuntimePoolBootWitness(w, identity, effect.RequestDigest)
}

func (r *RuntimePoolReconciler) runtimePoolRetirementFence(ctx context.Context) (store.ControllerEpochFence, error) {
	if r.ControlStore == nil || r.Epochs == nil || r.APIReader == nil {
		return store.ControllerEpochFence{}, errors.New("native RuntimePool retirement requires durable control, epoch and authoritative Pod reads")
	}
	fence, err := r.Epochs.CurrentFence(ctx)
	if err != nil {
		return fence, err
	}
	return fence, requireAgentRuntimeRecoveryFence(ctx, r.ControlStore, fence)
}

func (r *RuntimePoolReconciler) runtimePoolBootRecords(ctx context.Context, pool *corev1alpha1.RuntimePool, kind string) ([]runtimePoolBootWitness, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var effects corev1alpha1.ExternalEffectList
	if err := reader.List(ctx, &effects, client.InNamespace(pool.Namespace)); err != nil {
		return nil, err
	}
	var result []runtimePoolBootWitness
	for i := range effects.Items {
		e := &effects.Items[i]
		if e.Spec.Kind != kind || e.Spec.AggregateID != string(pool.UID) {
			continue
		}
		// An empty reservation cannot have retained a Pod or admitted work.
		if (e.Status.State == corev1alpha1.ExternalEffectControlState(store.ExternalEffectPending) || e.Status.State == "" && e.Status.Version == 0) && e.Status.Response == nil && e.Status.ResponseDigest == "" {
			continue
		}
		w, err := loadRuntimePoolBootWitness(ctx, r.ControlStore, agentRuntimeRecoveryIdentity(kind, pool.UID, pool.Namespace, e.Spec.OperationID))
		if err != nil {
			return nil, err
		}
		if w.PoolName != pool.Name {
			return nil, fmt.Errorf("%w: RuntimePool witness owner changed", store.ErrConflict)
		}
		result = append(result, w)
	}
	return result, nil
}

// observeNativeRuntimePoolBoot brackets the authenticated probe using uncached
// physical reads. The template marker opts in newly rendered native pools only;
// legacy templates use the separate authenticated-drain rollout path, never
// this new-admission path.
func (r *RuntimePoolReconciler) observeNativeRuntimePoolBoot(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, selected *corev1.Pod) (*runtimePoolBootWitness, error) {
	if pool.Spec.ExecutionWorkspace != nil {
		return nil, nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	pod := &corev1.Pod{}
	if err := reader.Get(ctx, client.ObjectKeyFromObject(selected), pod); err != nil {
		return nil, err
	}
	if pod.UID != selected.UID {
		return nil, fmt.Errorf("%w: selected runtime Pod UID changed", store.ErrConflict)
	}
	if pod.Annotations[runtimePoolBootEnrollmentAnnotation] == "" {
		return nil, fmt.Errorf("%w: an unenrolled native runtime Pod is cleanup-only", store.ErrNotReady)
	}
	if _, err := r.runtimePoolRetirementFence(ctx); err != nil {
		return nil, err
	}
	if pod.Annotations[runtimePoolBootEnrollmentAnnotation] != runtimePoolBootEnrollmentVersion ||
		!pod.DeletionTimestamp.IsZero() || pod.Namespace != cfg.namespace || pod.Status.PodIP == "" ||
		pod.Status.PodIP != selected.Status.PodIP || !nativeRuntimePoolProvider(cfg.profile.ProviderKind) {
		return nil, fmt.Errorf("%w: native runtime Pod enrollment identity changed", store.ErrConflict)
	}
	topology := &AgentRuntimeReconciler{Client: r.Client, APIReader: r.APIReader}
	if err := topology.validateAgentRuntimeRecoveryPodSpec(ctx, pod.Namespace, pod.Spec, runtimePoolBootContainerName, cfg.profile.ProviderKind); err != nil {
		return nil, err
	}
	if pod.Spec.Containers[0].Image != pool.Spec.Runtime.Image {
		return nil, fmt.Errorf("%w: native runtime image changed", store.ErrConflict)
	}
	deployment, rs, err := r.nativeRuntimePoolBootOwners(ctx, pool, cfg, pod)
	if err != nil {
		return nil, err
	}
	container, err := recoverySupervisorStatus(pod, runtimePoolBootContainerName)
	if err != nil {
		return nil, err
	}
	if container.ContainerID == "" || container.ImageID == "" || container.State.Running == nil || container.State.Running.StartedAt.IsZero() || !container.Ready {
		return nil, fmt.Errorf("%w: native runtime running container identity unavailable", store.ErrNotReady)
	}
	podDigest, err := recoveryPodSpecDigest(pod)
	if err != nil {
		return nil, err
	}
	return &runtimePoolBootWitness{
		SchemaVersion: 1, PoolNamespace: pool.Namespace, PoolName: pool.Name, PoolUID: pool.UID, Provider: cfg.profile.ProviderKind,
		PodNamespace: pod.Namespace, PodName: pod.Name, PodUID: pod.UID, PodAddress: pod.Status.PodIP, PodSpecDigest: podDigest,
		DeploymentName: deployment.Name, DeploymentUID: deployment.UID, ReplicaSetName: rs.Name, ReplicaSetUID: rs.UID,
		TemplateDigest: runtimePoolPodTemplateRevision(deployment.Spec.Template), ContainerID: container.ContainerID,
		ImageID: container.ImageID, RestartCount: container.RestartCount, StartedAt: container.State.Running.StartedAt,
	}, nil
}

// nativeRuntimePoolBootOwners validates the complete managed ownership chain,
// including cross-namespace pool labels and the controller template revision.
func (r *RuntimePoolReconciler) nativeRuntimePoolBootOwners(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, pod *corev1.Pod) (*appsv1.Deployment, *appsv1.ReplicaSet, error) {
	deployment := &appsv1.Deployment{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: cfg.namespace, Name: cfg.baseName}, deployment); err != nil {
		return nil, nil, err
	}
	if deployment.UID == "" || (deployment.Namespace == pool.Namespace && !metav1.IsControlledBy(deployment, pool)) || !deployment.DeletionTimestamp.IsZero() || deployment.Labels[runtimePoolUIDLabel] != string(pool.UID) ||
		pod.Labels[runtimePoolUIDLabel] != string(pool.UID) || pod.Labels[runtimePoolNamespaceLabel] != pool.Namespace || pod.Labels[runtimePoolNameLabel] != pool.Name ||
		deployment.Spec.Template.Annotations[runtimePoolBootEnrollmentAnnotation] != runtimePoolBootEnrollmentVersion {
		return nil, nil, fmt.Errorf("%w: native RuntimePool Deployment ownership changed", store.ErrConflict)
	}
	owner := metav1.GetControllerOf(pod)
	if owner == nil || owner.APIVersion != appsv1.SchemeGroupVersion.String() || owner.Kind != runtimePoolBootReplicaSetKind {
		return nil, nil, fmt.Errorf("%w: native runtime Pod lacks exact ReplicaSet ownership", store.ErrConflict)
	}
	rs := &appsv1.ReplicaSet{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: owner.Name}, rs); err != nil {
		return nil, nil, err
	}
	if rs.UID == "" || rs.UID != owner.UID || !metav1.IsControlledBy(rs, deployment) {
		return nil, nil, fmt.Errorf("%w: native runtime ReplicaSet ownership changed", store.ErrConflict)
	}
	// The revision was hashed before API defaulting. Compare its exact inherited
	// identity, not a rehash of the server-defaulted PodTemplateSpec.
	revision := deployment.Spec.Template.Annotations[runtimePoolTemplateRevisionAnnotation]
	if store.ValidateCanonicalDigest("runtime template revision", revision) != nil || pod.Annotations[runtimePoolTemplateRevisionAnnotation] != revision {
		return nil, nil, fmt.Errorf("%w: native RuntimePool template revision changed", store.ErrConflict)
	}
	if err := validateRecoveryPodTemplate(deployment, rs, pod); err != nil {
		return nil, nil, err
	}
	return deployment, rs, nil
}

func runtimePoolWitnessPod(w runtimePoolBootWitness, pod *corev1.Pod) (*corev1.ContainerStateTerminated, error) {
	if pod.Annotations[runtimePoolBootEnrollmentAnnotation] != runtimePoolBootEnrollmentVersion ||
		pod.Annotations[runtimePoolProfileAnnotation] != string(w.Fence.RuntimeProfileDigest) ||
		pod.Labels[runtimePoolUIDLabel] != string(w.PoolUID) || pod.Labels[runtimePoolNameLabel] != w.PoolName || pod.Labels[runtimePoolNamespaceLabel] != w.PoolNamespace {
		return nil, fmt.Errorf("%w: native runtime Pod enrollment ownership changed", store.ErrConflict)
	}
	return witnessedContainerTermination(w.physicalWitness(), pod)
}

func runtimePoolWitnessRunning(w runtimePoolBootWitness, pod *corev1.Pod) bool {
	status, err := recoverySupervisorStatus(pod, runtimePoolBootContainerName)
	return err == nil && status.ContainerID == w.ContainerID && status.ImageID == w.ImageID && status.RestartCount == w.RestartCount &&
		status.State.Running != nil && status.State.Running.StartedAt.Equal(&w.StartedAt)
}

func (r *RuntimePoolReconciler) retainRuntimePoolBoot(ctx context.Context, pool *corev1alpha1.RuntimePool, w runtimePoolBootWitness, fence store.ControllerEpochFence) error {
	return agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		current := &corev1alpha1.RuntimePool{}
		if err := r.APIReader.Get(writeCtx, client.ObjectKeyFromObject(pool), current); err != nil {
			return err
		}
		if current.UID != w.PoolUID || current.Spec.ExecutionWorkspace != nil || !controllerutil.ContainsFinalizer(current, runtimePoolFinalizer) {
			return fmt.Errorf("%w: original RuntimePool is not retained", store.ErrConflict)
		}
		pod := &corev1.Pod{}
		if err := r.APIReader.Get(writeCtx, client.ObjectKey{Namespace: w.PodNamespace, Name: w.PodName}, pod); err != nil {
			return err
		}
		terminal, err := runtimePoolWitnessPod(w, pod)
		if err != nil {
			return err
		}
		if terminal == nil && !runtimePoolWitnessRunning(w, pod) {
			return fmt.Errorf("%w: prepared runtime container lifetime changed", store.ErrConflict)
		}
		digest, err := runtimePoolBootWitnessDigest(w)
		if err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) && pod.Annotations[runtimePoolBootRetentionAnnotation] == digest {
			return nil
		}
		if controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) || pod.Annotations[runtimePoolBootRetentionAnnotation] != "" {
			return fmt.Errorf("%w: native Pod retention barrier was changed or removed", store.ErrConflict)
		}
		if uint64(current.Generation) != w.Fence.RuntimePoolGeneration || current.Spec.Runtime.Profile.Digest != string(w.Fence.RuntimeProfileDigest) {
			return fmt.Errorf("%w: native RuntimePool authority changed before first retention", store.ErrConflict)
		}
		if !current.DeletionTimestamp.IsZero() || !pod.DeletionTimestamp.IsZero() {
			return fmt.Errorf("%w: unretained runtime boot deletion is pending", store.ErrNotReady)
		}
		if current.ResourceVersion == "" || current.Annotations[runtimePoolBootOwnerVersionAnnotation] == current.ResourceVersion {
			return fmt.Errorf("%w: RuntimePool retention needs an advancing owner version", store.ErrConflict)
		}
		// Fence a delayed owner-finalizer removal before retaining its child.
		base := current.DeepCopy()
		if current.Annotations == nil {
			current.Annotations = map[string]string{}
		}
		current.Annotations[runtimePoolBootOwnerVersionAnnotation] = current.ResourceVersion
		if err := r.Patch(writeCtx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		pool.ResourceVersion = current.ResourceVersion
		basePod := pod.DeepCopy()
		if pod.Annotations == nil {
			pod.Annotations = map[string]string{}
		}
		pod.Annotations[runtimePoolBootRetentionAnnotation] = digest
		controllerutil.AddFinalizer(pod, runtimePoolBootPodFinalizer)
		return r.Patch(writeCtx, pod, client.MergeFromWithOptions(basePod, client.MergeFromWithOptimisticLock{}))
	})
}

func (r *RuntimePoolReconciler) enrollRuntimePoolBoot(ctx context.Context, pool *corev1alpha1.RuntimePool, cfg runtimePoolConfig, pod *corev1.Pod, before *runtimePoolBootWitness, status harnessv2.StatusResponse) error {
	if before == nil {
		return nil
	}
	fence, err := r.runtimePoolRetirementFence(ctx)
	if err != nil {
		return err
	}
	after, err := r.observeNativeRuntimePoolBoot(ctx, pool, cfg, pod)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(before, after) {
		return fmt.Errorf("%w: runtime container changed during authenticated probe", store.ErrConflict)
	}
	w := *before
	w.Fence = status.Fence
	digest, err := runtimePoolBootWitnessDigest(w)
	if err != nil {
		return err
	}
	if err := validateRuntimePoolBootWitness(w, w.identity(runtimePoolBootWitnessKind), digest); err != nil {
		return err
	}
	existing, loadErr := loadRuntimePoolBootWitness(ctx, r.ControlStore, w.identity(runtimePoolBootWitnessKind))
	if loadErr == nil {
		if !reflect.DeepEqual(existing, w) {
			return fmt.Errorf("%w: enrolled RuntimePool boot changed", store.ErrConflict)
		}
		retired, err := loadRuntimePoolBootRetirement(ctx, r.ControlStore, w)
		if err != nil {
			return err
		}
		if retired {
			if !upgradeDrainSupervisorIsQuiescent(status) {
				return fmt.Errorf("%w: retired RuntimePool boot cannot admit work", store.ErrNotReady)
			}
			// Its durable proof, not a released Pod finalizer, authorizes only
			// completion of the already-quiescent rotation/scale-down path.
			return nil
		}
		return r.requireRuntimePoolBootRetention(ctx, w)
	}
	if !errors.Is(loadErr, store.ErrNotFound) && !errors.Is(loadErr, store.ErrNotReady) {
		return loadErr
	}
	if !runtimeStatusIdle(&status) || pool.Status.ActiveInstance != nil && pool.Status.ActiveInstance.RuntimeInstanceID == string(w.Fence.RuntimeInstanceID) {
		return fmt.Errorf("%w: cannot first enroll a RuntimePool boot after admission", store.ErrConflict)
	}
	prior, err := r.runtimePoolBootRecords(ctx, pool, runtimePoolBootWitnessKind)
	if err != nil {
		return err
	}
	for _, old := range prior {
		if retired, err := loadRuntimePoolBootRetirement(ctx, r.ControlStore, old); err != nil || !retired {
			return errors.Join(fmt.Errorf("%w: prior RuntimePool boot lacks retirement proof", store.ErrNotReady), err)
		}
	}
	if err := persistAgentRuntimeRecoveryEffect(ctx, r.ControlStore, fence, w.identity(runtimePoolBootPreparationKind), digest, w); err != nil {
		return err
	}
	if err := r.retainRuntimePoolBoot(ctx, pool, w, fence); err != nil {
		return err
	}
	return persistAgentRuntimeRecoveryEffect(ctx, r.ControlStore, fence, w.identity(runtimePoolBootWitnessKind), digest, w)
}

func (r *RuntimePoolReconciler) requireRuntimePoolBootRetention(ctx context.Context, w runtimePoolBootWitness) error {
	pod := &corev1.Pod{}
	if err := r.APIReader.Get(ctx, client.ObjectKey{Namespace: w.PodNamespace, Name: w.PodName}, pod); err != nil {
		return err
	}
	if _, err := runtimePoolWitnessPod(w, pod); err != nil {
		return err
	}
	digest, err := runtimePoolBootWitnessDigest(w)
	if err != nil {
		return err
	}
	if !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) || pod.Annotations[runtimePoolBootRetentionAnnotation] != digest {
		return fmt.Errorf("%w: enrolled runtime Pod lost its retention barrier", store.ErrConflict)
	}
	return nil
}

func (r *RuntimePoolReconciler) resumeRuntimePoolBootPreparations(ctx context.Context, pool *corev1alpha1.RuntimePool, preparations []runtimePoolBootWitness, fence store.ControllerEpochFence) error {
	for _, w := range preparations {
		existing, err := loadRuntimePoolBootWitness(ctx, r.ControlStore, w.identity(runtimePoolBootWitnessKind))
		if err == nil {
			if !reflect.DeepEqual(existing, w) {
				return fmt.Errorf("%w: prepared and committed RuntimePool witnesses differ", store.ErrConflict)
			}
			continue
		}
		if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrNotReady) {
			return err
		}
		if discarded, err := r.discardSupersededRuntimePoolBootPreparation(ctx, pool, w, fence); err != nil || discarded {
			if err != nil {
				return err
			}
			continue
		}
		pod := &corev1.Pod{}
		err = r.APIReader.Get(ctx, client.ObjectKey{Namespace: w.PodNamespace, Name: w.PodName}, pod)
		if apierrors.IsNotFound(err) {
			continue
		} // Uncommitted: never admitted, and no retirement claim.
		if err != nil {
			return err
		}
		if pod.UID != w.PodUID {
			return fmt.Errorf("%w: prepared runtime Pod name reused", store.ErrConflict)
		}
		if !controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) && pod.Annotations[runtimePoolBootRetentionAnnotation] == "" && (!pod.DeletionTimestamp.IsZero() || !pool.DeletionTimestamp.IsZero()) {
			continue
		}
		if err := r.retainRuntimePoolBoot(ctx, pool, w, fence); err != nil {
			return err
		}
		digest, err := runtimePoolBootWitnessDigest(w)
		if err != nil {
			return err
		}
		if err := persistAgentRuntimeRecoveryEffect(ctx, r.ControlStore, fence, w.identity(runtimePoolBootWitnessKind), digest, w); err != nil {
			return err
		}
	}
	return nil
}

// discardSupersededRuntimePoolBootPreparation withdraws only a never-enrolled,
// unretained source. It does NOT certify process death, publish a boot witness,
// remove a finalizer, or create a cleanup receipt. Retain the immutable
// preparation for audit; replay ignores its absent/deleting original Pod.
// Obsolescence may be a newer pool generation or a positively observed different
// container boot. Neither case is evidence that an admitted process died.
func (r *RuntimePoolReconciler) discardSupersededRuntimePoolBootPreparation(ctx context.Context, pool *corev1alpha1.RuntimePool, w runtimePoolBootWitness, fence store.ControllerEpochFence) (bool, error) {
	discarded := false
	err := agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
		current := &corev1alpha1.RuntimePool{}
		if err := r.APIReader.Get(writeCtx, client.ObjectKeyFromObject(pool), current); err != nil {
			return err
		}
		if current.UID != w.PoolUID || current.Spec.ExecutionWorkspace != nil || !controllerutil.ContainsFinalizer(current, runtimePoolFinalizer) {
			return fmt.Errorf("%w: superseded native preparation owner changed", store.ErrConflict)
		}
		if uint64(current.Generation) < w.Fence.RuntimePoolGeneration || current.Status.ActiveInstance != nil || !runtimePoolRolloutControllerWorkIsQuiescent(current.Status.Capacity) {
			return nil // Any admission/reservation authority remains unresolved.
		}
		if _, err := loadRuntimePoolBootWitness(writeCtx, r.ControlStore, w.identity(runtimePoolBootWitnessKind)); err == nil {
			return nil // Publication won the race: use normal retained recovery.
		} else if !errors.Is(err, store.ErrNotFound) && !errors.Is(err, store.ErrNotReady) {
			return err
		}
		pod := &corev1.Pod{}
		if err := r.APIReader.Get(writeCtx, client.ObjectKey{Namespace: w.PodNamespace, Name: w.PodName}, pod); err != nil {
			if apierrors.IsNotFound(err) {
				discarded = true
				return nil
			}
			return err
		}
		if _, err := runtimePoolWitnessPod(w, pod); err != nil {
			return err
		}
		if controllerutil.ContainsFinalizer(pod, runtimePoolBootPodFinalizer) {
			return nil
		}
		if pod.Annotations[runtimePoolBootRetentionAnnotation] != "" {
			return fmt.Errorf("%w: superseded native preparation lost its retention barrier", store.ErrConflict)
		}
		if uint64(current.Generation) == w.Fence.RuntimePoolGeneration && !runtimePoolPreparedContainerChanged(w, pod) {
			return nil
		}
		if !pod.DeletionTimestamp.IsZero() {
			discarded = true
			return nil
		}
		if pod.ResourceVersion == "" {
			return fmt.Errorf("%w: superseded native Pod requires an exact version", store.ErrConflict)
		}
		deployment := &appsv1.Deployment{}
		if err := r.APIReader.Get(writeCtx, client.ObjectKey{Namespace: w.PodNamespace, Name: w.DeploymentName}, deployment); err != nil {
			return err
		}
		if deployment.UID != w.DeploymentUID || deployment.ResourceVersion == "" || deployment.Labels[runtimePoolUIDLabel] != string(w.PoolUID) || runtimePoolPodTemplateRevision(deployment.Spec.Template) != w.TemplateDigest {
			return fmt.Errorf("%w: superseded native preparation Deployment changed", store.ErrConflict)
		}
		// Stop the exact obsolete template before deleting the unadmitted Pod,
		// so its ReplicaSet cannot race rollout with another unadmitted boot.
		if deployment.Spec.Replicas == nil || *deployment.Spec.Replicas != 0 {
			base := deployment.DeepCopy()
			deployment.Spec.Replicas = new(int32(0))
			if err := r.Patch(writeCtx, deployment, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
				return err
			}
		}
		// A delayed retention PATCH either changes this version and defeats
		// deletion, or loses to deletion and cannot add a new finalizer.
		if err := r.Delete(writeCtx, pod, deleteCurrentObjectPreconditions(pod)...); err != nil && !apierrors.IsNotFound(err) {
			return err
		}
		discarded = true
		return nil
	})
	return discarded, err
}

// runtimePoolPreparedContainerChanged proves only that this unadmitted
// preparation cannot enroll the currently observed container. It deliberately
// does not supply termination evidence for the original container lifetime.
func runtimePoolPreparedContainerChanged(w runtimePoolBootWitness, pod *corev1.Pod) bool {
	status, err := recoverySupervisorStatus(pod, runtimePoolBootContainerName)
	if err != nil || status.ContainerID == "" || status.ContainerID == w.ContainerID ||
		status.ImageID != w.ImageID || status.RestartCount <= w.RestartCount {
		return false
	}
	if running := status.State.Running; running != nil {
		return running.StartedAt.After(w.StartedAt.Time)
	}
	for _, terminal := range []*corev1.ContainerStateTerminated{status.State.Terminated, status.LastTerminationState.Terminated} {
		if terminal != nil && terminal.ContainerID == status.ContainerID && terminal.StartedAt.After(w.StartedAt.Time) &&
			!terminal.FinishedAt.IsZero() && !terminal.FinishedAt.Before(&terminal.StartedAt) {
			return true
		}
	}
	return false
}

// runtimePoolTaskWitnessMatches is shared by read-only historical cleanup and
// eager Task receipts. It never compares old proof with the replacement pool.
func runtimePoolTaskWitnessMatches(task *corev1alpha1.Task, taskUID types.UID, w runtimePoolBootWitness) error {
	e := task.Status.Execution
	binding := executionBinding(task, corev1alpha1.AgentRuntimeContractHarnessV2)
	if e == nil || binding == nil || binding.Backend != corev1alpha1.AgentExecutionBackendRuntimePool || binding.Task.UID != taskUID ||
		task.Namespace != w.PoolNamespace || e.RuntimePoolName != w.PoolName || e.RuntimePoolUID != string(w.PoolUID) ||
		e.AgentRuntimeName != "" || e.AgentRuntimeUID != "" || e.RuntimeInstanceID != string(w.Fence.RuntimeInstanceID) ||
		e.RuntimeSessionSupervisorBootID != string(w.Fence.SupervisorBootID) || e.Attempt < 1 || strings.TrimSpace(e.RuntimeSessionUID) == "" || e.RuntimeSessionGeneration < 1 ||
		binding.RuntimeProfileDigest != string(w.Fence.RuntimeProfileDigest) ||
		(e.RuntimeSessionProfileDigest != string(w.Fence.RuntimeProfileDigest) && (task.Spec.SessionRef != nil || e.RuntimeSessionProfileDigest != "")) {
		return fmt.Errorf("%w: Task does not match the exact retired RuntimePool boot", store.ErrConflict)
	}
	digest, err := canonicalAgentExecutionBindingDigest(*binding)
	if err != nil || digest != binding.BindingDigest {
		return fmt.Errorf("%w: retired RuntimePool Task binding integrity failed", store.ErrConflict)
	}
	return nil
}
