/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	types "k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func bindingTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	return scheme
}

func bindingTestTask() *corev1alpha1.Task {
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "task",
			UID: types.UID("11111111-1111-1111-1111-111111111111"), Generation: 2,
			Finalizers: []string{labels.TaskFinalizer},
		},
		Spec: corev1alpha1.TaskSpec{
			Type: corev1alpha1.TaskTypeAgent, Prompt: "implement the fix",
			AgentRef: &corev1alpha1.AgentReference{Name: "agent"},
			Workspace: &corev1alpha1.WorkspaceConfig{
				Intent: corev1alpha1.WorkspaceIntentRead, GitRepo: "https://github.com/orka-agents/orka.git",
			},
		},
	}
}

func TestPersistAgentExecutionBindingRequiresCleanupFinalizerAtCAS(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
	candidate, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	current := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	current.Finalizers = nil
	if err := reconciler.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.persistAgentExecutionBinding(ctx, task, candidate); err == nil ||
		!strings.Contains(err.Error(), "cleanup finalizer is missing") {
		t.Fatalf("missing-finalizer binding error = %v, want refusal", err)
	}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.AgentExecutionBinding != nil {
		t.Fatalf("binding persisted after finalizer removal: %+v", current.Status.AgentExecutionBinding)
	}
}

func bindingTestAgent() *corev1alpha1.Agent {
	return &corev1alpha1.Agent{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "default", Name: "agent",
			UID: types.UID("22222222-2222-2222-2222-222222222222"), Generation: 3,
		},
		Spec: corev1alpha1.AgentSpec{
			Model:        &corev1alpha1.ModelConfig{Name: acpTestModel},
			SystemPrompt: &corev1alpha1.PromptSource{Inline: "You are a careful engineer."},
			Runtime: &corev1alpha1.AgentCLIRuntime{
				Type:            corev1alpha1.AgentRuntimeCodex,
				ContractVersion: ptr.To(corev1alpha1.AgentRuntimeContractHarnessV2),
			},
		},
	}
}

func bindingTestNamespace() *corev1.Namespace {
	return &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
		Name: "default", UID: types.UID("33333333-3333-3333-3333-333333333333"),
	}}
}

func bindingTestControl() *corev1alpha1.AgentExecutionControl {
	return &corev1alpha1.AgentExecutionControl{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:  corev1alpha1.AgentExecutionControlNamespace,
			Name:       corev1alpha1.AgentExecutionControlName,
			UID:        types.UID("44444444-4444-4444-4444-444444444444"),
			Generation: 1,
		},
		Spec: corev1alpha1.AgentExecutionControlSpec{Backends: corev1alpha1.AgentExecutionBackendsSpec{
			V1: corev1alpha1.AgentExecutionBackendSpec{DesiredMode: corev1alpha1.AgentExecutionModeDisabled},
			V2: corev1alpha1.AgentExecutionBackendSpec{DesiredMode: corev1alpha1.AgentExecutionModeEnabled},
		}},
		Status: corev1alpha1.AgentExecutionControlStatus{
			ObservedGeneration: 1,
			Backends: &corev1alpha1.AgentExecutionBackendsStatus{
				V1: corev1alpha1.AgentExecutionBackendStatus{EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeDisabled, ModeRevision: 1},
				V2: corev1alpha1.AgentExecutionBackendStatus{EffectiveMode: corev1alpha1.AgentExecutionEffectiveModeEnabled, ModeRevision: 7},
			},
		},
	}
}

