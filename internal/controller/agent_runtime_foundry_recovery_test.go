package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	storekube "github.com/orka-agents/orka/internal/store/kube"
	"github.com/orka-agents/orka/internal/store/sqlite"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

func TestAgentRuntimeFoundryRecoveryRequiresBrokerWitnessBeforeAdmission(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	configureFoundryRuntimeRecoveryFixture(t, f)
	// Advertise qualified recovery while removing only the fixture status
	// proxy that supplies the required authenticated broker observation.
	capabilities := newExternalRuntimeCapabilitiesProxy(t, f.server.URL(), func(response *harnessv2.CapabilitiesResponse) {
		response.SupportsFoundryRecovery = true
	})
	f.slice.Ports[0].Port = new(runtimeRecoveryServerPort(t, capabilities.URL))
	if err := f.r.Update(t.Context(), f.slice); err != nil {
		t.Fatal(err)
	}
	f.updateServiceTargetPort(t)
	before := f.server.Counts()
	f.reconcile(t)
	if f.runtime.Status.Ready || f.server.Counts() != before {
		t.Fatal("Foundry recovery admitted work without broker identity")
	}
	if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, f.server.Fence().SupervisorBootID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("missing broker identity created an immutable boot witness: %v", err)
	}
}

func TestAgentRuntimeFoundryRecoveryCombinesLocalAndBrokerProof(t *testing.T) {
	f := newFoundryRetirementFixture(t)
	witness := f.witness(t)
	originalDigest, err := runtimeWitnessDigest(witness)
	if err != nil {
		t.Fatal(err)
	}
	f.restartContainer(t, true, false)
	before := f.server.Counts()
	var earlyAdmission atomic.Bool
	relay := installFoundryRetirementRelay(t, f, foundryRetirementRelayOptions{
		beforeResponse: func() error {
			if f.server.Counts() != before {
				earlyAdmission.Store(true)
			}
			return nil
		},
	})
	f.reconcile(t)
	if !f.runtime.Status.Ready || relay.calls.Load() != 1 || earlyAdmission.Load() {
		t.Fatalf("combined proof did not gate replacement conformance: ready=%t calls=%d earlyAdmission=%t message=%s", f.runtime.Status.Ready, relay.calls.Load(), earlyAdmission.Load(), f.runtime.Status.Message)
	}
	proof := readFoundryRetirementProof(t, f, witness)
	if proof.Kind != "foundry-broker-retirement" || !validWitnessContainerTermination(witness, proof.ContainerTermination) ||
		proof.FoundryRetirement == nil || proof.FoundryRetirement.Request.RetiredFence != witness.Fence ||
		proof.FoundryRetirement.Response.Proof.OwnerCount != 1 {
		t.Fatal("combined retirement lost local termination or exact broker proof")
	}
	if proof.FoundryRetirement.Relay.Fence != f.server.Fence() || *proof.FoundryRetirement.Relay.FoundryBroker != *witness.FoundryBroker {
		t.Fatal("retirement was not relayed by the exact authenticated replacement")
	}
	if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, f.runtime.Namespace, f.runtime.UID, f.server.Fence().SupervisorBootID); err != nil {
		t.Fatalf("replacement was not enrolled after proof committed: %v", err)
	}
	original, err := loadAgentRuntimeBootWitness(t.Context(), f.control, witness.Namespace, witness.RuntimeUID, witness.Fence.SupervisorBootID)
	if err != nil {
		t.Fatal(err)
	}
	if digest, err := runtimeWitnessDigest(original); err != nil || digest != originalDigest {
		t.Fatalf("retirement rewrote its original witness: %v", err)
	}
	if retired, err := f.r.observeContainerRetirement(t.Context(), witness, f.fence); err != nil || !retired || relay.calls.Load() != 1 {
		t.Fatalf("durable retirement was not idempotent: retired=%t calls=%d err=%v", retired, relay.calls.Load(), err)
	}
}

