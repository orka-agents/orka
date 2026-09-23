package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The replacement is an authenticated relay only. Its observation is kept
// with the proof, but it cannot become an admitted boot until old retirement
// has committed. Container death alone never proves remote Foundry retirement.
type agentRuntimeFoundryRetirement struct {
	Relay    agentRuntimeBootWitness                 `json:"relay"`
	IssuedAt time.Time                               `json:"issuedAt"`
	Request  harnessv2.FoundryBootRetirementRequest  `json:"request"`
	Response harnessv2.FoundryBootRetirementResponse `json:"response"`
}

func validateAgentRuntimeFoundryRelay(old, relay agentRuntimeBootWitness) error {
	// A rollout changes the template and container image. The replacement is
	// independently authenticated and revalidated only as a retirement relay;
	// it never inherits the old boot's admission or execution authority.
	if recoveryProviderKind(old.Spec) != agentRuntimeFoundryProvider || old.FoundryBroker == nil ||
		old.FoundryBroker.Validate() != nil || recoveryProviderKind(relay.Spec) != agentRuntimeFoundryProvider ||
		relay.FoundryBroker == nil || *old.FoundryBroker != *relay.FoundryBroker ||
		old.Namespace != relay.Namespace || old.RuntimeName != relay.RuntimeName || old.RuntimeUID != relay.RuntimeUID ||
		old.DeploymentName != relay.DeploymentName || old.DeploymentUID != relay.DeploymentUID ||
		old.ContainerName != relay.ContainerName ||
		old.ServiceName != relay.ServiceName || old.ServiceUID != relay.ServiceUID ||
		old.Spec.Deployment.Endpoint != relay.Spec.Deployment.Endpoint ||
		old.Fence.SupervisorBootID == relay.Fence.SupervisorBootID || old.ContainerID == relay.ContainerID {
		return fmt.Errorf("%w: Foundry retirement relay does not match the witnessed runtime and broker", store.ErrConflict)
	}
	digest, err := runtimeWitnessDigest(relay)
	if err != nil {
		return err
	}
	return validateAgentRuntimeBootWitness(relay, old.Namespace, old.RuntimeUID, relay.Fence.SupervisorBootID, digest)
}

func validateAgentRuntimeFoundryRetirement(witness agentRuntimeBootWitness, proof agentRuntimeBootRetirement) error {
	retirement := proof.FoundryRetirement
	if proof.DrainedStatus != nil || retirement == nil || !validWitnessContainerTermination(witness, proof.ContainerTermination) {
		return fmt.Errorf("%w: Foundry retirement requires exact local and remote proof", store.ErrConflict)
	}
	if err := validateAgentRuntimeFoundryRelay(witness, retirement.Relay); err != nil {
		return err
	}
	request := retirement.Request
	if retirement.IssuedAt.IsZero() || retirement.IssuedAt.Before(proof.ContainerTermination.FinishedAt.Time) ||
		request.RetiredFence != witness.Fence || request.Metadata.Fence != retirement.Relay.Fence ||
		request.Broker != *witness.FoundryBroker {
		return fmt.Errorf("%w: Foundry retirement proof subject changed", store.ErrConflict)
	}
	// Durable proof outlives its transport capability. Validate the exact
	// original issuance time instead of requiring an unexpired request now.
	if err := request.ValidateAt(retirement.IssuedAt); err != nil {
		return fmt.Errorf("%w: invalid Foundry retirement request: %v", store.ErrConflict, err)
	}
	if err := retirement.Response.ValidateFor(request); err != nil {
		return fmt.Errorf("%w: invalid Foundry retirement response: %v", store.ErrConflict, err)
	}
	return nil
}

