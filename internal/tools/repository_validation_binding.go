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
	"errors"
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/store"
)

const (
	repositoryValidationBindingEventType = "validation_command_bound"
	repositoryValidationBindingSummary   = "Repository validation command bound"
	// RepositoryValidationCommandSecretKey is the only data key accepted in a
	// controller-owned repository validation command Secret.
	RepositoryValidationCommandSecretKey = "command"
)

var errRepositoryValidationBindingConflict = errors.New("repository validation command binding conflict")

var errRepositoryValidationBindingMissing = errors.New("repository validation command binding is missing")

// IsRepositoryValidationCommandBindingInvalid reports whether validation
// failed because the durable binding is missing or conflicts with the Task.
// Other errors may be transient storage failures.
func IsRepositoryValidationCommandBindingInvalid(err error) bool {
	return errors.Is(err, errRepositoryValidationBindingMissing) ||
		errors.Is(err, errRepositoryValidationBindingConflict)
}

// RepositoryValidationBindingStore is the least-privilege durable dependency
// used to bind a validation command before its Task is created.
type RepositoryValidationBindingStore interface {
	CreateMonitorEvent(context.Context, *store.MonitorEvent) error
	ListMonitorEvents(context.Context, store.MonitorEventFilter) ([]store.MonitorEvent, string, error)
}

type repositoryValidationCommandBinding struct {
	MonitorUID         string `json:"monitorUID"`
	ReviewTaskName     string `json:"reviewTaskName"`
	ReviewTaskUID      string `json:"reviewTaskUID"`
	ValidationTaskName string `json:"validationTaskName"`
	Image              string `json:"image"`
	HeadSHA            string `json:"headSHA"`
	CommandDigest      string `json:"commandDigest"`
}

// RepositoryValidationCommandBinding is the controller-owned execution
// provenance persisted before a repository validation Task is created.
type RepositoryValidationCommandBinding struct {
	MonitorNamespace   string
	MonitorName        string
	RunID              string
	ItemKind           string
	ItemNumber         int64
	MonitorUID         string
	ReviewTaskName     string
	ReviewTaskUID      string
	ValidationTaskName string
	Image              string
	HeadSHA            string
	CommandDigest      string
}

// FindRepositoryValidationCommandBinding finds the durable binding for one
// deterministic validation Task name. A conflicting duplicate fails closed.
func FindRepositoryValidationCommandBinding(ctx context.Context, bindingStore RepositoryValidationBindingStore, namespace, validationTaskName string) (*RepositoryValidationCommandBinding, error) {
	if bindingStore == nil {
		return nil, fmt.Errorf("repository validation command binding store is unavailable")
	}
	namespace = strings.TrimSpace(namespace)
	validationTaskName = strings.TrimSpace(validationTaskName)
	if namespace == "" || validationTaskName == "" {
		return nil, fmt.Errorf("repository validation task identity is incomplete")
	}

	filter := store.MonitorEventFilter{
		Namespace: namespace,
		ID:        repositoryValidationCommandBindingEventID(namespace, validationTaskName),
		EventType: repositoryValidationBindingEventType,
		Limit:     1,
	}
	var found *RepositoryValidationCommandBinding
	for {
		events, cursor, err := bindingStore.ListMonitorEvents(ctx, filter)
		if err != nil {
			return nil, fmt.Errorf("load repository validation command binding: %w", err)
		}
		for i := range events {
			event := &events[i]
			if event.ID != filter.ID {
				continue
			}
			var binding repositoryValidationCommandBinding
			if json.Unmarshal([]byte(event.MetadataJSON), &binding) != nil || binding.ValidationTaskName != validationTaskName {
				continue
			}
			candidate := &RepositoryValidationCommandBinding{
				MonitorNamespace:   event.MonitorNamespace,
				MonitorName:        event.MonitorName,
				RunID:              event.RunID,
				ItemKind:           event.ItemKind,
				ItemNumber:         event.ItemNumber,
				MonitorUID:         binding.MonitorUID,
				ReviewTaskName:     binding.ReviewTaskName,
				ReviewTaskUID:      binding.ReviewTaskUID,
				ValidationTaskName: binding.ValidationTaskName,
				Image:              binding.Image,
				HeadSHA:            binding.HeadSHA,
				CommandDigest:      binding.CommandDigest,
			}
			if event.Actor != "controller" || event.Summary != repositoryValidationBindingSummary ||
				candidate.MonitorNamespace != namespace || candidate.MonitorName == "" || candidate.MonitorUID == "" ||
				candidate.ReviewTaskName == "" || candidate.ReviewTaskUID == "" || candidate.Image == "" ||
				candidate.HeadSHA == "" || candidate.CommandDigest == "" || event.ItemSHA != candidate.HeadSHA {
				return nil, errRepositoryValidationBindingConflict
			}
			if found != nil && *found != *candidate {
				return nil, errRepositoryValidationBindingConflict
			}
			found = candidate
		}
		if cursor == "" {
			return found, nil
		}
		filter.Cursor = cursor
	}
}

