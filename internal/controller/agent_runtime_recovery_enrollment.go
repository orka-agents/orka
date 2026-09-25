package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	agentRuntimePreparedWitnessAnnotation  = "orka.ai/agent-runtime-boot-witness"
	agentRuntimePreparedOwnerAnnotation    = "orka.ai/agent-runtime-prepared-owner"
	agentRuntimeRetentionVersionAnnotation = "orka.ai/agent-runtime-retention-version"
	agentRuntimeBootPreparationKind        = "agent-runtime-boot-preparation"
)

// preparedRecoveryPodWitness reads only an observation whose exact digest was
// reserved before its atomic Pod retention patch. Pod metadata alone cannot
// create a recovery owner or alter the authenticated observation.
func (r *AgentRuntimeReconciler) preparedRecoveryPodWitness(ctx context.Context, runtime *corev1alpha1.AgentRuntime, pod *corev1.Pod) (agentRuntimeBootWitness, error) {
	var witness agentRuntimeBootWitness
	body := []byte(pod.Annotations[agentRuntimePreparedWitnessAnnotation])
	if err := json.Unmarshal(body, &witness); err != nil {
		return witness, errors.New("prepared runtime witness is not valid JSON")
	}
	canonical, err := harnessv2.CanonicalValue(witness)
	if err != nil || !bytes.Equal(canonical, body) {
		return witness, errors.New("prepared runtime witness is not canonical")
	}
	if pod.Annotations[agentRuntimePreparedOwnerAnnotation] != string(runtime.UID) ||
		!controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) ||
		witness.RuntimeUID != runtime.UID || witness.RuntimeName != runtime.Name || witness.Namespace != runtime.Namespace {
		return witness, errors.New("prepared runtime witness retention or owner changed")
	}
	if _, err := witnessedContainerTermination(witness, pod); err != nil {
		return witness, err
	}
	identity := agentRuntimeRecoveryIdentity(agentRuntimeBootWitnessKind, runtime.UID, runtime.Namespace, string(witness.Fence.SupervisorBootID))
	var prepared agentRuntimeBootWitness
	preparationIdentity := identity
	preparationIdentity.Kind = agentRuntimeBootPreparationKind
	effect, err := readAgentRuntimeRecoveryEffect(ctx, r.ControlStore, preparationIdentity, &prepared)
	if errors.Is(err, store.ErrNotFound) {
		// Legacy witness-first enrollment has no preparation record.
		effect, err = readAgentRuntimeRecoveryEffect(ctx, r.ControlStore, identity, &prepared)
	}
	if err != nil {
		return witness, err
	}
	if err := validateAgentRuntimeBootWitness(witness, runtime.Namespace, runtime.UID, witness.Fence.SupervisorBootID, effect.RequestDigest); err != nil {
		return witness, err
	}
	if !bytes.Equal(effect.Response, body) {
		return witness, errors.New("prepared runtime witness differs from its durable observation")
	}
	return witness, nil
}

func (r *AgentRuntimeReconciler) preparedRecoveryBoots(ctx context.Context, runtime *corev1alpha1.AgentRuntime) ([]agentRuntimeBootWitness, error) {
	var effects corev1alpha1.ExternalEffectList
	if err := r.endpointReader().List(ctx, &effects, client.InNamespace(runtime.Namespace)); err != nil {
		return nil, err
	}
	var result []agentRuntimeBootWitness
	for i := range effects.Items {
		effect := &effects.Items[i]
		if effect.Spec.Kind != agentRuntimeBootPreparationKind || effect.Spec.AggregateID != string(runtime.UID) {
			continue
		}
		var prepared agentRuntimeBootWitness
		observation, err := readAgentRuntimeRecoveryEffect(ctx, r.ControlStore,
			agentRuntimeRecoveryIdentity(agentRuntimeBootPreparationKind, runtime.UID, runtime.Namespace, effect.Spec.OperationID), &prepared)
		if errors.Is(err, store.ErrNotReady) {
			// Pod retention starts only after preparation publication succeeds.
			continue
		}
		if err != nil {
			return nil, err
		}
		if err := validateAgentRuntimeBootWitness(prepared, runtime.Namespace, runtime.UID,
			harnessv2.SupervisorBootID(effect.Spec.OperationID), observation.RequestDigest); err != nil {
			return nil, err
		}
		result = append(result, prepared)
	}
	return result, nil
}

