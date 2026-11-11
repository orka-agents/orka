package kube

import (
	"context"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestCleanupPagesAuthoritativePodsByExactOwnerUID(t *testing.T) {
	s, _ := testService(t, true)
	calls := 0
	reader := interceptor.NewClient(s.client.(client.WithWatch), interceptor.Funcs{
		List: func(_ context.Context, _ client.WithWatch, list client.ObjectList, options ...client.ListOption) error {
			opts := (&client.ListOptions{}).ApplyOptions(options)
			require.Equal(t, "default", opts.Namespace)
			require.Equal(t, int64(64), opts.Limit)
			pods := list.(*corev1.PodList)
			calls++
			if calls == 1 {
				require.Empty(t, opts.Continue)
				pods.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{Kind: "Job", UID: "other"}}}}}
				pods.Continue = "next-page"
			} else {
				require.Equal(t, "next-page", opts.Continue)
				pods.Items = []corev1.Pod{{ObjectMeta: metav1.ObjectMeta{OwnerReferences: []metav1.OwnerReference{{Kind: "Job", UID: "owned"}}}}}
			}
			return nil
		},
	})
	found, err := HasJobPods(t.Context(), reader, "default", map[string]bool{"owned": true})
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, 2, calls)
}

func TestRuntimeIdentityRequiresSuccessfulPinnedHelper(t *testing.T) {
	s, _ := testService(t, true)
	manifest, _ := testManifest(t, s)
	submission := &pv.KubernetesSubmission{Manifest: manifest}
	main := corev1.ContainerStatus{Name: "validation", ImageID: manifest.Environment.ImageID, ContainerID: "main",
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}}
	pod := &corev1.Pod{Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{
		{Name: "install-runner", ImageID: s.config.HelperImageID, ContainerID: "helper",
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}}},
	}}}
	require.True(t, runtimeIdentityMatches(submission, main, pod))
	require.NotEqual(t, imageDigest(s.config.HelperImage), s.config.HelperImageID)
	for _, mutate := range []func(*corev1.ContainerStatus){
		func(s *corev1.ContainerStatus) { s.ImageID = "unknown" },
		func(status *corev1.ContainerStatus) { status.ImageID = s.config.HelperImage },
		func(s *corev1.ContainerStatus) { s.RestartCount = 1 },
		func(s *corev1.ContainerStatus) { s.ContainerID = "" },
		func(s *corev1.ContainerStatus) { s.State.Terminated.ExitCode = 1 },
		func(s *corev1.ContainerStatus) { s.State.Terminated = nil },
	} {
		copied := pod.DeepCopy()
		mutate(&copied.Status.InitContainerStatuses[0])
		require.False(t, runtimeIdentityMatches(submission, main, copied))
	}
}
