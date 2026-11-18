package applicationexec

import (
	"context"
	"encoding/json"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func stoppingReceiptFixture(t *testing.T) (*fixture, *run, *corev1.Pod, []byte) {
	t.Helper()
	f := newFixture(t)
	installPolicyFixture(t, f)
	r := f.newRun()
	require.NoError(t, f.backend.ensureTask(t.Context(), r, pv.Original))
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, true))
	require.NoError(t, f.backend.ensureProtected(t.Context(), r, pv.Original))
	ready, err := f.backend.prepare(t.Context(), r, pv.Original)
	require.NoError(t, err)
	require.True(t, ready)
	check := r.state.Request.Checks.Checks[0]
	actors, err := f.backend.actorIdentities(r, pv.Original, check)
	require.NoError(t, err)
	targets, err := f.backend.reserveActions(t.Context(), r, pv.Original, "observe-0", check, actors)
	require.NoError(t, err)
	input, err := f.backend.workerInput(r, pv.Original, check.ID, "observe")
	require.NoError(t, err)
	input.Actors, input.ActionTargets = actors, targets
	require.NoError(t, pv.ValidateApplicationWorkerInput(input))
	f.autoPods = false
	physicalSide := protectedSide(pv.Original)
	job, err := f.backend.ensureJob(t.Context(), r, physicalSide, "observe-0", input, jobOptions{
		role: "observer", mode: "observe", image: f.config.WorkerImage, taskUID: input.TaskUID, serviceAccount: "observer"})
	require.NoError(t, err)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "receipt-observer", Namespace: job.Namespace, UID: "receipt-observer",
		Labels: map[string]string{runLabel: string(r.object.UID), "job-name": job.Name}, Finalizers: []string{actionReceiptFinalizer},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true)}}},
		Spec: *job.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodFailed,
			ContainerStatuses: []corev1.ContainerStatus{{Name: "observe", ImageID: f.config.WorkerImageID, ContainerID: "containerd://receipt",
				State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1}}}}}}
	pod.Spec.NodeName = r.state.Target.NodeName
	require.NoError(t, f.target.Create(t.Context(), pod))
	_, _, _, err = f.backend.observeJob(t.Context(), r, physicalSide, "observe-0", job, f.config.WorkerImageID, false, true)
	require.NoError(t, err)
	object, err := pv.ValidateApplicationResourceWithPolicy(check.Application.Actions[0].Resource, r.state.Request.ApplicationPolicy)
	require.NoError(t, err)
	object.SetNamespace(namespaceName(r, pv.Original))
	object.SetUID(types.UID("actor-created-before-exit"))
	require.NoError(t, f.target.Create(t.Context(), object))
	result := pv.ApplicationActionObservation{Index: 0, Actor: actors[0], Target: targets[0],
		APIVersion: targets[0].APIVersion, Resource: targets[0].Resource, Name: targets[0].Name,
		StatusCode: 201, UID: string(object.GetUID()), ResourceVersion: object.GetResourceVersion()}
	digest, err := pv.ApplicationWorkerInputDigest(input)
	require.NoError(t, err)
	frame := pv.ApplicationStreamFrame{Version: pv.ApplicationProtocolVersion, Kind: "action",
		TaskUID: input.TaskUID, PodUID: string(pod.UID), InputDigest: digest, Action: &result}
	content, err := json.Marshal(frame)
	require.NoError(t, err)
	uid := pod.UID
	require.NoError(t, f.target.Delete(t.Context(), pod, client.Preconditions{UID: &uid}))
	return f, r, pod, append(content, '\n')
}

func TestTransientTranscriptReadRetainsFinalizerUntilReceiptRecovery(t *testing.T) {
	f, r, pod, content := stoppingReceiptFixture(t)
	require.ErrorIs(t, f.backend.collectStoppingReceipts(t.Context(), r), errReceiptsPending)
	retained := &corev1.Pod{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKeyFromObject(pod), retained))
	require.Contains(t, retained.Finalizers, actionReceiptFinalizer)
	require.Nil(t, r.state.Actions[actionSlot(pv.Original, "observe-0", 0)].Outcome)
	f.reports.Store(pod.Namespace+"/"+pod.Name, content)
	require.NoError(t, f.backend.collectStoppingReceipts(t.Context(), r))
	require.NotNil(t, r.state.Actions[actionSlot(pv.Original, "observe-0", 0)].Outcome)
	require.True(t, apierrors.IsNotFound(f.target.Get(t.Context(), client.ObjectKeyFromObject(pod), retained)))
}

func TestTransientOutcomeSaveCannotReleaseOnlyRecoverableReceipt(t *testing.T) {
	f, r, pod, content := stoppingReceiptFixture(t)
	f.reports.Store(pod.Namespace+"/"+pod.Name, content)
	slot := actionSlot(pv.Original, "observe-0", 0)
	failSave := true
	f.backend.control = interceptor.NewClient(f.control.(client.WithWatch), interceptor.Funcs{
		Patch: func(ctx context.Context, c client.WithWatch, object client.Object, patch client.Patch, options ...client.PatchOption) error {
			if cm, ok := object.(*corev1.ConfigMap); ok && failSave {
				var proposed state
				require.NoError(t, json.Unmarshal([]byte(cm.Data[stateKey]), &proposed))
				if proposed.Actions[slot].Outcome != nil {
					return apierrors.NewServiceUnavailable("fixture outcome save failed")
				}
			}
			return c.Patch(ctx, object, patch, options...)
		},
	})
	require.ErrorIs(t, f.backend.collectStoppingReceipts(t.Context(), r), errReceiptsPending)
	require.Nil(t, r.state.Actions[slot].Outcome, "failed in-memory save is not a durable outcome")
	current := &corev1.Pod{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKeyFromObject(pod), current))
	require.Contains(t, current.Finalizers, actionReceiptFinalizer)
	failSave = false
	require.NoError(t, f.backend.collectStoppingReceipts(t.Context(), r))
	require.NotNil(t, r.state.Actions[slot].Outcome)
	require.True(t, apierrors.IsNotFound(f.target.Get(t.Context(), client.ObjectKeyFromObject(pod), current)))
}

func TestUnknownActorCreateKeepsReceiptFenceAndUnsettledClaim(t *testing.T) {
	f, r, pod, _ := stoppingReceiptFixture(t)
	f.reports.Store(pod.Namespace+"/"+pod.Name, []byte{})
	for range 2 {
		require.ErrorIs(t, f.backend.collectStoppingReceipts(t.Context(), r), errReceiptsPending)
	}
	action := r.state.Actions[actionSlot(pv.Original, "observe-0", 0)]
	require.Nil(t, action.Outcome)
	require.Empty(t, r.effect(action.EffectKey).UID, "a live same-name resource cannot supply an unknown create UID")
	retained := &corev1.Pod{}
	require.NoError(t, f.target.Get(t.Context(), client.ObjectKeyFromObject(pod), retained))
	require.Contains(t, retained.Finalizers, actionReceiptFinalizer)
	require.False(t, r.state.Settled)
	require.ErrorIs(t, EnsureNoActive(t.Context(), f.control, "control"), ErrActive)
}
