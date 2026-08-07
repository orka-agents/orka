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

	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const (
	statusSubresource = "status"

	// AgentContractWebhookPath protects explicit built-in Agent classification.
	AgentContractWebhookPath = "/validate-core-orka-ai-v1alpha1-agent-contract"
	// AgentRuntimeContractWebhookPath protects registered runtime classification.
	AgentRuntimeContractWebhookPath = "/validate-core-orka-ai-v1alpha1-agentruntime-contract"
	// TaskExecutionAuthorityWebhookPath protects Task binding and migration evidence.
	TaskExecutionAuthorityWebhookPath = "/validate-core-orka-ai-v1alpha1-task-execution-authority"
	// AdjudicationWebhookPath protects admin-authored adjudication identity and fences.
	AdjudicationWebhookPath = "/validate-core-orka-ai-v1alpha1-agentexecutionadjudication"
	// ControlPolicyWebhookPath protects admin-authored backend control and compatibility policy specs.
	ControlPolicyWebhookPath = "/validate-core-orka-ai-v1alpha1-agentexecution-control-policy"
	// SessionResolutionWebhookPath protects RuntimeSessionControl resolution references.
	SessionResolutionWebhookPath = "/validate-core-orka-ai-v1alpha1-session-resolution"
)

var kubernetesCleanupUsernames = []string{
	"system:serviceaccount:kube-system:generic-garbage-collector",
	"system:serviceaccount:kube-system:garbage-collector",
	"system:serviceaccount:kube-system:namespace-controller",
	"system:kube-controller-manager",
}

// CoexistenceConfig identifies the narrowly authorized writers. Kubernetes
// RBAC remains the primary authorization boundary; these exact usernames are a
// second fail-closed identity fence for controller-owned status transitions.
type CoexistenceConfig struct {
	ControllerUsername             string
	AdjudicationControllerUsername string
	ClassificationUsernames        []string
	AdminGroups                    []string
}

func (c CoexistenceConfig) normalized() CoexistenceConfig {
	c.ControllerUsername = strings.TrimSpace(c.ControllerUsername)
	c.AdjudicationControllerUsername = strings.TrimSpace(c.AdjudicationControllerUsername)
	if c.AdjudicationControllerUsername == "" {
		c.AdjudicationControllerUsername = c.ControllerUsername
	}
	values := make([]string, 0, len(c.ClassificationUsernames)+1)
	for _, value := range c.ClassificationUsernames {
		if value = strings.TrimSpace(value); value != "" && !slices.Contains(values, value) {
			values = append(values, value)
		}
	}
	if c.ControllerUsername != "" && !slices.Contains(values, c.ControllerUsername) {
		values = append(values, c.ControllerUsername)
	}
	c.ClassificationUsernames = values
	adminGroups := make([]string, 0, len(c.AdminGroups))
	for _, value := range c.AdminGroups {
		if value = strings.TrimSpace(value); value != "" && !slices.Contains(adminGroups, value) {
			adminGroups = append(adminGroups, value)
		}
	}
	c.AdminGroups = adminGroups
	return c
}

// RegisterCoexistenceWebhooks registers the mandatory fail-closed bridge
// admission handlers. The standalone admission process supplies a live API
// reader and owns no SQLite, runtime credentials, dispatch, or controller Lease.
func RegisterCoexistenceWebhooks(
	server webhook.Server,
	scheme *runtime.Scheme,
	reader client.Reader,
	config CoexistenceConfig,
) {
	config = config.normalized()
	server.Register(AgentContractWebhookPath, &ctrladmission.Webhook{Handler: &AgentContractValidator{
		decoder: ctrladmission.NewDecoder(scheme), config: config,
	}})
	server.Register(AgentRuntimeContractWebhookPath, &ctrladmission.Webhook{Handler: &AgentRuntimeContractValidator{
		decoder: ctrladmission.NewDecoder(scheme), config: config,
	}})
	server.Register(TaskExecutionAuthorityWebhookPath, &ctrladmission.Webhook{Handler: &TaskExecutionAuthorityValidator{
		decoder: ctrladmission.NewDecoder(scheme), reader: reader, config: config,
	}})
	server.Register(AdjudicationWebhookPath, &ctrladmission.Webhook{Handler: &AdjudicationValidator{
		decoder: ctrladmission.NewDecoder(scheme), reader: reader, config: config,
	}})
	server.Register(ControlPolicyWebhookPath, &ctrladmission.Webhook{Handler: &ControlPolicyValidator{
		decoder: ctrladmission.NewDecoder(scheme), config: config,
	}})
	server.Register(SessionResolutionWebhookPath, &ctrladmission.Webhook{Handler: &SessionResolutionValidator{
		decoder: ctrladmission.NewDecoder(scheme), reader: reader, config: config,
	}})
}

