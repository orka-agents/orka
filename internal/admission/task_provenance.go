/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/gateway"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	TaskProvenanceWebhookPath = "/validate-core-orka-ai-v1alpha1-task-provenance"

	fieldSpecRequestedBy     = "spec.requestedBy"
	fieldSpecTransaction     = "spec.transaction"
	fieldMetadataLabels      = "metadata.labels"
	fieldMetadataAnnotations = "metadata.annotations"

	managedWorkspaceMetadataPrefix = "acp.workspace.orka.ai/"
)

var (
	defaultTrustedWorkerServiceAccounts = []string{
		"orka-ai-worker",
		"orka-vendor-worker",
	}

	managedTransactionLabelKeys = []string{
		labels.LabelTransactionID,
		labels.LabelAuthProfile,
	}

	managedTransactionAnnotationKeys = []string{
		labels.AnnotationTransactionID,
		labels.AnnotationContextTokenProfile,
		labels.AnnotationTransactionIssuer,
		labels.AnnotationTransactionSubject,
		labels.AnnotationTransactionRequestingWorkload,
		labels.AnnotationTransactionScope,
		labels.AnnotationTransactionContextDigest,
		labels.AnnotationRequesterContextDigest,
		labels.AnnotationTransactionTokenSecret,
		labels.AnnotationTransactionTokenPending,
		labels.AnnotationTransactionTokenPendingSince,
		labels.AnnotationTraceParent,
		labels.AnnotationTraceState,
		labels.AnnotationTraceBaggage,
	}

	controllerManagedTaskLabelKeys = []string{
		labels.LabelAHPTurn,
	}

	controllerManagedTaskAnnotationKeys = []string{
		labels.AnnotationCancelRequested,
		labels.AnnotationParentTaskUID,
		labels.AnnotationParentTaskNamespace,
		labels.AnnotationRequestedBySource,
		labels.AnnotationRequestedByStamp,
		labels.AnnotationContinuationOfName,
		labels.AnnotationContinuationOfUID,
		labels.AnnotationContinuationIndex,
		labels.AnnotationContinuationChainDeadline,
	}
)

type TaskProvenanceConfig struct {
	Enabled                    bool
	ControllerUsernames        []string
	TrustedUsernames           []string
	TrustedServiceAccountNames []string
	WorkerServiceAccountNames  []string
}

func NewTaskProvenanceConfig(
	enabled bool,
	controllerUsernames, trustedUsernames, trustedServiceAccountNames, controllerNamespace string,
) TaskProvenanceConfig {
	cfg := TaskProvenanceConfig{Enabled: enabled}
	cfg.ControllerUsernames = workerenv.SplitCSV(controllerUsernames)
	if len(cfg.ControllerUsernames) == 0 {
		cfg.ControllerUsernames = defaultControllerServiceAccountUsernames(controllerNamespace)
	}
	cfg.TrustedUsernames = workerenv.SplitCSV(trustedUsernames)
	if len(cfg.TrustedUsernames) == 0 {
		cfg.TrustedUsernames = defaultControllerServiceAccountUsernames(controllerNamespace)
	}
	cfg.TrustedServiceAccountNames = workerenv.SplitCSV(trustedServiceAccountNames)
	if len(cfg.TrustedServiceAccountNames) == 0 {
		cfg.TrustedServiceAccountNames = append([]string{}, defaultTrustedWorkerServiceAccounts...)
	}
	cfg.WorkerServiceAccountNames = WorkerServiceAccountNames(cfg.TrustedServiceAccountNames)
	return cfg
}

func RegisterTaskProvenanceWebhook(server webhook.Server, scheme *runtime.Scheme, cfg TaskProvenanceConfig, reader client.Reader) {
	if !cfg.Enabled {
		return
	}
	server.Register(TaskProvenanceWebhookPath, &ctrladmission.Webhook{
		Handler: NewTaskProvenanceValidator(scheme, cfg, reader),
	})
}

type TaskProvenanceValidator struct {
	decoder ctrladmission.Decoder
	config  TaskProvenanceConfig
	reader  client.Reader
}