func newBindingTestReconciler(t *testing.T, objects ...client.Object) (*TaskReconciler, *sqlite.Store) {
	t.Helper()
	control := bindingTestControl()
	hasControl := false
	for _, object := range objects {
		if candidate, ok := object.(*corev1alpha1.AgentExecutionControl); ok {
			hasControl = true
			control = candidate
			break
		}
	}
	if !hasControl {
		objects = append(objects, control)
	}
	scheme := bindingTestScheme(t)
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}, &corev1alpha1.AgentExecutionControl{}).
		WithObjects(objects...).Build()
	db, err := sqlite.NewDB(filepath.Join(t.TempDir(), "binding.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	snapshotStore := sqlite.NewStore(db, "test")
	cipher, err := sqlite.NewAgentExecutionSnapshotCipher(bytes.Repeat([]byte{0x24}, sqlite.AgentExecutionSnapshotKeyBytes))
	if err != nil {
		t.Fatal(err)
	}
	snapshotStore.SetAgentExecutionSnapshotCipher(cipher)
	configureAgentExecutionBindingTestGate(
		t, context.Background(), snapshotStore, control, store.AgentExecutionBackendV2,
	)
	return &TaskReconciler{
		Client: kubeClient, Scheme: scheme, Recorder: record.NewFakeRecorder(10),
		AgentExecutionSnapshots:           snapshotStore,
		AgentExecutionBindingReservations: snapshotStore,
		ACPRuntimeEnabled:                 true, ACPRuntimeNamespace: "orka-runtimes",
		ACPRuntimeImages: ACPRuntimeImages{Codex: "docker.io/example/codex@sha256:" + strings.Repeat("a", 64)},
	}, snapshotStore
}

func TestEnsureAgentExecutionBindingFreezesSnapshotBeforeDispatch(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, snapshots := newBindingTestReconciler(t, task, bindingTestNamespace())

	if reconciler.AgentExecutionSnapshots == nil {
		t.Fatal("binding stage requires a snapshot store")
	}

	live := task.DeepCopy()
	_, err, handled := reconciler.ensureAgentExecutionBinding(ctx, live, agent)
	if err != nil || handled {
		t.Fatalf("ensure binding = handled=%v err=%v", handled, err)
	}

	bound := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "task"}, bound); err != nil {
		t.Fatal(err)
	}
	binding := bound.Status.AgentExecutionBinding
	if binding == nil {
		t.Fatal("binding was not persisted before dispatch")
	}
	if binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 ||
		binding.Backend != corev1alpha1.AgentExecutionBackendRuntimePool ||
		binding.Provenance != corev1alpha1.AgentExecutionProvenanceNewlyBound ||
		binding.Mode != corev1alpha1.AgentExecutionBindingModeExecute {
		t.Fatalf("unexpected binding route: %+v", binding)
	}
	if binding.Task.NamespaceUID != "33333333-3333-3333-3333-333333333333" || binding.Task.UID != task.UID {
		t.Fatalf("binding task identity = %+v", binding.Task)
	}
	if !strings.HasPrefix(binding.Snapshot.ID, string(task.UID)+"/sha256:") {
		t.Fatalf("snapshot ID = %q", binding.Snapshot.ID)
	}

	// The immutable snapshot is durable, encrypted, and contains the resolved
	// executable inputs.
	snapshot, err := snapshots.GetAgentExecutionSnapshot(ctx, store.AgentExecutionSnapshotKey{
		TaskUID: string(task.UID), Digest: binding.Snapshot.Digest,
	})
	if err != nil {
		t.Fatalf("get snapshot: %v", err)
	}
	for _, required := range []string{"implement the fix", "You are a careful engineer.", "runtimeImage", "profileDigest"} {
		if !strings.Contains(string(snapshot.Body), required) {
			t.Fatalf("snapshot body is missing %q: %s", required, snapshot.Body)
		}
	}

	// A second pass verifies the existing binding instead of re-binding.
	_, err, handled = reconciler.ensureAgentExecutionBinding(ctx, bound, agent)
	if err != nil || handled {
		t.Fatalf("verify pass = handled=%v err=%v", handled, err)
	}
	again := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "task"}, again); err != nil {
		t.Fatal(err)
	}
	if again.Status.AgentExecutionBinding.BindingDigest != binding.BindingDigest {
		t.Fatal("verify pass must not rewrite the binding")
	}
}

func TestResolveAgentExecutionCandidateDigestIsStable(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())

	first, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	second, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	if first.binding.BindingDigest != second.binding.BindingDigest {
		t.Fatalf("binding digest is not stable: %s vs %s", first.binding.BindingDigest, second.binding.BindingDigest)
	}
	if first.binding.Snapshot.Digest != second.binding.Snapshot.Digest {
		t.Fatal("snapshot digest is not stable")
	}

	// Changing an executable input changes the candidate identity.
	changed := agent.DeepCopy()
	changed.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "different prompt"}
	third, err := reconciler.resolveAgentExecutionCandidate(ctx, task, changed)
	if err != nil {
		t.Fatal(err)
	}
	if third.binding.Snapshot.Digest == first.binding.Snapshot.Digest {
		t.Fatal("snapshot digest must change when executable inputs change")
	}
}

