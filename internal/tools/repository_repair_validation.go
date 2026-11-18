/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"
	"strconv"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/workerenv"
)

const (
	// RepositoryValidationKindRepair marks a validation Task that gates one
	// repository monitor repair patch before publication (ADR 0038). Review
	// validation bindings carry no kind.
	RepositoryValidationKindRepair = "repair"

	// RepositoryRepairValidationCapabilitySecretKey is the only data key in the
	// controller-owned Secret that carries the single-use patch download
	// capability to the validation Pod.
	RepositoryRepairValidationCapabilitySecretKey = "capability.json"

	repositoryRepairValidationCapabilitySecretSuffix = "-patch"
	repositoryValidationShell                        = "/bin/sh"
	repositoryRepairValidationResultEventType        = "repair_validation_result"
	repositoryRepairValidationResultSummary          = "Repository repair validation result"
)

// RepositoryRepairValidationStatus is the recorded outcome for one exact
// repair patch digest.
type RepositoryRepairValidationStatus string

const (
	RepositoryRepairValidationPassed      RepositoryRepairValidationStatus = "passed"
	RepositoryRepairValidationFailed      RepositoryRepairValidationStatus = "failed"
	RepositoryRepairValidationUnavailable RepositoryRepairValidationStatus = "unavailable"
)

// ValidRepositoryRepairValidationCommand reports whether command satisfies the
// spec.validation.repairCommand contract. The same rules apply to commands a
// reviewer passes to run_validation.
func ValidRepositoryRepairValidationCommand(command string) bool {
	return command != "" && command == strings.TrimSpace(command) &&
		len(command) <= workerenv.RepositoryValidationMaxCommandBytes && utf8.ValidString(command) &&
		strings.IndexByte(command, 0) < 0 && redact.SensitiveText(command) == command
}

// RepositoryRepairValidationTaskName returns the deterministic validation Task
// name for one exact repair Task incarnation and patch digest. A different
// patch from the same repair always gets a different validation Task.
func RepositoryRepairValidationTaskName(repairTask *corev1alpha1.Task, deltaDigest string) string {
	if repairTask == nil {
		return ""
	}
	name := strings.Trim(strings.TrimSpace(repairTask.Name), "-")
	uid := strings.TrimSpace(string(repairTask.UID))
	deltaDigest = strings.TrimSpace(deltaDigest)
	if name == "" || uid == "" || deltaDigest == "" {
		return ""
	}
	const suffix = "-validation"
	digest := sha256.Sum256([]byte(name + "\x00" + uid + "\x00" + deltaDigest))
	hash := hex.EncodeToString(digest[:])[:12]
	prefixLength := 63 - len(hash) - len(suffix) - 1 - len(repositoryRepairValidationCapabilitySecretSuffix)
	prefix := strings.Trim(name[:min(len(name), prefixLength)], "-")
	if prefix == "" {
		prefix = "repair"
	}
	return prefix + "-" + hash + suffix
}

// RepositoryRepairValidationCapabilitySecretName returns the Secret that holds
// the patch download capability for one validation Task.
func RepositoryRepairValidationCapabilitySecretName(validationTaskName string) string {
	return strings.TrimSpace(validationTaskName) + repositoryRepairValidationCapabilitySecretSuffix
}

// RepositoryRepairValidationInput is the controller-frozen identity of one
// repair validation request.
type RepositoryRepairValidationInput struct {
	Monitor     *corev1alpha1.RepositoryMonitor
	RepairTask  *corev1alpha1.Task
	Image       string
	Command     string
	DeltaDigest string
	// RelativeRoot is the repair workspace subpath the delta is rooted at.
	RelativeRoot string
}

