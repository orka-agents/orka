package controller

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

func enforcedACPAdmissionFixture(t *testing.T) (*ACPDispatcher, *corev1alpha1.Task, acpDispatchTarget, harnessv2.CreateRuntimeSessionRequest, store.ControllerEpochFence, *recordingEnforcementCoordinator) {
	t.Helper()
	d, task, target, runtimeFence, coordinator := acpEnforcementFixture(t)
	persistence, fence, closeStore := newACPSessionTestStore(t, filepath.Join(t.TempDir(), "admission.db"))
	t.Cleanup(closeStore)
	d.Store = persistence
	target.pool.Status.ControllerEpoch = fence.Epoch
	target.pool.Status.ActiveInstance.ControllerEpoch = fence.Epoch
	runtimeFence.ControllerEpoch = uint64(fence.Epoch)
	runtimeFence.RuntimeSessionUID, runtimeFence.RuntimeSessionGeneration = "session", 3
	task.CreationTimestamp = metav1.Now()
	task.Status.Phase = corev1alpha1.TaskPhasePending
	task.Status.Execution = &corev1alpha1.TaskExecutionStatus{
		State: corev1alpha1.TaskExecutionStatePlanned, Attempt: 1, PromptID: "prompt",
		RequestDigest: testControlDigestForDispatcher("enforced-admission"), ControllerEpoch: fence.Epoch,
		RuntimeInstanceID: string(runtimeFence.RuntimeInstanceID), RuntimeSessionUID: string(runtimeFence.RuntimeSessionUID),
		RuntimeSessionGeneration: int64(runtimeFence.RuntimeSessionGeneration),
	}
	pod, node := &corev1.Pod{}, &corev1.Node{}
	require.NoError(t, d.APIReader.Get(t.Context(), client.ObjectKey{Namespace: task.Namespace, Name: "runtime"}, pod))
	require.NoError(t, d.APIReader.Get(t.Context(), client.ObjectKey{Name: "node"}, node))
	d.Client = fake.NewClientBuilder().WithScheme(newTestScheme()).
		WithStatusSubresource(&corev1alpha1.Task{}, &corev1alpha1.RuntimePool{}, &corev1.Pod{}).
		WithObjects(task, target.pool, pod, node).Build()
	d.APIReader = d.Client
	attempt, err := persistence.CreatePromptAttempt(t.Context(), boundPromptAttemptForTest(&store.PromptAttempt{
		Key:           store.PromptAttemptKey{Namespace: task.Namespace, TaskUID: string(task.UID), Attempt: 1, PromptID: "prompt"},
		RequestDigest: task.Status.Execution.RequestDigest,
	}), fence)
	require.NoError(t, err)
	for _, state := range []store.PromptExecutionState{store.PromptExecutionReserved, store.PromptExecutionSessionStarting, store.PromptExecutionPlanned} {
		require.NoError(t, d.transitionAttempt(t.Context(), attempt.ID, fence, attempt.ExecutionState, state, string(state), &attemptRuntimeBinding{
			RuntimeInstanceID: string(runtimeFence.RuntimeInstanceID), SessionUID: string(runtimeFence.RuntimeSessionUID),
			// A Session lease generation is not its recreated runtime generation.
			SessionGeneration: 1,
		}))
		attempt, err = persistence.GetPromptAttempt(t.Context(), attempt.ID)
		require.NoError(t, err)
	}
	create := harnessv2.CreateRuntimeSessionRequest{
		Protocol: harnessv2.ProtocolVersion, PrepareOnly: true,
		Metadata:         mutationMetadata(runtimeFence, task, "prepare", false, time.Now().Add(time.Minute)),
		RuntimeSessionID: harnessv2.RuntimeSessionID(runtimeSessionID(runtimeFence)),
	}
	return d, task, target, create, fence, coordinator
}

func TestACPAdmissionPreservesPreRPCObservation(t *testing.T) {
	d, task, target, create, fence, coordinator := enforcedACPAdmissionFixture(t)
	observation := task.DeepCopy()
	coordinator.beforeEnsure = func(_ *corev1alpha1.ContainerEnforcementBinding, _ *enforcementv1.Requirement) {
		current := &corev1alpha1.Task{}
		require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
		current.Status.EnforcementOutcome = enforcement.Outcome("EnforcementRefused", "supported-format")
		current.Status.EnforcementOutcome.ObservedGeneration = current.Generation
		require.NoError(t, d.Client.Status().Update(t.Context(), current))
	}
	binding, err := d.admitEnforcedRuntimeSession(t.Context(), nil, task, target, create, false, fence)
	require.NoError(t, err)
	status := TaskReconciler{Client: d.Client, APIReader: d.APIReader}
	require.Error(t, status.markEnforcementAdmission(t.Context(), task, binding, observation))
	current := &corev1alpha1.Task{}
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Nil(t, current.Status.EnforcementAdmission)
	require.Equal(t, "EnforcementRefused", current.Status.EnforcementOutcome.Reason)
}