func TestPersistAgentExecutionBindingNeverOverwrites(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())

	candidate, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.persistAgentExecutionBinding(ctx, task, candidate); err != nil {
		t.Fatal(err)
	}
	// The identical candidate replays idempotently.
	if _, err := reconciler.persistAgentExecutionBinding(ctx, task, candidate); err != nil {
		t.Fatalf("idempotent persist: %v", err)
	}

	// A mismatched candidate is a permanent conflict and never overwrites.
	changedAgent := agent.DeepCopy()
	changedAgent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "changed"}
	mismatched, err := reconciler.resolveAgentExecutionCandidate(ctx, task, changedAgent)
	if err != nil {
		t.Fatal(err)
	}
	_, err = reconciler.persistAgentExecutionBinding(ctx, task, mismatched)
	conflict := &errAgentExecutionBindingConflict{}
	if !errors.As(err, &conflict) {
		t.Fatalf("expected binding conflict, got %v", err)
	}

	stored := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, types.NamespacedName{Namespace: "default", Name: "task"}, stored); err != nil {
		t.Fatal(err)
	}
	if stored.Status.AgentExecutionBinding.BindingDigest != candidate.binding.BindingDigest {
		t.Fatal("conflict must not overwrite the original binding")
	}
}

func TestPersistAgentExecutionBindingRefusesDeletingTask(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	task.Finalizers = []string{"orka.ai/cleanup"}
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())

	candidate, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Delete(ctx, task.DeepCopy()); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.persistAgentExecutionBinding(ctx, task, candidate); err == nil ||
		!strings.Contains(err.Error(), "deleting") {
		t.Fatalf("expected deleting-task refusal, got %v", err)
	}
}

func TestPersistAgentExecutionBindingRejectsSpecGenerationRace(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
	candidate, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	current := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	current.Generation++
	current.Spec.Prompt = "changed concurrently"
	if err := reconciler.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.persistAgentExecutionBinding(ctx, task, candidate); err == nil ||
		!strings.Contains(err.Error(), "spec generation changed") {
		t.Fatalf("expected stale candidate refusal, got %v", err)
	}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if current.Status.AgentExecutionBinding != nil {
		t.Fatal("generation race persisted a stale immutable binding")
	}
}

func TestResolveCandidateEnforcesBackendControlModes(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()

	control := bindingTestControl()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace(), control)

	candidate, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	ref := candidate.binding.BackendControl
	if ref == nil || ref.UID != control.UID || ref.ModeRevision != 7 ||
		ref.AdmittedMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
		t.Fatalf("backend control ref = %+v", ref)
	}

	// A drain-only v2 backend rejects new bindings and never falls back.
	closed := control.DeepCopy()
	closed.Status.Backends.V2.EffectiveMode = corev1alpha1.AgentExecutionEffectiveModeDrainOnly
	closed.Status.Backends.V2.ModeRevision = 8
	if err := reconciler.Status().Update(ctx, closed); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent); err == nil ||
		!strings.Contains(err.Error(), "never fall back") {
		t.Fatalf("expected drain-only rejection, got %v", err)
	}
}

func TestResolveCandidateFailsClosedWithoutCurrentControlOrSnapshotStore(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()

	t.Run("snapshot store", func(t *testing.T) {
		reconciler, _ := newBindingTestReconciler(t, task.DeepCopy(), bindingTestNamespace())
		reconciler.AgentExecutionSnapshots = nil
		if _, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent); err == nil ||
			!strings.Contains(err.Error(), "snapshot store is required") {
			t.Fatalf("expected missing snapshot store refusal, got %v", err)
		}
	})

	t.Run("missing control", func(t *testing.T) {
		control := bindingTestControl()
		reconciler, _ := newBindingTestReconciler(t, task.DeepCopy(), bindingTestNamespace(), control)
		if err := reconciler.Delete(ctx, control.DeepCopy()); err != nil {
			t.Fatal(err)
		}
		if _, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent); err == nil ||
			!strings.Contains(err.Error(), "is missing") {
			t.Fatalf("expected missing control refusal, got %v", err)
		}
	})

	t.Run("stale observed generation", func(t *testing.T) {
		control := bindingTestControl()
		control.Status.ObservedGeneration = 0
		reconciler, _ := newBindingTestReconciler(t, task.DeepCopy(), bindingTestNamespace(), control)
		if _, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent); err == nil ||
			!strings.Contains(err.Error(), "not exactly observed") {
			t.Fatalf("expected stale control refusal, got %v", err)
		}
	})
}