func (in RepositoryRepairValidationInput) validate() error {
	repair := in.RepairTask
	if in.Monitor == nil || repair == nil || repair.Spec.Workspace == nil {
		return fmt.Errorf("repository monitor, repair task, and repair workspace are required")
	}
	workspace := repair.Spec.Workspace
	if repair.Namespace == "" || repair.Name == "" || repair.UID == "" || in.Monitor.Namespace != repair.Namespace ||
		in.Monitor.Name == "" || in.Monitor.UID == "" {
		return fmt.Errorf("repair validation identity is incomplete")
	}
	if !ValidRepositoryValidationImage(strings.TrimSpace(in.Image)) {
		return fmt.Errorf("repair validation image must be digest-pinned")
	}
	if !ValidRepositoryRepairValidationCommand(in.Command) {
		return fmt.Errorf("repair validation command is invalid")
	}
	if !strings.HasPrefix(in.DeltaDigest, "sha256:") || len(in.DeltaDigest) != len("sha256:")+64 {
		return fmt.Errorf("repair validation patch digest is invalid")
	}
	if workspace.Intent != corev1alpha1.WorkspaceIntentWrite || strings.TrimSpace(workspace.GitRepo) == "" ||
		strings.TrimSpace(workspace.ExpectedRemoteSHA) == "" {
		return fmt.Errorf("repair task is not bound to an exact writable pull request head")
	}
	if strings.TrimSpace(in.RelativeRoot) != strings.TrimSpace(workspace.SubPath) {
		return fmt.Errorf("repair validation relative root does not match the repair workspace")
	}
	return nil
}

// BuildRepositoryRepairValidationTask renders the validation Task for one
// repair patch. The checkout is the exact head the repair started from and
// carries only the repair's read credential.
func BuildRepositoryRepairValidationTask(in RepositoryRepairValidationInput) (*corev1alpha1.Task, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	repair := in.RepairTask
	headSHA := strings.TrimSpace(repair.Spec.Workspace.ExpectedRemoteSHA)
	source := repair.Spec.Workspace
	workspace := &corev1alpha1.WorkspaceConfig{
		Intent:            corev1alpha1.WorkspaceIntentRead,
		GitRepo:           source.GitRepo,
		Ref:               headSHA,
		SubPath:           source.SubPath,
		ReadCredentialRef: source.ReadCredentialRef.DeepCopy(),
	}
	if source.SourceRepository != nil {
		workspace.SourceRepository = source.SourceRepository.DeepCopy()
	}
	timeout := metav1.Duration{Duration: RepositoryValidationTimeout}
	annotations := map[string]string{
		labels.AnnotationParentTaskName:                    repair.Name,
		labels.AnnotationParentTaskUID:                     string(repair.UID),
		labels.AnnotationRepositoryMonitorName:             in.Monitor.Name,
		labels.AnnotationMonitorRunID:                      repair.Annotations[labels.AnnotationMonitorRunID],
		labels.AnnotationMonitorItemKind:                   repair.Annotations[labels.AnnotationMonitorItemKind],
		labels.AnnotationMonitorItemNumber:                 repair.Annotations[labels.AnnotationMonitorItemNumber],
		labels.AnnotationMonitorHeadSHA:                    headSHA,
		labels.AnnotationGitHubRepository:                  repair.Annotations[labels.AnnotationGitHubRepository],
		labels.AnnotationRepositoryValidationImage:         strings.TrimSpace(in.Image),
		labels.AnnotationRepositoryValidationCommandDigest: RepositoryValidationCommandDigest(in.Command),
		labels.AnnotationRepositoryValidationKind:          RepositoryValidationKindRepair,
		labels.AnnotationRepositoryValidationPatchDigest:   in.DeltaDigest,
		labels.AnnotationWorkspaceInitContainer:            trueStr,
	}
	if source.DenyRepositoryControlPaths {
		annotations[labels.AnnotationRepositoryValidationDenyControlPaths] = trueStr
	}
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RepositoryRepairValidationTaskName(repair, in.DeltaDigest),
			Namespace: repair.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:           trueStr,
				labels.LabelCreatedBy:         repositoryValidationCreatedBy,
				labels.LabelPurpose:           repositoryValidationPurpose,
				labels.LabelParentTask:        labels.SelectorValue(repair.Name),
				labels.LabelRepositoryMonitor: labels.SelectorValue(in.Monitor.Name),
				labels.LabelMonitorRun:        repair.Labels[labels.LabelMonitorRun],
				labels.LabelGitHubRepository:  repair.Labels[labels.LabelGitHubRepository],
				labels.LabelGitHubTarget:      repair.Labels[labels.LabelGitHubTarget],
				labels.LabelGitHubNumber:      repair.Labels[labels.LabelGitHubNumber],
			},
			Annotations: annotations,
			// The exact repair incarnation owns its validation work, so replacing
			// or deleting the repair garbage-collects it.
			OwnerReferences: []metav1.OwnerReference{
				*metav1.NewControllerRef(repair, corev1alpha1.GroupVersion.WithKind(taskKindString)),
			},
		},
		Spec: corev1alpha1.TaskSpec{
			Type:      corev1alpha1.TaskTypeContainer,
			Image:     strings.TrimSpace(in.Image),
			Command:   []string{repositoryValidationShell, "-c"},
			Args:      []string{repositoryValidationTaskPlaceholder},
			Timeout:   &timeout,
			Workspace: workspace,
		},
	}
	if repair.Spec.Priority != nil {
		priority := *repair.Spec.Priority
		task.Spec.Priority = &priority
	}
	inheritTaskProvenance(task, repair)
	return task, nil
}