func TestAgentRuntimeFoundryRecoveryAfterImageRollout(t *testing.T) {
	for _, change := range []string{"none", "replacement template", "replacement Pod"} {
		t.Run(change, func(t *testing.T) {
			f := newFoundryRetirementFixture(t)
			witness := f.witness(t)
			rolloutFoundryRecoverySupervisor(t, f)
			before := f.server.Counts()
			var earlyAdmission atomic.Bool
			relay := installFoundryRetirementRelay(t, f, foundryRetirementRelayOptions{
				beforeResponse: func() error {
					if f.server.Counts() != before {
						earlyAdmission.Store(true)
					}
					switch change {
					case "replacement template":
						deployment := &appsv1.Deployment{}
						if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: defaultNS, Name: "runtime"}, deployment); err != nil {
							return err
						}
						container, err := recoverySupervisorContainer(&deployment.Spec.Template.Spec, witness.ContainerName)
						if err != nil {
							return err
						}
						container.Image = "docker.io/example/supervisor@" + testControllerDigest("another-image")
						return f.r.Update(t.Context(), deployment)
					case "replacement Pod":
						pod := &corev1.Pod{}
						if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), pod); err != nil {
							return err
						}
						container, err := recoverySupervisorContainer(&pod.Spec, witness.ContainerName)
						if err != nil {
							return err
						}
						container.Image = "docker.io/example/supervisor@" + testControllerDigest("another-image")
						return f.r.Update(t.Context(), pod)
					default:
						return nil
					}
				},
			})
			f.reconcile(t)
			if relay.calls.Load() != 1 || earlyAdmission.Load() {
				t.Fatalf("rollout bypassed the retirement relay: calls=%d earlyAdmission=%t message=%s", relay.calls.Load(), earlyAdmission.Load(), f.runtime.Status.Message)
			}
			if change != "none" {
				if f.runtime.Status.Ready || f.server.Counts() != before {
					t.Fatal("replacement drift admitted runtime work")
				}
				if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); err != nil || retired {
					t.Fatalf("replacement drift committed retirement: retired=%t err=%v", retired, err)
				}
				return
			}
			if !f.runtime.Status.Ready {
				t.Fatalf("image rollout did not recover: %s", f.runtime.Status.Message)
			}
			proof := readFoundryRetirementProof(t, f, witness)
			if err := validateAgentRuntimeFoundryRetirement(witness, proof); err != nil {
				t.Fatalf("image rollout retirement proof: %v", err)
			}
			current := f.witness(t)
			if current.TemplateDigest == witness.TemplateDigest || current.ImageID == witness.ImageID || current.PodUID == witness.PodUID ||
				current.ReplicaSetUID == witness.ReplicaSetUID || !reflect.DeepEqual(proof.FoundryRetirement.Relay, current) {
				t.Fatal("retirement failed to bind the distinct replacement image, Pod and ReplicaSet")
			}
			original, err := loadAgentRuntimeBootWitness(t.Context(), f.control, witness.Namespace, witness.RuntimeUID, witness.Fence.SupervisorBootID)
			if err != nil || !reflect.DeepEqual(original, witness) {
				t.Fatalf("image rollout changed the original immutable witness: %v", err)
			}
		})
	}
}

func rolloutFoundryRecoverySupervisor(t *testing.T, f *runtimeRecoveryFixture) {
	t.Helper()
	witness := f.witness(t)
	f.restartContainer(t, true, false)
	brokerStatus := *f.pod.Status.ContainerStatuses[1].DeepCopy()
	brokerStatus.ContainerID = "containerd://broker-2"
	brokerStatus.State.Running.StartedAt = metav1.NewTime(witness.StartedAt.Add(45 * time.Second))
	// Retain the exact original Pod and termination while the Service selects
	// a new Pod and ReplicaSet from the same opted-in Deployment.
	f.pod.Status.ContainerStatuses[0] = corev1.ContainerStatus{
		Name: witness.ContainerName, ContainerID: witness.ContainerID, ImageID: witness.ImageID,
		RestartCount: witness.RestartCount, State: corev1.ContainerState{Terminated: recoveryTerminal(witness)},
	}
	if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Delete(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	deployment := &appsv1.Deployment{}
	if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: defaultNS, Name: "runtime"}, deployment); err != nil {
		t.Fatal(err)
	}
	imageDigest := testControllerDigest("rollout-image")
	container, err := recoverySupervisorContainer(&deployment.Spec.Template.Spec, witness.ContainerName)
	if err != nil {
		t.Fatal(err)
	}
	container.Image = "docker.io/example/supervisor@" + imageDigest
	if err := f.r.Update(t.Context(), deployment); err != nil {
		t.Fatal(err)
	}
	f.rs = &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "runtime-rs-2", UID: "replicaset-uid-2",
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(deployment, appsv1.SchemeGroupVersion.WithKind("Deployment"))}},
		Spec: appsv1.ReplicaSetSpec{Template: *deployment.Spec.Template.DeepCopy()},
	}
	f.rs.Spec.Template.Labels[appsv1.DefaultDeploymentUniqueLabelKey] = "hash-2"
	if err := f.r.Create(t.Context(), f.rs); err != nil {
		t.Fatal(err)
	}
	f.pod = &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "runtime-pod-2", UID: "pod-uid-2", Labels: map[string]string{"app": "recovery"},
			OwnerReferences: []metav1.OwnerReference{*metav1.NewControllerRef(f.rs, appsv1.SchemeGroupVersion.WithKind("ReplicaSet"))}},
		Spec: *deployment.Spec.Template.Spec.DeepCopy(),
		Status: corev1.PodStatus{PodIP: "127.0.0.1", PodIPs: []corev1.PodIP{{IP: "127.0.0.1"}},
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
			ContainerStatuses: []corev1.ContainerStatus{{Name: witness.ContainerName, ContainerID: "containerd://rollout", ImageID: imageDigest, Ready: true,
				State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{StartedAt: metav1.NewTime(witness.StartedAt.Add(45 * time.Second))}}}, brokerStatus},
		},
	}
	status := *f.pod.Status.DeepCopy()
	if err := f.r.Create(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	f.pod.Status = status
	if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
		t.Fatal(err)
	}
	f.slice.Endpoints[0].TargetRef = &corev1.ObjectReference{Kind: "Pod", Namespace: defaultNS, Name: f.pod.Name, UID: f.pod.UID}
	if err := f.r.Update(t.Context(), f.slice); err != nil {
		t.Fatal(err)
	}
}

