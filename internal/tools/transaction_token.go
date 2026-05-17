/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	kontxttoken "github.com/aramase/kontxt/pkg/token"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/contexttoken"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/taskmeta"
	"github.com/sozercan/orka/internal/workerenv"
)

// prepareChildTransactionToken exchanges the parent's transaction token for a
// child-scoped TxToken and stores it in a Secret referenced by the child task.
// The Secret is initially owned by the parent task because a newly delegated
// child may not yet have a UID. After the child is created,
// adoptChildTransactionTokenSecret rewrites the owner reference to the child.
func prepareChildTransactionToken(ctx context.Context, k8sClient client.Client, parentTask, childTask *corev1alpha1.Task, operation, agent string) error {
	ttsConfig, enabled, err := childTransactionTokenExchangeConfigForParent(parentTask)
	if err != nil {
		return err
	}
	if !enabled {
		return nil
	}

	if parentTask.UID == "" {
		return fmt.Errorf("parent task UID is required for child transaction token exchange")
	}
	subjectToken, err := childTransactionSubjectToken(ttsConfig.TokenSource)
	if err != nil {
		return err
	}
	scope := strings.TrimSpace(os.Getenv(workerenv.ContextTokenChildScope))
	if scope == "" {
		return fmt.Errorf("%s is required when %s is set for child task tokens", workerenv.ContextTokenChildScope, workerenv.ContextTokenTTSURL)
	}
	if err := validateChildTransactionScope(parentTask, scope); err != nil {
		return err
	}
	subjectTokenType := strings.TrimSpace(os.Getenv(workerenv.ContextTokenSubjectTokenType))
	if subjectTokenType == "" {
		subjectTokenType = kontxttoken.SubjectTokenTypeTxnToken
	}

	requestDetails := map[string]any{
		"operation":  operation,
		"parentTask": parentTask.Name,
		"namespace":  childTask.Namespace,
	}
	if agent != "" {
		requestDetails["agent"] = agent
	}
	if parentTask.Spec.Transaction != nil && parentTask.Spec.Transaction.ID != "" {
		requestDetails["txn"] = parentTask.Spec.Transaction.ID
	}
	ttsClient, err := contexttoken.NewKontxtTTSClient(ttsConfig)
	if err != nil {
		return fmt.Errorf("configuring child transaction token exchange: %w", err)
	}
	token, err := ttsClient.Exchange(ctx, contexttoken.ExchangeRequest{
		SubjectToken:     subjectToken,
		SubjectTokenType: subjectTokenType,
		Scope:            scope,
		RequestedTTL:     ttsConfig.ChildTokenTTL,
		RequestDetails:   requestDetails,
	})
	if err != nil {
		return fmt.Errorf("exchanging child transaction token: %w", err)
	}

	secretName, err := childTransactionTokenSecretName(parentTask.Name)
	if err != nil {
		return err
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            secretName,
			Namespace:       childTask.Namespace,
			OwnerReferences: taskOwnerReference(parentTask),
			Labels: map[string]string{
				labels.LabelParentTask: labels.SelectorValue(parentTask.Name),
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName: parentTask.Name,
			},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"token": []byte(token),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		return fmt.Errorf("creating child transaction token secret: %w", err)
	}
	stampChildTransactionScope(childTask, scope)
	if childTask.Annotations == nil {
		childTask.Annotations = map[string]string{}
	}
	childTask.Annotations[labels.AnnotationTransactionTokenSecret] = secretName
	return nil
}

func childTransactionTokenExchangeConfig() (contexttoken.TTSConfig, bool, error) {
	ttsURL := strings.TrimSpace(os.Getenv(workerenv.ContextTokenTTSURL))
	if ttsURL == "" {
		return contexttoken.TTSConfig{}, false, nil
	}
	ttsConfig, err := contexttoken.NewTTSConfig(
		ttsURL,
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

func childTransactionTokenExchangeConfigForParent(parentTask *corev1alpha1.Task) (contexttoken.TTSConfig, bool, error) {
	ttsConfig, enabled, err := childTransactionTokenExchangeConfig()
	if err != nil {
		return contexttoken.TTSConfig{}, false, err
	}
	if !enabled || parentTask == nil || parentTask.Spec.Transaction == nil {
		return ttsConfig, false, nil
	}
	return ttsConfig, true, nil
}

func shouldPrepareChildTransactionToken(parentTask *corev1alpha1.Task) (bool, error) {
	_, enabled, err := childTransactionTokenExchangeConfigForParent(parentTask)
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
		Kind:       "Task",
		Name:       task.Name,
		UID:        task.UID,
	}}
}

func childOwnerReference(childTask *corev1alpha1.Task) []metav1.OwnerReference {
	return taskOwnerReference(childTask)
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

func adoptChildTransactionTokenSecret(ctx context.Context, k8sClient client.Client, childTask *corev1alpha1.Task) error {
	if childTask == nil || childTask.Annotations == nil {
		return nil
	}
	secretName := strings.TrimSpace(childTask.Annotations[labels.AnnotationTransactionTokenSecret])
	if secretName == "" {
		return nil
	}
	if childTask.UID == "" {
		return fmt.Errorf("child task UID is required to adopt child transaction token secret %q", secretName)
	}
	secret := &corev1.Secret{}
	if err := k8sClient.Get(ctx, client.ObjectKey{Name: secretName, Namespace: childTask.Namespace}, secret); err != nil {
		return fmt.Errorf("getting child transaction token secret for adoption: %w", err)
	}
	secret.OwnerReferences = childOwnerReference(childTask)
	if err := k8sClient.Update(ctx, secret); err != nil {
		return fmt.Errorf("adopting child transaction token secret: %w", err)
	}
	return nil
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

func cleanupChildTransactionTokenSecret(ctx context.Context, k8sClient client.Client, childTask *corev1alpha1.Task) {
	if childTask == nil || childTask.Annotations == nil {
		return
	}
	secretName := strings.TrimSpace(childTask.Annotations[labels.AnnotationTransactionTokenSecret])
	if secretName == "" {
		return
	}
	if err := k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: childTask.Namespace}}); err != nil && !apierrors.IsNotFound(err) {
		log.FromContext(ctx).Error(err, "failed to cleanup child transaction token secret", "secret", secretName, "namespace", childTask.Namespace)
	}
}

func cleanupChildTaskAfterTokenAdoptionFailure(ctx context.Context, k8sClient client.Client, childTask *corev1alpha1.Task) {
	if childTask == nil || childTask.Name == "" {
		cleanupChildTransactionTokenSecret(ctx, k8sClient, childTask)
		return
	}
	err := k8sClient.Delete(ctx, &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: childTask.Name, Namespace: childTask.Namespace}})
	if err != nil && !apierrors.IsNotFound(err) {
		log.FromContext(ctx).Error(err, "failed to cleanup child task after transaction token secret adoption failure", "task", childTask.Name, "namespace", childTask.Namespace)
	}
	cleanupChildTransactionTokenSecret(ctx, k8sClient, childTask)
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
