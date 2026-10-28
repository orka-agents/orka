package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/enforcement"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func acpEnforcementFixture(t *testing.T) (*ACPDispatcher, *corev1alpha1.Task, acpDispatchTarget, harnessv2.Fence, *recordingEnforcementCoordinator) {
	t.Helper()
	r, pool, pod, coordinator := runtimePoolEnforcementFixture(t)
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: pool.Namespace, Name: "first", UID: "first-task"},
		Spec: corev1alpha1.TaskSpec{Type: corev1alpha1.TaskTypeAgent, Enforcement: &corev1alpha1.EnforcementRequirement{
			Format: "example.policy/v1", Policy: []byte(" complete\x00policy\n"),
			RequiredCapabilities: []string{"filesystem/deny-write/v1"}, Mode: "enforce",
		}},
	}
	requirement, err := enforcement.TaskRequirement(task)
	require.NoError(t, err)
	digest, err := enforcement.RequirementDigest(requirement)
	require.NoError(t, err)
	pool.Spec.Runtime.Profile.EnforcementRequirementDigest = digest
	profile, err := runtimePoolHarnessProfile(pool.Spec.Runtime.Profile)
	require.NoError(t, err)
	profileDigest, err := harnessv2.CanonicalProfileDigest(profile)
	require.NoError(t, err)
	pool.Spec.Runtime.Profile.Digest = string(profileDigest)
	require.NoError(t, r.Update(t.Context(), pool))
	pool.Status = corev1alpha1.RuntimePoolStatus{
		ControllerEpoch: 7, Lifecycle: corev1alpha1.RuntimePoolLifecycleServing,
		AdmissionState: corev1alpha1.RuntimePoolAdmissionAccepting,
		ActiveInstance: &corev1alpha1.RuntimePoolActiveInstanceStatus{
			PodNamespace: pod.Namespace, PodName: pod.Name, PodUID: string(pod.UID),
			RuntimeInstanceID: "instance", BootID: "boot", ControllerEpoch: 7, ProfileDigest: string(profileDigest),
		},
	}
	require.NoError(t, r.Status().Update(t.Context(), pool))
	pod.Labels = map[string]string{runtimePoolUIDLabel: string(pool.UID)}
	pod.Spec = corev1.PodSpec{NodeName: "node", Containers: []corev1.Container{{
		Name: runtimeField, Image: pool.Spec.Runtime.Image,
		Env: []corev1.EnvVar{{Name: "ORKA_ACP_ENFORCEMENT_REQUIREMENT_DIGEST", Value: digest}},
	}}}
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("a", 64)
	require.NoError(t, r.Create(t.Context(), pod))
	require.NoError(t, r.Create(t.Context(), &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-uid"}}))
	fence := harnessv2.Fence{
		RuntimeInstanceID: "instance", SupervisorBootID: "boot", ControllerEpoch: 7,
		RuntimePoolUID: harnessv2.RuntimePoolUID(pool.UID), RuntimePoolGeneration: uint64(pool.Generation),
		RuntimeProfileDigest: profileDigest, ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	return &ACPDispatcher{Client: r.Client, APIReader: r.APIReader, Enforcement: coordinator}, task, acpDispatchTarget{pool: pool}, fence, coordinator
}

func TestACPEnforcementPersistsPoolIntentAndVerifiesEachConsumer(t *testing.T) {
	d, task, target, fence, coordinator := acpEnforcementFixture(t)
	coordinator.beforeEnsure = func(binding *corev1alpha1.ContainerEnforcementBinding, requirement *enforcementv1.Requirement) {
		current := &corev1alpha1.RuntimePool{}
		require.NoError(t, d.APIReader.Get(t.Context(), client.ObjectKeyFromObject(target.pool), current))
		require.Equal(t, binding, current.Status.EnforcementBinding)
		require.Equal(t, task.Spec.Enforcement.Policy, requirement.Policy)
		require.Empty(t, binding.JobUID)
		require.False(t, binding.Admitted)
		require.False(t, binding.Published)
	}
	first, err := d.ensureRuntimePoolEnforcement(t.Context(), task, target, fence)
	require.NoError(t, err)
	require.NotNil(t, first)
	secondTask := task.DeepCopy()
	secondTask.Name, secondTask.UID = "second", "second-task"
	second, err := d.ensureRuntimePoolEnforcement(t.Context(), secondTask, target, fence)
	require.NoError(t, err)
	require.Equal(t, first, second)
	require.Equal(t, 2, coordinator.ensureCalls)
	require.Zero(t, coordinator.retireCalls)
	require.Empty(t, task.Status.EnforcementBindings)
	require.Empty(t, secondTask.Status.EnforcementBindings)
}

func TestACPEnforcementCannotActivateWithoutPersistedIntent(t *testing.T) {
	d, task, target, fence, coordinator := acpEnforcementFixture(t)
	d.Client = fake.NewClientBuilder().WithScheme(newTestScheme()).WithStatusSubresource(&corev1alpha1.RuntimePool{}).Build()
	_, err := d.ensureRuntimePoolEnforcement(t.Context(), task, target, fence)
	require.Error(t, err)
	require.Zero(t, coordinator.ensureCalls)
}

func TestACPEnforcementPendingRefusalAndUncertaintyRetainPoolIntent(t *testing.T) {
	for _, outcome := range []string{"pending", "refused", "uncertain", "incomplete"} {
		t.Run(outcome, func(t *testing.T) {
			d, task, target, fence, coordinator := acpEnforcementFixture(t)
			switch outcome {
			case "pending":
				coordinator.outcome = &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Pending{Pending: &enforcementv1.Pending{Reason: "preparing"}}}
			case "refused":
				coordinator.outcome = &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Refused{Refused: &enforcementv1.Refused{Reason: "unsupported"}}}
			case "uncertain":
				coordinator.ensureErr = errors.New("RPC unavailable")
			case "incomplete":
				coordinator.outcome = &enforcementv1.EnsureResponse{}
			}
			binding, err := d.ensureRuntimePoolEnforcement(t.Context(), task, target, fence)
			require.Error(t, err)
			require.Nil(t, binding)
			current := &corev1alpha1.RuntimePool{}
			require.NoError(t, d.APIReader.Get(t.Context(), client.ObjectKeyFromObject(target.pool), current))
			require.NotNil(t, current.Status.EnforcementBinding)
			require.Zero(t, coordinator.retireCalls)
		})
	}
}

