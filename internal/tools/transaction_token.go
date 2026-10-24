/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/contexttoken"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/taskmeta"
	"github.com/orka-agents/orka/internal/transactiontoken"
	"github.com/orka-agents/orka/internal/workerenv"
)

// childTransactionTokenPreparation retains renewal authority in memory until a
// generated-name child receives its Kubernetes identity. Completion persists it
// only in a hidden, child-owned authority Secret.
type childTransactionTokenPreparation struct {
	subjectToken     string
	subjectTokenType string
	scope            string
	parentName       string
	parentNamespace  string
	parentUID        string
	placeholderUID   types.UID
}

const childTransactionTokenCleanupTimeout = 5 * time.Second

type childTransactionTokenSettings struct {
	subjectToken     string
	subjectTokenType string
	scope            string
}

// prepareChildTransactionToken validates the delegated scope and prepares an
// empty ownerless workload placeholder referenced by the child Task. The
// placeholder is bound to the exact parent identity so an unrelated Secret is
// never adopted or deleted.
func prepareChildTransactionToken(
	ctx context.Context,
	k8sClient client.Client,
	parentTask, childTask *corev1alpha1.Task,
) (*childTransactionTokenPreparation, error) {
	settings, enabled, err := childTransactionTokenSettingsForParent(parentTask)
	if err != nil || !enabled {
		return nil, err
	}
	return prepareChildTransactionTokenWithSettings(ctx, k8sClient, parentTask, childTask, settings)
}

func prepareChildTransactionTokenWithSettings(
	ctx context.Context,
	k8sClient client.Client,
	parentTask, childTask *corev1alpha1.Task,
	settings childTransactionTokenSettings,
) (*childTransactionTokenPreparation, error) {
	if parentTask == nil || parentTask.UID == "" || strings.TrimSpace(parentTask.Name) == "" ||
		strings.TrimSpace(parentTask.Namespace) == "" {
		return nil, fmt.Errorf("parent task identity is required for child transaction token exchange")
	}
	if childTask == nil || strings.TrimSpace(childTask.Namespace) == "" {
		return nil, fmt.Errorf("child task namespace is required for child transaction token exchange")
	}
	if err := validateChildTransactionScope(parentTask, settings.scope); err != nil {
		return nil, err
	}

	if err := requireSameNamespaceChildTokenExchange(parentTask, childTask); err != nil {
		return nil, err
	}
	secretName, err := childTransactionTokenSecretName("orka-task-token")
	if err != nil {
		return nil, err
	}
	labelsSet, annotations := childTransactionTokenPlaceholderMetadata(parentTask)
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:        secretName,
			Namespace:   childTask.Namespace,
			Labels:      labelsSet,
			Annotations: annotations,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		return nil, fmt.Errorf("creating child transaction token placeholder: %w", err)
	}
	if secret.UID == "" {
		_ = k8sClient.Delete(ctx, secret)
		return nil, errors.New("created child transaction token placeholder is missing its UID")
	}

	stampChildTransactionScope(childTask, settings.scope)
	if childTask.Annotations == nil {
		childTask.Annotations = map[string]string{}
	}
	childTask.Annotations[labels.AnnotationTransactionTokenSecret] = secretName
	childTask.Annotations[transactiontoken.ParentUIDAnnotation] = string(parentTask.UID)
	childTask.Annotations[transactiontoken.ParentNamespaceAnnotation] = parentTask.Namespace
	childTask.Annotations[transactiontoken.PlaceholderUIDAnnotation] = string(secret.UID)

	return &childTransactionTokenPreparation{
		subjectToken:     settings.subjectToken,
		subjectTokenType: settings.subjectTokenType,
		scope:            settings.scope,
		parentName:       parentTask.Name,
		parentNamespace:  parentTask.Namespace,
		parentUID:        string(parentTask.UID),
		placeholderUID:   secret.UID,
	}, nil
}

