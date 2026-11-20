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
	"slices"
	"strings"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

// +kubebuilder:rbac:groups=admissionregistration.k8s.io,resources=validatingadmissionpolicies;validatingadmissionpolicybindings,verbs=get

// DefaultContainerTaskSecretUsePolicyName names the ValidatingAdmissionPolicy,
// and its binding, that requires the `use` verb on every Secret a container
// Task names (config/container-task-secret-use-policy).
const DefaultContainerTaskSecretUsePolicyName = "container-task-secret-use.orka.ai"

// ErrContainerTaskSecretUseUnavailable reports that the controller could not
// confirm the Secret use policy is in force because of a transient read error.
// The dispatch is retried rather than failed.
var ErrContainerTaskSecretUseUnavailable = errors.New("container Task Secret use policy could not be confirmed")

// ContainerTaskSecretUsePolicy lets operator-authorized container Tasks
// receive the Secrets they name without the cluster-wide direct Secret mount
// switch. Admission requires the identity that writes such a Task to hold
// `use` on each Secret; Orka never grants that verb to the controller or to a
// worker, so Tasks created by agents, tools, chat, or the API cannot qualify.
// The controller delivers only after it has confirmed the policy is bound.
type ContainerTaskSecretUsePolicy struct {
	// Enabled turns the mode on.
	Enabled bool
	// PolicyName names the ValidatingAdmissionPolicy and its binding.
	PolicyName string
	// Reader reads the policy, its binding, and scheduled parents uncached.
	Reader client.Reader
}

func (p ContainerTaskSecretUsePolicy) policyName() string {
	if name := strings.TrimSpace(p.PolicyName); name != "" {
		return name
	}
	return DefaultContainerTaskSecretUsePolicyName
}