// AgentContractValidator requires explicit built-in classification and permits
// the bridge's absent-to-present update only as a classification-only write by
// an authorized migration identity.
type AgentContractValidator struct {
	decoder ctrladmission.Decoder
	config  CoexistenceConfig
}

func (v *AgentContractValidator) Handle(_ context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.SubResource == statusSubresource || (req.Operation != admissionv1.Create && req.Operation != admissionv1.Update) {
		return ctrladmission.Allowed("not an Agent contract write")
	}
	object := &corev1alpha1.Agent{}
	if err := v.decoder.Decode(req, object); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode agent: %w", err))
	}
	var oldObject *corev1alpha1.Agent
	if req.Operation == admissionv1.Update {
		oldObject = &corev1alpha1.Agent{}
		if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
			return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old agent: %w", err))
		}
		if agentUsesBuiltInRuntime(oldObject) && object.Spec.Runtime != nil && object.Spec.Runtime.RuntimeRef != nil {
			return ctrladmission.Denied("a built-in Agent cannot switch to an external runtimeRef")
		}
	}
	if !agentUsesBuiltInRuntime(object) {
		return ctrladmission.Allowed("Agent derives no built-in contract")
	}
	if object.Spec.Runtime.ContractVersion == nil {
		return ctrladmission.Denied("built-in Agent runtime requires an explicit contractVersion")
	}
	if req.Operation == admissionv1.Create {
		return ctrladmission.Allowed("new Agent has an explicit contract")
	}
	if oldObject.Spec.Runtime != nil && oldObject.Spec.Runtime.ContractVersion != nil {
		if !agentUsesBuiltInRuntime(oldObject) ||
			*object.Spec.Runtime.ContractVersion != *oldObject.Spec.Runtime.ContractVersion {
			return ctrladmission.Denied("Agent contractVersion is immutable once explicit")
		}
		return ctrladmission.Allowed("Agent contract remains explicit and immutable")
	}
	if !agentUsesBuiltInRuntime(oldObject) {
		return ctrladmission.Denied("an external Agent cannot be reclassified as a built-in runtime")
	}
	if !v.config.classifier(req.UserInfo.Username) {
		return ctrladmission.Denied("only an authorized bridge classifier may classify a stored Agent")
	}
	oldCopy, newCopy := oldObject.DeepCopy(), object.DeepCopy()
	oldCopy.Spec.Runtime.ContractVersion = nil
	newCopy.Spec.Runtime.ContractVersion = nil
	if !reflect.DeepEqual(oldCopy.Spec, newCopy.Spec) {
		return ctrladmission.Denied("Agent bridge classification must not change other executable fields")
	}
	return ctrladmission.Allowed("stored Agent classified explicitly")
}

// AgentRuntimeContractValidator applies the equivalent bridge rule to
// registered runtimes.
type AgentRuntimeContractValidator struct {
	decoder ctrladmission.Decoder
	config  CoexistenceConfig
}

