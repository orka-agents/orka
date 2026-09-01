/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

const (
	repositoryMonitorValidationNetworkGateVolume     = "validation-network-gate"
	repositoryMonitorValidationNetworkGateContainer  = "await-validation-network-policy"
	repositoryMonitorValidationNetworkGateMount      = "/var/run/orka/validation-network"
	repositoryMonitorValidationNetworkGateKey        = "ready"
	repositoryMonitorValidationNetworkGatePending    = "false"
	repositoryMonitorValidationNetworkGateReady      = "true"
	repositoryMonitorValidationNetworkGateWorkerMode = "--wait-for-validation-network-policy"
)

var errRepositoryMonitorValidationConfinement = errors.New("repository validation confinement failed")

func isRepositoryMonitorValidationTask(task *corev1alpha1.Task) bool {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeContainer ||
		task.Labels[labels.LabelCreatedBy] != repositoryMonitorTaskCreatedBy ||
		task.Labels[labels.LabelPurpose] != repositoryMonitorValidationPurpose ||
		strings.TrimSpace(task.Annotations[labels.AnnotationRepositoryValidationImage]) == "" ||
		strings.TrimSpace(task.Spec.Image) != strings.TrimSpace(task.Annotations[labels.AnnotationRepositoryValidationImage]) {
		return false
	}
	monitorName := strings.TrimSpace(task.Annotations[labels.AnnotationRepositoryMonitorName])
	owner := metav1.GetControllerOf(task)
	return monitorName != "" && owner != nil &&
		owner.APIVersion == corev1alpha1.GroupVersion.String() &&
		owner.Kind == "RepositoryMonitor" && owner.Name == monitorName
}

func repositoryMonitorValidationResourceLabels(task *corev1alpha1.Task) map[string]string {
	return map[string]string{
		labels.LabelManaged: managedLabelValue,
		labels.LabelPurpose: repositoryMonitorValidationPurpose,
		labels.LabelTask:    labels.SelectorValue(task.Name),
	}
}

func repositoryMonitorValidationOwnerReference(task *corev1alpha1.Task) metav1.OwnerReference {
	return *metav1.NewControllerRef(task, corev1alpha1.GroupVersion.WithKind("Task"))
}

func repositoryMonitorValidationGateConfigMap(task *corev1alpha1.Task) *corev1.ConfigMap {
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:            task.Name,
			Namespace:       task.Namespace,
			Labels:          repositoryMonitorValidationResourceLabels(task),
			OwnerReferences: []metav1.OwnerReference{repositoryMonitorValidationOwnerReference(task)},
		},
		Data: map[string]string{repositoryMonitorValidationNetworkGateKey: repositoryMonitorValidationNetworkGatePending},
	}
}

func repositoryMonitorValidationNetworkPolicy(task *corev1alpha1.Task) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:            task.Name,
			Namespace:       task.Namespace,
			Labels:          repositoryMonitorValidationResourceLabels(task),
			OwnerReferences: []metav1.OwnerReference{repositoryMonitorValidationOwnerReference(task)},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{
				labels.LabelTask: labels.SelectorValue(task.Name),
			}},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
		},
	}
}

func repositoryMonitorValidationConfinementErrorf(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errRepositoryMonitorValidationConfinement, fmt.Sprintf(format, args...))
}

func (r *TaskReconciler) validationResourceReader() client.Reader {
	if r.APIReader != nil {
		return r.APIReader
	}
	return r.Client
}

func (r *TaskReconciler) ensureRepositoryMonitorValidationNetworkGate(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if !isRepositoryMonitorValidationTask(task) {
		return true, nil
	}
	current := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
	if err := r.validationResourceReader().Get(ctx, key, current); err != nil {
		if !apierrors.IsNotFound(err) {
			return false, err
		}
		if err := r.Create(ctx, repositoryMonitorValidationGateConfigMap(task)); err != nil && !apierrors.IsAlreadyExists(err) {
			return false, err
		}
		return false, nil
	}
	if err := validateRepositoryMonitorValidationGateConfigMap(task, current); err != nil {
		return false, err
	}
	if current.Data[repositoryMonitorValidationNetworkGateKey] != repositoryMonitorValidationNetworkGatePending {
		return false, repositoryMonitorValidationConfinementErrorf("network gate was released before validation Job creation")
	}
	return true, nil
}

func validateRepositoryMonitorValidationGateConfigMap(task *corev1alpha1.Task, gate *corev1.ConfigMap) error {
	if gate == nil || !metav1.IsControlledBy(gate, task) {
		return repositoryMonitorValidationConfinementErrorf("network gate is not controlled by validation Task %s/%s", task.Namespace, task.Name)
	}
	for key, value := range repositoryMonitorValidationResourceLabels(task) {
		if gate.Labels[key] != value {
			return repositoryMonitorValidationConfinementErrorf("network gate label %s does not match validation Task", key)
		}
	}
	state := gate.Data[repositoryMonitorValidationNetworkGateKey]
	if state != repositoryMonitorValidationNetworkGatePending && state != repositoryMonitorValidationNetworkGateReady {
		return repositoryMonitorValidationConfinementErrorf("network gate state is invalid")
	}
	return nil
}

