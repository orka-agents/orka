package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func newManagedExternalACPDispatchFixture(t *testing.T) (*externalACPDispatchFixture, *AgentRuntimeReconciler, agentRuntimeBootWitness) {
	t.Helper()
	// These tests span enrollment, multiple full event streams and takeover.
	// Race instrumentation makes the existing event redaction work exceed the
	// ordinary single-dispatch fixture's deadline.
	f := newExternalACPDispatchFixtureWithOptions(t, "external-v2", testAgentRuntimeMCPPolicy(),
		externalACPDispatchFixtureOptions{contextTimeout: time.Minute})
	runtime := f.runtime.DeepCopy()
	backendURL := runtime.Spec.Deployment.Endpoint
	runtime.Spec.Deployment.Endpoint = "http://runtime.default.svc.cluster.local:8080"
	runtime.Spec.Deployment.KubernetesRecovery = &corev1alpha1.AgentRuntimeKubernetesRecoverySpec{
		DeploymentName: "runtime", DeploymentUID: "deployment-uid", ContainerName: "supervisor",
	}
	if err := appsv1.AddToScheme(f.client.Scheme()); err != nil {
		t.Fatal(err)
	}
	if err := discoveryv1.AddToScheme(f.client.Scheme()); err != nil {
		t.Fatal(err)
	}
	deployment, rs, pod, service, slice := runtimeRecoveryObjects(t, runtime, backendURL)
	for _, object := range []client.Object{deployment, rs, pod, service, slice} {
		if err := f.client.Create(f.ctx, object); err != nil {
			t.Fatal(err)
		}
	}
	secret := &corev1.Secret{}
	if err := f.client.Get(f.ctx, client.ObjectKey{Namespace: defaultNS, Name: runtime.Spec.ClientAuth.ControllerBearerTokenSecretRef.Name}, secret); err != nil {
		t.Fatal(err)
	}
	secret.Annotations[agentRuntimeAuthEndpointAnnotation] = runtime.Spec.Deployment.Endpoint
	if err := f.client.Update(f.ctx, secret); err != nil {
		t.Fatal(err)
	}
	if err := f.client.Update(f.ctx, runtime); err != nil {
		t.Fatal(err)
	}
	runtime.Status.ObservedControllerAuthRefResourceVersion = secret.ResourceVersion
	runtime.Status.ObservedOperationCapabilityRefResourceVersion = secret.ResourceVersion
	if err := f.client.Status().Update(f.ctx, runtime); err != nil {
		t.Fatal(err)
	}
	r := &AgentRuntimeReconciler{Client: f.client, APIReader: f.client, Scheme: f.client.Scheme(), ControlStore: f.controlStore, ControllerEpochManager: f.epochs}
	backend, err := r.recoveryBackend(f.ctx, runtime)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := f.epochs.CurrentFence(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	witness, err := r.observeRecoveryBoot(f.ctx, runtime, backend, fence)
	if err != nil {
		t.Fatal(err)
	}
	// The fixture publishes Ready directly, so seed the matching physical
	// conformance record through the same resolver and persistence as reconcile.
	serviceBackend, err := r.agentRuntimeServiceBackendState(f.ctx, runtime)
	if err != nil || serviceBackend.conformanceDigest == "" {
		t.Fatalf("managed fixture Service identity is incomplete: %v", err)
	}
	if err := r.persistAgentRuntimeDeletionSnapshot(f.ctx, runtime, runtime.Status.ObservedCapabilities,
		runtime.Status.ObservedControllerAuthRefResourceVersion, runtime.Status.ObservedOperationCapabilityRefResourceVersion,
		serviceBackend.conformanceDigest); err != nil {
		t.Fatal(err)
	}
	f.runtime = runtime
	return f, r, witness
}

func retireManagedFixtureBoot(t *testing.T, f *externalACPDispatchFixture, r *AgentRuntimeReconciler, witness agentRuntimeBootWitness) {
	t.Helper()
	pod := &corev1.Pod{}
	if err := f.client.Get(f.ctx, client.ObjectKey{Namespace: witness.Namespace, Name: witness.PodName}, pod); err != nil {
		t.Fatal(err)
	}
	pod.Status.ContainerStatuses[0].State = corev1.ContainerState{Terminated: recoveryTerminal(witness)}
	if err := f.client.Status().Update(f.ctx, pod); err != nil {
		t.Fatal(err)
	}
	fence, err := f.epochs.CurrentFence(f.ctx)
	if err != nil {
		t.Fatal(err)
	}
	if retired, err := r.observeContainerRetirement(f.ctx, witness, fence); err != nil || !retired {
		t.Fatalf("exact old-container retirement failed: %v", err)
	}
}

func TestAgentRuntimeRecoverySessionCleanupUsesExposureAndPreservesProjection(t *testing.T) {
	for _, taskFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("task-first-%t", taskFirst), func(t *testing.T) {
			f, r, witness := newManagedExternalACPDispatchFixture(t)
			tasks := make([]*corev1alpha1.Task, 0, 2)
			before := make(map[string][]byte)
			for i := range 2 {
				name := fmt.Sprintf("managed-turn-%d", i)
				queued := f.queueTask(t, name, types.UID(name), name, &corev1alpha1.SessionReference{Name: "cleanup-conversation", Create: i == 0, Append: true})
				task := f.dispatch(t, queued)
				if task.Status.Phase != corev1alpha1.TaskPhaseSucceeded {
					t.Fatalf("managed Task did not succeed: %s", task.Status.Execution.Message)
				}
				if retired, err := verifiedKubernetesRuntimeRetirement(f.ctx, f.controlStore, task, task.UID); retired || err != nil {
					t.Fatalf("live boot had retirement proof: %t, %v", retired, err)
				}
				_, projection := sessionRuntimeCleanupTurnProjection(t, f, task)
				before[name] = bytes.Clone(projection.Payload)
				tasks = append(tasks, task)
			}
			projector := &ACPOutboxProjector{Client: f.client, Store: f.controlStore, Epochs: f.epochs, WorkerID: "managed-cleanup-test"}
			if err := projector.projectOnce(f.ctx); err != nil {
				t.Fatal(err)
			}
			if taskFirst {
				for _, task := range tasks {
					markSessionCleanupTaskDeleting(t, f, task)
				}
			}
			retireManagedFixtureBoot(t, f, r, witness)
			epochs, stop := startACPRecoveryEpochManager(t, f.ctx, f.controlStore, "managed-cleanup-successor")
			defer stop()
			f.epochs, f.dispatcher.Epochs = epochs, epochs
			fence, err := epochs.CurrentFence(f.ctx)
			if err != nil {
				t.Fatal(err)
			}
			for _, task := range tasks {
				current := &corev1alpha1.Task{}
				if err := f.client.Get(f.ctx, client.ObjectKeyFromObject(task), current); err != nil {
					t.Fatal(err)
				}
				current.Status.Execution.ControllerEpoch = fence.Epoch
				if err := f.client.Status().Update(f.ctx, current); err != nil {
					t.Fatal(err)
				}
			}
			cleanup := sessionRuntimeCleanupStore(t, f, f.dispatcher.CleanupSessionRuntime)
			manager := NewSessionManager(f.persistence)
			manager.SetACPSessionCleanup(cleanup, epochs)
			if err := manager.DeleteSession(f.ctx, defaultNS, "cleanup-conversation"); err != nil {
				t.Fatalf("Session cleanup with archived boot proof: %v", err)
			}
			assertSessionRuntimeCleanupCompleted(t, f, cleanup, tasks)
			for _, task := range tasks {
				id, err := promptAttemptIDFromTask(task)
				if err != nil {
					t.Fatal(err)
				}
				receipt, err := f.persistence.GetSessionTurnCleanupReceipt(f.ctx, defaultNS, "cleanup-conversation", id)
				if err != nil || !bytes.Equal(receipt.Payload, before[task.Name]) {
					t.Fatalf("cleanup changed original terminal projection: %v", err)
				}
			}
			if f.createCalls.Load() != 1 || f.deleteCalls.Load() != 0 {
				t.Fatal("recovery replayed work or inferred cleanup from a live/replacement endpoint")
			}
		})
	}
}