func TestVerifyBoundExecutionDetectsDriftAndDeletion(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())

	candidate, err := reconciler.resolveAgentExecutionCandidate(ctx, task, agent)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := reconciler.persistAgentExecutionBinding(ctx, task, candidate)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.persistAgentExecutionSnapshot(ctx, task, candidate); err != nil {
		t.Fatal(err)
	}
	reservation, err := reconciler.createAgentExecutionBindingReservation(ctx, task, binding)
	if err != nil {
		t.Fatal(err)
	}
	if err := reconciler.settleAgentExecutionBindingReservation(
		ctx, reservation, store.AgentExecutionBindingReservationBound, "",
	); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.verifyBoundExecution(ctx, task, binding); err != nil {
		t.Fatalf("verify bound execution: %v", err)
	}

	drifted := binding.DeepCopy()
	drifted.BindingDigest = "sha256:" + strings.Repeat("f", 64)
	if err := reconciler.verifyBoundExecution(ctx, task, drifted); err == nil {
		t.Fatal("digest drift must block dispatch")
	}
}

func TestQueueACPRuntimeTaskRequiresBindingSnapshotAndExactControlRevision(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, snapshots := newBindingTestReconciler(t, task, bindingTestNamespace())

	if _, err := reconciler.queueACPRuntimeTask(ctx, task.DeepCopy(), agent); err == nil ||
		!strings.Contains(err.Error(), "binding is required") {
		t.Fatalf("expected unbound queue refusal, got %v", err)
	}
	var pools corev1alpha1.RuntimePoolList
	if err := reconciler.List(ctx, &pools); err != nil {
		t.Fatal(err)
	}
	if len(pools.Items) != 0 {
		t.Fatalf("unbound queue created RuntimePools: %#v", pools.Items)
	}

	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, task.DeepCopy(), agent); err != nil || handled {
		t.Fatalf("ensure binding = handled=%v err=%v", handled, err)
	}
	bound := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), bound); err != nil {
		t.Fatal(err)
	}

	control := &corev1alpha1.AgentExecutionControl{}
	if err := reconciler.Get(ctx, client.ObjectKey{
		Namespace: corev1alpha1.AgentExecutionControlNamespace, Name: corev1alpha1.AgentExecutionControlName,
	}, control); err != nil {
		t.Fatal(err)
	}
	control.Status.Backends.V2.ModeRevision++
	if err := reconciler.Status().Update(ctx, control); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.queueACPRuntimeTask(ctx, bound.DeepCopy(), agent); err == nil ||
		!strings.Contains(err.Error(), "mode revision is stale") {
		t.Fatalf("expected stale control revision refusal, got %v", err)
	}

	control.Status.Backends.V2.ModeRevision--
	if err := reconciler.Status().Update(ctx, control); err != nil {
		t.Fatal(err)
	}
	if err := snapshots.DeleteAgentExecutionSnapshots(ctx, string(task.UID)); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.queueACPRuntimeTask(ctx, bound.DeepCopy(), agent); err == nil ||
		!strings.Contains(err.Error(), "load immutable execution snapshot") {
		t.Fatalf("expected missing snapshot refusal, got %v", err)
	}
}

func TestQueueACPRuntimeTaskRejectsTaskSpecGenerationRace(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, task.DeepCopy(), agent); err != nil || handled {
		t.Fatalf("ensure binding = handled=%v err=%v", handled, err)
	}
	current := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	current.Generation++
	current.Spec.Prompt = "changed after binding"
	if err := reconciler.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.queueACPRuntimeTask(ctx, current, agent); err == nil ||
		!strings.Contains(err.Error(), "spec generation no longer exactly matches") {
		t.Fatalf("expected Task generation race refusal, got %v", err)
	}
	var pools corev1alpha1.RuntimePoolList
	if err := reconciler.List(ctx, &pools); err != nil {
		t.Fatal(err)
	}
	if len(pools.Items) != 0 {
		t.Fatalf("Task generation race created RuntimePools: %#v", pools.Items)
	}
}