func validateRepositoryMonitorValidationNetworkPolicy(task *corev1alpha1.Task, policy *networkingv1.NetworkPolicy) error {
	if policy == nil || !metav1.IsControlledBy(policy, task) {
		return repositoryMonitorValidationConfinementErrorf("NetworkPolicy is not controlled by validation Task %s/%s", task.Namespace, task.Name)
	}
	for key, value := range repositoryMonitorValidationResourceLabels(task) {
		if policy.Labels[key] != value {
			return repositoryMonitorValidationConfinementErrorf("NetworkPolicy label %s does not match validation Task", key)
		}
	}
	if !reflect.DeepEqual(policy.Spec, repositoryMonitorValidationNetworkPolicy(task).Spec) {
		return repositoryMonitorValidationConfinementErrorf("NetworkPolicy no longer denies all validation Pod ingress and egress")
	}
	return nil
}

func (r *TaskReconciler) reconcileRepositoryMonitorValidationConfinement(ctx context.Context, task *corev1alpha1.Task, job *batchv1.Job) error {
	if !isRepositoryMonitorValidationTask(task) {
		return nil
	}
	gate := &corev1.ConfigMap{}
	key := types.NamespacedName{Name: task.Name, Namespace: task.Namespace}
	if err := r.validationResourceReader().Get(ctx, key, gate); err != nil {
		if apierrors.IsNotFound(err) {
			return repositoryMonitorValidationConfinementErrorf("network gate is missing after validation Job creation")
		}
		return err
	}
	if err := validateRepositoryMonitorValidationGateConfigMap(task, gate); err != nil {
		return err
	}

	pod, err := r.repositoryMonitorValidationPod(ctx, task, job)
	if err != nil {
		return err
	}
	gateState := gate.Data[repositoryMonitorValidationNetworkGateKey]
	if pod == nil {
		if gateState == repositoryMonitorValidationNetworkGateReady {
			return repositoryMonitorValidationConfinementErrorf("validation Pod disappeared after network gate release")
		}
		return nil
	}
	prepared, err := repositoryMonitorValidationWorkspacePrepared(pod)
	if err != nil {
		return err
	}
	if !prepared {
		if gateState == repositoryMonitorValidationNetworkGateReady {
			return repositoryMonitorValidationConfinementErrorf("network gate was released before workspace preparation completed")
		}
		return nil
	}

	policy := &networkingv1.NetworkPolicy{}
	policyErr := r.validationResourceReader().Get(ctx, key, policy)
	if gateState == repositoryMonitorValidationNetworkGateReady {
		if apierrors.IsNotFound(policyErr) {
			return repositoryMonitorValidationConfinementErrorf("NetworkPolicy disappeared after validation command release")
		}
		if policyErr != nil {
			return policyErr
		}
		return validateRepositoryMonitorValidationNetworkPolicy(task, policy)
	}
	if policyErr != nil {
		if !apierrors.IsNotFound(policyErr) {
			return policyErr
		}
		if err := r.Create(ctx, repositoryMonitorValidationNetworkPolicy(task)); err != nil && !apierrors.IsAlreadyExists(err) {
			return err
		}
		// A later reconcile marks the policy ready. The Pod gate then proves the
		// repository endpoint is blocked from the Pod network namespace before
		// the validation container can start.
		return nil
	}
	if err := validateRepositoryMonitorValidationNetworkPolicy(task, policy); err != nil {
		return err
	}
	patch := client.MergeFrom(gate.DeepCopy())
	gate.Data[repositoryMonitorValidationNetworkGateKey] = repositoryMonitorValidationNetworkGateReady
	return r.Patch(ctx, gate, patch)
}

func (r *TaskReconciler) repositoryMonitorValidationPod(ctx context.Context, task *corev1alpha1.Task, job *batchv1.Job) (*corev1.Pod, error) {
	if job == nil {
		return nil, repositoryMonitorValidationConfinementErrorf("validation Job is missing")
	}
	var pods corev1.PodList
	if err := r.validationResourceReader().List(ctx, &pods,
		client.InNamespace(task.Namespace),
		client.MatchingLabels{labels.LabelTask: labels.SelectorValue(task.Name)},
	); err != nil {
		return nil, err
	}
	var matched *corev1.Pod
	for i := range pods.Items {
		pod := &pods.Items[i]
		if !podBelongsToJob(pod, job.Name) {
			continue
		}
		if matched != nil {
			return nil, repositoryMonitorValidationConfinementErrorf("multiple Pods belong to validation Job %s/%s", job.Namespace, job.Name)
		}
		matched = pod
	}
	return matched, nil
}

func repositoryMonitorValidationWorkspacePrepared(pod *corev1.Pod) (bool, error) {
	if pod == nil {
		return false, nil
	}
	found := false
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == workspacePreparationInitContainerName {
			found = true
			break
		}
	}
	if !found {
		return false, repositoryMonitorValidationConfinementErrorf("validation Pod is missing the workspace preparation init container")
	}
	for i := range pod.Status.InitContainerStatuses {
		status := &pod.Status.InitContainerStatuses[i]
		if status.Name != workspacePreparationInitContainerName || status.State.Terminated == nil {
			continue
		}
		return status.State.Terminated.ExitCode == 0, nil
	}
	return false, nil
}