func TestAgentRuntimeFoundryRecoveryCleansOnlyExposedTaskAndPreservesUnknownOutcome(t *testing.T) {
	f := newFoundryRetirementFixture(t)
	witness := f.witness(t)
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "foundry-recovery-outbox.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	f.control, err = storekube.NewComposite(f.r.Client, defaultNS, sqlite.NewStore(db, "foundry-recovery"), storekube.WithAPIReader(f.r.APIReader))
	if err != nil {
		t.Fatal(err)
	}
	f.r.ControlStore = f.control
	d := &ACPDispatcher{Client: f.r.Client, APIReader: f.r.APIReader, Store: f.control, Epochs: f.r.ControllerEpochManager}
	task := foundryRecoveryExposedTask(t, f, d)
	projectionID := standaloneTaskTerminalProjectionID(task, task.Status.Execution.Attempt)
	projection, err := f.control.GetOutboxProjection(t.Context(), projectionID)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := verifiedKubernetesRuntimeRetirement(t.Context(), f.control, task, task.UID); retired || err != nil {
		t.Fatalf("live boot had retirement proof: retired=%t err=%v", retired, err)
	}
	f.restartContainer(t, true, false)
	relay := installFoundryRetirementRelay(t, f, foundryRetirementRelayOptions{})
	before := f.server.Counts()
	if retired, err := f.r.observeContainerRetirement(t.Context(), witness, f.fence); err != nil || !retired {
		t.Fatalf("combined retirement: retired=%t err=%v", retired, err)
	}
	for _, change := range []string{"Task UID", "unexposed session", "prompt", "binding"} {
		t.Run(change, func(t *testing.T) {
			wrong := task.DeepCopy()
			switch change {
			case "Task UID":
				wrong.UID = "another-task"
			case "unexposed session":
				wrong.Status.Execution.RuntimeSessionUID = "unexposed-session"
			case "prompt":
				wrong.Status.Execution.PromptID = "another-prompt"
			case "binding":
				wrong.Status.AgentExecutionBinding.BindingDigest = testControllerDigest("another-binding")
			}
			if retired, _ := verifiedKubernetesRuntimeRetirement(t.Context(), f.control, wrong, wrong.UID); retired {
				t.Fatal("changed or unexposed Task inherited the Foundry retirement proof")
			}
		})
	}
	// Controller takeover changes ownership, not the admitted Task or boot.
	f.advanceEpoch(t)
	task.Status.Execution.ControllerEpoch = f.fence.Epoch
	if err := f.r.Status().Update(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	original := task.DeepCopy()
	complete, err := d.cleanupRecoveredTaskScopedRuntimeSession(t.Context(), task)
	if err != nil || !complete {
		t.Fatalf("exact exposed Task did not clean after retirement: complete=%t err=%v", complete, err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	if !runtimeSessionCleanupCompleteForUID(task, task.UID) {
		t.Fatal("combined proof did not create the exact Task cleanup receipt")
	}
	if complete, err := d.cleanupRecoveredTaskScopedRuntimeSession(t.Context(), task); err != nil || !complete {
		t.Fatalf("persisted cleanup receipt was not idempotent: complete=%t err=%v", complete, err)
	}
	task.Status.Execution.RuntimeSessionCleanupDigest = ""
	if !reflect.DeepEqual(task.Status, original.Status) {
		t.Fatal("cleanup changed the original unknown outcome or Task authority")
	}
	after, err := f.control.GetOutboxProjection(t.Context(), projectionID)
	if err != nil || !bytes.Equal(projection.Payload, after.Payload) || projection.PayloadDigest != after.PayloadDigest {
		t.Fatalf("cleanup rewrote immutable terminal evidence: %v", err)
	}
	if f.server.Counts() != before || relay.calls.Load() != 1 {
		t.Fatal("Task cleanup repeated remote retirement or invoked replacement execution")
	}
}

func foundryRecoveryExposedTask(t *testing.T, f *runtimeRecoveryFixture, d *ACPDispatcher) *corev1alpha1.Task {
	t.Helper()
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: defaultNS, Name: "foundry-recovery-task", UID: "foundry-recovery-task-uid", Generation: 1},
		Spec:       corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent},
		Status: corev1alpha1.TaskStatus{Phase: corev1alpha1.TaskPhaseRunning, Execution: &corev1alpha1.TaskExecutionStatus{
			Attempt: 1, PromptID: "foundry-recovery-prompt", RequestDigest: testControllerDigest("foundry-recovery-prompt"),
			State: corev1alpha1.TaskExecutionStateSubmittedUnknown, ControllerEpoch: f.fence.Epoch,
			AgentRuntimeName: f.runtime.Name, AgentRuntimeUID: string(f.runtime.UID),
		}},
	}
	snapshotDigest := testControllerDigest("foundry-recovery-snapshot")
	binding := &corev1alpha1.AgentExecutionBinding{
		SchemaVersion: 1, ContractVersion: corev1alpha1.AgentRuntimeContractHarnessV2, Backend: corev1alpha1.AgentExecutionBackendExternalEndpoint,
		Task:                 corev1alpha1.AgentExecutionBindingTaskRef{NamespaceUID: "namespace-uid", UID: task.UID, BoundSpecGeneration: task.Generation},
		Snapshot:             corev1alpha1.AgentExecutionSnapshotRef{ID: string(task.UID) + "/" + snapshotDigest, Digest: snapshotDigest, SchemaVersion: 1},
		RuntimeRef:           &corev1alpha1.AgentExecutionRuntimeRef{Name: f.runtime.Name, UID: f.runtime.UID, Generation: f.runtime.Generation},
		RuntimeProfileDigest: string(f.server.Fence().RuntimeProfileDigest), RuntimeProfileDigestSchemaVersion: 1, BoundAt: metav1.Now(),
	}
	var err error
	binding.BindingDigest, err = canonicalAgentExecutionBindingDigest(*binding)
	if err != nil {
		t.Fatal(err)
	}
	task.Status.AgentExecutionBinding = binding
	if err := f.r.Create(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	fence := f.server.Fence()
	fence.RuntimeSessionUID, fence.RuntimeSessionGeneration = "foundry-recovery-session", 1
	if err := d.recordKubernetesRuntimeExposure(t.Context(), task, f.runtime, fence, f.fence); err != nil {
		t.Fatal(err)
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(task), task); err != nil {
		t.Fatal(err)
	}
	task.Status.Phase = corev1alpha1.TaskPhaseFailed
	task.Status.Execution.State = corev1alpha1.TaskExecutionStateOutcomeUnknown
	task.Status.Execution.Outcome = corev1alpha1.TaskExecutionOutcomeOutcomeUnknown
	task.Status.Execution.Reason = "RuntimeLost"
	task.Status.Delivery = &corev1alpha1.TaskDeliveryStatus{State: corev1alpha1.TaskDeliveryStateNotRequested, Outcome: corev1alpha1.TaskDeliveryOutcomeNotRequested}
	if err := f.r.Status().Update(t.Context(), task); err != nil {
		t.Fatal(err)
	}
	payload := taskTerminalProjection{
		Namespace: task.Namespace, Task: task.Name, TaskUID: string(task.UID), Attempt: task.Status.Execution.Attempt,
		BindingDigest: binding.BindingDigest, Phase: task.Status.Phase,
		Execution: *task.Status.Execution.DeepCopy(), Delivery: task.Status.Delivery.DeepCopy(),
	}
	if err := enqueueDurableTaskTerminalProjection(t.Context(), f.control, f.fence, task, payload); err != nil {
		t.Fatal(err)
	}
	return task
}

func TestAgentRuntimeFoundryRecoveryDeletingRegistrationRetiresLostBoot(t *testing.T) {
	f := newFoundryRetirementFixture(t)
	witness := f.witness(t)
	f.restartContainer(t, true, false)
	relay := installFoundryRetirementRelay(t, f, foundryRetirementRelayOptions{})
	before := f.server.Counts()
	if err := f.r.Delete(t.Context(), f.runtime); err != nil {
		t.Fatal(err)
	}
	for range 4 {
		if _, err := f.r.Reconcile(t.Context(), reconcileRequestFor(f.runtime)); err != nil {
			t.Fatal(err)
		}
		current := &corev1alpha1.AgentRuntime{}
		err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.runtime), current)
		if apierrors.IsNotFound(err) {
			readFoundryRetirementProof(t, f, witness)
			if relay.calls.Load() != 1 || f.server.Counts() != before {
				t.Fatal("registration deletion admitted replacement work or repeated retirement")
			}
			return
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("exact broker retirement did not permit normal registration finalization")
}

func TestAgentRuntimeFoundryRecoveryAdmissionRejectsBrokerDrift(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	configureFoundryRuntimeRecoveryFixture(t, f)
	var changed atomic.Bool
	proxy := newExternalRuntimeStatusProxy(t, f.server.URL(), func(status *harnessv2.StatusResponse) {
		status.FoundryBroker = testFoundryRecoveryBrokerIdentity()
		if changed.Load() {
			status.FoundryBroker.LedgerIdentityDigest = testControllerDigest("changed-after-admission-witness")
		}
	})
	capabilities := newExternalRuntimeCapabilitiesProxy(t, proxy.URL, func(response *harnessv2.CapabilitiesResponse) {
		response.SupportsFoundryRecovery = true
	})
	f.slice.Ports[0].Port = new(runtimeRecoveryServerPort(t, capabilities.URL))
	if err := f.r.Update(t.Context(), f.slice); err != nil {
		t.Fatal(err)
	}
	f.updateServiceTargetPort(t)
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatalf("initial Foundry enrollment: %s", f.runtime.Status.Message)
	}
	witness, before := f.witness(t), f.server.Counts()
	changed.Store(true)
	if err := f.r.validateRecoveryAdmissionWitness(t.Context(), f.runtime, witness); err == nil {
		t.Fatal("a different ledger reused the previous boot's admission witness")
	}
	if f.server.Counts() != before {
		t.Fatal("admission validation invoked lifecycle work")
	}
}

