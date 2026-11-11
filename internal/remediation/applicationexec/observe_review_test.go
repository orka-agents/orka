package applicationexec

import (
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func readinessFixture(t *testing.T, initializer bool) (*fixture, *run, *batchv1.Job, *corev1.Pod, string) {
	t.Helper()
	f := newFixture(t)
	f.autoPods = false
	r := f.newRun()
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, false))
	image, imageID := f.config.WorkerImage, f.config.WorkerImageID
	if initializer {
		image, imageID = f.config.ToolchainImage, f.config.ToolchainImageID
	}
	job, err := f.backend.ensureJob(t.Context(), r, pv.Original, "observe-ready", struct{}{}, jobOptions{
		role: "observer", mode: "observe", image: image, taskUID: string(r.state.Task.UID),
		serviceAccount: "observer", receiverPort: 8080, target: "receiver", deadline: 30})
	require.NoError(t, err)
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "observer-ready", Namespace: job.Namespace, UID: "observer-ready",
		Labels: map[string]string{runLabel: string(r.object.UID), "job-name": job.Name, pv.ApplicationTargetLabel: "receiver"},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID,
			Controller: new(true)}}},
		Spec: *job.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.244.0.10",
			ContainerStatuses: []corev1.ContainerStatus{{Name: "observe", Image: image, ImageID: imageID,
				ContainerID: "containerd://observer-ready", State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
	pod.Spec.NodeName = r.state.Target.NodeName
	if initializer {
		pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{Name: "install", ImageID: f.config.WorkerImageID,
			ContainerID: "containerd://installer", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}}
	}
	require.NoError(t, f.target.Create(t.Context(), pod))
	return f, r, job, pod, imageID
}

func TestObserverWaitsForReadinessWithAndWithoutInitializer(t *testing.T) {
	for _, initializer := range []bool{false, true} {
		name := "no-initializer"
		if initializer {
			name = "successful-initializer"
		}
		t.Run(name, func(t *testing.T) {
			f, r, job, pod, imageID := readinessFixture(t, initializer)
			observed, identity, ready, err := f.backend.observeJob(t.Context(), r, pv.Original, "observe-ready", job, imageID, false)
			require.NoError(t, err)
			require.NotNil(t, observed)
			require.Equal(t, string(pod.UID), identity.PodUID)
			require.False(t, ready, "a running worker is not a ready TCP receiver")
			pod.Status.ContainerStatuses[0].Ready = true
			require.NoError(t, f.target.Status().Update(t.Context(), pod))
			_, current, ready, err := f.backend.observeJob(t.Context(), r, pv.Original, "observe-ready", job, imageID, false)
			require.NoError(t, err)
			require.True(t, ready)
			require.Equal(t, identity, current)
		})
	}
}

func TestObserverReadyCannotHideInvalidInitializer(t *testing.T) {
	for _, which := range []string{"missing", "failed", "wrong-image", "restarted", "still-running"} {
		t.Run(which, func(t *testing.T) {
			f, r, job, pod, imageID := readinessFixture(t, true)
			pod.Status.ContainerStatuses[0].Ready = true
			switch which {
			case "missing":
				pod.Status.InitContainerStatuses = nil
			case "failed":
				pod.Status.InitContainerStatuses[0].State.Terminated.ExitCode = 1
			case "wrong-image":
				pod.Status.InitContainerStatuses[0].ImageID = pv.Digest([]byte("unapproved-installer"))
			case "restarted":
				pod.Status.InitContainerStatuses[0].RestartCount = 1
			case "still-running":
				pod.Status.InitContainerStatuses[0].State = corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}
			}
			require.NoError(t, f.target.Status().Update(t.Context(), pod))
			_, _, ready, err := f.backend.observeJob(t.Context(), r, pv.Original, "observe-ready", job, imageID, false)
			require.ErrorIs(t, err, contract.ErrConflict)
			require.False(t, ready)
		})
	}
}