// MatchesReview reports whether a durable binding belongs to the exact review
// Task and RepositoryMonitor that requested it.
func (b *RepositoryValidationCommandBinding) MatchesReview(parent *corev1alpha1.Task, monitor *corev1alpha1.RepositoryMonitor, image, headSHA string) bool {
	if b == nil || parent == nil || monitor == nil {
		return false
	}
	itemNumber, err := strconv.ParseInt(strings.TrimSpace(parent.Annotations[labels.AnnotationMonitorItemNumber]), 10, 64)
	return err == nil && itemNumber > 0 &&
		b.MonitorNamespace == parent.Namespace && b.MonitorNamespace == monitor.Namespace &&
		b.MonitorName == monitor.Name && b.MonitorUID == string(monitor.UID) &&
		b.ReviewTaskName == parent.Name && b.ReviewTaskUID == string(parent.UID) &&
		b.ValidationTaskName == RepositoryValidationTaskName(parent.Name) &&
		b.RunID == strings.TrimSpace(parent.Annotations[labels.AnnotationMonitorRunID]) &&
		b.ItemKind == strings.TrimSpace(parent.Annotations[labels.AnnotationMonitorItemKind]) &&
		b.ItemNumber == itemNumber && b.Image == strings.TrimSpace(image) &&
		b.HeadSHA == strings.TrimSpace(headSHA)
}

// MatchesCommand reports whether command is the value accepted before the
// validation Task was created.
func (b *RepositoryValidationCommandBinding) MatchesCommand(command string) bool {
	return b != nil && b.CommandDigest == repositoryValidationCommandDigest(command)
}

// RepositoryValidationCommandSecretName returns the deterministic Secret name
// used for one repository validation Task. Secret and Task names may match
// because Kubernetes names are scoped by resource kind.
func RepositoryValidationCommandSecretName(validationTaskName string) string {
	return strings.TrimSpace(validationTaskName)
}

// ValidateRepositoryValidationCommandSecret verifies that the immutable Secret
// belongs to the exact review and contains the command bound before Task
// creation. It never returns or embeds the command value in an error.
func ValidateRepositoryValidationCommandSecret(parent, validationTask *corev1alpha1.Task, secret *corev1.Secret, binding *RepositoryValidationCommandBinding) error {
	if parent == nil || validationTask == nil || secret == nil || binding == nil {
		return errRepositoryValidationBindingConflict
	}
	if !repositoryValidationCommandSecretObjectMatches(parent, validationTask, secret) ||
		!repositoryValidationCommandSecretMetadataMatches(parent, validationTask, secret, binding) ||
		!repositoryValidationCommandSecretDataMatches(secret, binding) {
		return errRepositoryValidationBindingConflict
	}
	return nil
}

func repositoryValidationCommandSecretObjectMatches(parent, validationTask *corev1alpha1.Task, secret *corev1.Secret) bool {
	owner := metav1.GetControllerOf(secret)
	return secret.Namespace == validationTask.Namespace &&
		secret.Name == RepositoryValidationCommandSecretName(validationTask.Name) &&
		secret.Type == corev1.SecretTypeOpaque && secret.Immutable != nil && *secret.Immutable &&
		owner != nil && owner.APIVersion == corev1alpha1.GroupVersion.String() && owner.Kind == taskKindString &&
		owner.Name == parent.Name && owner.UID == parent.UID
}