func TestAgentRuntimeFoundryRecoveryRejectsIncompleteRemoteProof(t *testing.T) {
	for _, defect := range []string{"wrong ledger", "wrong configuration", "wrong boot", "session scope", "not sealed", "unsettled", "not retired", "active invocation", "ambiguous invocation", "pending create", "wrong proof digest", "wrong relay", "wrong request digest"} {
		t.Run(defect, func(t *testing.T) {
			f := newFoundryRetirementFixture(t)
			witness := f.witness(t)
			f.restartContainer(t, true, false)
			relay := installFoundryRetirementRelay(t, f, foundryRetirementRelayOptions{
				mutateResponse: func(response *harnessv2.FoundryBootRetirementResponse) {
					proof := &response.Proof
					switch defect {
					case "wrong ledger":
						proof.LedgerIdentityDigest = testControllerDigest("another-ledger")
					case "wrong configuration":
						proof.AgentConfigurationDigest = testControllerDigest("another-agent")
					case "wrong boot":
						proof.RetiredFenceDigest = testControllerDigest("another-boot")
					case "session scope":
						proof.RetiredFenceDigest = testControllerDigest("only-one-session")
					case "not sealed":
						proof.Sealed = false
					case "unsettled":
						proof.SettlementProven = false
					case "not retired":
						proof.RetirementProven = false
					case "active invocation":
						proof.ActiveInvocations = 1
					case "ambiguous invocation":
						proof.AmbiguousInvocations = 1
					case "pending create":
						proof.PendingCreates = 1
					case "wrong relay":
						response.RelayFence.SupervisorBootID = "another-relay"
					case "wrong request digest":
						response.RequestDigest = harnessv2.RequestDigest(testControllerDigest("another-request"))
					}
					proof.ProofDigest = proof.CanonicalProofDigest()
					if defect == "wrong proof digest" {
						proof.ProofDigest = testControllerDigest("invalid-proof")
					}
				},
			})
			before := f.server.Counts()
			retired, err := f.r.observeContainerRetirement(t.Context(), witness, f.fence)
			if err == nil || retired || relay.calls.Load() != 1 {
				t.Fatalf("incomplete remote proof accepted: retired=%t calls=%d err=%v", retired, relay.calls.Load(), err)
			}
			assertFoundryRecoveryUnproven(t, f, witness, before)
		})
	}
}

