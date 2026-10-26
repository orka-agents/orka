package common

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/enforcement"
	"github.com/orka-agents/orka/internal/workerenv"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
	clienttesting "k8s.io/client-go/testing"
)

func TestEnforcedEntryPointCannotSilentlySkipMissingConfiguration(t *testing.T) {
	t.Setenv(workerenv.EnforcementRequirementDigest, "")
	if err := WaitForEnforcementFromEnv(context.Background(), false); err != nil {
		t.Fatalf("ordinary worker behavior changed: %v", err)
	}
	if err := WaitForEnforcementFromEnv(context.Background(), true); err == nil {
		t.Fatal("enforced entrypoint skipped admission without its frozen identity")
	}
}

func TestNativeWorkerWaitsForCurrentPodAdmission(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "tasks", UID: "pod-1"},
		Spec:       corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "worker", ContainerID: "containerd://incarnation-1",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	client := fake.NewClientset(pod.DeepCopy())
	watcher := watch.NewRaceFreeFake()
	watching := make(chan struct{})
	client.PrependWatchReactor("pods", func(_ clienttesting.Action) (bool, watch.Interface, error) {
		close(watching)
		return true, watcher, nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- WaitForPodEnforcement(ctx, client, "tasks", "worker", "pod-1", digest)
	}()
	select {
	case <-watching:
	case <-ctx.Done():
		t.Fatal("worker did not establish its authenticated Pod watch")
	}
	select {
	case err := <-done:
		t.Fatalf("worker left its hold before controller admission: %v", err)
	default:
	}
	value, err := json.Marshal(enforcement.PodAdmission{
		Version: enforcement.PodAdmissionVersion, BindingID: "container-1",
		PodUID: "pod-1", ContainerID: "containerd://incarnation-1", RequirementDigest: digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	pod.Annotations = map[string]string{enforcement.PodAdmissionAnnotation: string(value)}
	watcher.Modify(pod)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("worker did not release after exact current admission")
	}
}