func (v *AgentRuntimeContractValidator) Handle(_ context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.SubResource == statusSubresource || (req.Operation != admissionv1.Create && req.Operation != admissionv1.Update) {
		return ctrladmission.Allowed("not an AgentRuntime contract write")
	}
	object := &corev1alpha1.AgentRuntime{}
	if err := v.decoder.Decode(req, object); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode agent runtime: %w", err))
	}
	if object.Spec.ContractVersion == nil {
		return ctrladmission.Denied("AgentRuntime requires an explicit contractVersion")
	}
	if req.Operation == admissionv1.Create {
		return ctrladmission.Allowed("new AgentRuntime has an explicit contract")
	}
	oldObject := &corev1alpha1.AgentRuntime{}
	if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old agent runtime: %w", err))
	}
	if oldObject.Spec.ContractVersion != nil {
		if *object.Spec.ContractVersion != *oldObject.Spec.ContractVersion {
			return ctrladmission.Denied("AgentRuntime contractVersion is immutable once explicit")
		}
		return ctrladmission.Allowed("AgentRuntime contract remains explicit and immutable")
	}
	if !v.config.classifier(req.UserInfo.Username) {
		return ctrladmission.Denied("only an authorized bridge classifier may classify a stored AgentRuntime")
	}
	oldCopy, newCopy := oldObject.DeepCopy(), object.DeepCopy()
	oldCopy.Spec.ContractVersion = nil
	newCopy.Spec.ContractVersion = nil
	if !reflect.DeepEqual(oldCopy.Spec, newCopy.Spec) {
		return ctrladmission.Denied("AgentRuntime bridge classification must not change other executable fields")
	}
	return ctrladmission.Allowed("stored AgentRuntime classified explicitly")
}

// TaskExecutionAuthorityValidator protects binding, sealed-inventory evidence,
// and immutable resolution references even from identities that possess a
// broad status verb.
type TaskExecutionAuthorityValidator struct {
	decoder ctrladmission.Decoder
	reader  client.Reader
	config  CoexistenceConfig
}

func (v *TaskExecutionAuthorityValidator) Handle(ctx context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return ctrladmission.Allowed("not a Task execution-authority write")
	}
	object := &corev1alpha1.Task{}
	if err := v.decoder.Decode(req, object); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode task execution authority: %w", err))
	}
	if req.Operation == admissionv1.Create {
		if taskHasExecutionAuthority(object) {
			return ctrladmission.Denied("Task creation cannot pre-populate controller-owned execution authority")
		}
		return ctrladmission.Allowed("Task has no controller-owned execution authority")
	}
	oldObject := &corev1alpha1.Task{}
	if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old task execution authority: %w", err))
	}
	if taskHasExecutionAuthority(oldObject) && !reflect.DeepEqual(oldObject.Spec, object.Spec) {
		return ctrladmission.Denied(
			"Task spec is immutable after execution authority or a migration disposition is recorded",
		)
	}

	if response := v.validateBinding(ctx, req, oldObject, object); !response.Allowed {
		return response
	}
	if response := v.validateDisposition(req, oldObject, object); !response.Allowed {
		return response
	}
	if response := v.validateResolution(ctx, req, oldObject, object); !response.Allowed {
		return response
	}
	return ctrladmission.Allowed("Task execution authority is unchanged or exactly authorized")
}

func (v *TaskExecutionAuthorityValidator) validateBinding(
	ctx context.Context,
	req ctrladmission.Request,
	oldObject, object *corev1alpha1.Task,
) ctrladmission.Response {
	oldValue, newValue := oldObject.Status.AgentExecutionBinding, object.Status.AgentExecutionBinding
	if reflect.DeepEqual(oldValue, newValue) {
		return ctrladmission.Allowed("Task binding unchanged")
	}
	if oldValue != nil || newValue == nil {
		return ctrladmission.Denied("Task execution binding is write-once and cannot be removed or replaced")
	}
	if strings.TrimSpace(req.UserInfo.Username) != v.config.ControllerUsername {
		return ctrladmission.Denied("only the controller identity may create an execution binding")
	}
	if v.reader == nil {
		return ctrladmission.Errored(http.StatusInternalServerError, fmt.Errorf("live admission reader is unavailable"))
	}
	legacyCleanup := legacyCleanupOnlyBinding(newValue)
	if !object.DeletionTimestamp.IsZero() && !legacyCleanup {
		return ctrladmission.Denied("a deleting Task cannot acquire an execution binding")
	}
	if newValue.Task.UID != object.UID || newValue.Task.BoundSpecGeneration != object.Generation {
		return ctrladmission.Denied("execution binding does not match the immutable Task UID and generation")
	}
	namespace := &corev1.Namespace{}
	if err := v.reader.Get(ctx, client.ObjectKey{Name: object.Namespace}, namespace); err != nil {
		return admissionReadError("read Task namespace identity", err)
	}
	if newValue.Task.NamespaceUID != namespace.UID {
		return ctrladmission.Denied("execution binding namespace UID does not match the live namespace")
	}
	if legacyCleanup {
		return ctrladmission.Allowed("Task carries migration-scoped legacy cleanup-only execution authority")
	}
	if err := validateLiveBindingControl(ctx, v.reader, newValue); err != nil {
		return ctrladmission.Denied(err.Error())
	}
	return ctrladmission.Allowed("Task binding matches the live enabled admission revision")
}