func TestAgentRuntimeFoundryRecoveryRejectsChangedRelayAuthority(t *testing.T) {
	for _, defect := range []string{"no old termination", "wrong old termination", "same container", "changed ledger", "changed agent", "busy replacement", "changed Service UID", "changed registration endpoint", "unwitnessed historical broker"} {
		t.Run(defect, func(t *testing.T) {
			f := newFoundryRetirementFixture(t)
			witness := f.witness(t)
			f.restartContainer(t, defect != "no old termination", defect == "same container")
			options := foundryRetirementRelayOptions{}
			switch defect {
			case "wrong old termination":
				f.pod.Status.ContainerStatuses[0].LastTerminationState.Terminated.ContainerID = "another-container"
				if err := f.r.Status().Update(t.Context(), f.pod); err != nil {
					t.Fatal(err)
				}
			case "changed ledger", "changed agent":
				options.identity = testFoundryRecoveryBrokerIdentity()
				if defect == "changed ledger" {
					options.identity.LedgerIdentityDigest = testControllerDigest("new-empty-ledger")
				} else {
					options.identity.AgentConfigurationDigest = testControllerDigest("another-agent")
				}
			case "busy replacement":
				options.statusTransform = func(status *harnessv2.StatusResponse) { status.Pressure.ResidentSessions = 1 }
			case "unwitnessed historical broker":
				witness.FoundryBroker = nil
			}
			relay := installFoundryRetirementRelay(t, f, options)
			switch defect {
			case "changed Service UID":
				service := &corev1.Service{}
				if err := f.r.Get(t.Context(), client.ObjectKey{Namespace: defaultNS, Name: "runtime"}, service); err != nil {
					t.Fatal(err)
				}
				service.UID = "replacement-service-uid"
				if err := f.r.Update(t.Context(), service); err != nil {
					t.Fatal(err)
				}
			case "changed registration endpoint":
				f.runtime.Spec.Deployment.Endpoint = "http://replacement.default.svc.cluster.local:8080"
				if err := f.r.Update(t.Context(), f.runtime); err != nil {
					t.Fatal(err)
				}
			}
			before := f.server.Counts()
			retired, _ := f.r.observeContainerRetirement(t.Context(), witness, f.fence)
			if retired || relay.calls.Load() != 0 || f.server.Counts() != before {
				t.Fatalf("changed authority reached retirement: retired=%t calls=%d", retired, relay.calls.Load())
			}
		})
	}
}