func enforcedACPLaunchClient(t *testing.T, handler http.HandlerFunc, options ...harnessv2.ClientOption) *harnessv2.Client {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	options = append(options,
		harnessv2.WithControllerBearerToken(strings.Repeat("t", 32)),
		harnessv2.WithOperationCapabilitySecret([]byte(strings.Repeat("s", 32))))
	c, err := harnessv2.NewClient(server.URL, options...)
	require.NoError(t, err)
	return c
}

func TestACPEnforcementAdmissionLaunchesOnlyAfterCurrentVerification(t *testing.T) {
	d, task, target, create, fence, coordinator := enforcedACPAdmissionFixture(t)
	requests := make(chan harnessv2.LaunchRuntimeSessionRequest, 1)
	c := enforcedACPLaunchClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request harnessv2.LaunchRuntimeSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests <- request
		f := request.Metadata.Fence
		writeDispatcherJSON(w, harnessv2.LaunchRuntimeSessionResponse{
			Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
			PreparationOperationID: request.PreparationOperationID,
			Session: harnessv2.RuntimeSessionDescriptor{
				RuntimeSessionID: create.RuntimeSessionID, RuntimeSessionUID: f.RuntimeSessionUID, Generation: f.RuntimeSessionGeneration,
				RuntimeInstanceID: f.RuntimeInstanceID, SupervisorBootID: f.SupervisorBootID, RuntimeProfileDigest: f.RuntimeProfileDigest,
				State: harnessv2.RuntimeSessionStateIdle, ProviderSessionID: "provider",
				WorkspaceBaseline: harnessv2.WorkspaceBaseline{RepositoryIdentity: "repo", Revision: "revision", TreeDigest: store.CanonicalBytesDigest(nil)},
				CreatedAt:         time.Now(), LastTransitionAt: time.Now(),
			},
		})
	})
	coordinator.beforeEnsure = func(_ *corev1alpha1.ContainerEnforcementBinding, _ *enforcementv1.Requirement) {
		require.Empty(t, requests)
	}
	binding, err := d.admitEnforcedRuntimeSession(t.Context(), c, task, target, create, true, fence)
	require.NoError(t, err)
	require.NotNil(t, binding)
	require.Equal(t, string(target.pool.Status.ActiveInstance.PodUID), binding.PodUID)
	status := TaskReconciler{Client: d.Client, APIReader: d.APIReader}
	require.NoError(t, status.markEnforcementAdmission(t.Context(), task, binding, task.DeepCopy()))
	observed := &corev1alpha1.Task{}
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), observed))
	require.Equal(t, enforcementAdmission(task.Generation, binding), *observed.Status.EnforcementAdmission)
	require.Empty(t, observed.Status.EnforcementBindings, "the consuming ACP Task must not own pool cleanup")
	request := <-requests
	require.Equal(t, create.Metadata.OperationID, request.PreparationOperationID)
	require.NotEqual(t, create.Metadata.OperationID, request.Metadata.OperationID)
	require.Equal(t, create.Metadata.TaskUID, request.Metadata.TaskUID)
	require.Equal(t, create.Metadata.TaskAttempt, request.Metadata.TaskAttempt)
	require.Equal(t, create.Metadata.Fence, request.Metadata.Fence)
	require.NoError(t, request.ValidateAt(time.Now()))
	require.Equal(t, 1, coordinator.ensureCalls)
	require.Zero(t, coordinator.retireCalls)
}

func TestACPEnforcementAdmissionRechecksAuthorityAfterProviderWait(t *testing.T) {
	for _, changed := range []string{"cancelled", "changed-attempt", "changed-policy", "expired", "lost-epoch", "terminal-attempt"} {
		t.Run(changed, func(t *testing.T) {
			d, task, target, create, fence, coordinator := enforcedACPAdmissionFixture(t)
			var launches atomic.Int32
			c := enforcedACPLaunchClient(t, func(w http.ResponseWriter, _ *http.Request) {
				launches.Add(1)
				w.WriteHeader(http.StatusInternalServerError)
			})
			coordinator.beforeEnsure = func(_ *corev1alpha1.ContainerEnforcementBinding, _ *enforcementv1.Requirement) {
				current := task.DeepCopy()
				require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
				switch changed {
				case "cancelled":
					current.Status.Phase = corev1alpha1.TaskPhaseCancelled
					require.NoError(t, d.Client.Status().Update(t.Context(), current))
				case "changed-attempt":
					current.Status.Execution.Attempt++
					require.NoError(t, d.Client.Status().Update(t.Context(), current))
				case "changed-policy":
					current.Spec.Enforcement.Policy = []byte("different")
					require.NoError(t, d.Client.Update(t.Context(), current))
				case "expired":
					current.Spec.Timeout = &metav1.Duration{Duration: time.Nanosecond}
					require.NoError(t, d.Client.Update(t.Context(), current))
				case "lost-epoch":
					epoch, err := d.Store.GetControllerEpoch(t.Context(), fence.Name)
					require.NoError(t, err)
					_, err = d.Store.CompareAndSwapControllerEpoch(t.Context(), store.ControllerEpochCAS{
						Name: epoch.Name, ExpectedVersion: epoch.Version, ExpectedEpoch: epoch.Epoch,
						NewEpoch: epoch.Epoch + 1, HolderID: "replacement-controller",
						RequestDigest: testControlDigestForDispatcher("replacement"), UpdatedAt: time.Now(),
					})
					require.NoError(t, err)
				case "terminal-attempt":
					id, err := promptAttemptIDFromTask(task)
					require.NoError(t, err)
					require.NoError(t, d.transitionAttempt(t.Context(), id, fence, store.PromptExecutionPlanned, store.PromptExecutionFailed, "failed", nil))
				}
			}
			binding, err := d.admitEnforcedRuntimeSession(t.Context(), c, task, target, create, true, fence)
			require.Error(t, err)
			require.Nil(t, binding)
			require.Zero(t, launches.Load())
			require.Zero(t, coordinator.retireCalls)
		})
	}
}

