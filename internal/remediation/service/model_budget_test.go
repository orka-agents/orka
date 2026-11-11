package service

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

func boundedRunFixture(t *testing.T) (*serviceFixture, *run) {
	t.Helper()
	f := newServiceFixture(t)
	f.service.config.BoundedExecution = true
	request := f.request(contract.Generate)
	_, err := f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	r, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.True(t, r.state.Authority.BoundedExecution)
	require.NoError(t, f.service.acquireRunSlot(t.Context(), r))
	return f, r
}

func boundedRoleFixture(t *testing.T, f *serviceFixture, id, role string) contract.ModelCallClaim {
	t.Helper()
	r, err := f.service.load(t.Context(), id)
	require.NoError(t, err)
	if r.state.Parent.UID == "" {
		parent := f.service.newTask(r, parentRole, "")
		spec, err := f.service.putJSON(t.Context(), r, parent)
		require.NoError(t, err)
		parent.UID = "parent-uid"
		require.NoError(t, f.client.Create(t.Context(), parent))
		parent.Status.Phase = corev1alpha1.TaskPhaseRunning
		require.NoError(t, f.client.Status().Update(t.Context(), parent))
		r.state.Parent = taskOperation{Name: parent.Name, UID: string(parent.UID), Attempted: true, Spec: spec}
		r.state.Status.TaskName, r.state.Status.TaskUID = parent.Name, string(parent.UID)
		r.state.Status.Phase = contract.PhaseRunning
	}
	task := f.service.newTask(r, role, "synthetic proposal")
	spec, err := f.service.putJSON(t.Context(), r, task)
	require.NoError(t, err)
	task.UID = types.UID("task-" + role)
	r.state.Agents[role] = &taskOperation{Name: task.Name, UID: string(task.UID), Attempted: true, Spec: spec}
	require.NoError(t, f.service.save(t.Context(), r))
	require.NoError(t, f.client.Create(t.Context(), task))
	job := f.proposalJob(task)
	job.UID = types.UID("job-" + role)
	job.Spec.BackoffLimit = new(int32)
	job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	job.Spec.Template.Spec.Containers[0].Env = task.Spec.Env
	require.NoError(t, f.client.Create(t.Context(), job))
	task.Status.JobName, task.Status.JobUID = job.Name, string(job.UID)
	task.Status.Phase = corev1alpha1.TaskPhaseRunning
	require.NoError(t, f.client.Status().Update(t.Context(), task))
	return contract.ModelCallClaim{
		Namespace: task.Namespace, TaskName: task.Name, TaskUID: string(task.UID),
		JobUID: string(job.UID), PodUID: "pod-" + role,
	}
}

func TestModelCallPermitIsDurableSingleUseAcrossPodsAndRestart(t *testing.T) {
	f, r := boundedRunFixture(t)
	claim := boundedRoleFixture(t, f, r.state.ID, "checks")
	require.NoError(t, f.service.ClaimModelCall(t.Context(), claim))
	require.ErrorIs(t, f.service.ClaimModelCall(t.Context(), claim), contract.ErrConflict)
	claim.PodUID = "replacement-pod"
	restarted, err := New(f.service.config)
	require.NoError(t, err)
	require.ErrorIs(t, restarted.ClaimModelCall(t.Context(), claim), contract.ErrConflict)
	saved, err := restarted.load(t.Context(), r.state.ID)
	require.NoError(t, err)
	require.Equal(t, "pod-checks", saved.state.Agents["checks"].ModelCall.PodUID)
}

func TestModelCallPermitConcurrentClaimsAuthorizeOnlyOneAttempt(t *testing.T) {
	f, r := boundedRunFixture(t)
	claim := boundedRoleFixture(t, f, r.state.ID, "checks")
	var accepted atomic.Int32
	var workers sync.WaitGroup
	for index := range 12 {
		workers.Go(func() {
			other := claim
			other.PodUID = fmt.Sprintf("pod-%d", index)
			if f.service.ClaimModelCall(t.Context(), other) == nil {
				accepted.Add(1)
			}
		})
	}
	workers.Wait()
	require.EqualValues(t, 1, accepted.Load())
}