func NewTaskProvenanceValidator(scheme *runtime.Scheme, cfg TaskProvenanceConfig, reader client.Reader) *TaskProvenanceValidator {
	return &TaskProvenanceValidator{
		decoder: ctrladmission.NewDecoder(scheme),
		config:  cfg,
		reader:  reader,
	}
}

func (v *TaskProvenanceValidator) Handle(ctx context.Context, req ctrladmission.Request) ctrladmission.Response {
	if (req.SubResource != "" && req.SubResource != statusSubresource) ||
		(req.Operation != admissionv1.Create && req.Operation != admissionv1.Update) {
		return ctrladmission.Allowed("not a Task provenance write")
	}
	task := &corev1alpha1.Task{}
	if err := v.decoder.Decode(req, task); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode Task: %w", err))
	}
	var oldTask *corev1alpha1.Task
	if req.Operation == admissionv1.Update {
		oldTask = &corev1alpha1.Task{}
		if err := v.decoder.DecodeRaw(req.OldObject, oldTask); err != nil {
			return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old Task: %w", err))
		}
		fields := changedTaskCoordinationFields(oldTask, task, isKubernetesCleanupController(req.UserInfo.Username))
		if len(fields) > 0 {
			return ctrladmission.Denied("Task coordination ancestry is immutable: " + strings.Join(fields, ", "))
		}
		if slices.Contains(oldTask.Finalizers, labels.ContinuationLinkFinalizer) &&
			!slices.Contains(task.Finalizers, labels.ContinuationLinkFinalizer) &&
			!isTrustedControllerProvenanceUser(v.config, req.UserInfo) {
			return ctrladmission.Denied("only the Orka controller may remove the " + labels.ContinuationLinkFinalizer + " finalizer")
		}
	}
	if isTrustedControllerProvenanceUser(v.config, req.UserInfo) {
		return ctrladmission.Allowed("trusted Task provenance writer")
	}
	workerTrusted := isTrustedWorkerProvenanceUser(v.config, req.UserInfo, req.Namespace)

	switch req.Operation {
	case admissionv1.Create:
		fields := presentTaskProvenanceFields(task, workerTrusted)
		if len(fields) > 0 {
			return ctrladmission.Denied("direct Task create cannot set Orka-managed provenance fields: " + strings.Join(fields, ", "))
		}
		if workerTrusted && task.Spec.SessionRef != nil && taskCoordinationOwner(task) == nil {
			return ctrladmission.Denied("worker session references require an authorized coordination parent")
		}
		if (workerTrusted || isWorkerServiceAccountUser(v.config, req.UserInfo, req.Namespace)) && taskCoordinationOwner(task) == nil {
			identityBound, err := v.namesIdentityBoundAgent(ctx, req.Namespace, task)
			if err != nil {
				return ctrladmission.Errored(http.StatusInternalServerError, fmt.Errorf("read the Task's Agent: %w", err))
			}
			if identityBound {
				return ctrladmission.Denied("a worker may name an identity-bound Agent only in a Task delegated from its own Task")
			}
		}
		allowed, err := v.authorizedTaskCoordinationParent(ctx, req, task)
		if err != nil {
			return ctrladmission.Errored(http.StatusInternalServerError, fmt.Errorf("verify Task coordination parent: %w", err))
		}
		if !allowed {
			return ctrladmission.Denied("Task coordination parent must be the caller's active Task and any session reference must be inherited unchanged")
		}
	case admissionv1.Update:
		fields := changedTaskProvenanceFields(oldTask, task, workerTrusted)
		if len(fields) > 0 {
			return ctrladmission.Denied("direct Task update cannot modify Orka-managed provenance fields: " + strings.Join(fields, ", "))
		}
	}

	return ctrladmission.Allowed("Task provenance fields unchanged")
}

func (v *TaskProvenanceValidator) namesIdentityBoundAgent(ctx context.Context, namespace string, task *corev1alpha1.Task) (bool, error) {
	if task.Spec.AgentRef == nil || strings.TrimSpace(task.Spec.AgentRef.Name) == "" {
		return false, nil
	}
	if v.reader == nil {
		return false, errors.New("no reader is configured for Agents")
	}
	agent := &corev1alpha1.Agent{}
	if task.Namespace != "" {
		namespace = task.Namespace
	}
	if err := v.reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: task.Spec.AgentRef.Name}, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	return agent.HoldsIdentity(), nil
}