func TestACPEnforcementRejectsChangedPhysicalTargetAfterVerification(t *testing.T) {
	d, task, target, fence, coordinator := acpEnforcementFixture(t)
	coordinator.beforeEnsure = func(binding *corev1alpha1.ContainerEnforcementBinding, _ *enforcementv1.Requirement) {
		pod := &corev1.Pod{}
		require.NoError(t, d.APIReader.Get(t.Context(), client.ObjectKey{Namespace: binding.PodNamespace, Name: binding.PodName}, pod))
		pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("b", 64)
		require.NoError(t, d.Client.Status().Update(t.Context(), pod))
	}
	_, err := d.ensureRuntimePoolEnforcement(t.Context(), task, target, fence)
	require.ErrorIs(t, err, store.ErrConflict)
	current := &corev1alpha1.RuntimePool{}
	require.NoError(t, d.APIReader.Get(t.Context(), client.ObjectKeyFromObject(target.pool), current))
	require.Equal(t, "containerd://"+strings.Repeat("a", 64), current.Status.EnforcementBinding.ContainerID)
}

func TestACPEnforcementWaitsForPriorBindingRetirement(t *testing.T) {
	d, task, target, fence, coordinator := acpEnforcementFixture(t)
	current := target.pool.DeepCopy()
	current.Status.EnforcementBinding = &corev1alpha1.ContainerEnforcementBinding{BindingID: "container:previous"}
	require.NoError(t, d.Client.Status().Update(t.Context(), current))
	_, err := d.ensureRuntimePoolEnforcement(t.Context(), task, target, fence)
	require.ErrorIs(t, err, store.ErrNotReady)
	require.Zero(t, coordinator.ensureCalls)
	require.Zero(t, coordinator.retireCalls)
}

func TestACPEnforcementAllowsReservedSessionAtCapacity(t *testing.T) {
	d, task, target, fence, _ := acpEnforcementFixture(t)
	current := target.pool.DeepCopy()
	current.Status.AdmissionState = corev1alpha1.RuntimePoolAdmissionClosed
	current.Status.Capacity.ResidentSessions = corev1alpha1.DefaultRuntimePoolMaxResidentSessions
	require.NoError(t, d.Client.Status().Update(t.Context(), current))
	_, err := d.ensureRuntimePoolEnforcement(t.Context(), task, target, fence)
	require.NoError(t, err)
}