// completeChildTransactionToken validates and adopts the workload placeholder
// to the child and creates the hidden child-owned renewal authority. The
// pending annotation stays set until the Task controller exchanges and persists
// a renewable child token.
func completeChildTransactionToken(
	ctx context.Context,
	k8sClient client.Client,
	childTask *corev1alpha1.Task,
	preparation *childTransactionTokenPreparation,
) error {
	if preparation == nil {
		return nil
	}
	if childTask == nil || strings.TrimSpace(childTask.Name) == "" || childTask.UID == "" {
		return fmt.Errorf("child task identity is required for renewable transaction token setup")
	}
	workloadName := strings.TrimSpace(childTask.Annotations[labels.AnnotationTransactionTokenSecret])
	if workloadName == "" {
		return errors.New("child transaction token workload Secret reference is required")
	}
	if err := transactiontoken.ValidateSubjectTokenType(preparation.subjectTokenType); err != nil {
		return err
	}
	workload := &corev1.Secret{}
	if err := childTransactionTokenReader(ctx, k8sClient).Get(ctx, client.ObjectKey{Name: workloadName, Namespace: childTask.Namespace}, workload); err != nil {
		return fmt.Errorf("getting child transaction token placeholder: %w", err)
	}
	if !validChildTransactionTokenPlaceholder(workload, preparation) {
		return errors.New("child transaction token placeholder identity is invalid")
	}

	requestDetails, err := json.Marshal(childTransactionTokenRequestDetails(childTask, preparation))
	if err != nil {
		return fmt.Errorf("encoding child transaction token request details: %w", err)
	}
	taskUIDLabel := labels.SelectorValue(string(childTask.UID))
	authorityName, err := childTransactionTokenSecretName("orka-task-authority")
	if err != nil {
		return err
	}
	authority := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: authorityName, Namespace: childTask.Namespace,
			Labels: map[string]string{
				labels.LabelPurpose: transactiontoken.AuthoritySecretPurpose,
				labels.LabelTaskUID: taskUIDLabel,
			},
			OwnerReferences: childOwnerReference(childTask),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			transactiontoken.SubjectSecretKey:          []byte(preparation.subjectToken),
			transactiontoken.SubjectTokenTypeSecretKey: []byte(preparation.subjectTokenType),
			transactiontoken.RequestDetailsSecretKey:   requestDetails,
		},
	}
	if err := k8sClient.Create(ctx, authority); err != nil {
		return fmt.Errorf("creating child transaction renewal authority: %w", err)
	}
	workload.OwnerReferences = childOwnerReference(childTask)
	workload.Labels = map[string]string{
		labels.LabelPurpose: transactiontoken.WorkloadSecretPurpose,
		labels.LabelTaskUID: labels.SelectorValue(string(childTask.UID)),
	}
	workload.Annotations = nil
	workload.Data = map[string][]byte{}
	if err := k8sClient.Update(ctx, workload); err != nil {
		_ = k8sClient.Delete(ctx, authority)
		return fmt.Errorf("adopting child transaction token workload Secret: %w", err)
	}
	return nil
}

func childTransactionTokenSettingsForParent(parentTask *corev1alpha1.Task) (childTransactionTokenSettings, bool, error) {
	if parentTask == nil || parentTask.Spec.Transaction == nil {
		return childTransactionTokenSettings{}, false, nil
	}
	config, enabled, err := childTransactionTokenExchangeConfig()
	if err != nil || !enabled {
		return childTransactionTokenSettings{}, enabled, err
	}
	subjectToken, err := childTransactionSubjectToken(config.TokenSource)
	if err != nil {
		return childTransactionTokenSettings{}, false, err
	}
	subjectTokenType := strings.TrimSpace(os.Getenv(workerenv.ContextTokenSubjectTokenType))
	if subjectTokenType == "" {
		subjectTokenType = contexttoken.SubjectTokenTypeForSource(config.TokenSource)
	}
	if err := transactiontoken.ValidateSubjectTokenType(subjectTokenType); err != nil {
		return childTransactionTokenSettings{}, false, err
	}
	scope := strings.TrimSpace(os.Getenv(workerenv.ContextTokenChildScope))
	if scope == "" {
		return childTransactionTokenSettings{}, false, fmt.Errorf("%s is required when %s is set for child task tokens", workerenv.ContextTokenChildScope, workerenv.ContextTokenTTSEndpoint)
	}
	return childTransactionTokenSettings{
		subjectToken: subjectToken, subjectTokenType: subjectTokenType, scope: scope,
	}, true, nil
}