func TestEnsureAgentExecutionBindingUsesUncachedExistingBinding(t *testing.T) {
	ctx := context.Background()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, _ := newBindingTestReconciler(t, task, bindingTestNamespace())
	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, task.DeepCopy(), agent); err != nil || handled {
		t.Fatalf("ensure binding = handled=%v err=%v", handled, err)
	}
	mutatedAgent := agent.DeepCopy()
	mutatedAgent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "changed after binding"}
	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, task.DeepCopy(), mutatedAgent); err != nil || handled {
		t.Fatalf("stale cache should verify uncached binding, handled=%v err=%v", handled, err)
	}
}

func TestQueueACPRuntimeTaskUsesFrozenSnapshotAfterLiveInputsMutate(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	task := bindingTestTask()
	agent := bindingTestAgent()
	reconciler, durableStore := newBindingTestReconciler(t, task, bindingTestNamespace())
	reconciler.DurableControlStore = durableStore
	epochs := NewControllerEpochManager(durableStore, "binding-queue-test")
	reconciler.ControllerEpochManager = epochs
	epochCtx, cancelEpoch := context.WithCancel(context.Background())
	epochDone := make(chan error, 1)
	go func() { epochDone <- epochs.Start(epochCtx) }()
	defer func() {
		cancelEpoch()
		if err := <-epochDone; err != nil {
			t.Errorf("stop epoch manager: %v", err)
		}
	}()
	if _, err := epochs.CurrentFence(ctx); err != nil {
		t.Fatal(err)
	}

	if _, err, handled := reconciler.ensureAgentExecutionBinding(ctx, task.DeepCopy(), agent); err != nil || handled {
		t.Fatalf("ensure binding = handled=%v err=%v", handled, err)
	}
	boundTask := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), boundTask); err != nil {
		t.Fatal(err)
	}
	verified, err := reconciler.loadVerifiedBoundExecution(ctx, boundTask, boundTask.Status.AgentExecutionBinding)
	if err != nil {
		t.Fatal(err)
	}
	wantImage := verified.plan.Image
	wantProfileDigest := string(verified.plan.Digest)
	wantRequestDigest, err := acpBoundTaskRequestDigest(verified, 1, fmt.Sprintf("prompt-%s-1", task.UID))
	if err != nil {
		t.Fatal(err)
	}

	mutatedTask := boundTask.DeepCopy()
	mutatedTask.Spec.Prompt = "mutable prompt must be ignored"
	mutatedTask.Spec.Workspace.GitRepo = "https://example.invalid/mutated.git"
	mutatedAgent := agent.DeepCopy()
	mutatedAgent.Spec.Model.Name = "mutated/model"
	mutatedAgent.Spec.SystemPrompt = &corev1alpha1.PromptSource{Inline: "mutable system prompt must be ignored"}
	reconciler.ACPRuntimeImages.Codex = "docker.io/example/mutated@sha256:" + strings.Repeat("b", 64)

	if _, err := reconciler.queueACPRuntimeTask(ctx, mutatedTask, mutatedAgent); err != nil {
		t.Fatal(err)
	}
	var pools corev1alpha1.RuntimePoolList
	if err := reconciler.List(ctx, &pools); err != nil {
		t.Fatal(err)
	}
	if len(pools.Items) != 1 || pools.Items[0].Spec.Runtime.Image != wantImage ||
		pools.Items[0].Spec.Runtime.Profile.Digest != wantProfileDigest {
		t.Fatalf("RuntimePool did not use frozen plan: %#v", pools.Items)
	}
	queued := &corev1alpha1.Task{}
	if err := reconciler.Get(ctx, client.ObjectKeyFromObject(task), queued); err != nil {
		t.Fatal(err)
	}
	attemptID, err := promptAttemptIDFromTask(queued)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := durableStore.GetPromptAttempt(ctx, attemptID)
	if err != nil {
		t.Fatal(err)
	}
	if attempt.RequestDigest != wantRequestDigest ||
		attempt.BindingDigest != verified.binding.BindingDigest || attempt.SnapshotDigest != verified.snapshot.Digest {
		t.Fatalf("PromptAttempt immutable digests = request:%s binding:%s snapshot:%s, want %s/%s/%s",
			attempt.RequestDigest, attempt.BindingDigest, attempt.SnapshotDigest,
			wantRequestDigest, verified.binding.BindingDigest, verified.snapshot.Digest)
	}
}