func TestACPEnforcementAdmissionReuseVerifiesWithoutRelaunch(t *testing.T) {
	d, task, target, create, fence, coordinator := enforcedACPAdmissionFixture(t)
	binding, err := d.admitEnforcedRuntimeSession(t.Context(), nil, task, target, create, false, fence)
	require.NoError(t, err)
	require.NotNil(t, binding)
	require.Equal(t, 1, coordinator.ensureCalls)
	require.Zero(t, coordinator.retireCalls)
}

func TestEnforcementAdmissionObservationRefusesChangedRequirement(t *testing.T) {
	d, task, target, create, fence, _ := enforcedACPAdmissionFixture(t)
	binding, err := d.admitEnforcedRuntimeSession(t.Context(), nil, task, target, create, false, fence)
	require.NoError(t, err)
	current := &corev1alpha1.Task{}
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	current.Spec.Enforcement.Policy = []byte("different policy")
	require.NoError(t, d.Client.Update(t.Context(), current))
	status := TaskReconciler{Client: d.Client, APIReader: d.APIReader}
	require.Error(t, status.markEnforcementAdmission(t.Context(), task, binding, task.DeepCopy()))
	require.NoError(t, d.Client.Get(t.Context(), client.ObjectKeyFromObject(task), current))
	require.Nil(t, current.Status.EnforcementAdmission)
}

func TestACPEnforcementAdmissionRPCDeadlineIsNotConsumerExpiry(t *testing.T) {
	d, task, target, create, fence, _ := enforcedACPAdmissionFixture(t)
	c := enforcedACPLaunchClient(t, func(_ http.ResponseWriter, r *http.Request) {
		var request harnessv2.LaunchRuntimeSessionRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}

		<-r.Context().Done()
	}, harnessv2.WithControlTimeout(20*time.Millisecond))
	_, err := d.admitEnforcedRuntimeSession(t.Context(), c, task, target, create, true, fence)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.NotErrorIs(t, err, errEnforcedACPTaskAuthority)
	require.NoError(t, d.verifyEnforcedACPTaskAuthority(t.Context(), task, create.Metadata.Fence, fence))
}

type enforcedACPTaskReadDeadline struct{ client.Reader }

func (r enforcedACPTaskReadDeadline) Get(ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption) error {
	if _, task := object.(*corev1alpha1.Task); task {
		return context.DeadlineExceeded
	}
	return r.Reader.Get(ctx, key, object, options...)
}

type enforcedACPStoreReadDeadline struct {
	store.DurableControlStore
	epoch bool
}

func (s enforcedACPStoreReadDeadline) GetPromptAttempt(ctx context.Context, id string) (*store.PromptAttempt, error) {
	if !s.epoch {
		return nil, context.DeadlineExceeded
	}
	return s.DurableControlStore.GetPromptAttempt(ctx, id)
}

func (s enforcedACPStoreReadDeadline) GetControllerEpoch(ctx context.Context, name string) (*store.ControllerEpoch, error) {
	if s.epoch {
		return nil, context.DeadlineExceeded
	}
	return s.DurableControlStore.GetControllerEpoch(ctx, name)
}

func TestACPEnforcementAdmissionAuthorityReadDeadlineIsNotConsumerExpiry(t *testing.T) {
	for _, dependency := range []string{"task", "attempt", "epoch"} {
		t.Run(dependency, func(t *testing.T) {
			d, task, target, create, fence, _ := enforcedACPAdmissionFixture(t)
			reader, persistence := d.APIReader, d.Store
			switch dependency {
			case "task":
				d.APIReader = enforcedACPTaskReadDeadline{Reader: reader}
			default:
				d.Store = enforcedACPStoreReadDeadline{DurableControlStore: persistence, epoch: dependency == "epoch"}
			}
			_, err := d.admitEnforcedRuntimeSession(t.Context(), nil, task, target, create, true, fence)
			require.ErrorIs(t, err, context.DeadlineExceeded)
			require.NotErrorIs(t, err, errEnforcedACPTaskAuthority)
			d.APIReader, d.Store = reader, persistence
			require.NoError(t, d.verifyEnforcedACPTaskAuthority(t.Context(), task, create.Metadata.Fence, fence))
		})
	}
}