func childTransactionTokenExchangeConfig() (contexttoken.TTSConfig, bool, error) {
	ttsEndpoint := strings.TrimSpace(os.Getenv(workerenv.ContextTokenTTSEndpoint))
	if ttsEndpoint == "" {
		return contexttoken.TTSConfig{}, false, nil
	}
	ttsConfig, err := contexttoken.NewTTSConfig(
		ttsEndpoint,
		os.Getenv(workerenv.ContextTokenTTSAudience),
		os.Getenv(workerenv.ContextTokenTTSTimeout),
		os.Getenv(workerenv.ContextTokenTTSTokenSource),
		os.Getenv(workerenv.ContextTokenChildTokenTTL),
		"",
	)
	if err != nil {
		return contexttoken.TTSConfig{}, false, fmt.Errorf("configuring child transaction token exchange: %w", err)
	}
	return ttsConfig, ttsConfig.Enabled(), nil
}

func shouldPrepareChildTransactionToken(parentTask *corev1alpha1.Task) (bool, error) {
	_, enabled, err := childTransactionTokenSettingsForParent(parentTask)
	return enabled, err
}

func childTransactionSubjectToken(tokenSource string) (string, error) {
	switch tokenSource {
	case contexttoken.TTSTokenSourceIncoming:
		if token, ok, err := workerenv.ReadTokenFileEnv(workerenv.ContextTokenSubjectTokenFile, "context token subject token"); ok || err != nil {
			return token, err
		}
		return workerenv.RequireTokenFileEnv(workerenv.TransactionTokenFile, "transaction token")
	case contexttoken.TTSTokenSourceServiceAccount:
		return serviceAccountSubjectToken()
	case contexttoken.TTSTokenSourceNone:
		return "", fmt.Errorf("context token TTS token source %q does not provide a subject token", tokenSource)
	default:
		return "", fmt.Errorf("unsupported context token TTS token source %q", tokenSource)
	}
}

func serviceAccountSubjectToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv(workerenv.ServiceAccountToken)); token != "" {
		return token, nil
	}
	return workerenv.ReadTokenFile(workerenv.ServiceAccountTokenFile, "service account token")
}

func taskOwnerReference(task *corev1alpha1.Task) []metav1.OwnerReference {
	if task == nil || task.UID == "" {
		return nil
	}
	return []metav1.OwnerReference{{
		APIVersion: corev1alpha1.GroupVersion.String(),
		Kind:       taskKindString,
		Name:       task.Name,
		UID:        task.UID,
	}}
}

func childOwnerReference(childTask *corev1alpha1.Task) []metav1.OwnerReference {
	return taskOwnerReference(childTask)
}

// requireSameNamespaceChildTokenExchange fails child transaction token
// exchange closed for cross-namespace delegation, before any placeholder or
// renewal authority is created.
func requireSameNamespaceChildTokenExchange(parentTask, childTask *corev1alpha1.Task) error {
	if parentTask == nil || childTask == nil || parentTask.Namespace == childTask.Namespace {
		return nil
	}
	return fmt.Errorf(
		"child transaction token exchange requires the child Task namespace %q to match the parent namespace %q; cross-namespace delegation is not supported while child token exchange is enabled",
		childTask.Namespace, parentTask.Namespace,
	)
}

func stampChildTransactionScope(childTask *corev1alpha1.Task, scope string) {
	if childTask == nil || childTask.Spec.Transaction == nil {
		return
	}
	scope = strings.TrimSpace(scope)
	childTask.Spec.Transaction.Scope = scope
	childTask.Spec.Transaction.Scopes = strings.Fields(scope)
	taskmeta.ApplyTransactionMetadata(&childTask.ObjectMeta, childTask.Spec.Transaction)
}