func (r *AgentRuntimeReconciler) originalPreparedRecoveryPod(ctx context.Context, witness agentRuntimeBootWitness) (*corev1.Pod, error) {
	pod := &corev1.Pod{}
	err := r.endpointReader().Get(ctx, client.ObjectKey{Namespace: witness.Namespace, Name: witness.PodName}, pod)
	if apierrors.IsNotFound(err) || err == nil && pod.UID != witness.PodUID {
		// This only discharges an unpublished preparation. A committed boot
		// still needs its independent positive retirement proof.
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if _, err := witnessedContainerTermination(witness, pod); err != nil {
		return nil, err
	}
	return pod, nil
}

// requireLegacyRecoveryPodRetention permits an interrupted rollover to fill
// annotations that the witness-first implementation never wrote. An exact
// retired witness without any preparation distinguishes that legacy format
// from missing metadata on a newly retained Pod.
func (r *AgentRuntimeReconciler) requireLegacyRecoveryPodRetention(ctx context.Context, runtime *corev1alpha1.AgentRuntime, pod *corev1.Pod) error {
	witnesses, err := r.recoveryWitnesses(ctx, runtime)
	if err != nil {
		return err
	}
	for _, old := range witnesses {
		if old.PodUID != pod.UID {
			continue
		}
		var prepared agentRuntimeBootWitness
		_, err := readAgentRuntimeRecoveryEffect(ctx, r.ControlStore,
			agentRuntimeRecoveryIdentity(agentRuntimeBootPreparationKind, old.RuntimeUID, old.Namespace, string(old.Fence.SupervisorBootID)), &prepared)
		if err == nil || errors.Is(err, store.ErrNotReady) {
			continue
		}
		if !errors.Is(err, store.ErrNotFound) {
			return err
		}
		if _, err := witnessedContainerTermination(old, pod); err != nil {
			return err
		}
		if retired, err := loadAgentRuntimeBootRetirement(ctx, r.ControlStore, old); err != nil || !retired {
			return errors.Join(errors.New("legacy retained Pod has an unretired original boot"), err)
		}
		return nil
	}
	return errors.New("retained Pod has no exact retired legacy witness")
}

// resumePreparedRecoveryPods discovers exact original Pods from durable
// preparation, including missing metadata and writes that completed late.
func (r *AgentRuntimeReconciler) resumePreparedRecoveryPods(ctx context.Context, runtime *corev1alpha1.AgentRuntime, fence store.ControllerEpochFence) error {
	preparations, err := r.preparedRecoveryBoots(ctx, runtime)
	if err != nil {
		return err
	}
	for _, witness := range preparations {
		committed, err := loadAgentRuntimeBootWitness(ctx, r.ControlStore, runtime.Namespace, runtime.UID, witness.Fence.SupervisorBootID)
		if err == nil {
			if !reflect.DeepEqual(committed, witness) {
				return errors.New("prepared runtime boot differs from its published witness")
			}
			continue
		}
		if !errors.Is(err, store.ErrNotReady) && !errors.Is(err, store.ErrNotFound) {
			return err
		}
		pod, err := r.originalPreparedRecoveryPod(ctx, witness)
		if err != nil {
			return err
		}
		if pod == nil {
			continue
		}
		retain := !controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer)
		if retain {
			if pod.Annotations[agentRuntimePreparedWitnessAnnotation] != "" || pod.Annotations[agentRuntimePreparedOwnerAnnotation] != "" {
				return errors.New("prepared runtime Pod lost its retention finalizer")
			}
		} else if pod.Annotations[agentRuntimePreparedWitnessAnnotation] == "" && pod.Annotations[agentRuntimePreparedOwnerAnnotation] == "" {
			if err := r.requireLegacyRecoveryPodRetention(ctx, runtime, pod); err != nil {
				return err
			}
			retain = true
		}
		if retain {
			if err := agentRuntimeRecoveryGuard(ctx, r.ControlStore, fence, func(writeCtx context.Context) error {
				return r.writeRecoveryPodRetention(writeCtx, runtime, witness)
			}); err != nil {
				return err
			}
			pod, err = r.originalPreparedRecoveryPod(ctx, witness)
			if err != nil || pod == nil {
				return errors.Join(errors.New("original prepared Pod vanished during retention"), err)
			}
		}
		if _, err := r.preparedRecoveryPodWitness(ctx, runtime, pod); err != nil {
			return err
		}
		digest, err := runtimeWitnessDigest(witness)
		if err != nil {
			return err
		}
		if err := persistAgentRuntimeRecoveryEffect(ctx, r.ControlStore, fence,
			agentRuntimeRecoveryIdentity(agentRuntimeBootWitnessKind, runtime.UID, runtime.Namespace, string(witness.Fence.SupervisorBootID)), digest, witness); err != nil {
			return err
		}
	}
	return nil
}