func changedTaskCoordinationFields(oldTask, newTask *corev1alpha1.Task, cleanupController bool) []string {
	fields := changedManagedMapFields(fieldMetadataLabels, oldTask.Labels, newTask.Labels,
		[]string{labels.LabelParentTask, labels.LabelChatSession, labels.LabelChatSessionEpoch, labels.LabelChatSessionNamespace,
			labels.LabelRepositoryMonitor, labels.LabelSecurityScanID, gateway.TaskGatewayBindingLabel})
	fields = append(fields, changedManagedMapFields(fieldMetadataAnnotations, oldTask.Annotations, newTask.Annotations,
		[]string{labels.AnnotationParentTaskName, labels.AnnotationRepositoryMonitorName, gateway.TaskGatewayBindingAnnotation})...)
	changed := func(oldOwner, newOwner *metav1.OwnerReference) bool {
		return !reflect.DeepEqual(oldOwner, newOwner) && (!cleanupController || newOwner != nil)
	}
	if changed(taskCoordinationOwner(oldTask), taskCoordinationOwner(newTask)) ||
		changed(taskAutomationOwner(oldTask), taskAutomationOwner(newTask)) {
		fields = append(fields, "metadata.ownerReferences")
	}
	return fields
}

func taskAutomationOwner(task *corev1alpha1.Task) *metav1.OwnerReference {
	owner := metav1.GetControllerOf(task)
	if owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() ||
		(owner.Kind != "RepositoryScan" && owner.Kind != "RepositoryMonitor") {
		return nil
	}
	owner.BlockOwnerDeletion = nil
	return owner
}

func taskCoordinationOwner(task *corev1alpha1.Task) *metav1.OwnerReference {
	owner := metav1.GetControllerOf(task)
	if owner == nil || owner.APIVersion != corev1alpha1.GroupVersion.String() || owner.Kind != "Task" {
		return nil
	}
	owner.BlockOwnerDeletion = nil
	return owner
}

func presentTaskProvenanceFields(task *corev1alpha1.Task, workerTrusted bool) []string {
	fields := []string{}
	if !workerTrusted {
		if task.Spec.RequestedBy != nil {
			fields = append(fields, fieldSpecRequestedBy)
		}
		if task.Spec.Transaction != nil {
			fields = append(fields, fieldSpecTransaction)
		}
		fields = append(fields, presentManagedMapFields(fieldMetadataLabels, task.Labels, managedTransactionLabelKeys)...)
		fields = append(fields, presentManagedMapFields(fieldMetadataAnnotations, task.Annotations, managedTransactionAnnotationKeys)...)
	}
	fields = append(fields, presentManagedMapFields(fieldMetadataAnnotations, task.Annotations, controllerManagedTaskAnnotationKeys)...)
	fields = append(fields, presentManagedPrefixFields(fieldMetadataLabels, task.Labels)...)
	fields = append(fields, presentManagedPrefixFields(fieldMetadataAnnotations, task.Annotations)...)
	return fields
}

func changedTaskProvenanceFields(oldTask, newTask *corev1alpha1.Task, workerTrusted bool) []string {
	fields := []string{}
	stamped := oldTask.Annotations[labels.AnnotationRequestedBySource] == labels.RequestedBySourceAPI
	if (!workerTrusted || stamped) && !reflect.DeepEqual(oldTask.Spec.RequestedBy, newTask.Spec.RequestedBy) {
		fields = append(fields, fieldSpecRequestedBy)
	}
	if !workerTrusted {
		if !reflect.DeepEqual(oldTask.Spec.Transaction, newTask.Spec.Transaction) {
			fields = append(fields, fieldSpecTransaction)
		}
		fields = append(fields, changedManagedMapFields(fieldMetadataLabels, oldTask.Labels, newTask.Labels, managedTransactionLabelKeys)...)
		fields = append(fields, changedManagedMapFields(fieldMetadataAnnotations, oldTask.Annotations, newTask.Annotations, managedTransactionAnnotationKeys)...)
	}
	fields = append(fields, changedManagedMapFields(fieldMetadataAnnotations, oldTask.Annotations, newTask.Annotations, controllerManagedTaskAnnotationKeys)...)
	fields = append(fields, changedManagedMapFields(fieldMetadataLabels, oldTask.Labels, newTask.Labels, controllerManagedTaskLabelKeys)...)
	fields = append(fields, changedManagedPrefixFields(fieldMetadataLabels, oldTask.Labels, newTask.Labels)...)
	fields = append(fields, changedManagedPrefixFields(fieldMetadataAnnotations, oldTask.Annotations, newTask.Annotations)...)
	return fields
}