func (v *TaskExecutionAuthorityValidator) validateDisposition(
	req ctrladmission.Request,
	oldObject, object *corev1alpha1.Task,
) ctrladmission.Response {
	oldNoExecution, newNoExecution := oldObject.Status.AgentExecutionNoExecution, object.Status.AgentExecutionNoExecution
	oldQuarantine, newQuarantine := oldObject.Status.AgentExecutionQuarantine, object.Status.AgentExecutionQuarantine
	if !reflect.DeepEqual(oldNoExecution, newNoExecution) {
		if oldNoExecution != nil || newNoExecution == nil {
			return ctrladmission.Denied("Task no-execution evidence is write-once and immutable")
		}
		if !v.config.classifier(req.UserInfo.Username) {
			return ctrladmission.Denied("only an authorized bridge classifier may record no-execution evidence")
		}
		if object.DeletionTimestamp.IsZero() && !terminalTaskPhase(object.Status.Phase) {
			return ctrladmission.Denied("UnboundNoExecution is limited to deleting or terminal-cleanup Tasks")
		}
	}
	if !reflect.DeepEqual(oldQuarantine, newQuarantine) {
		if oldQuarantine != nil || newQuarantine == nil {
			return ctrladmission.Denied("Task quarantine evidence is write-once and immutable")
		}
		if !v.config.classifier(req.UserInfo.Username) {
			return ctrladmission.Denied("only an authorized bridge classifier may record quarantine evidence")
		}
	}
	return ctrladmission.Allowed("Task migration disposition is unchanged or authorized")
}

func (v *TaskExecutionAuthorityValidator) validateResolution(
	ctx context.Context,
	req ctrladmission.Request,
	oldObject, object *corev1alpha1.Task,
) ctrladmission.Response {
	oldValue, newValue := oldObject.Status.AgentExecutionResolutionRef, object.Status.AgentExecutionResolutionRef
	if reflect.DeepEqual(oldValue, newValue) {
		return ctrladmission.Allowed("Task resolution unchanged")
	}
	if oldValue != nil || newValue == nil {
		return ctrladmission.Denied("Task resolution reference is append-once and immutable")
	}
	if strings.TrimSpace(req.UserInfo.Username) != v.config.AdjudicationControllerUsername {
		return ctrladmission.Denied("only the adjudication controller may append a Task resolution reference")
	}
	if err := validateResolutionAppend(ctx, v.reader, object.Namespace, newValue, object, nil); err != nil {
		return ctrladmission.Denied(err.Error())
	}
	return ctrladmission.Allowed("Task resolution references an exact Applied adjudication")
}

// AdjudicationValidator binds requestedBy to AdmissionRequest.UserInfo and
// rejects stale subject identities before the object can enter the queue.
type AdjudicationValidator struct {
	decoder ctrladmission.Decoder
	reader  client.Reader
	config  CoexistenceConfig
}