func TestBoundedRolesCannotExceedTenReservedRequests(t *testing.T) {
	f, r := boundedRunFixture(t)
	for index := range contract.MaxModelCalls {
		claim := boundedRoleFixture(t, f, r.state.ID, fmt.Sprintf("role-%d", index))
		require.NoError(t, f.service.ClaimModelCall(t.Context(), claim))
	}
	r, err := f.service.load(t.Context(), r.state.ID)
	require.NoError(t, err)
	_, ready, err := f.service.ensureTask(t.Context(), r, "overflow", "synthetic")
	require.False(t, ready)
	var failure *stageFailure
	require.ErrorAs(t, err, &failure)
	require.Equal(t, "model-call-budget-exhausted", failure.reason)
	var tasks corev1alpha1.TaskList
	require.NoError(t, f.client.List(t.Context(), &tasks))
	require.Len(t, tasks.Items, contract.MaxModelCalls+1, "the additional parent is inert, not a model role")
}

func TestModelCallPermitRejectsChangedJobPolicyCancellationAndDeadline(t *testing.T) {
	for _, invalid := range []string{"worker-image", "retry", "environment", "job-uid", "cancel", "parent-cancel", "deadline", "authority"} {
		t.Run(invalid, func(t *testing.T) {
			f, r := boundedRunFixture(t)
			claim := boundedRoleFixture(t, f, r.state.ID, "checks")
			switch invalid {
			case "cancel", "deadline":
				_, err := f.service.update(t.Context(), r.state.ID, func(current *run) error {
					if invalid == "cancel" {
						current.state.CancelRequested = true
					} else {
						current.state.CreatedAt = time.Now().Add(-time.Minute)
						current.state.Deadline = time.Now().Add(-time.Second)
					}
					return nil
				})
				require.NoError(t, err)
			case "job-uid":
				claim.JobUID = "replacement-job"
			case "authority":
				f.backend.configurationVersion.Add(1)
			case "parent-cancel":
				current, err := f.service.load(t.Context(), r.state.ID)
				require.NoError(t, err)
				require.NoError(t, f.service.markParentCancelled(t.Context(), current))
				require.False(t, current.state.CancelRequested, "parent revocation must work before the state update is acknowledged")
			default:
				job := new(batchv1.Job)
				require.NoError(t, f.client.Get(t.Context(), client.ObjectKey{
					Namespace: "private", Name: claim.TaskName + "-job",
				}, job))
				switch invalid {
				case "worker-image":
					job.Spec.Template.Spec.Containers[0].Image = "example.invalid/unqualified:latest"
				case "retry":
					*job.Spec.BackoffLimit = 1
				case "environment":
					job.Spec.Template.Spec.Containers[0].Env = nil
				}
				require.NoError(t, f.client.Update(t.Context(), job))
			}
			require.Error(t, f.service.ClaimModelCall(t.Context(), claim))
			saved, err := f.service.load(t.Context(), r.state.ID)
			require.NoError(t, err)
			require.Nil(t, saved.state.Agents["checks"].ModelCall)
		})
	}
}

func TestBoundedRunSlotSurvivesDeadlineAndCoordinatorRestart(t *testing.T) {
	f, first := boundedRunFixture(t)
	secondRequest := f.request(contract.Validate)
	secondRequest.RequestID = "request-two"
	_, err := f.service.Submit(t.Context(), "private", "subject", secondRequest)
	require.NoError(t, err)
	second, err := f.service.load(t.Context(), secondRequest.RequestID)
	require.NoError(t, err)
	restarted, err := New(f.service.config)
	require.NoError(t, err)
	require.ErrorIs(t, restarted.acquireRunSlot(t.Context(), second), contract.ErrConflict)
	require.NoError(t, restarted.acquireRunSlot(t.Context(), first))
	first.state.CreatedAt, first.state.Deadline = time.Now().Add(-time.Minute), time.Now().Add(-time.Second)
	require.NoError(t, restarted.save(t.Context(), first))
	require.ErrorIs(t, restarted.acquireRunSlot(t.Context(), second), contract.ErrConflict,
		"deadline is not proof that earlier model or application effects settled")
	first.state.Status.Phase, first.state.Status.Stage = contract.PhaseCancelled, contract.StageComplete
	first.state.Status.Outcome, first.state.Status.Reason = contract.OutcomeCancelled, reasonCancelled
	first.state.Settled = true
	require.NoError(t, restarted.save(t.Context(), first))
	require.NoError(t, restarted.releaseRunSlot(t.Context(), first))
	require.NoError(t, restarted.acquireRunSlot(t.Context(), second))
}