func presentManagedPrefixFields(prefix string, values map[string]string) []string {
	fields := []string{}
	for key := range values {
		if strings.HasPrefix(key, managedWorkspaceMetadataPrefix) {
			fields = append(fields, prefix+"["+key+"]")
		}
	}
	slices.Sort(fields)
	return fields
}

func changedManagedPrefixFields(prefix string, oldValues, newValues map[string]string) []string {
	fields := []string{}
	keys := map[string]struct{}{}
	for key := range oldValues {
		if strings.HasPrefix(key, managedWorkspaceMetadataPrefix) {
			keys[key] = struct{}{}
		}
	}
	for key := range newValues {
		if strings.HasPrefix(key, managedWorkspaceMetadataPrefix) {
			keys[key] = struct{}{}
		}
	}
	for key := range keys {
		oldValue, oldOK := oldValues[key]
		newValue, newOK := newValues[key]
		if oldOK != newOK || oldValue != newValue {
			fields = append(fields, prefix+"["+key+"]")
		}
	}
	slices.Sort(fields)
	return fields
}

func presentManagedMapFields(prefix string, values map[string]string, keys []string) []string {
	fields := []string{}
	for _, key := range keys {
		if _, ok := values[key]; ok {
			fields = append(fields, prefix+"["+key+"]")
		}
	}
	return fields
}

func changedManagedMapFields(prefix string, oldValues, newValues map[string]string, keys []string) []string {
	fields := []string{}
	for _, key := range keys {
		oldValue, oldOK := oldValues[key]
		newValue, newOK := newValues[key]
		if oldOK != newOK || oldValue != newValue {
			fields = append(fields, prefix+"["+key+"]")
		}
	}
	return fields
}

func isTrustedControllerProvenanceUser(cfg TaskProvenanceConfig, user authenticationv1.UserInfo) bool {
	username := strings.TrimSpace(user.Username)
	return username != "" && slices.Contains(cfg.ControllerUsernames, username)
}

func WorkerServiceAccountNames(extra ...[]string) []string {
	names := append([]string{}, defaultTrustedWorkerServiceAccounts...)
	for _, list := range extra {
		for _, name := range list {
			if name = strings.TrimSpace(name); name != "" && !slices.Contains(names, name) {
				names = append(names, name)
			}
		}
	}
	return names
}

func isWorkerServiceAccountUser(cfg TaskProvenanceConfig, user authenticationv1.UserInfo, namespace string) bool {
	username := strings.TrimSpace(user.Username)
	for _, name := range cfg.WorkerServiceAccountNames {
		if username != "" && username == serviceAccountUsername(namespace, name) {
			return true
		}
	}
	return false
}

func isTrustedWorkerProvenanceUser(cfg TaskProvenanceConfig, user authenticationv1.UserInfo, namespace string) bool {
	username := strings.TrimSpace(user.Username)
	if username == "" {
		return false
	}
	if slices.Contains(cfg.TrustedUsernames, username) {
		return true
	}
	for _, serviceAccountName := range cfg.TrustedServiceAccountNames {
		if username == serviceAccountUsername(namespace, serviceAccountName) {
			return true
		}
	}
	return false
}

func defaultControllerServiceAccountUsernames(namespace string) []string {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return nil
	}
	return []string{
		serviceAccountUsername(namespace, "orka-controller-manager"),
		serviceAccountUsername(namespace, "controller-manager"),
	}
}

func serviceAccountUsername(namespace, name string) string {
	return "system:serviceaccount:" + strings.TrimSpace(namespace) + ":" + strings.TrimSpace(name)
}