// RepositoryRepairValidationTaskMatches reports whether existing is exactly
// the validation Task the controller renders for in.
func RepositoryRepairValidationTaskMatches(existing *corev1alpha1.Task, in RepositoryRepairValidationInput) bool {
	expected, err := BuildRepositoryRepairValidationTask(in)
	if err != nil || existing == nil {
		return false
	}
	return repositoryValidationTaskMatches(existing, expected)
}

// BuildRepositoryRepairValidationCommandSecret renders the immutable command
// Secret consumed by the validation Job's command materializer. It has the
// same shape as a review command Secret, owned by the repair Task.
func BuildRepositoryRepairValidationCommandSecret(repairTask, validationTask *corev1alpha1.Task, command string) *corev1.Secret {
	return buildRepositoryValidationCommandSecret(repairTask, validationTask, command)
}

// EnsureRepositoryRepairValidationCommandSecret creates or verifies the
// immutable command Secret.
func EnsureRepositoryRepairValidationCommandSecret(ctx context.Context, k8sClient client.Client, expected *corev1.Secret) error {
	return ensureRepositoryValidationCommandSecret(ctx, k8sClient, expected)
}

// RepositoryRepairValidationBindingEvent returns the append-only event that
// binds a repair validation Task to its repair, image, head, command, and
// patch digest before the Task is created.
func RepositoryRepairValidationBindingEvent(in RepositoryRepairValidationInput, validationTask *corev1alpha1.Task) (*store.MonitorEvent, error) {
	if err := in.validate(); err != nil {
		return nil, err
	}
	repair := in.RepairTask
	itemNumber, err := strconv.ParseInt(strings.TrimSpace(repair.Annotations[labels.AnnotationMonitorItemNumber]), 10, 64)
	runID := strings.TrimSpace(repair.Annotations[labels.AnnotationMonitorRunID])
	itemKind := strings.TrimSpace(repair.Annotations[labels.AnnotationMonitorItemKind])
	headSHA := strings.TrimSpace(repair.Spec.Workspace.ExpectedRemoteSHA)
	if err != nil || itemNumber <= 0 || runID == "" || itemKind == "" || validationTask == nil ||
		validationTask.Namespace != repair.Namespace ||
		validationTask.Name != RepositoryRepairValidationTaskName(repair, in.DeltaDigest) {
		return nil, fmt.Errorf("repair validation binding identity is incomplete")
	}
	binding := repositoryValidationCommandBinding{
		MonitorUID:         string(in.Monitor.UID),
		ReviewTaskName:     repair.Name,
		ReviewTaskUID:      string(repair.UID),
		ValidationTaskName: validationTask.Name,
		Image:              strings.TrimSpace(in.Image),
		HeadSHA:            headSHA,
		CommandDigest:      RepositoryValidationCommandDigest(in.Command),
		Kind:               RepositoryValidationKindRepair,
		PatchDigest:        in.DeltaDigest,
	}
	metadata, err := json.Marshal(binding)
	if err != nil {
		return nil, fmt.Errorf("encode repair validation binding: %w", err)
	}
	return &store.MonitorEvent{
		ID:               repositoryValidationCommandBindingEventID(repair.Namespace, validationTask.Name),
		MonitorNamespace: in.Monitor.Namespace,
		MonitorName:      in.Monitor.Name,
		RunID:            runID,
		ItemKind:         itemKind,
		ItemNumber:       itemNumber,
		ItemSHA:          headSHA,
		EventType:        repositoryValidationBindingEventType,
		Actor:            repositoryValidationBindingActor,
		Summary:          repositoryValidationBindingSummary,
		MetadataJSON:     string(metadata),
	}, nil
}

