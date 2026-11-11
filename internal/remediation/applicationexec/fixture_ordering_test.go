package applicationexec

import (
	"context"
	"fmt"
	"strings"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPeerDependentFixturesAreAllCreatedBeforeReadiness(t *testing.T) {
	f := newFixture(t)
	installPolicyFixture(t, f)
	f.backend.config.ResourcePolicy.Fixtures = []pv.ApplicationFixtureProfile{{
		ID: "peer-service", Image: f.config.WorkerImage, ImageID: f.config.WorkerImageID,
		Args: []string{"/usr/bin/sleep", "3600"}, Ports: []int{8080},
	}}
	f.request.ApplicationPolicy = f.backend.ApplicationPolicy()
	f.request.Checks.Application.Fixtures = []pv.ApplicationFixture{
		{ID: "first", Kind: "image", Profile: "peer-service", Port: 8080},
		{ID: "second", Kind: "image", Profile: "peer-service", Port: 8080},
	}
	require.NoError(t, pv.ValidateApplicationPolicy(*f.request.ApplicationPolicy))
	require.NoError(t, pv.ValidateApplicationPlanWithPolicy(*f.request.Checks.Application, f.request.ApplicationPolicy))
	r := f.newRun()
	require.NoError(t, f.backend.ensureTask(t.Context(), r, pv.Original))
	require.NoError(t, f.backend.ensureClaim(t.Context(), r))
	require.NoError(t, f.backend.ensureSideInfrastructure(t.Context(), r, pv.Original, true))
	serviceNumber := 0
	f.createHook = func(_ context.Context, _ client.WithWatch, object client.Object) error {
		if service, ok := object.(*corev1.Service); ok && service.Spec.ClusterIP == "" {
			serviceNumber++
			service.Spec.ClusterIP = fmt.Sprintf("10.96.0.%d", 20+serviceNumber)
		}
		return nil
	}
	require.NoError(t, f.backend.ensureProtected(t.Context(), r, pv.Original))
	ready, err := f.backend.prepare(t.Context(), r, pv.Original)
	require.NoError(t, err)
	require.True(t, ready)
	f.autoPods = false
	var pods []*corev1.Pod
	f.createHook = func(ctx context.Context, c client.WithWatch, object client.Object) error {
		job, ok := object.(*batchv1.Job)
		if !ok || job.Spec.Template.Spec.Containers[0].Name != "fixture" {
			return nil
		}
		pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace,
			UID: types.UID("pod-" + string(job.UID)), Labels: job.Spec.Template.Labels,
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: new(true)}}},
			Spec: *job.Spec.Template.Spec.DeepCopy(), Status: corev1.PodStatus{Phase: corev1.PodRunning,
				PodIP: fmt.Sprintf("10.244.0.%d", 30+len(pods)),
				ContainerStatuses: []corev1.ContainerStatus{{Name: "fixture", ImageID: f.config.WorkerImageID,
					ContainerID: "containerd://" + strings.ReplaceAll(string(job.UID), "-", ""),
					State:       corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}}}}}
		pod.Spec.NodeName = r.state.Target.NodeName
		if err := c.Create(ctx, pod); err != nil {
			return err
		}
		pods = append(pods, pod)
		if len(pods) == 2 {
			for _, peer := range pods {
				peer.Status.ContainerStatuses[0].Ready = true
				if err := c.Status().Update(ctx, peer); err != nil {
					return err
				}
			}
		}
		return nil
	}
	endpoints, ready, err := f.backend.protectedFixtures(t.Context(), r, pv.Original)
	require.NoError(t, err)
	require.True(t, ready, "first service can become ready only after its peer has a Job")
	require.Len(t, pods, 2)
	require.Len(t, endpoints, 2)
	require.NotEqual(t, endpoints[0].Pod.PodUID, endpoints[1].Pod.PodUID)
	created := len(f.created)
	again, ready, err := f.backend.protectedFixtures(t.Context(), r, pv.Original)
	require.NoError(t, err)
	require.True(t, ready)
	require.Equal(t, endpoints, again)
	require.Len(t, f.created, created, "readiness retries cannot recreate fixture Jobs")
}