func TestAgentRuntimeFoundryRecoveryRevalidatesAfterAuthenticatedResponse(t *testing.T) {
	for _, defect := range []string{"leadership", "broker identity", "registration", "old termination", "current credentials"} {
		t.Run(defect, func(t *testing.T) {
			f := newFoundryRetirementFixture(t)
			witness := f.witness(t)
			f.restartContainer(t, true, false)
			var changed atomic.Bool
			mutationErrors := make(chan error, 1)
			relay := installFoundryRetirementRelay(t, f, foundryRetirementRelayOptions{
				statusTransform: func(status *harnessv2.StatusResponse) {
					if defect == "broker identity" && changed.Load() {
						status.FoundryBroker.LedgerIdentityDigest = testControllerDigest("changed-after-response")
					}
				},
				beforeResponse: func() error {
					changed.Store(true)
					err := mutateFoundryRetirementAuthority(t.Context(), f, defect)
					mutationErrors <- err
					return err
				},
			})
			before := f.server.Counts()
			retired, err := f.r.observeContainerRetirement(t.Context(), witness, f.fence)
			if err == nil || retired || relay.calls.Load() != 1 {
				t.Fatalf("post-response drift was accepted: retired=%t calls=%d err=%v", retired, relay.calls.Load(), err)
			}
			select {
			case err := <-mutationErrors:
				if err != nil {
					t.Fatal(err)
				}
			default:
				t.Fatal("retirement never reached the authority mutation boundary")
			}
			assertFoundryRecoveryUnproven(t, f, witness, before)
		})
	}
}

func TestAgentRuntimeFoundryRecoveryRetriesOnlyMonotonicRetirement(t *testing.T) {
	f := newFoundryRetirementFixture(t)
	witness := f.witness(t)
	f.restartContainer(t, true, false)
	var lost atomic.Bool
	relay := installFoundryRetirementRelay(t, f, foundryRetirementRelayOptions{
		beforeResponse: func() error {
			if lost.CompareAndSwap(false, true) {
				return errors.New("simulated retirement response loss")
			}
			return nil
		},
	})
	before := f.server.Counts()
	if retired, err := f.r.observeContainerRetirement(t.Context(), witness, f.fence); err == nil || retired || relay.calls.Load() != 1 {
		t.Fatalf("response loss minted a proof: retired=%t calls=%d err=%v", retired, relay.calls.Load(), err)
	}
	assertFoundryRecoveryUnproven(t, f, witness, before)
	// A new controller can use the original immutable witness and the same
	// permanent broker seal. It never retries a prompt or adopts old execution.
	f.advanceEpoch(t)
	if retired, err := f.r.observeContainerRetirement(t.Context(), witness, f.fence); err != nil || !retired || relay.calls.Load() != 2 {
		t.Fatalf("retirement did not recover after response loss/takeover: retired=%t calls=%d err=%v", retired, relay.calls.Load(), err)
	}
	first, second := <-relay.requests, <-relay.requests
	if first.Metadata.OperationID == second.Metadata.OperationID || first.RetiredFence != second.RetiredFence || first.Broker != second.Broker || f.server.Counts() != before {
		t.Fatal("retirement replay changed its subject or invoked execution")
	}
	readFoundryRetirementProof(t, f, witness)
}