// EnsureRepositoryRepairValidationBinding persists the binding event or
// verifies the existing one is identical.
func EnsureRepositoryRepairValidationBinding(ctx context.Context, bindingStore RepositoryValidationBindingStore, expected *store.MonitorEvent) error {
	return ensureRepositoryValidationCommandBinding(ctx, bindingStore, expected)
}

// MatchesRepair reports whether a durable binding belongs to the exact repair
// Task, RepositoryMonitor, image, head, and patch digest.
func (b *RepositoryValidationCommandBinding) MatchesRepair(repairTask *corev1alpha1.Task, monitor *corev1alpha1.RepositoryMonitor, image string) bool {
	if b == nil || repairTask == nil || monitor == nil || repairTask.Spec.Workspace == nil || b.Kind != RepositoryValidationKindRepair {
		return false
	}
	itemNumber, err := strconv.ParseInt(strings.TrimSpace(repairTask.Annotations[labels.AnnotationMonitorItemNumber]), 10, 64)
	return err == nil && itemNumber > 0 &&
		b.MonitorNamespace == repairTask.Namespace && b.MonitorNamespace == monitor.Namespace &&
		b.MonitorName == monitor.Name && b.MonitorUID == string(monitor.UID) &&
		b.ReviewTaskName == repairTask.Name && b.ReviewTaskUID == string(repairTask.UID) &&
		b.ValidationTaskName == RepositoryRepairValidationTaskName(repairTask, b.PatchDigest) &&
		b.RunID == strings.TrimSpace(repairTask.Annotations[labels.AnnotationMonitorRunID]) &&
		b.ItemKind == strings.TrimSpace(repairTask.Annotations[labels.AnnotationMonitorItemKind]) &&
		b.ItemNumber == itemNumber && b.Image == strings.TrimSpace(image) &&
		b.HeadSHA == strings.TrimSpace(repairTask.Spec.Workspace.ExpectedRemoteSHA)
}

// RepositoryRepairValidationCapability is the document stored in the patch
// capability Secret. It carries no repository data and no credentials beyond
// one single-use, download-only artifact capability.
type RepositoryRepairValidationCapability struct {
	Capability    string                       `json:"capability"`
	RequestDigest string                       `json:"requestDigest"`
	Request       RepositoryRepairPatchRequest `json:"request"`
}

// RepositoryRepairPatchRequest mirrors the bound artifact download request.
type RepositoryRepairPatchRequest struct {
	Operation     string `json:"operation"`
	ObjectDigest  string `json:"objectDigest"`
	Namespace     string `json:"namespace"`
	TaskID        string `json:"taskID"`
	ContentLength int64  `json:"contentLength"`
	MediaType     string `json:"mediaType"`
	OperationID   string `json:"operationID"`
}