func (v *AdjudicationValidator) Handle(ctx context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.Operation == admissionv1.Delete {
		return v.validateDelete(ctx, req)
	}
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update {
		return ctrladmission.Allowed("not an adjudication write")
	}
	if req.Operation == admissionv1.Create && !v.config.admin(req.UserInfo.Groups) {
		return ctrladmission.Denied("adjudication creation is restricted to configured admin groups")
	}
	object := &corev1alpha1.AgentExecutionAdjudication{}
	if err := v.decoder.Decode(req, object); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode agent execution adjudication: %w", err))
	}
	if req.SubResource == statusSubresource {
		if strings.TrimSpace(req.UserInfo.Username) != v.config.AdjudicationControllerUsername {
			return ctrladmission.Denied("only the adjudication controller may update adjudication status")
		}
		return ctrladmission.Allowed("adjudication controller status update")
	}
	if req.Operation == admissionv1.Create {
		username := strings.TrimSpace(req.UserInfo.Username)
		if username == "" || object.Spec.RequestedBy != username {
			return ctrladmission.Denied("spec.requestedBy must exactly equal the nonempty authenticated admission caller")
		}
	}
	if req.Operation == admissionv1.Update {
		oldObject := &corev1alpha1.AgentExecutionAdjudication{}
		if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
			return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old agent execution adjudication: %w", err))
		}
		if !reflect.DeepEqual(oldObject.Spec, object.Spec) {
			return ctrladmission.Denied("adjudication spec is immutable")
		}
		return ctrladmission.Allowed("adjudication spec unchanged")
	}
	return v.validateCreateSubject(ctx, object)
}

func (v *AdjudicationValidator) validateDelete(
	ctx context.Context,
	req ctrladmission.Request,
) ctrladmission.Response {
	cleanupController := slices.Contains(kubernetesCleanupUsernames, strings.TrimSpace(req.UserInfo.Username))
	if !cleanupController && !v.config.admin(req.UserInfo.Groups) {
		return ctrladmission.Denied("adjudication deletion is restricted to configured admin groups and Kubernetes cleanup controllers")
	}
	oldObject := &corev1alpha1.AgentExecutionAdjudication{}
	if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode deleted adjudication: %w", err))
	}
	switch oldObject.Status.State {
	case corev1alpha1.AgentExecutionAdjudicationRejected,
		corev1alpha1.AgentExecutionAdjudicationSuperseded:
		return ctrladmission.Allowed("authorized terminal adjudication deletion")
	default:
		return v.validateRetainedAdjudicationDelete(ctx, oldObject, cleanupController)
	}
}

func (v *AdjudicationValidator) validateRetainedAdjudicationDelete(
	ctx context.Context,
	object *corev1alpha1.AgentExecutionAdjudication,
	cleanupController bool,
) ctrladmission.Response {
	if !cleanupController {
		return ctrladmission.Denied("pending, applying, or applied adjudications must be retained for recovery and cleanup")
	}
	if v.reader == nil {
		return ctrladmission.Errored(http.StatusInternalServerError, fmt.Errorf("live admission reader is unavailable"))
	}
	task := &corev1alpha1.Task{}
	err := v.reader.Get(ctx, client.ObjectKey{
		Namespace: object.Namespace,
		Name:      object.Spec.TaskRef.Name,
	}, task)
	if apierrors.IsNotFound(err) || (err == nil && task.UID != object.Spec.TaskRef.UID) {
		return ctrladmission.Allowed("Kubernetes cleanup controller removed an orphaned adjudication")
	}
	if err != nil {
		return admissionReadError("read adjudication subject before cleanup deletion", err)
	}
	return ctrladmission.Denied("pending, applying, or applied adjudications must be retained for recovery and cleanup")
}

func (v *AdjudicationValidator) validateCreateSubject(
	ctx context.Context,
	object *corev1alpha1.AgentExecutionAdjudication,
) ctrladmission.Response {
	if v.reader == nil {
		return ctrladmission.Errored(http.StatusInternalServerError, fmt.Errorf("live admission reader is unavailable"))
	}
	task := &corev1alpha1.Task{}
	if err := v.reader.Get(ctx, client.ObjectKey{Namespace: object.Namespace, Name: object.Spec.TaskRef.Name}, task); err != nil {
		return admissionReadError("read adjudication Task", err)
	}
	if task.UID != object.Spec.TaskRef.UID {
		return ctrladmission.Denied("adjudication Task UID does not match the live subject")
	}
	if object.Spec.QuarantineDigest != "" {
		if task.ResourceVersion != object.Spec.ExpectedState.SubjectResourceVersion ||
			task.Status.AgentExecutionQuarantine == nil {
			return ctrladmission.Denied("adjudication Task version or quarantine subject is stale")
		}
		return ctrladmission.Allowed("adjudication Task identity and version are current")
	}
	if object.Spec.SessionRef == nil {
		return ctrladmission.Denied("blocked-state adjudication requires an exact sessionRef")
	}
	session, err := findRuntimeSessionControl(ctx, v.reader, object.Namespace, object.Spec.SessionRef.Name)
	if err != nil {
		return admissionReadError("read adjudication Session", err)
	}
	if session.UID != object.Spec.SessionRef.UID ||
		session.ResourceVersion != object.Spec.ExpectedState.SubjectResourceVersion ||
		session.Status.Version != object.Spec.ExpectedState.SubjectDomainVersion ||
		session.Status.Availability != "ReconciliationBlocked" {
		return ctrladmission.Denied("adjudication Session identity, version, or blocked state is stale")
	}
	return ctrladmission.Allowed("adjudication Session identity and version are current")
}