func TestAgentRuntimeRecoveryExposureRejectsTaskAndProofDrift(t *testing.T) {
	f, r, witness := newManagedExternalACPDispatchFixture(t)
	task := f.dispatch(t, f.queueTask(t, "exposure-guard", "exposure-guard-uid", "hello", &corev1alpha1.SessionReference{Name: "cleanup-conversation", Create: true, Append: true}))
	retireManagedFixtureBoot(t, f, r, witness)
	if retired, err := verifiedKubernetesRuntimeRetirement(f.ctx, f.controlStore, task, task.UID); err != nil || !retired {
		t.Fatalf("original exposed Task did not validate: %v", err)
	}
	for _, test := range []struct {
		name   string
		change func(*corev1alpha1.Task)
	}{
		{"UID", func(task *corev1alpha1.Task) { task.UID = "other" }},
		{"attempt", func(task *corev1alpha1.Task) { task.Status.Execution.Attempt++ }},
		{"prompt", func(task *corev1alpha1.Task) { task.Status.Execution.PromptID = "other" }},
		{"request", func(task *corev1alpha1.Task) { task.Status.Execution.RequestDigest = testControllerDigest("other") }},
		{"boot", func(task *corev1alpha1.Task) { task.Status.Execution.RuntimeSessionSupervisorBootID = "other" }},
		{"generation", func(task *corev1alpha1.Task) { task.Status.Execution.RuntimeSessionGeneration++ }},
		{"binding", func(task *corev1alpha1.Task) {
			task.Status.AgentExecutionBinding.BindingDigest = testControllerDigest("other")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			wrong := task.DeepCopy()
			test.change(wrong)
			if retired, _ := verifiedKubernetesRuntimeRetirement(f.ctx, f.controlStore, wrong, wrong.UID); retired {
				t.Fatal("changed Task authority inherited cleanup proof")
			}
		})
	}
	// Mutable controller epoch is deliberately not original prompt authority.
	current := task.DeepCopy()
	current.Status.Execution.ControllerEpoch += 5
	if retired, err := verifiedKubernetesRuntimeRetirement(f.ctx, f.controlStore, current, current.UID); err != nil || !retired {
		t.Fatalf("current owner could not read original boot proof: %v", err)
	}
	missing := &missingRuntimeExposureStore{DurableControlStore: f.controlStore}
	if retired, err := verifiedKubernetesRuntimeRetirement(f.ctx, missing, task, task.UID); err != nil || retired {
		t.Fatalf("historical Task without enrollment was retroactively retired: %t, %v", retired, err)
	}
}