// BuildRepositoryRepairValidationCapabilitySecret renders the mutable Secret
// that carries the current capability to the validation Pod. The controller
// re-issues the same bound request while the download has not started, so
// the ledger still permits only one download.
func BuildRepositoryRepairValidationCapabilitySecret(validationTask *corev1.ObjectReference, document []byte) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      RepositoryRepairValidationCapabilitySecretName(validationTask.Name),
			Namespace: validationTask.Namespace,
			Labels: map[string]string{
				labels.LabelManaged:   trueStr,
				labels.LabelCreatedBy: repositoryValidationCreatedBy,
				labels.LabelPurpose:   repositoryValidationPurpose,
				labels.LabelTask:      labels.SelectorValue(validationTask.Name),
			},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: corev1alpha1.GroupVersion.String(), Kind: taskKindString,
				Name: validationTask.Name, UID: validationTask.UID,
				Controller: new(true), BlockOwnerDeletion: new(true),
			}},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{RepositoryRepairValidationCapabilitySecretKey: document},
	}
}

// RepositoryRepairValidationResult is the durable result recorded against
// one exact patch digest.
type RepositoryRepairValidationResult struct {
	RepairTaskName     string                           `json:"repairTaskName"`
	RepairTaskUID      string                           `json:"repairTaskUID"`
	ValidationTaskName string                           `json:"validationTaskName"`
	ValidationTaskUID  string                           `json:"validationTaskUID"`
	PatchDigest        string                           `json:"patchDigest"`
	CommandDigest      string                           `json:"commandDigest"`
	Image              string                           `json:"image"`
	Status             RepositoryRepairValidationStatus `json:"status"`
	Evidence           string                           `json:"evidence,omitempty"`
}

func repositoryRepairValidationResultEventID(namespace, validationTaskName string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(namespace) + "\x00" + strings.TrimSpace(validationTaskName) + "\x00result"))
	return "mevt-repair-validation-" + hex.EncodeToString(digest[:16])
}

// RecordRepositoryRepairValidationResult appends the result event for one
// validation Task. A conflicting earlier result fails closed.
func RecordRepositoryRepairValidationResult(ctx context.Context, bindingStore RepositoryValidationBindingStore, binding *store.MonitorEvent, result RepositoryRepairValidationResult) error {
	if bindingStore == nil || binding == nil {
		return fmt.Errorf("repair validation result store is unavailable")
	}
	metadata, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("encode repair validation result: %w", err)
	}
	event := &store.MonitorEvent{
		ID:               repositoryRepairValidationResultEventID(binding.MonitorNamespace, result.ValidationTaskName),
		MonitorNamespace: binding.MonitorNamespace,
		MonitorName:      binding.MonitorName,
		RunID:            binding.RunID,
		ItemKind:         binding.ItemKind,
		ItemNumber:       binding.ItemNumber,
		ItemSHA:          binding.ItemSHA,
		EventType:        repositoryRepairValidationResultEventType,
		Actor:            repositoryValidationBindingActor,
		Summary:          repositoryRepairValidationResultSummary,
		MetadataJSON:     string(metadata),
	}
	createErr := bindingStore.CreateMonitorEvent(ctx, event)
	if createErr == nil {
		return nil
	}
	existing, err := FindRepositoryRepairValidationResult(ctx, bindingStore, binding.MonitorNamespace, result.ValidationTaskName)
	if err != nil {
		return fmt.Errorf("persist repair validation result: %v; verification failed: %w", createErr, err)
	}
	if existing == nil {
		return fmt.Errorf("persist repair validation result: %w", createErr)
	}
	if *existing != result {
		return errRepositoryValidationBindingConflict
	}
	return nil
}