// SessionResolutionValidator applies the same Applied-adjudication proof to
// RuntimeSessionControl status.
type SessionResolutionValidator struct {
	decoder ctrladmission.Decoder
	reader  client.Reader
	config  CoexistenceConfig
}

func (v *SessionResolutionValidator) Handle(ctx context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.Operation != admissionv1.Update {
		return ctrladmission.Allowed("not a Session resolution write")
	}
	object := &corev1alpha1.RuntimeSessionControl{}
	oldObject := &corev1alpha1.RuntimeSessionControl{}
	if err := v.decoder.Decode(req, object); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode runtime session control: %w", err))
	}
	if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("decode old runtime session control: %w", err))
	}
	oldValue, newValue := oldObject.Status.AgentExecutionResolutionRef, object.Status.AgentExecutionResolutionRef
	if reflect.DeepEqual(oldValue, newValue) {
		return ctrladmission.Allowed("Session resolution unchanged")
	}
	if oldValue != nil || newValue == nil {
		return ctrladmission.Denied("Session resolution reference is append-once and immutable")
	}
	if strings.TrimSpace(req.UserInfo.Username) != v.config.AdjudicationControllerUsername {
		return ctrladmission.Denied("only the adjudication controller may append a Session resolution reference")
	}
	if err := validateResolutionAppend(ctx, v.reader, object.Namespace, newValue, nil, object); err != nil {
		return ctrladmission.Denied(err.Error())
	}
	return ctrladmission.Allowed("Session resolution references an exact Applied adjudication")
}

func validateLiveBindingControl(
	ctx context.Context,
	reader client.Reader,
	binding *corev1alpha1.AgentExecutionBinding,
) error {
	if reader == nil || binding == nil || binding.BackendControl == nil {
		return fmt.Errorf("new execution binding requires a live backend-control revision")
	}
	control := &corev1alpha1.AgentExecutionControl{}
	if err := reader.Get(ctx, client.ObjectKey{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}, control); err != nil {
		return fmt.Errorf("read agent execution control: %w", err)
	}
	ref := binding.BackendControl
	if ref.Name != control.Name || ref.UID != control.UID || ref.Generation != control.Generation ||
		control.Status.ObservedGeneration != control.Generation || control.Status.Backends == nil {
		return fmt.Errorf("execution binding carries a stale or incomplete AgentExecutionControl identity")
	}
	status := control.Status.Backends.V2
	if binding.ContractVersion == corev1alpha1.AgentRuntimeContractHarnessV1 {
		status = control.Status.Backends.V1
	} else if binding.ContractVersion != corev1alpha1.AgentRuntimeContractHarnessV2 {
		return fmt.Errorf("execution binding has an unsupported contract version")
	}
	if status.EffectiveMode != corev1alpha1.AgentExecutionEffectiveModeEnabled ||
		status.AdmissionClosedAt != nil || ref.ModeRevision != status.ModeRevision ||
		ref.AdmittedMode != corev1alpha1.AgentExecutionEffectiveModeEnabled {
		return fmt.Errorf("execution binding does not match the live enabled backend admission revision")
	}
	return nil
}