func TestAgentRuntimeFoundryRetirementProofOutlivesTransportExpiry(t *testing.T) {
	f := newFoundryRetirementFixture(t)
	witness := f.witness(t)
	f.restartContainer(t, true, false)
	installFoundryRetirementRelay(t, f, foundryRetirementRelayOptions{})
	if retired, err := f.r.observeContainerRetirement(t.Context(), witness, f.fence); err != nil || !retired {
		t.Fatalf("retirement: %t %v", retired, err)
	}
	proof := readFoundryRetirementProof(t, f, witness)
	// Keep every identity and digest check, shifting this complete saved proof
	// into the past to exercise validation after its capability has expired.
	witness.StartedAt = metav1.NewTime(time.Now().UTC().Add(-3 * time.Hour))
	proof.ContainerTermination = recoveryTerminal(witness)
	retirement := proof.FoundryRetirement
	retirement.IssuedAt = time.Now().UTC().Add(-2 * time.Hour)
	retirement.Request.Metadata.ExpiresAt = retirement.IssuedAt.Add(time.Minute)
	if err := sealMutation(&retirement.Request.Metadata.RequestDigest, retirement.Request); err != nil {
		t.Fatal(err)
	}
	retirement.Response.RequestDigest = retirement.Request.Metadata.RequestDigest
	if err := retirement.Request.ValidateAt(time.Now().UTC()); err == nil {
		t.Fatal("fixture capability did not expire")
	}
	if err := validateAgentRuntimeFoundryRetirement(witness, proof); err != nil {
		t.Fatalf("permanent retirement incorrectly required a live capability: %v", err)
	}
	retirement.Request.Metadata.Fence.RuntimeSessionUID = "only-one-session"
	retirement.Request.Metadata.Fence.RuntimeSessionGeneration = 1
	if err := validateAgentRuntimeFoundryRetirement(witness, proof); err == nil {
		t.Fatal("durable proof validation accepted changed request scope")
	}
}

type foundryRetirementRelayOptions struct {
	identity        *harnessv2.FoundryBrokerIdentity
	statusTransform func(*harnessv2.StatusResponse)
	mutateResponse  func(*harnessv2.FoundryBootRetirementResponse)
	beforeResponse  func() error
}

type foundryRetirementRelay struct {
	calls    atomic.Int32
	requests chan harnessv2.FoundryBootRetirementRequest
}

func newFoundryRetirementFixture(t *testing.T) *runtimeRecoveryFixture {
	t.Helper()
	f := newRuntimeRecoveryFixture(t)
	configureFoundryRuntimeRecoveryFixture(t, f)
	f.reconcile(t)
	if !f.runtime.Status.Ready {
		t.Fatalf("Foundry fixture failed initial enrollment: %s", f.runtime.Status.Message)
	}
	return f
}

func installFoundryRetirementRelay(t *testing.T, f *runtimeRecoveryFixture, options foundryRetirementRelayOptions) *foundryRetirementRelay {
	t.Helper()
	identity := options.identity
	if identity == nil {
		identity = testFoundryRecoveryBrokerIdentity()
	}
	statusProxy := newExternalRuntimeStatusProxy(t, f.server.URL(), func(status *harnessv2.StatusResponse) {
		copy := *identity
		status.FoundryBroker = &copy
		if options.statusTransform != nil {
			options.statusTransform(status)
		}
	})
	capabilities := newExternalRuntimeCapabilitiesProxy(t, statusProxy.URL, func(response *harnessv2.CapabilitiesResponse) {
		response.SupportsFoundryRecovery = true
	})
	upstream, err := url.Parse(capabilities.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstream)
	relay := &foundryRetirementRelay{requests: make(chan harnessv2.FoundryBootRetirementRequest, 8)}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != harnessv2.FoundryBootRetirementPath {
			// Preserve the original prompt stream while the upstream transport
			// completes its final read of the shared request body.
			if err := http.NewResponseController(w).EnableFullDuplex(); err != nil {
				t.Errorf("enable full-duplex retirement proxy: %v", err)
				return
			}
			proxy.ServeHTTP(w, r)
			return
		}
		var request harnessv2.FoundryBootRetirementRequest
		if r.Method != http.MethodPut || json.NewDecoder(r.Body).Decode(&request) != nil || request.ValidateAt(time.Now().UTC()) != nil ||
			r.Header.Get("Authorization") != "Bearer "+f.config.ControllerBearerToken ||
			harnessv2.VerifyOperationCapability(f.config.OperationCapabilitySecret, r.Header.Get(harnessv2.OperationCapabilityHeader), request.Metadata, false, time.Now().UTC()) != nil ||
			request.Metadata.Fence != f.server.Fence() {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		relay.calls.Add(1)
		relay.requests <- request
		response, err := foundryRetirementResponseForTest(request)
		if err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if options.mutateResponse != nil {
			options.mutateResponse(&response)
		}
		if options.beforeResponse != nil && options.beforeResponse() != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		writeDispatcherJSON(w, response)
	}))
	t.Cleanup(server.Close)
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.slice), f.slice); err != nil {
		t.Fatal(err)
	}
	f.slice.Ports[0].Port = new(runtimeRecoveryServerPort(t, server.URL))
	if err := f.r.Update(t.Context(), f.slice); err != nil {
		t.Fatal(err)
	}
	f.updateServiceTargetPort(t)
	return relay
}