func repositoryValidationCommandSecretMetadataMatches(parent, validationTask *corev1alpha1.Task, secret *corev1.Secret, binding *RepositoryValidationCommandBinding) bool {
	return secret.Labels[labels.LabelManaged] == trueStr &&
		secret.Labels[labels.LabelCreatedBy] == repositoryValidationCreatedBy &&
		secret.Labels[labels.LabelPurpose] == repositoryValidationPurpose &&
		secret.Labels[labels.LabelParentTask] == labels.SelectorValue(parent.Name) &&
		secret.Annotations[labels.AnnotationParentTaskName] == parent.Name &&
		secret.Annotations[labels.AnnotationParentTaskUID] == string(parent.UID) &&
		secret.Annotations[labels.AnnotationRepositoryMonitorName] == validationTask.Annotations[labels.AnnotationRepositoryMonitorName] &&
		secret.Annotations[labels.AnnotationMonitorHeadSHA] == binding.HeadSHA &&
		secret.Annotations[labels.AnnotationRepositoryValidationImage] == binding.Image
}

func repositoryValidationCommandSecretDataMatches(secret *corev1.Secret, binding *RepositoryValidationCommandBinding) bool {
	command := string(secret.Data[RepositoryValidationCommandSecretKey])
	return len(secret.Data) == 1 && command != "" && command == strings.TrimSpace(command) &&
		len(command) <= repositoryValidationMaxCommand && utf8.ValidString(command) && strings.IndexByte(command, 0) < 0 &&
		binding.MatchesCommand(command)
}

// RepositoryValidationCommandBindingEvent returns the append-only event that
// binds a review Task to the originally requested validation command.
func RepositoryValidationCommandBindingEvent(parent *corev1alpha1.Task, monitor *corev1alpha1.RepositoryMonitor, validationTask *corev1alpha1.Task, image, headSHA, command string) (*store.MonitorEvent, error) {
	if parent == nil || monitor == nil || validationTask == nil {
		return nil, fmt.Errorf("review task, repository monitor, and validation task are required")
	}
	image = strings.TrimSpace(image)
	headSHA = strings.TrimSpace(headSHA)
	command = strings.TrimSpace(command)
	runID := strings.TrimSpace(parent.Annotations[labels.AnnotationMonitorRunID])
	itemKind := strings.TrimSpace(parent.Annotations[labels.AnnotationMonitorItemKind])
	itemNumberText := strings.TrimSpace(parent.Annotations[labels.AnnotationMonitorItemNumber])
	itemNumber, err := strconv.ParseInt(itemNumberText, 10, 64)
	if parent.Namespace == "" || parent.Name == "" || parent.UID == "" ||
		monitor.Namespace != parent.Namespace || monitor.Name == "" || monitor.UID == "" ||
		validationTask.Namespace != parent.Namespace || validationTask.Name != RepositoryValidationTaskName(parent.Name) ||
		image == "" || headSHA == "" || command == "" || runID == "" || itemKind == "" || itemNumber <= 0 || err != nil {
		return nil, fmt.Errorf("repository validation command binding identity is incomplete")
	}
	binding := repositoryValidationCommandBinding{
		MonitorUID:         string(monitor.UID),
		ReviewTaskName:     parent.Name,
		ReviewTaskUID:      string(parent.UID),
		ValidationTaskName: validationTask.Name,
		Image:              image,
		HeadSHA:            headSHA,
		CommandDigest:      repositoryValidationCommandDigest(command),
	}
	metadata, err := json.Marshal(binding)
	if err != nil {
		return nil, fmt.Errorf("encode repository validation command binding: %w", err)
	}
	return &store.MonitorEvent{
		ID:               repositoryValidationCommandBindingEventID(parent.Namespace, validationTask.Name),
		MonitorNamespace: monitor.Namespace,
		MonitorName:      monitor.Name,
		RunID:            runID,
		ItemKind:         itemKind,
		ItemNumber:       itemNumber,
		ItemSHA:          headSHA,
		EventType:        repositoryValidationBindingEventType,
		Actor:            "controller",
		Summary:          repositoryValidationBindingSummary,
		MetadataJSON:     string(metadata),
	}, nil
}