func validateResolutionAppend(
	ctx context.Context,
	reader client.Reader,
	namespace string,
	ref *corev1alpha1.AgentExecutionResolutionRef,
	task *corev1alpha1.Task,
	session *corev1alpha1.RuntimeSessionControl,
) error {
	if reader == nil || ref == nil {
		return fmt.Errorf("live admission reader and resolution reference are required")
	}
	adjudication := &corev1alpha1.AgentExecutionAdjudication{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.AdjudicationName}, adjudication); err != nil {
		return fmt.Errorf("read referenced adjudication: %w", err)
	}
	if adjudication.UID != ref.AdjudicationUID ||
		adjudication.Status.State != corev1alpha1.AgentExecutionAdjudicationApplying ||
		adjudication.Spec.Action != ref.Action || adjudication.Status.OperationDigest != ref.OperationDigest ||
		adjudication.Status.ResolutionRefDigest != "" ||
		adjudication.Status.ResultingSubjectResourceVersion != "" {
		return fmt.Errorf("resolution reference does not match an exact Applying adjudication operation")
	}
	expectedDigest, err := store.CanonicalAgentExecutionResolutionRefDigest(namespace, ref)
	if err != nil {
		return fmt.Errorf("compute canonical resolution reference digest: %w", err)
	}
	if expectedDigest != ref.ResolutionDigest {
		return fmt.Errorf("resolution reference digest does not match canonical content")
	}
	if task != nil && (adjudication.Spec.TaskRef.Name != task.Name || adjudication.Spec.TaskRef.UID != task.UID) {
		return fmt.Errorf("applying adjudication does not name the exact Task subject")
	}
	if session != nil {
		if adjudication.Spec.SessionRef == nil || adjudication.Spec.SessionRef.Name != session.Spec.SessionName ||
			adjudication.Spec.SessionRef.UID != session.UID {
			return fmt.Errorf("applying adjudication does not name the exact Session subject")
		}
	}
	return nil
}

func findRuntimeSessionControl(
	ctx context.Context,
	reader client.Reader,
	namespace, sessionName string,
) (*corev1alpha1.RuntimeSessionControl, error) {
	controls := &corev1alpha1.RuntimeSessionControlList{}
	if err := reader.List(ctx, controls, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	for i := range controls.Items {
		if controls.Items[i].Spec.SessionName == sessionName {
			return &controls.Items[i], nil
		}
	}
	return nil, apierrors.NewNotFound(corev1alpha1.GroupVersion.WithResource("runtimesessioncontrols").GroupResource(), sessionName)
}

func (c CoexistenceConfig) classifier(username string) bool {
	return slices.Contains(c.ClassificationUsernames, strings.TrimSpace(username))
}

func (c CoexistenceConfig) admin(groups []string) bool {
	for _, group := range groups {
		if slices.Contains(c.AdminGroups, strings.TrimSpace(group)) {
			return true
		}
	}
	return false
}

func legacyCleanupOnlyBinding(binding *corev1alpha1.AgentExecutionBinding) bool {
	return binding != nil &&
		binding.Provenance == corev1alpha1.AgentExecutionProvenanceLegacyCleanupOnly &&
		binding.Mode == corev1alpha1.AgentExecutionBindingModeCleanupOnly &&
		binding.BackendControl == nil
}

func agentUsesBuiltInRuntime(agent *corev1alpha1.Agent) bool {
	return agent != nil && agent.Spec.Runtime != nil && agent.Spec.Runtime.Type != ""
}

func taskHasExecutionAuthority(task *corev1alpha1.Task) bool {
	return task != nil && (task.Status.AgentExecutionBinding != nil ||
		task.Status.AgentExecutionNoExecution != nil || task.Status.AgentExecutionQuarantine != nil ||
		task.Status.AgentExecutionResolutionRef != nil)
}

func terminalTaskPhase(phase corev1alpha1.TaskPhase) bool {
	switch phase {
	case corev1alpha1.TaskPhaseSucceeded, corev1alpha1.TaskPhaseFailed, corev1alpha1.TaskPhaseCancelled:
		return true
	default:
		return false
	}
}

func admissionReadError(operation string, err error) ctrladmission.Response {
	if apierrors.IsNotFound(err) {
		return ctrladmission.Denied(operation + ": referenced object does not exist")
	}
	return ctrladmission.Errored(http.StatusInternalServerError, fmt.Errorf("%s: %w", operation, err))
}