func (r *AgentRuntimeReconciler) observeFoundryContainerRetirement(
	ctx context.Context,
	witness agentRuntimeBootWitness,
	terminal *corev1.ContainerStateTerminated,
	owner store.ControllerEpochFence,
) (bool, error) {
	if witness.FoundryBroker == nil {
		return false, errors.New("foundry boot has no broker identity witnessed before admission")
	}
	runtime := &corev1alpha1.AgentRuntime{}
	if err := r.endpointReader().Get(ctx, client.ObjectKey{Namespace: witness.Namespace, Name: witness.RuntimeName}, runtime); err != nil {
		return false, err
	}
	if runtime.UID != witness.RuntimeUID {
		return false, errors.New("foundry retirement registration authority changed")
	}
	backend, err := r.recoveryBackend(ctx, runtime)
	if err != nil {
		return false, err
	}
	relay, err := r.authenticateRecoveryBoot(ctx, runtime, backend)
	if err != nil {
		return false, err
	}
	if err := validateAgentRuntimeFoundryRelay(witness, relay.witness); err != nil {
		return false, err
	}
	if !runtimeStatusIdle(relay.status) || relay.witness.Fence.ControllerEpoch > uint64(owner.Epoch) {
		return false, errors.New("foundry retirement relay must be idle and no newer than the current controller")
	}
	validate := func(checkCtx context.Context) error {
		return r.revalidateFoundryRetirement(checkCtx, runtime, backend, witness, relay, terminal, owner)
	}
	retirementClient, err := harnessv2.NewClient(runtime.Spec.Deployment.Endpoint,
		harnessv2.WithControllerBearerToken(relay.auth.controllerBearerToken),
		harnessv2.WithOperationCapabilitySecret(relay.auth.operationCapabilitySecret),
		harnessv2.WithHTTPClient(externalRuntimeHTTPClient(PinnedBackendDialTransport(relay.witness.Pins))),
		harnessv2.WithControlTimeout(30*time.Second),
		harnessv2.WithBeforeMutation(func(checkCtx context.Context, operation string) error {
			if operation != harnessv2.FoundryBootRetirementOperation {
				return errors.New("foundry recovery relay cannot perform admission or execution")
			}
			return validate(checkCtx)
		}))
	if err != nil {
		return false, err
	}
	issuedAt := time.Now().UTC()
	request := harnessv2.FoundryBootRetirementRequest{
		Protocol: harnessv2.ProtocolVersion,
		Metadata: harnessv2.MutationMetadata{
			Fence: relay.witness.Fence,
			OperationID: harnessv2.OperationID(store.CanonicalControlID("foundry-boot-retirement", string(witness.RuntimeUID),
				string(witness.Fence.SupervisorBootID), string(relay.witness.Fence.SupervisorBootID), issuedAt.Format(time.RFC3339Nano))),
			RequestDigestSchemaVersion: harnessv2.RequestDigestSchemaVersion,
			ExpiresAt:                  issuedAt.Add(time.Minute),
		},
		RetiredFence: witness.Fence, Broker: *witness.FoundryBroker,
	}
	if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
		return false, err
	}
	response, err := retirementClient.RetireFoundryBoot(ctx, request)
	if err != nil {
		return false, err
	}
	proof := agentRuntimeBootRetirement{
		Kind: "foundry-broker-retirement", ContainerTermination: terminal,
		FoundryRetirement: &agentRuntimeFoundryRetirement{Relay: relay.witness, IssuedAt: issuedAt, Request: request, Response: *response},
	}
	if err := validateAgentRuntimeFoundryRetirement(witness, proof); err != nil {
		return false, err
	}
	if err := validate(ctx); err != nil {
		return false, err
	}
	// The control store guards both reservation and immutable proof commit by
	// the current epoch. This never claims a prompt or an execution result.
	if err := r.persistBootRetirement(ctx, witness, proof, owner); err != nil {
		return false, err
	}
	return true, nil
}

func (r *AgentRuntimeReconciler) revalidateFoundryRetirement(
	ctx context.Context,
	runtime *corev1alpha1.AgentRuntime,
	backend *agentRuntimeRecoveryBackend,
	witness agentRuntimeBootWitness,
	relay *agentRuntimeBootObservation,
	terminal *corev1.ContainerStateTerminated,
	owner store.ControllerEpochFence,
) error {
	currentOwner, err := r.ControllerEpochManager.CurrentFence(ctx)
	if err != nil || currentOwner != owner {
		return errors.New("foundry retirement controller authority changed")
	}
	if err := requireAgentRuntimeRecoveryFence(ctx, r.ControlStore, owner); err != nil {
		return err
	}
	original, err := loadAgentRuntimeBootWitness(ctx, r.ControlStore, witness.Namespace, witness.RuntimeUID, witness.Fence.SupervisorBootID)
	if err != nil || !reflect.DeepEqual(original, witness) {
		return errors.Join(errors.New("foundry retirement requires the original immutable boot witness"), err)
	}
	current := &corev1alpha1.AgentRuntime{}
	if err := r.endpointReader().Get(ctx, client.ObjectKeyFromObject(runtime), current); err != nil {
		return err
	}
	if current.UID != runtime.UID || current.Generation != runtime.Generation || !reflect.DeepEqual(current.Spec, runtime.Spec) {
		return errors.New("foundry retirement registration changed during relay")
	}
	pod := &corev1.Pod{}
	if err := r.endpointReader().Get(ctx, client.ObjectKey{Namespace: witness.Namespace, Name: witness.PodName}, pod); err != nil {
		return err
	}
	observedTermination, err := witnessedContainerTermination(witness, pod)
	if err != nil || !reflect.DeepEqual(observedTermination, terminal) {
		return errors.Join(errors.New("foundry retirement lost its exact container termination"), err)
	}
	observed, err := r.authenticateRecoveryBoot(ctx, current, backend)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(observed.witness, relay.witness) || !runtimeStatusIdle(observed.status) {
		return errors.New("foundry retirement relay authority or idle state changed")
	}
	return requireAgentRuntimeRecoveryFence(ctx, r.ControlStore, owner)
}