func TestACPEnforcementRejectsUnqualifiedSelectionBeforeActivation(t *testing.T) {
	for _, invalid := range []string{"changed-policy", "foreign-runtime", "missing-reader", "missing-provider", "wrong-boot", "changed-selection", "missing-finalizer", "missing-bootstrap"} {
		t.Run(invalid, func(t *testing.T) {
			d, task, target, fence, coordinator := acpEnforcementFixture(t)
			switch invalid {
			case "changed-policy":
				task.Spec.Enforcement.Policy = append(task.Spec.Enforcement.Policy, '\n')
			case "foreign-runtime":
				target.external = &corev1alpha1.AgentRuntime{}
			case "missing-reader":
				d.APIReader = nil
			case "missing-provider":
				d.Enforcement = nil
			case "wrong-boot":
				fence.SupervisorBootID = "different-boot"
			case "changed-selection":
				current := target.pool.DeepCopy()
				current.Status.ActiveInstance.PodUID = "replacement-pod"
				require.NoError(t, d.Client.Status().Update(t.Context(), current))
				pod := &corev1.Pod{}
				require.NoError(t, d.Client.Get(t.Context(), client.ObjectKey{Namespace: target.pool.Namespace, Name: "runtime"}, pod))
				require.NoError(t, d.Client.Delete(t.Context(), pod))
				pod.ResourceVersion, pod.UID = "", "replacement-pod"
				pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("b", 64)
				require.NoError(t, d.Client.Create(t.Context(), pod))
			case "missing-finalizer":
				current := target.pool.DeepCopy()
				current.Finalizers = nil
				require.NoError(t, d.Client.Update(t.Context(), current))
			case "missing-bootstrap":
				pod := &corev1.Pod{}
				require.NoError(t, d.Client.Get(t.Context(), client.ObjectKey{Namespace: target.pool.Namespace, Name: "runtime"}, pod))
				pod.Spec.Containers[0].Env = nil
				require.NoError(t, d.Client.Update(t.Context(), pod))
			}
			_, err := d.ensureRuntimePoolEnforcement(t.Context(), task, target, fence)
			require.Error(t, err)
			require.Zero(t, coordinator.ensureCalls)
		})
	}
}

func TestACPEnforcementCancellationDoesNotReturnAuthorization(t *testing.T) {
	d, task, target, fence, coordinator := acpEnforcementFixture(t)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	coordinator.beforeEnsure = func(*corev1alpha1.ContainerEnforcementBinding, *enforcementv1.Requirement) {
		cancel()
	}
	binding, err := d.ensureRuntimePoolEnforcement(ctx, task, target, fence)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, binding)
	current := &corev1alpha1.RuntimePool{}
	require.NoError(t, d.APIReader.Get(t.Context(), client.ObjectKeyFromObject(target.pool), current))
	require.NotNil(t, current.Status.EnforcementBinding)
	require.Zero(t, coordinator.retireCalls)
}

func TestACPEnforcementConcurrentConsumersShareOnePhysicalBinding(t *testing.T) {
	d, firstTask, target, fence, coordinator := acpEnforcementFixture(t)
	secondTask := firstTask.DeepCopy()
	secondTask.Name, secondTask.UID = "second", "second-task"
	type result struct {
		binding *corev1alpha1.ContainerEnforcementBinding
		err     error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, task := range []*corev1alpha1.Task{firstTask, secondTask} {
		go func() {
			<-start
			binding, err := d.ensureRuntimePoolEnforcement(t.Context(), task, target, fence)
			results <- result{binding, err}
		}()
	}
	close(start)
	first, second := <-results, <-results
	require.NoError(t, first.err)
	require.NoError(t, second.err)
	require.NotNil(t, first.binding)
	require.Equal(t, first.binding, second.binding)
	require.Equal(t, 2, coordinator.ensureCalls)
	require.Zero(t, coordinator.retireCalls)
	current := &corev1alpha1.RuntimePool{}
	require.NoError(t, d.APIReader.Get(t.Context(), client.ObjectKeyFromObject(target.pool), current))
	require.Equal(t, first.binding, current.Status.EnforcementBinding)
}