// RepositoryValidationCommandBindingFilter returns the narrow lookup used for
// one command-binding event.
func RepositoryValidationCommandBindingFilter(event *store.MonitorEvent) store.MonitorEventFilter {
	if event == nil {
		return store.MonitorEventFilter{Limit: 1}
	}
	return store.MonitorEventFilter{
		Namespace:   event.MonitorNamespace,
		ID:          event.ID,
		MonitorName: event.MonitorName,
		RunID:       event.RunID,
		ItemKind:    event.ItemKind,
		ItemNumber:  event.ItemNumber,
		EventType:   event.EventType,
		Limit:       1,
	}
}

func repositoryValidationCommandBindingEventID(namespace, validationTaskName string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(namespace) + "\x00" + strings.TrimSpace(validationTaskName)))
	return "mevt-validation-" + hex.EncodeToString(digest[:16])
}

// RepositoryValidationCommandBindingMatches reports whether a stored event
// contains the exact controller-generated binding.
func RepositoryValidationCommandBindingMatches(existing, expected *store.MonitorEvent) bool {
	if existing == nil || expected == nil || existing.ID != expected.ID ||
		existing.MonitorNamespace != expected.MonitorNamespace || existing.MonitorName != expected.MonitorName ||
		existing.RunID != expected.RunID || existing.ItemKind != expected.ItemKind || existing.ItemNumber != expected.ItemNumber ||
		existing.ItemSHA != expected.ItemSHA || existing.EventType != expected.EventType || existing.Actor != expected.Actor ||
		existing.Summary != expected.Summary {
		return false
	}
	var existingBinding, expectedBinding repositoryValidationCommandBinding
	if json.Unmarshal([]byte(existing.MetadataJSON), &existingBinding) != nil || json.Unmarshal([]byte(expected.MetadataJSON), &expectedBinding) != nil {
		return false
	}
	return existingBinding == expectedBinding
}

func repositoryValidationCommandDigest(command string) string {
	digest := sha256.Sum256([]byte(strings.TrimSpace(command)))
	return "sha256:" + hex.EncodeToString(digest[:])
}

// ValidateRepositoryValidationCommandBinding verifies that the durable event
// still binds the validation Task to the command accepted by run_validation.
func ValidateRepositoryValidationCommandBinding(ctx context.Context, bindingStore RepositoryValidationBindingStore, expected *store.MonitorEvent) error {
	if bindingStore == nil || expected == nil {
		return errRepositoryValidationBindingMissing
	}
	filter := RepositoryValidationCommandBindingFilter(expected)
	for {
		events, cursor, err := bindingStore.ListMonitorEvents(ctx, filter)
		if err != nil {
			return fmt.Errorf("load repository validation command binding: %w", err)
		}
		for i := range events {
			if events[i].ID != expected.ID {
				continue
			}
			if RepositoryValidationCommandBindingMatches(&events[i], expected) {
				return nil
			}
			return errRepositoryValidationBindingConflict
		}
		if cursor == "" {
			return errRepositoryValidationBindingMissing
		}
		filter.Cursor = cursor
	}
}

func ensureRepositoryValidationCommandBinding(ctx context.Context, bindingStore RepositoryValidationBindingStore, expected *store.MonitorEvent) error {
	if bindingStore == nil || expected == nil {
		return fmt.Errorf("repository validation command binding store is unavailable")
	}
	createErr := bindingStore.CreateMonitorEvent(ctx, expected)
	if createErr == nil {
		return nil
	}
	verifyErr := ValidateRepositoryValidationCommandBinding(ctx, bindingStore, expected)
	if verifyErr == nil || errors.Is(verifyErr, errRepositoryValidationBindingConflict) {
		return verifyErr
	}
	return fmt.Errorf("persist repository validation command binding: %v; verification failed: %w", createErr, verifyErr)
}
