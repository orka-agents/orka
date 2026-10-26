package enforcement

import (
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

const PodAdmissionAnnotation = "orka.ai/enforcement-admission"

// PodAdmission is controller-written release authority, read through the
// authenticated Kubernetes API rather than an unauthenticated HTTP response.
// Provider credentials and large physical readbacks never enter the workload.
type PodAdmission struct {
	Version           string `json:"version"`
	BindingID         string `json:"bindingID"`
	RequirementDigest string `json:"requirementDigest"`
	PodUID            string `json:"podUID"`
	ContainerID       string `json:"containerID"`
}

const PodAdmissionVersion = "orka.enforcement.admission/v1"

// PublishedPodAdmission decodes a historical publication receipt. It does not
// establish a live container or authorize a worker to start.
func PublishedPodAdmission(pod *corev1.Pod) (*PodAdmission, error) {
	if pod == nil {
		return nil, errors.New("physical Pod required")
	}
	value := pod.Annotations[PodAdmissionAnnotation]
	if value == "" {
		return nil, nil
	}
	if len(value) > 4096 {
		return nil, errors.New("enforcement Pod admission exceeds its bound")
	}
	var admission PodAdmission
	if err := json.Unmarshal([]byte(value), &admission); err != nil {
		return nil, fmt.Errorf("invalid enforcement Pod admission: %w", err)
	}
	if admission.Version != PodAdmissionVersion || !validText(admission.BindingID, 128) ||
		admission.PodUID != string(pod.UID) || admission.RequirementDigest == "" || admission.ContainerID == "" {
		return nil, errors.New("enforcement publication does not match this Pod")
	}
	return &admission, nil
}

func PodApproved(pod *corev1.Pod, podUID, containerName, requirementDigest string) (bool, error) {
	if pod == nil || podUID == "" || requirementDigest == "" || containerName == "" {
		return false, errors.New("complete physical Pod admission identity required")
	}
	if string(pod.UID) != podUID {
		return false, errors.New("enforcement Pod incarnation changed")
	}
	if pod.DeletionTimestamp != nil || pod.Spec.RestartPolicy != corev1.RestartPolicyNever {
		return false, errors.New("enforcement requires a live non-restarting worker Pod")
	}
	admission, err := PublishedPodAdmission(pod)
	if err != nil || admission == nil {
		return false, err
	}
	if admission.RequirementDigest != requirementDigest {
		return false, errors.New("enforcement admission does not match this requirement and Pod")
	}
	for _, status := range pod.Status.ContainerStatuses {
		if status.Name == containerName {
			if status.ContainerID != admission.ContainerID || status.State.Running == nil {
				return false, errors.New("enforcement container incarnation changed or is not running")
			}
			return true, nil
		}
	}
	return false, errors.New("enforcement admission lacks the current worker container identity")
}
