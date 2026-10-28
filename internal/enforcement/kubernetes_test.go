package enforcement

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestKubernetesContainerBindingMatchesPythonAndNotConsumerIdentity(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "worker", Namespace: "default", UID: "pod-uid"},
		Spec:       corev1.PodSpec{NodeName: "node"},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "worker", ContainerID: "containerd://" + strings.Repeat("a", 64),
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
	node := &corev1.Node{ObjectMeta: metav1.ObjectMeta{Name: "node", UID: "node-uid"}}
	binding, err := BindContainer(pod, node, "worker", "first-requirement", "first-trust")
	require.NoError(t, err)
	// Independently calculated by Guardd's Python protobuf digest implementation.
	require.Equal(t, "container:4d06df1b1125d7e99cd5f7984496df17e21888ce16b95e5c619f3b014db5ff52", binding.BindingID)
	pod.Labels = map[string]string{"orka.ai/task": "different-consuming-task"}
	otherConsumer, err := BindContainer(pod, node, "worker", "other-requirement", "other-trust")
	require.NoError(t, err)
	require.Equal(t, binding.BindingID, otherConsumer.BindingID, "policy or consumer changes cannot allocate another physical binding")
	pod.Status.ContainerStatuses[0].ContainerID = "containerd://" + strings.Repeat("b", 64)
	replacement, err := BindContainer(pod, node, "worker", "first-requirement", "first-trust")
	require.NoError(t, err)
	require.NotEqual(t, binding.BindingID, replacement.BindingID)
	node.UID = "replacement-node-uid"
	replacedNode, err := BindContainer(pod, node, "worker", "first-requirement", "first-trust")
	require.NoError(t, err)
	require.NotEqual(t, replacement.BindingID, replacedNode.BindingID)
	pod.Spec.NodeName = "different-node"
	_, err = BindContainer(pod, node, "worker", "first-requirement", "first-trust")
	require.Error(t, err)
}