// containerTaskSecretNames returns the distinct Secret names a Task asks to
// receive through spec.secretRef and spec.env[].valueFrom.secretKeyRef.
func containerTaskSecretNames(task *corev1alpha1.Task) []string {
	if task == nil {
		return nil
	}
	var names []string
	add := func(name string) {
		name = strings.TrimSpace(name)
		if name != "" && !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if task.Spec.SecretRef != nil {
		add(task.Spec.SecretRef.Name)
	}
	for _, envVar := range task.Spec.Env {
		if envVar.ValueFrom != nil && envVar.ValueFrom.SecretKeyRef != nil {
			add(envVar.ValueFrom.SecretKeyRef.Name)
		}
	}
	return names
}

// scheduledRunSpec returns the spec the controller gives each run of a
// scheduled container Task: a copy with the scheduling fields cleared.
func scheduledRunSpec(scheduled *corev1alpha1.Task) corev1alpha1.TaskSpec {
	spec := *scheduled.Spec.DeepCopy()
	spec.Schedule = ""
	spec.TimeZone = nil
	spec.ConcurrencyPolicy = ""
	spec.StartingDeadlineSeconds = nil
	spec.SuccessfulRunsHistoryLimit = nil
	spec.FailedRunsHistoryLimit = nil
	spec.Suspend = nil
	return spec
}

// containerTaskSecretUseGranted reports whether a container Task may receive
// the Secrets it names under the Secret use policy. It returns false with no
// error when the mode does not apply, so the direct Secret mount switch keeps
// its behavior. A Task that names Secrets but does not qualify returns an
// error that fails the Task, naming the field and Secret, never a value.
func (b *JobBuilder) containerTaskSecretUseGranted(ctx context.Context, task *corev1alpha1.Task) (bool, error) {
	if b == nil || !b.ContainerTaskSecretUse.Enabled || b.directSecrets.secretMounts || !isUntrustedComputeTask(task) {
		return false, nil
	}
	names := containerTaskSecretNames(task)
	if len(names) == 0 || taskRequestsReadOnlyAgent(task) || taskRequestsRuntimeAuthOnly(task) {
		return false, nil
	}
	if ref := task.Spec.SecretRef; ref != nil {
		if namespace := strings.TrimSpace(ref.Namespace); namespace != "" && namespace != task.Namespace {
			return false, fmt.Errorf("container Task spec.secretRef names Secret %q in namespace %q; a container Task receives only Secrets in its own namespace", ref.Name, namespace)
		}
	}
	if reason := containerTaskSecretUseIneligible(task); reason != "" {
		return false, fmt.Errorf("container Task names Secret %q but %s, so it cannot receive Secrets", names[0], reason)
	}
	reader := b.ContainerTaskSecretUse.Reader
	if reader == nil {
		return false, fmt.Errorf("%w: no reader configured", ErrContainerTaskSecretUseUnavailable)
	}
	parent, err := b.checkScheduledRunSecretReferences(ctx, reader, task)
	if err != nil {
		return false, err
	}
	enforcedSince, err := b.checkContainerTaskSecretUseBinding(ctx, reader)
	if err != nil {
		return false, err
	}
	// Admission checks only writes made after the policy is in force, so a
	// Task, or for a run its scheduled Task, written earlier was never checked.
	for _, admitted := range []*corev1alpha1.Task{task, parent} {
		if admitted != nil && !admitted.CreationTimestamp.After(enforcedSince) {
			return false, fmt.Errorf("container Task %q was created before the container Task Secret use admission policy "+
				"was in force, so it was never checked and cannot receive Secrets; recreate it", admitted.Name)
		}
	}
	log.FromContext(ctx).Info("delivering Secrets to container Task under the Secret use policy",
		"task", task.Name, "namespace", task.Namespace, "taskUID", task.UID, "secrets", names)
	return true, nil
}

// containerTaskSecretUseIneligible returns why a Task's provenance rules it
// out, or "". Admission already refuses these writers; this keeps the
// guarantee for Tasks created by agents, tools, chat, or the API from resting
// on admission alone. Scheduled runs are checked against their parent.
func containerTaskSecretUseIneligible(task *corev1alpha1.Task) string {
	switch {
	case task.Labels[labels.LabelChatSession] != "":
		return "it was created from a chat session"
	case task.Labels[labels.LabelCoordinator] != "":
		return "it was created by a coordinator"
	case task.Annotations[labels.AnnotationRequestedBySource] != "":
		return "it was created through the Orka API"
	}
	parent := labels.ParentTaskName(task.Labels, task.Annotations)
	owner := ownerTaskReference(task)
	if parent == "" && owner == nil && task.Labels[labels.LabelScheduledRun] == "" {
		return ""
	}
	if task.Labels[labels.LabelScheduledRun] == scheduledRunLabelValue && owner != nil && owner.Controller != nil && *owner.Controller {
		return ""
	}
	return "it was created by another Task"
}

func ownerTaskReference(task *corev1alpha1.Task) *metav1.OwnerReference {
	for i := range task.OwnerReferences {
		ref := &task.OwnerReferences[i]
		if ref.Kind == taskResourceKind && strings.HasPrefix(ref.APIVersion, corev1alpha1.GroupVersion.Group+"/") {
			return ref
		}
	}
	return nil
}

// checkScheduledRunSecretReferences requires a scheduled run to name exactly
// the Secrets its scheduled Task names, and that Task to be a root Task. It
// returns the scheduled Task, or nil for a Task that is not a run.
func (b *JobBuilder) checkScheduledRunSecretReferences(
	ctx context.Context, reader client.Reader, task *corev1alpha1.Task,
) (*corev1alpha1.Task, error) {
	owner := ownerTaskReference(task)
	if owner == nil {
		return nil, nil
	}
	parent := &corev1alpha1.Task{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: task.Namespace, Name: owner.Name}, parent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("scheduled run's Task %q no longer exists, so the run cannot receive Secrets", owner.Name)
		}
		return nil, fmt.Errorf("%w: reading scheduled Task %q: %w", ErrContainerTaskSecretUseUnavailable, owner.Name, err)
	}
	if task.Annotations[labels.AnnotationParentTaskName] != owner.Name {
		return nil, fmt.Errorf("scheduled run's owner %q does not match the scheduled Task the controller recorded, so the run cannot receive Secrets", owner.Name)
	}
	if uid := task.Annotations[labels.AnnotationParentTaskUID]; uid != "" && uid != string(owner.UID) {
		return nil, fmt.Errorf("scheduled run's owner %q does not match the scheduled Task the controller recorded, so the run cannot receive Secrets", owner.Name)
	}
	if parent.UID != owner.UID || strings.TrimSpace(parent.Spec.Schedule) == "" || !isUntrustedComputeTask(parent) {
		return nil, fmt.Errorf("scheduled run's owner %q is not its scheduled Task, so the run cannot receive Secrets", owner.Name)
	}
	if reason := containerTaskSecretUseIneligible(parent); reason != "" || ownerTaskReference(parent) != nil {
		return nil, fmt.Errorf("scheduled Task %q cannot pass Secrets to its runs because it was created by another Task or an agent", parent.Name)
	}
	// The run must be the copy the controller made: same image, command,
	// Secrets and everything else that decides what receives the Secrets.
	if !reflect.DeepEqual(scheduledRunSpec(parent), task.Spec) {
		return nil, fmt.Errorf("scheduled run's spec differs from its scheduled Task %q, so it cannot receive Secrets", parent.Name)
	}
	return parent, nil
}

