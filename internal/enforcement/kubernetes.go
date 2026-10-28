package enforcement

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
)

const KubernetesContainerFormat = "orka.kubernetes-container/v1"

func KubernetesFacts(binding *corev1alpha1.ContainerEnforcementBinding) (*enforcementv1.TargetFacts, error) {
	if binding == nil {
		return nil, errors.New("physical container binding required")
	}
	values := map[string]string{
		"nodeName": binding.NodeName, "nodeUID": binding.NodeUID,
		"namespace": binding.PodNamespace, "podName": binding.PodName, "podUID": binding.PodUID,
		"containerName": binding.ContainerName, "containerID": binding.ContainerID,
	}
	for name, value := range values {
		if !validText(value, 512) {
			return nil, fmt.Errorf("physical container identity requires %s", name)
		}
	}
	value, err := json.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("encode physical container identity: %w", err)
	}
	return &enforcementv1.TargetFacts{Format: KubernetesContainerFormat, Value: value}, nil
}

func BindContainer(pod *corev1.Pod, node *corev1.Node, containerName, requirementDigest, providerSetDigest string) (*corev1alpha1.ContainerEnforcementBinding, error) {
	if pod == nil || node == nil || pod.Spec.NodeName != node.Name ||
		pod.DeletionTimestamp != nil || node.DeletionTimestamp != nil {
		return nil, errors.New("current prepared Pod and exact Node required")
	}
	binding := &corev1alpha1.ContainerEnforcementBinding{
		RequirementDigest: requirementDigest, ProviderSetDigest: providerSetDigest,
		NodeName: node.Name, NodeUID: string(node.UID),
		PodNamespace: pod.Namespace, PodName: pod.Name, PodUID: string(pod.UID),
		ContainerName: containerName,
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName && status.State.Running != nil {
			binding.ContainerID = status.ContainerID
		}
	}
	facts, err := KubernetesFacts(binding)
	if err != nil {
		return nil, err
	}
	digest, err := TargetFactsDigest(facts)
	if err != nil {
		return nil, err
	}
	// Deliberately excludes Task/session/requirement identity: one physical
	// incarnation cannot acquire a second binding merely by changing policy.
	binding.BindingID = "container:" + strings.TrimPrefix(digest, "sha256:")
	return binding, nil
}