// FindRepositoryRepairValidationResult loads the recorded result for one
// validation Task, or nil when none exists.
func FindRepositoryRepairValidationResult(ctx context.Context, bindingStore RepositoryValidationBindingStore, namespace, validationTaskName string) (*RepositoryRepairValidationResult, error) {
	if bindingStore == nil {
		return nil, fmt.Errorf("repair validation result store is unavailable")
	}
	filter := store.MonitorEventFilter{
		Namespace: strings.TrimSpace(namespace),
		ID:        repositoryRepairValidationResultEventID(namespace, validationTaskName),
		EventType: repositoryRepairValidationResultEventType,
		Limit:     1,
	}
	var found *RepositoryRepairValidationResult
	for {
		events, cursor, err := bindingStore.ListMonitorEvents(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("load repair validation result: %w", err)
		}
		for i := range events {
			if events[i].ID != filter.ID {
				continue
			}
			var result RepositoryRepairValidationResult
			if events[i].Actor != repositoryValidationBindingActor ||
				json.Unmarshal([]byte(events[i].MetadataJSON), &result) != nil ||
				result.ValidationTaskName != strings.TrimSpace(validationTaskName) {
				return nil, errRepositoryValidationBindingConflict
			}
			if found != nil && *found != result {
				return nil, errRepositoryValidationBindingConflict
			}
			found = &result
		}
		if cursor == "" {
			return found, nil
		}
		filter.Cursor = cursor
	}
}

// ValidateRepositoryRepairValidationCapabilitySecret verifies the patch
// capability Secret belongs to the exact validation Task.
func ValidateRepositoryRepairValidationCapabilitySecret(validationTask *corev1alpha1.Task, secret *corev1.Secret) error {
	if validationTask == nil || secret == nil {
		return errRepositoryValidationBindingConflict
	}
	owner := metav1.GetControllerOf(secret)
	if secret.Namespace != validationTask.Namespace ||
		secret.Name != RepositoryRepairValidationCapabilitySecretName(validationTask.Name) ||
		secret.Type != corev1.SecretTypeOpaque || owner == nil ||
		owner.APIVersion != corev1alpha1.GroupVersion.String() || owner.Kind != taskKindString ||
		owner.Name != validationTask.Name || owner.UID != validationTask.UID ||
		len(secret.Data) != 1 || len(secret.Data[RepositoryRepairValidationCapabilitySecretKey]) == 0 ||
		secret.Labels[labels.LabelPurpose] != repositoryValidationPurpose ||
		secret.Labels[labels.LabelTask] != labels.SelectorValue(validationTask.Name) {
		return errRepositoryValidationBindingConflict
	}
	return nil
}

// EnsureRepositoryRepairValidationCapabilitySecret creates the capability
// Secret or replaces its document after verifying ownership.
func EnsureRepositoryRepairValidationCapabilitySecret(ctx context.Context, k8sClient client.Client, validationTask *corev1alpha1.Task, document []byte) error {
	if k8sClient == nil || validationTask == nil || validationTask.UID == "" {
		return fmt.Errorf("repair validation capability client is unavailable")
	}
	expected := BuildRepositoryRepairValidationCapabilitySecret(&corev1.ObjectReference{
		Namespace: validationTask.Namespace, Name: validationTask.Name, UID: validationTask.UID,
	}, document)
	if err := k8sClient.Create(ctx, expected); err == nil {
		return nil
	} else if !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create repair validation capability Secret: %w", err)
	}
	existing := &corev1.Secret{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Namespace: expected.Namespace, Name: expected.Name}, existing); err != nil {
		return fmt.Errorf("load repair validation capability Secret: %w", err)
	}
	if err := ValidateRepositoryRepairValidationCapabilitySecret(validationTask, existing); err != nil {
		return err
	}
	if reflect.DeepEqual(existing.Data, expected.Data) {
		return nil
	}
	existing.Data = expected.Data
	if err := k8sClient.Update(ctx, existing); err != nil {
		return fmt.Errorf("refresh repair validation capability Secret: %w", err)
	}
	return nil
}