func childTransactionTokenPlaceholderMetadata(parentTask *corev1alpha1.Task) (map[string]string, map[string]string) {
	return map[string]string{
		labels.LabelPurpose:    transactiontoken.PlaceholderSecretPurpose,
		labels.LabelParentTask: labels.SelectorValue(parentTask.Name),
		labels.LabelTaskUID:    labels.SelectorValue(string(parentTask.UID)),
	}, map[string]string{
		labels.AnnotationParentTaskName:            parentTask.Name,
		transactiontoken.ParentUIDAnnotation:       string(parentTask.UID),
		transactiontoken.ParentNamespaceAnnotation: parentTask.Namespace,
	}
}

func validChildTransactionTokenPlaceholder(secret *corev1.Secret, preparation *childTransactionTokenPreparation) bool {
	if secret == nil || preparation == nil || preparation.placeholderUID == "" || secret.UID != preparation.placeholderUID ||
		secret.Type != corev1.SecretTypeOpaque || len(secret.OwnerReferences) != 0 || len(secret.Data) != 0 {
		return false
	}
	parent := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{
		Name: preparation.parentName, Namespace: preparation.parentNamespace, UID: types.UID(preparation.parentUID),
	}}
	expectedLabels, expectedAnnotations := childTransactionTokenPlaceholderMetadata(parent)
	return maps.Equal(secret.Labels, expectedLabels) && maps.Equal(secret.Annotations, expectedAnnotations)
}

func childTransactionTokenRequestDetails(
	childTask *corev1alpha1.Task,
	preparation *childTransactionTokenPreparation,
) map[string]any {
	operation := "delegateTask"
	if childTask.Spec.Type == corev1alpha1.TaskTypeContainer {
		operation = "createContainerTask"
	}
	details := map[string]any{
		"operation":        operation,
		"namespace":        childTask.Namespace,
		taskNameCamelField: childTask.Name,
		"taskUID":          string(childTask.UID),
		"parentTask":       preparation.parentName,
	}
	if childTask.Spec.Transaction != nil {
		if transactionID := strings.TrimSpace(childTask.Spec.Transaction.ID); transactionID != "" {
			details["txn"] = transactionID
		}
	}
	if childTask.Spec.AgentRef != nil {
		if agent := strings.TrimSpace(childTask.Spec.AgentRef.Name); agent != "" {
			details["agent"] = agent
		}
	}
	return details
}

func validateChildTransactionScope(parentTask *corev1alpha1.Task, childScope string) error {
	childScopes := strings.Fields(childScope)
	if len(childScopes) == 0 {
		return fmt.Errorf("child transaction scope is required")
	}
	if parentTask == nil || parentTask.Spec.Transaction == nil {
		return fmt.Errorf("parent transaction metadata is required for child token exchange")
	}
	parentScopes := parentTask.Spec.Transaction.Scopes
	if len(parentScopes) == 0 {
		parentScopes = strings.Fields(parentTask.Spec.Transaction.Scope)
	}
	if len(parentScopes) == 0 {
		return fmt.Errorf("parent transaction scopes are required for child token exchange")
	}
	for _, child := range childScopes {
		if !slices.Contains(parentScopes, child) {
			return fmt.Errorf("child transaction scope %q is not present in parent transaction scopes", child)
		}
	}
	return nil
}

func childTransactionTokenReader(ctx context.Context, fallback client.Reader) client.Reader {
	if toolContext := GetToolContext(ctx); toolContext != nil && toolContext.PolicyReader != nil {
		return toolContext.PolicyReader
	}
	return fallback
}

func cleanupChildTransactionTokenSecret(
	ctx context.Context,
	k8sClient client.Client,
	childTask *corev1alpha1.Task,
	preparation *childTransactionTokenPreparation,
) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), childTransactionTokenCleanupTimeout)
	defer cancel()
	cleanupChildTransactionTokenSecretWithContext(cleanupCtx, k8sClient, childTask, preparation)
}

