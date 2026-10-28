package enforcement

import (
	"encoding/json"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func approvedPod(t *testing.T) *corev1.Pod {
	t.Helper()
	approval := PodAdmission{
		Version: PodAdmissionVersion, BindingID: "container-1",
		RequirementDigest: "requirement-1", PodUID: "pod-1", ContainerID: "containerd://incarnation-1",
	}
	value, err := json.Marshal(approval)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker", Namespace: "tasks", UID: "pod-1",
			Annotations: map[string]string{PodAdmissionAnnotation: string(value)},
		},
		Spec: corev1.PodSpec{RestartPolicy: corev1.RestartPolicyNever},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			Name: "worker", ContainerID: "containerd://incarnation-1",
			State: corev1.ContainerState{Running: &corev1.ContainerStateRunning{}},
		}}},
	}
}

func TestPodAdmissionMatchesPhysicalContainerAndRequirement(t *testing.T) {
	pod := approvedPod(t)
	if approved, err := PodApproved(pod, "pod-1", "worker", "requirement-1"); !approved || err != nil {
		t.Fatalf("current admission refused: %v", err)
	}
	pod.Annotations = nil
	if approved, err := PodApproved(pod, "pod-1", "worker", "requirement-1"); approved || err != nil {
		t.Fatalf("missing approval is not a pending hold: %v", err)
	}
}

func TestStaleOrUnsafePodAdmissionNeverReleases(t *testing.T) {
	mutations := map[string]func(*corev1.Pod){
		"Pod replaced": func(p *corev1.Pod) { p.UID = "replacement" },
		"container replaced": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].ContainerID = "containerd://replacement"
		},
		"deleting":        func(p *corev1.Pod) { now := metav1.Now(); p.DeletionTimestamp = &now },
		"restart enabled": func(p *corev1.Pod) { p.Spec.RestartPolicy = corev1.RestartPolicyAlways },
		"no running container": func(p *corev1.Pod) {
			p.Status.ContainerStatuses[0].State = corev1.ContainerState{}
		},
		"wrong worker":    func(p *corev1.Pod) { p.Status.ContainerStatuses[0].Name = "other" },
		"malformed proof": func(p *corev1.Pod) { p.Annotations[PodAdmissionAnnotation] = "{}" },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			pod := approvedPod(t)
			mutate(pod)
			if approved, err := PodApproved(pod, "pod-1", "worker", "requirement-1"); approved || err == nil {
				t.Fatal("stale/unsafe physical admission released")
			}
		})
	}
	if approved, err := PodApproved(approvedPod(t), "pod-1", "worker", "different-policy"); approved || err == nil {
		t.Fatal("different policy reused current admission")
	}
}
