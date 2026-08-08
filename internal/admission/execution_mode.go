/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/executionmode"
	"github.com/orka-agents/orka/internal/labels"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

const (
	statusSubresource = "status"

	NamespaceExecutionModeWebhookPath = "/validate-v1-namespace-execution-mode"
	AgentContractWebhookPath          = "/validate-core-orka-ai-v1alpha1-agent-contract"
	AgentRuntimeContractWebhookPath   = "/validate-core-orka-ai-v1alpha1-agentruntime-contract"
	TaskExecutionAuthorityWebhookPath = "/validate-core-orka-ai-v1alpha1-task-execution-authority"
)

// ExecutionModeConfig identifies exact controller writers. Namespace-scoped
// RBAC remains the primary authorization boundary; these usernames are a
// second fail-closed identity fence for controller-owned Task status.
type ExecutionModeConfig struct {
	ControllerUsernames []string
}

func (c ExecutionModeConfig) normalized() ExecutionModeConfig {
	values := make([]string, 0, len(c.ControllerUsernames))
	for _, value := range c.ControllerUsernames {
		value = strings.TrimSpace(value)
		if value != "" && !slices.Contains(values, value) {
			values = append(values, value)
		}
	}
	c.ControllerUsernames = values
	return c
}

func (c ExecutionModeConfig) controller(username string) bool {
	return slices.Contains(c.ControllerUsernames, strings.TrimSpace(username))
}

// RegisterExecutionModeWebhooks registers the static namespace-mode boundary.
func RegisterExecutionModeWebhooks(
	server webhook.Server,
	scheme *runtime.Scheme,
	reader client.Reader,
	config ExecutionModeConfig,
) {
	config = config.normalized()
	decoder := ctrladmission.NewDecoder(scheme)
	server.Register(NamespaceExecutionModeWebhookPath, &ctrladmission.Webhook{Handler: &NamespaceExecutionModeValidator{
		decoder: decoder,
	}})
	server.Register(AgentContractWebhookPath, &ctrladmission.Webhook{Handler: &AgentContractValidator{
		decoder: decoder, reader: reader,
	}})
	server.Register(AgentRuntimeContractWebhookPath, &ctrladmission.Webhook{Handler: &AgentRuntimeContractValidator{
		decoder: decoder, reader: reader,
	}})
	server.Register(TaskExecutionAuthorityWebhookPath, &ctrladmission.Webhook{Handler: &TaskExecutionAuthorityValidator{
		decoder: decoder, reader: reader, config: config,
	}})
}

// NamespaceExecutionModeValidator permits a namespace to acquire one valid
// execution-mode label and then makes that claim immutable.
type NamespaceExecutionModeValidator struct {
	decoder ctrladmission.Decoder
}

func (v *NamespaceExecutionModeValidator) Handle(_ context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return ctrladmission.Allowed("not a namespace execution-mode write")
	}
	object := &corev1.Namespace{}
	if err := v.decoder.Decode(req, object); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode namespace: %w", err))
	}
	newValue := strings.TrimSpace(object.Labels[executionmode.NamespaceLabel])
	if req.Operation == admissionv1.Create {
		if newValue == "" {
			return ctrladmission.Allowed("namespace has no Orka execution-mode claim")
		}
		if _, err := executionmode.Parse(newValue); err != nil {
			return ctrladmission.Denied(err.Error())
		}
		return ctrladmission.Allowed("namespace acquired an immutable execution-mode claim")
	}

	oldObject := &corev1.Namespace{}
	if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old namespace: %w", err))
	}
	oldValue := strings.TrimSpace(oldObject.Labels[executionmode.NamespaceLabel])
	if oldValue != "" && newValue != oldValue {
		return ctrladmission.Denied("namespace execution-mode claim is immutable; recreate the installation in a different namespace")
	}
	if newValue == "" {
		return ctrladmission.Allowed("namespace has no Orka execution-mode claim")
	}
	if _, err := executionmode.Parse(newValue); err != nil {
		return ctrladmission.Denied(err.Error())
	}
	return ctrladmission.Allowed("namespace execution-mode claim is unchanged")
}

type AgentContractValidator struct {
	decoder ctrladmission.Decoder
	reader  client.Reader
}