type missingRuntimeExposureStore struct{ store.DurableControlStore }

func (*missingRuntimeExposureStore) GetExternalEffect(context.Context, string) (*store.ExternalEffect, error) {
	return nil, store.ErrNotFound
}

func TestAgentRuntimeRecoveryFirstEnrollmentAtOldEpoch(t *testing.T) {
	f := newRuntimeRecoveryFixture(t)
	f.advanceEpoch(t)
	f.reconcile(t)
	if f.runtime.Status.Ready || f.server.Counts().SessionCreates != 0 {
		t.Fatal("initial old-epoch enrollment admitted a lifecycle probe")
	}
	witness := f.witness(t)
	if witness.Fence.ControllerEpoch != 1 {
		t.Fatal("enrollment rewrote the original supervisor epoch")
	}
	f.reconcile(t)
	if retired, err := loadAgentRuntimeBootRetirement(t.Context(), f.control, witness); err != nil || !retired {
		t.Fatalf("initial idle old boot was not safely drained: %v", err)
	}
	_, _, epoch, err := f.r.recoveryDeployment(t.Context(), f.runtime)
	if err != nil || epoch != 2 {
		t.Fatalf("initial old boot was not replaced at the current epoch: %d, %v", epoch, err)
	}
	// The witnessed idle observation belongs only to this boot and cannot
	// satisfy cleanup for the historical boot lost before enrollment existed.
	if _, err := loadAgentRuntimeBootWitness(t.Context(), f.control, defaultNS, f.runtime.UID, harnessv2.SupervisorBootID("historical-killsup04")); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("historical unwitnessed boot gained proof: %v", err)
	}
}