func foundryRetirementResponseForTest(request harnessv2.FoundryBootRetirementRequest) (harnessv2.FoundryBootRetirementResponse, error) {
	body, err := request.BrokerRequestBody()
	if err != nil {
		return harnessv2.FoundryBootRetirementResponse{}, err
	}
	fence, err := json.Marshal(request.RetiredFence)
	if err != nil {
		return harnessv2.FoundryBootRetirementResponse{}, err
	}
	proof := harnessv2.FoundryBootRetirementProof{
		Protocol: request.Broker.Protocol, LedgerIdentityDigest: request.Broker.LedgerIdentityDigest,
		AgentConfigurationDigest: request.Broker.AgentConfigurationDigest, OperationID: string(request.Metadata.OperationID),
		ContextSHA256: store.CanonicalBytesDigest(body), RetiredFenceDigest: store.CanonicalBytesDigest(fence),
		State: "retired", Sealed: true, SettlementProven: true, RetirementProven: true,
		OwnerCount: 1, OwnerSetDigest: testControllerDigest("one-old-owner"),
	}
	proof.ProofDigest = proof.CanonicalProofDigest()
	return harnessv2.FoundryBootRetirementResponse{
		Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
		RelayFence: request.Metadata.Fence, RequestDigest: request.Metadata.RequestDigest, Proof: proof,
	}, nil
}

func readFoundryRetirementProof(t *testing.T, f *runtimeRecoveryFixture, witness agentRuntimeBootWitness) agentRuntimeBootRetirement {
	t.Helper()
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); err != nil || !retired {
		t.Fatalf("durable combined proof is invalid: retired=%t err=%v", retired, err)
	}
	var proof agentRuntimeBootRetirement
	_, err := readAgentRuntimeRecoveryEffect(t.Context(), f.control,
		agentRuntimeRecoveryIdentity(agentRuntimeBootRetirementKind, witness.RuntimeUID, witness.Namespace, string(witness.Fence.SupervisorBootID)), &proof)
	if err != nil {
		t.Fatal(err)
	}
	return proof
}

func assertFoundryRecoveryUnproven(t *testing.T, f *runtimeRecoveryFixture, witness agentRuntimeBootWitness, before any) {
	t.Helper()
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); err != nil || retired {
		t.Fatalf("failed retirement created positive evidence: retired=%t err=%v", retired, err)
	}
	if !reflect.DeepEqual(f.server.Counts(), before) {
		t.Fatal("failed retirement invoked runtime admission or prompts")
	}
	if err := f.r.Get(t.Context(), client.ObjectKeyFromObject(f.pod), f.pod); err != nil {
		t.Fatal(err)
	}
	if !controllerutil.ContainsFinalizer(f.pod, agentRuntimeRecoveryPodFinalizer) {
		t.Fatal("failed retirement released the retained old Pod")
	}
}

func mutateFoundryRetirementAuthority(ctx context.Context, f *runtimeRecoveryFixture, defect string) error {
	switch defect {
	case "leadership":
		current, err := f.control.GetControllerEpoch(ctx, f.fence.Name)
		if err != nil {
			return err
		}
		_, err = f.control.CompareAndSwapControllerEpoch(ctx, store.ControllerEpochCAS{
			Name: current.Name, ExpectedVersion: current.Version, ExpectedEpoch: current.Epoch, NewEpoch: current.Epoch + 1,
			HolderID: "foundry-retirement-successor", UpdatedAt: time.Now().UTC(), RequestDigest: testControllerDigest("foundry-retirement-successor"),
		})
		return err
	case "registration":
		current := &corev1alpha1.AgentRuntime{}
		if err := f.r.Get(ctx, client.ObjectKeyFromObject(f.runtime), current); err != nil {
			return err
		}
		current.Generation++
		return f.r.Update(ctx, current)
	case "old termination":
		current := &corev1.Pod{}
		if err := f.r.Get(ctx, client.ObjectKeyFromObject(f.pod), current); err != nil {
			return err
		}
		current.Status.ContainerStatuses[0].LastTerminationState.Terminated = nil
		return f.r.Status().Update(ctx, current)
	case "current credentials":
		secret := &corev1.Secret{}
		key := client.ObjectKey{Namespace: f.runtime.Namespace, Name: f.runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef.Name}
		if err := f.r.Get(ctx, key, secret); err != nil {
			return err
		}
		if secret.Annotations == nil {
			secret.Annotations = make(map[string]string)
		}
		secret.Annotations["changed-during-retirement"] = "true"
		return f.r.Update(ctx, secret)
	default:
		return nil
	}
}