func (v *AgentContractValidator) Handle(ctx context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.SubResource == statusSubresource || (req.Operation != admissionv1.Create && req.Operation != admissionv1.Update) {
		return ctrladmission.Allowed("not an Agent contract write")
	}
	object := &corev1alpha1.Agent{}
	if err := v.decoder.Decode(req, object); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode Agent: %w", err))
	}
	mode, response := namespaceExecutionMode(ctx, v.reader, object.Namespace)
	if !response.Allowed {
		return response
	}

	var oldObject *corev1alpha1.Agent
	if req.Operation == admissionv1.Update {
		oldObject = &corev1alpha1.Agent{}
		if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
			return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old Agent: %w", err))
		}
		if agentUsesBuiltInRuntime(oldObject) && !agentUsesBuiltInRuntime(object) {
			return ctrladmission.Denied("a built-in Agent cannot switch execution route or remove its runtime configuration")
		}
		if !agentUsesBuiltInRuntime(oldObject) && agentUsesBuiltInRuntime(object) {
			return ctrladmission.Denied("an external Agent cannot be reclassified as a built-in runtime")
		}
	}
	if !agentUsesBuiltInRuntime(object) {
		return ctrladmission.Allowed("Agent derives its contract from AgentRuntime")
	}
	if object.Spec.Runtime.ContractVersion == nil {
		return ctrladmission.Denied("built-in Agent runtime requires an explicit contractVersion")
	}
	if *object.Spec.Runtime.ContractVersion != mode.ContractVersion() {
		return ctrladmission.Denied(fmt.Sprintf("Agent contractVersion must match namespace execution mode %q", mode))
	}
	if oldObject != nil {
		if oldObject.Spec.Runtime == nil || oldObject.Spec.Runtime.ContractVersion == nil {
			return ctrladmission.Denied("stored unclassified Agent cannot be adopted; recreate it in the mode namespace")
		}
		if *oldObject.Spec.Runtime.ContractVersion != *object.Spec.Runtime.ContractVersion {
			return ctrladmission.Denied("Agent contractVersion is immutable")
		}
	}
	return ctrladmission.Allowed("Agent contract matches the immutable namespace mode")
}

type AgentRuntimeContractValidator struct {
	decoder ctrladmission.Decoder
	reader  client.Reader
}

func (v *AgentRuntimeContractValidator) Handle(ctx context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.SubResource == statusSubresource || (req.Operation != admissionv1.Create && req.Operation != admissionv1.Update) {
		return ctrladmission.Allowed("not an AgentRuntime contract write")
	}
	object := &corev1alpha1.AgentRuntime{}
	if err := v.decoder.Decode(req, object); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode AgentRuntime: %w", err))
	}
	mode, response := namespaceExecutionMode(ctx, v.reader, object.Namespace)
	if !response.Allowed {
		return response
	}
	if object.Spec.ContractVersion == nil {
		return ctrladmission.Denied("AgentRuntime requires an explicit contractVersion")
	}
	if *object.Spec.ContractVersion != mode.ContractVersion() {
		return ctrladmission.Denied(fmt.Sprintf("AgentRuntime contractVersion must match namespace execution mode %q", mode))
	}
	if req.Operation == admissionv1.Update {
		oldObject := &corev1alpha1.AgentRuntime{}
		if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
			return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old AgentRuntime: %w", err))
		}
		if oldObject.Spec.ContractVersion == nil {
			return ctrladmission.Denied("stored unclassified AgentRuntime cannot be adopted; recreate it in the mode namespace")
		}
		if *oldObject.Spec.ContractVersion != *object.Spec.ContractVersion {
			return ctrladmission.Denied("AgentRuntime contractVersion is immutable")
		}
	}
	return ctrladmission.Allowed("AgentRuntime contract matches the immutable namespace mode")
}

type TaskExecutionAuthorityValidator struct {
	decoder ctrladmission.Decoder
	reader  client.Reader
	config  ExecutionModeConfig
}

