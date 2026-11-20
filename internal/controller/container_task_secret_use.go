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
	"sync"
	"time"

	admissionregistrationv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
)

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

// scheduledRunSpec returns a Task's spec without the scheduling fields. The
// controller copies a scheduled Task's spec into each run with these fields
// cleared, and the API server then applies their CRD defaults to the run, so
// they are cleared on both sides before a run is compared with its parent.
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
	enforcedSince, err := b.checkContainerTaskSecretUseBinding(ctx, reader, task.Namespace)
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
	if !reflect.DeepEqual(scheduledRunSpec(parent), scheduledRunSpec(task)) {
		return nil, fmt.Errorf("scheduled run's spec differs from its scheduled Task %q, so it cannot receive Secrets", parent.Name)
	}
	return parent, nil
}

// containerTaskSecretUsePropagation is how long after the policy or its
// binding last changed the controller still treats a write as possibly
// unchecked, while API servers load the change.
const containerTaskSecretUsePropagation = 30 * time.Second

// checkContainerTaskSecretUseBinding confirms the admission policy and a Deny
// binding for it exist and cover every Task write in namespace, so the mode
// never delivers while admission is not checking the writers. It returns the
// time from which admission is known to have been enforcing: the latest
// change to either object plus a propagation window.
func (b *JobBuilder) checkContainerTaskSecretUseBinding(
	ctx context.Context, reader client.Reader, namespace string,
) (time.Time, error) {
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
	if reason := containerTaskSecretUseCoverageGap(policy, binding, namespace); reason != "" {
		return time.Time{}, fmt.Errorf("the container Task Secret use admission policy %q %s, so container Tasks cannot receive Secrets", name, reason)
	}
	return latestChange(&policy.ObjectMeta, &binding.ObjectMeta).Add(containerTaskSecretUsePropagation), nil
}

// containerTaskSecretUseCoverageGap returns why the policy and binding might
// not check every Task write in namespace, or "". Anything it cannot prove
// covers the namespace, such as an object selector, an exclusion, or a
// namespace selector other than the namespace's own name, counts as a gap.
func containerTaskSecretUseCoverageGap(
	policy *admissionregistrationv1.ValidatingAdmissionPolicy,
	binding *admissionregistrationv1.ValidatingAdmissionPolicyBinding,
	namespace string,
) string {
	if policy.Spec.FailurePolicy != nil && *policy.Spec.FailurePolicy != admissionregistrationv1.Fail {
		return "does not fail closed"
	}
	checksUse := false
	for _, validation := range policy.Spec.Validations {
		if strings.Contains(validation.Expression, "resource('secrets')") && strings.Contains(validation.Expression, "check('use')") {
			checksUse = true
		}
	}
	if !checksUse {
		return "does not check use on Secrets"
	}
	constraints := policy.Spec.MatchConstraints
	if constraints == nil || !selectorEmpty(constraints.NamespaceSelector) || !selectorEmpty(constraints.ObjectSelector) ||
		len(constraints.ExcludeResourceRules) > 0 || !rulesCoverTaskWrites(constraints.ResourceRules) {
		return "does not match every Task create and update"
	}
	if binding.Spec.ParamRef != nil {
		return "is bound with parameters"
	}
	if resources := binding.Spec.MatchResources; resources != nil {
		if !selectorEmpty(resources.ObjectSelector) || len(resources.ExcludeResourceRules) > 0 || len(resources.ResourceRules) > 0 {
			return "is bound to only some Tasks"
		}
		if !selectorEmpty(resources.NamespaceSelector) && !selectorIsNamespace(resources.NamespaceSelector, namespace) {
			return fmt.Sprintf("is not bound to namespace %q", namespace)
		}
	}
	return ""
}

func selectorEmpty(selector *metav1.LabelSelector) bool {
	return selector == nil || (len(selector.MatchLabels) == 0 && len(selector.MatchExpressions) == 0)
}

// selectorIsNamespace reports whether selector matches exactly the namespace
// by its immutable name label.
func selectorIsNamespace(selector *metav1.LabelSelector, namespace string) bool {
	return len(selector.MatchExpressions) == 0 && len(selector.MatchLabels) == 1 &&
		selector.MatchLabels[corev1.LabelMetadataName] == namespace
}

func rulesCoverTaskWrites(rules []admissionregistrationv1.NamedRuleWithOperations) bool {
	covers := func(values []string, want string) bool {
		return slices.Contains(values, want) || slices.Contains(values, "*")
	}
	for _, rule := range rules {
		if len(rule.ResourceNames) > 0 || !covers(rule.APIGroups, corev1alpha1.GroupVersion.Group) ||
			!covers(rule.APIVersions, corev1alpha1.GroupVersion.Version) || !covers(rule.Resources, "tasks") {
			continue
		}
		operations := make([]string, 0, len(rule.Operations))
		for _, operation := range rule.Operations {
			operations = append(operations, string(operation))
		}
		if covers(operations, string(admissionregistrationv1.Create)) && covers(operations, string(admissionregistrationv1.Update)) {
			return true
		}
	}
	return false
}

// specChanges remembers, per object UID, the generation last seen and the
// time from which that generation is known to have been in place, so
// metadata-only writes after a spec change do not move the cutoff again.
var specChanges = struct {
	sync.Mutex
	seen map[types.UID]specChange
}{seen: map[types.UID]specChange{}}

type specChange struct {
	generation int64
	since      time.Time
}

// latestChange returns the latest time the objects' specs may have changed.
// An object still at its first generation changed when it was created. For a
// later generation, the change time is not recorded, so the first time this
// process sees that generation it takes the object's latest managed-field
// update, which is no earlier than the spec change, and keeps it while the
// generation stays the same. Metadata-only writes, such as a chart upgrade
// relabelling the policy, leave the generation alone and do not move the
// cutoff.
func latestChange(objects ...*metav1.ObjectMeta) time.Time {
	var latest time.Time
	for _, object := range objects {
		since := object.CreationTimestamp.Time
		if object.Generation > 1 {
			since = generationSince(object)
		}
		if since.After(latest) {
			latest = since
		}
	}
	return latest
}

func generationSince(object *metav1.ObjectMeta) time.Time {
	specChanges.Lock()
	defer specChanges.Unlock()
	if seen, ok := specChanges.seen[object.UID]; ok && object.UID != "" && seen.generation == object.Generation {
		return seen.since
	}
	since := object.CreationTimestamp.Time
	for _, entry := range object.ManagedFields {
		if entry.Time != nil && entry.Time.After(since) {
			since = entry.Time.Time
		}
	}
	if object.UID != "" {
		specChanges.seen[object.UID] = specChange{generation: object.Generation, since: since}
	}
	return since
}