// containerTaskSecretUsePropagation is how long after the policy or its
// binding last changed the controller still treats a write as possibly
// unchecked, while API servers load the change.
const containerTaskSecretUsePropagation = 30 * time.Second

// checkContainerTaskSecretUseBinding confirms the admission policy and a Deny
// binding for it exist, so the mode never delivers while admission is not
// checking the writers. It returns the time from which admission is known to
// have been enforcing: the latest change to either object plus a propagation
// window.
func (b *JobBuilder) checkContainerTaskSecretUseBinding(ctx context.Context, reader client.Reader) (time.Time, error) {
	name := b.ContainerTaskSecretUse.policyName()
	policy := &admissionregistrationv1.ValidatingAdmissionPolicy{}
	if err := reader.Get(ctx, types.NamespacedName{Name: name}, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return time.Time{}, fmt.Errorf("the container Task Secret use admission policy %q is not installed, so container Tasks cannot receive Secrets", name)
		}
		return time.Time{}, fmt.Errorf("%w: reading ValidatingAdmissionPolicy %q: %w", ErrContainerTaskSecretUseUnavailable, name, err)
	}
	binding := &admissionregistrationv1.ValidatingAdmissionPolicyBinding{}
	if err := reader.Get(ctx, types.NamespacedName{Name: name}, binding); err != nil {
		if apierrors.IsNotFound(err) {
			return time.Time{}, fmt.Errorf("the container Task Secret use admission policy %q is not bound, so container Tasks cannot receive Secrets", name)
		}
		return time.Time{}, fmt.Errorf("%w: reading ValidatingAdmissionPolicyBinding %q: %w", ErrContainerTaskSecretUseUnavailable, name, err)
	}
	if binding.Spec.PolicyName != name || !slices.Contains(binding.Spec.ValidationActions, admissionregistrationv1.Deny) {
		return time.Time{}, fmt.Errorf("the container Task Secret use admission policy %q is not bound with Deny, so container Tasks cannot receive Secrets", name)
	}
	return latestChange(&policy.ObjectMeta, &binding.ObjectMeta).Add(containerTaskSecretUsePropagation), nil
}

// latestChange returns the latest time the objects' specs may have changed:
// their creation, or, once a spec change has raised an object's generation,
// its latest managed-field update. Metadata-only writes, such as a chart
// upgrade relabelling the policy, leave the generation alone and do not move
// the cutoff.
func latestChange(objects ...*metav1.ObjectMeta) time.Time {
	var latest time.Time
	for _, object := range objects {
		if object.CreationTimestamp.After(latest) {
			latest = object.CreationTimestamp.Time
		}
		if object.Generation <= 1 {
			continue
		}
		for _, entry := range object.ManagedFields {
			if entry.Time != nil && entry.Time.After(latest) {
				latest = entry.Time.Time
			}
		}
	}
	return latest
}