func (v *TaskExecutionAuthorityValidator) Handle(ctx context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return ctrladmission.Allowed("not a Task execution-authority write")
	}
	object := &corev1alpha1.Task{}
	if err := v.decoder.Decode(req, object); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode Task execution authority: %w", err))
	}
	if req.Operation == admissionv1.Create {
		if taskHasExecutionAuthority(object) {
			return ctrladmission.Denied("Task creation cannot pre-populate controller-owned execution authority")
		}
		return ctrladmission.Allowed("Task has no controller-owned execution authority")
	}
	oldObject := &corev1alpha1.Task{}
	if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old Task execution authority: %w", err))
	}
	if taskHasExecutionAuthority(oldObject) && !reflect.DeepEqual(oldObject.Spec, object.Spec) {
		return ctrladmission.Denied("Task spec is immutable after execution authority is recorded")
	}
	if taskHasExecutionAuthority(oldObject) && slices.Contains(oldObject.Finalizers, labels.TaskFinalizer) &&
		!slices.Contains(object.Finalizers, labels.TaskFinalizer) {
		if !v.config.controller(req.UserInfo.Username) || oldObject.DeletionTimestamp.IsZero() || object.DeletionTimestamp.IsZero() {
			return ctrladmission.Denied("only an authorized controller may remove the Task cleanup finalizer while completing deletion")
		}
	}

	oldBinding, newBinding := oldObject.Status.AgentExecutionBinding, object.Status.AgentExecutionBinding
	if response, handled := taskStatusWriteResponse(v.config, req.UserInfo.Username, oldObject, object); handled {
		return response
	}
	if oldBinding != nil || newBinding == nil {
		return ctrladmission.Denied("Task execution binding is write-once and cannot be removed or replaced")
	}
	if !v.config.controller(req.UserInfo.Username) {
		return ctrladmission.Denied("only an authorized controller identity may create an execution binding")
	}
	if !object.DeletionTimestamp.IsZero() {
		return ctrladmission.Denied("a deleting Task cannot acquire an execution binding")
	}
	if newBinding.Task.UID != object.UID || newBinding.Task.BoundSpecGeneration != object.Generation {
		return ctrladmission.Denied("execution binding does not match the immutable Task UID and generation")
	}
	mode, response := namespaceExecutionMode(ctx, v.reader, object.Namespace)
	if !response.Allowed {
		return response
	}
	namespace := &corev1.Namespace{}
	if err := v.reader.Get(ctx, client.ObjectKey{Name: object.Namespace}, namespace); err != nil {
		return admissionReadError("read Task namespace identity", err)
	}
	if newBinding.Task.NamespaceUID != namespace.UID {
		return ctrladmission.Denied("execution binding namespace UID does not match the live namespace")
	}
	if newBinding.ContractVersion != mode.ContractVersion() {
		return ctrladmission.Denied(fmt.Sprintf("execution binding contractVersion must match namespace execution mode %q", mode))
	}
	if !bindingBackendMatchesMode(newBinding, mode) {
		return ctrladmission.Denied(fmt.Sprintf("execution binding backend does not belong to namespace execution mode %q", mode))
	}
	if newBinding.Agent != nil && newBinding.Agent.Namespace != object.Namespace {
		return ctrladmission.Denied("execution binding Agent identity must be in the Task namespace")
	}
	return ctrladmission.Allowed("Task binding matches the immutable namespace mode")
}

func taskStatusWriteResponse(
	config ExecutionModeConfig,
	username string,
	oldObject, object *corev1alpha1.Task,
) (ctrladmission.Response, bool) {
	if !reflect.DeepEqual(oldObject.Status, object.Status) && !config.controller(username) {
		return ctrladmission.Denied("only an authorized controller identity may update Task status"), true
	}
	if reflect.DeepEqual(oldObject.Status.AgentExecutionBinding, object.Status.AgentExecutionBinding) {
		return ctrladmission.Allowed("Task execution binding is unchanged"), true
	}
	return ctrladmission.Response{}, false
}

func namespaceExecutionMode(ctx context.Context, reader client.Reader, namespaceName string) (executionmode.Mode, ctrladmission.Response) {
	if reader == nil {
		return "", ctrladmission.Errored(http.StatusInternalServerError, fmt.Errorf("live admission reader is unavailable"))
	}
	namespace := &corev1.Namespace{}
	if err := reader.Get(ctx, client.ObjectKey{Name: namespaceName}, namespace); err != nil {
		return "", admissionReadError("read namespace execution mode", err)
	}
	mode, err := executionmode.FromNamespace(namespace)
	if err != nil {
		return "", ctrladmission.Denied(err.Error())
	}
	return mode, ctrladmission.Allowed("namespace execution mode is valid")
}

func bindingBackendMatchesMode(binding *corev1alpha1.AgentExecutionBinding, mode executionmode.Mode) bool {
	if binding == nil {
		return false
	}
	switch mode {
	case executionmode.HarnessV1:
		return binding.Backend == corev1alpha1.AgentExecutionBackendHarnessWrapper ||
			binding.Backend == corev1alpha1.AgentExecutionBackendExternalEndpoint
	case executionmode.HarnessV2:
		return binding.Backend == corev1alpha1.AgentExecutionBackendRuntimePool ||
			binding.Backend == corev1alpha1.AgentExecutionBackendExternalEndpoint
	default:
		return false
	}
}

func agentUsesBuiltInRuntime(agent *corev1alpha1.Agent) bool {
	return agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.Type != ""
}

func taskHasExecutionAuthority(task *corev1alpha1.Task) bool {
	return task != nil && task.Status.AgentExecutionBinding != nil
}

func admissionReadError(operation string, err error) ctrladmission.Response {
	if apierrors.IsNotFound(err) {
		return ctrladmission.Denied(operation + ": referenced object does not exist")
	}
	return ctrladmission.Errored(http.StatusInternalServerError, fmt.Errorf("%s: %w", operation, err))
}