func cleanupChildTransactionTokenSecretWithContext(
	ctx context.Context,
	k8sClient client.Client,
	childTask *corev1alpha1.Task,
	preparation *childTransactionTokenPreparation,
) {
	if childTask == nil || childTask.Annotations == nil || preparation == nil {
		return
	}
	secretName := strings.TrimSpace(childTask.Annotations[labels.AnnotationTransactionTokenSecret])
	if secretName == "" {
		return
	}
	secret := &corev1.Secret{}
	if err := childTransactionTokenReader(ctx, k8sClient).Get(ctx, client.ObjectKey{Name: secretName, Namespace: childTask.Namespace}, secret); err != nil {
		if !apierrors.IsNotFound(err) {
			log.FromContext(ctx).Error(err, "failed to read child transaction token Secret for cleanup", "secret", secretName, "namespace", childTask.Namespace)
		}
		return
	}
	// Once the Secret is adopted, its owner reference is the authoritative
	// cleanup mechanism. Delete only the exact ownerless placeholder. UID and
	// resource-version preconditions close the read/delete race with adoption.
	if !validChildTransactionTokenPlaceholder(secret, preparation) {
		log.FromContext(ctx).Info("refusing to cleanup child transaction token Secret with mismatched identity", "secret", secretName, "namespace", childTask.Namespace)
		return
	}
	if secret.ResourceVersion == "" {
		log.FromContext(ctx).Info("refusing to cleanup child transaction token placeholder without resource version", "secret", secretName, "namespace", childTask.Namespace)
		return
	}
	uid := secret.UID
	resourceVersion := secret.ResourceVersion
	preconditions := client.Preconditions{UID: &uid, ResourceVersion: &resourceVersion}
	if err := k8sClient.Delete(ctx, secret, preconditions); err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		log.FromContext(ctx).Error(err, "failed to cleanup child transaction token Secret", "secret", secretName, "namespace", childTask.Namespace)
	}
}

func cleanupChildTaskAfterTokenAdoptionFailure(
	ctx context.Context,
	k8sClient client.Client,
	childTask *corev1alpha1.Task,
	preparation *childTransactionTokenPreparation,
) {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), childTransactionTokenCleanupTimeout)
	defer cancel()
	if childTask == nil || childTask.Name == "" {
		cleanupChildTransactionTokenSecretWithContext(cleanupCtx, k8sClient, childTask, preparation)
		return
	}
	if childTask.UID == "" {
		log.FromContext(cleanupCtx).Info("refusing to cleanup child task without UID", "task", childTask.Name, "namespace", childTask.Namespace)
		cleanupChildTransactionTokenSecretWithContext(cleanupCtx, k8sClient, childTask, preparation)
		return
	}
	uid := childTask.UID
	err := k8sClient.Delete(cleanupCtx, &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: childTask.Name, Namespace: childTask.Namespace}}, client.Preconditions{UID: &uid})
	if err != nil && !apierrors.IsNotFound(err) && !apierrors.IsConflict(err) {
		log.FromContext(cleanupCtx).Error(err, "failed to cleanup child task after transaction token secret adoption failure", "task", childTask.Name, "namespace", childTask.Namespace)
	}
	cleanupChildTransactionTokenSecretWithContext(cleanupCtx, k8sClient, childTask, preparation)
}

func childTransactionTokenSecretName(parentName string) (string, error) {
	timestamp := fmt.Sprintf("%x", time.Now().UnixNano())
	randomBytes := make([]byte, 5)
	if _, err := rand.Read(randomBytes); err != nil {
		return "", fmt.Errorf("generating child transaction token secret suffix: %w", err)
	}
	suffix := fmt.Sprintf("txn-%s-%s", timestamp, hex.EncodeToString(randomBytes))
	base := dnsLabelPrefix(parentName)
	maxBaseLen := 63 - len(suffix) - 1
	if maxBaseLen < 1 {
		return "", fmt.Errorf("child transaction token secret suffix exceeds DNS label length")
	}
	if len(base) > maxBaseLen {
		base = strings.Trim(base[:maxBaseLen], "-")
	}
	if base == "" {
		base = "task"
	}
	return base + "-" + suffix, nil
}

func dnsLabelPrefix(value string) string {
	value = strings.ToLower(strings.TrimSpace(value))
	var out strings.Builder
	out.Grow(len(value))
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			out.WriteRune(r)
		case r == '-':
			out.WriteRune(r)
		default:
			out.WriteRune('-')
		}
	}
	result := strings.Trim(out.String(), "-")
	if result == "" {
		return "task"
	}
	return result
}
