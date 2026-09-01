/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

func TestRepositoryMonitorValidationJobIsReadOnlyAndNetworkGated(t *testing.T) {
	task := repositoryMonitorValidationRuntimeTask()
	job, err := setupJobBuilder().Build(context.Background(), task, nil, nil)
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	workspace, ok := findVolumeMount(job.Spec.Template.Spec.Containers[0].VolumeMounts, "workspace")
	if !ok || !workspace.ReadOnly {
		t.Fatalf("validation workspace mount = %#v, want read-only", workspace)
	}
	if len(job.Spec.Template.Spec.InitContainers) != 2 {
		t.Fatalf("init containers = %#v, want workspace preparation followed by network gate", job.Spec.Template.Spec.InitContainers)
	}
	if job.Spec.Template.Spec.InitContainers[0].Name != workspacePreparationInitContainerName || job.Spec.Template.Spec.InitContainers[1].Name != repositoryMonitorValidationNetworkGateContainer {
		t.Fatalf("init container order = %q, %q", job.Spec.Template.Spec.InitContainers[0].Name, job.Spec.Template.Spec.InitContainers[1].Name)
	}
	gate := job.Spec.Template.Spec.InitContainers[1]
	if mount, ok := findVolumeMount(gate.VolumeMounts, repositoryMonitorValidationNetworkGateVolume); !ok || !mount.ReadOnly {
		t.Fatalf("network gate mount = %#v, want read-only ConfigMap mount", mount)
	}
	if len(gate.Args) != 1 || !strings.Contains(gate.Args[0], repositoryMonitorValidationNetworkGateKey) {
		t.Fatalf("network gate args = %#v", gate.Args)
	}
	foundGateVolume := false
	for i := range job.Spec.Template.Spec.Volumes {
		volume := &job.Spec.Template.Spec.Volumes[i]
		if volume.Name != repositoryMonitorValidationNetworkGateVolume {
			continue
		}
		foundGateVolume = volume.ConfigMap != nil && volume.ConfigMap.Name == task.Name
	}
	if !foundGateVolume {
		t.Fatal("validation Job is missing its network gate ConfigMap volume")
	}
	if job.Spec.Template.Spec.AutomountServiceAccountToken == nil || *job.Spec.Template.Spec.AutomountServiceAccountToken {
		t.Fatal("validation Pod must not automount a service account token")
	}
}

func TestRepositoryMonitorValidationConfinementLifecycle(t *testing.T) {
	ctx := context.Background()
	task := repositoryMonitorValidationRuntimeTask()
	scheme := runtime.NewScheme()
	for name, add := range map[string]func(*runtime.Scheme) error{
		"batch":      batchv1.AddToScheme,
		"core":       corev1.AddToScheme,
		"networking": networkingv1.AddToScheme,
		"orka":       corev1alpha1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("add %s scheme: %v", name, err)
		}
	}
	k8sClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(task).Build()
	reconciler := &TaskReconciler{Client: k8sClient, Scheme: scheme}

	ready, err := reconciler.ensureRepositoryMonitorValidationNetworkGate(ctx, task)
	if err != nil || ready {
		t.Fatalf("first gate reconcile = (%v, %v), want created and pending", ready, err)
	}
	ready, err = reconciler.ensureRepositoryMonitorValidationNetworkGate(ctx, task)
	if err != nil || !ready {
		t.Fatalf("second gate reconcile = (%v, %v), want ready for Job creation", ready, err)
	}

	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "validation-job", Namespace: task.Namespace, UID: types.UID("validation-job-uid")}}
	controller := true
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "validation-job-pod",
			Namespace: task.Namespace,
			Labels: map[string]string{
				labels.LabelTask:     labels.SelectorValue(task.Name),
				batchv1.JobNameLabel: job.Name,
			},
			OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}},
		},
		Spec: corev1.PodSpec{InitContainers: []corev1.Container{{Name: workspacePreparationInitContainerName}, {Name: repositoryMonitorValidationNetworkGateContainer}}},
		Status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{
			Name:  workspacePreparationInitContainerName,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
		}}},
	}
	if err := k8sClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}

	if err := reconciler.reconcileRepositoryMonitorValidationConfinement(ctx, task, job); err != nil {
		t.Fatalf("create NetworkPolicy: %v", err)
	}
	policy := &networkingv1.NetworkPolicy{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, policy); err != nil {
		t.Fatal(err)
	}
	if len(policy.Spec.Ingress) != 0 || len(policy.Spec.Egress) != 0 || len(policy.Spec.PolicyTypes) != 2 {
		t.Fatalf("NetworkPolicy spec = %#v, want deny-all ingress and egress", policy.Spec)
	}
	gate := &corev1.ConfigMap{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, gate); err != nil {
		t.Fatal(err)
	}
	if gate.Data[repositoryMonitorValidationNetworkGateKey] != repositoryMonitorValidationNetworkGatePending {
		t.Fatalf("gate released in same reconcile as NetworkPolicy creation: %#v", gate.Data)
	}

	if err := reconciler.reconcileRepositoryMonitorValidationConfinement(ctx, task, job); err != nil {
		t.Fatalf("release network gate: %v", err)
	}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: task.Namespace}, gate); err != nil {
		t.Fatal(err)
	}
	if gate.Data[repositoryMonitorValidationNetworkGateKey] != repositoryMonitorValidationNetworkGateReady {
		t.Fatalf("gate state = %#v, want released", gate.Data)
	}

	if err := k8sClient.Delete(ctx, policy); err != nil {
		t.Fatal(err)
	}
	err = reconciler.reconcileRepositoryMonitorValidationConfinement(ctx, task, job)
	if !errors.Is(err, errRepositoryMonitorValidationConfinement) || !strings.Contains(err.Error(), "disappeared") {
		t.Fatalf("missing released NetworkPolicy error = %v", err)
	}
}

func repositoryMonitorValidationRuntimeTask() *corev1alpha1.Task {
	controller := true
	const monitorName = "repository-monitor"
	return &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "repository-review-validation",
			Namespace: "default",
			UID:       types.UID("validation-task-uid"),
			Labels: map[string]string{
				labels.LabelCreatedBy: repositoryMonitorTaskCreatedBy,
				labels.LabelPurpose:   repositoryMonitorValidationPurpose,
			},
			Annotations: map[string]string{
				labels.AnnotationRepositoryMonitorName:     monitorName,
				labels.AnnotationRepositoryValidationImage: repositoryMonitorValidationTestImage,
				labels.AnnotationWorkspaceInitContainer:    scheduledRunLabelValue,
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: "RepositoryMonitor",
				Name: monitorName, UID: types.UID("monitor-uid"), Controller: &controller,
			}},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:    corev1alpha1.TaskTypeContainer,
			Image:   repositoryMonitorValidationTestImage,
			Command: []string{"/bin/sh", "-c"},
			Args:    []string{"go test ./..."},
			Workspace: &corev1alpha1.WorkspaceConfig{
				Intent:  corev1alpha1.WorkspaceIntentRead,
				GitRepo: "https://github.com/example/repository.git",
				Ref:     strings.Repeat("a", 40),
			},
		},
	}
}