func TestSucceededTaskReloadsConcurrentModelPermit(t *testing.T) {
	f, r := boundedRunFixture(t)
	claim := boundedRoleFixture(t, f, r.state.ID, "select")
	r, err := f.service.load(t.Context(), r.state.ID)
	require.NoError(t, err)
	task := new(corev1alpha1.Task)
	require.NoError(t, f.client.Get(t.Context(), client.ObjectKey{Namespace: claim.Namespace, Name: claim.TaskName}, task))
	ready, err := f.service.observeNativeJob(t.Context(), r, "select", task)
	require.NoError(t, err)
	require.False(t, ready)
	stale, err := f.service.load(t.Context(), r.state.ID)
	require.NoError(t, err)
	require.Nil(t, stale.state.Agents["select"].ModelCall)
	require.NoError(t, f.service.ClaimModelCall(t.Context(), claim))
	require.NoError(t, f.results.SaveResult(t.Context(), task.Namespace, task.Name, []byte(f.responses["select"])))
	task.Status.Phase = corev1alpha1.TaskPhaseSucceeded
	require.NoError(t, f.client.Status().Update(t.Context(), task))

	_, ready, err = f.service.agentOutput(t.Context(), stale, "select", "synthetic proposal")
	require.ErrorIs(t, err, contract.ErrUnavailable, "a stale negative claim read is retryable, not an identity conflict")
	require.False(t, ready)
	for range 2 {
		current, err := f.service.load(t.Context(), r.state.ID)
		require.NoError(t, err)
		_, _, err = f.service.agentOutput(t.Context(), current, "select", "synthetic proposal")
		require.NoError(t, err)
	}
	current, err := f.service.load(t.Context(), r.state.ID)
	require.NoError(t, err)
	output, ready, err := f.service.agentOutput(t.Context(), current, "select", "synthetic proposal")
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, f.responses["select"], output)
	require.Equal(t, claim.PodUID, current.state.Agents["select"].ModelCall.PodUID)
	require.ErrorIs(t, f.service.ClaimModelCall(t.Context(), claim), contract.ErrConflict, "the permit is still single-use")
}

func TestBoundedExecutionLimitsAreOptInAndFrozen(t *testing.T) {
	f := newServiceFixture(t)
	config := f.service.config
	config.RunTimeout = 2 * time.Hour
	_, err := New(config)
	require.NoError(t, err, "legacy source-harness timeout behavior is preserved")
	config.BoundedExecution = true
	_, err = New(config)
	require.ErrorIs(t, err, contract.ErrInvalid)
	config.RunTimeout = contract.MaxRunDuration
	_, err = New(config)
	require.NoError(t, err)
	config.BoundedExecution = false
	config.Backend = scopedBackend{ExecutionBackend: f.backend, scope: contract.ApplicationRuntime}
	_, err = New(config)
	require.ErrorIs(t, err, contract.ErrInvalid, "application authority cannot omit the bounded native policy")
}

func TestBoundedCancellationPersistsExactParentRevocation(t *testing.T) {
	f, r := boundedRunFixture(t)
	claim := boundedRoleFixture(t, f, r.state.ID, "checks")
	_, err := f.service.Cancel(t.Context(), "private", r.state.ID)
	require.NoError(t, err)
	current, err := f.service.load(t.Context(), r.state.ID)
	require.NoError(t, err)
	require.True(t, current.state.CancelRequested)
	parent := new(corev1alpha1.Task)
	require.NoError(t, f.client.Get(t.Context(), client.ObjectKey{Namespace: "private", Name: current.state.Parent.Name}, parent))
	require.Equal(t, current.state.Parent.UID, string(parent.UID))
	require.NotEmpty(t, parent.Annotations[labels.AnnotationCancelRequested])
	require.Error(t, f.service.ClaimModelCall(t.Context(), claim))
}