// requireSettledRecoveryPreparations is read-only and runs inside the final
// runtime-owner removal guard. A preparation added after the earlier scan must
// not leave a late Pod PATCH able to install an orphaned finalizer.
func (r *AgentRuntimeReconciler) requireSettledRecoveryPreparations(ctx context.Context, runtime *corev1alpha1.AgentRuntime) error {
	preparations, err := r.preparedRecoveryBoots(ctx, runtime)
	if err != nil {
		return err
	}
	for _, witness := range preparations {
		committed, witnessErr := loadAgentRuntimeBootWitness(ctx, r.ControlStore, runtime.Namespace, runtime.UID, witness.Fence.SupervisorBootID)
		if witnessErr == nil {
			if !reflect.DeepEqual(committed, witness) {
				return errors.New("prepared runtime boot differs from its published witness")
			}
			if retired, err := loadAgentRuntimeBootRetirement(ctx, r.ControlStore, witness); err != nil || !retired {
				return errors.Join(fmt.Errorf("%w: prepared runtime boot is not retired", store.ErrNotReady), err)
			}
		} else if !errors.Is(witnessErr, store.ErrNotReady) && !errors.Is(witnessErr, store.ErrNotFound) {
			return witnessErr
		}
		if witnessErr != nil && !controllerutil.ContainsFinalizer(runtime, agentRuntimeFinalizer) {
			// Owner removal may have won before a delayed preparation was
			// published. It fences every new retention write. Only a still
			// unretained, unpublished preparation can be abandoned here.
			if err := r.requireUnretainedRecoveryPreparation(ctx, witness); err != nil {
				return err
			}
			continue
		}
		pod, err := r.originalPreparedRecoveryPod(ctx, witness)
		if err != nil {
			return err
		}
		if pod != nil && (witnessErr != nil || controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer)) {
			return fmt.Errorf("%w: original prepared Pod retention is unresolved", store.ErrNotReady)
		}
	}
	return nil
}

func (r *AgentRuntimeReconciler) requireUnretainedRecoveryPreparation(ctx context.Context, witness agentRuntimeBootWitness) error {
	pod := &corev1.Pod{}
	err := r.endpointReader().Get(ctx, client.ObjectKey{Namespace: witness.Namespace, Name: witness.PodName}, pod)
	if apierrors.IsNotFound(err) || err == nil && pod.UID != witness.PodUID {
		return nil
	}
	if err != nil {
		return err
	}
	if controllerutil.ContainsFinalizer(pod, agentRuntimeRecoveryPodFinalizer) ||
		pod.Annotations[agentRuntimePreparedWitnessAnnotation] != "" || pod.Annotations[agentRuntimePreparedOwnerAnnotation] != "" {
		return fmt.Errorf("%w: released runtime still has original prepared Pod retention", store.ErrNotReady)
	}
	return nil
}
