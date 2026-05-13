/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"fmt"
	"os"
	"slices"
	"strings"
	"time"

	kontxttoken "github.com/aramase/kontxt/pkg/token"
	sdktts "github.com/aramase/kontxt/sdk/tts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
	"github.com/sozercan/orka/internal/metrics"
	"github.com/sozercan/orka/internal/workerenv"
)

func prepareChildTransactionToken(ctx context.Context, k8sClient client.Client, parentTask, childTask *corev1alpha1.Task, operation, agent string) error {
	ttsURL := strings.TrimSpace(os.Getenv(workerenv.ContextTokenTTSURL))
	if ttsURL == "" {
		return nil
	}
	subjectToken, err := readWorkerTokenFile(workerenv.ContextTokenSubjectTokenFile, "context token subject token")
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
	start := time.Now()
	token, err := sdktts.NewClient(ttsURL).Exchange(ctx, &sdktts.ExchangeRequest{
		SubjectToken:     subjectToken,
		SubjectTokenType: subjectTokenType,
		Scope:            scope,
		RequestDetails:   requestDetails,
	})
	if err != nil {
		metrics.RecordContextTokenTTSExchange("failure", "exchange_error", time.Since(start).Seconds())
		return fmt.Errorf("exchanging child transaction token: %w", err)
	}
	metrics.RecordContextTokenTTSExchange("success", "ok", time.Since(start).Seconds())

	secretName := childTransactionTokenSecretName(parentTask.Name)
	if childTask.Annotations == nil {
		childTask.Annotations = map[string]string{}
	}
	childTask.Annotations[labels.AnnotationTransactionTokenSecret] = secretName

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      secretName,
			Namespace: childTask.Namespace,
			Labels: map[string]string{
				labels.LabelParentTask: labels.SelectorValue(parentTask.Name),
			},
			Annotations: map[string]string{
				labels.AnnotationParentTaskName: parentTask.Name,
			},
			OwnerReferences: parentOwnerReference(parentTask),
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"token": []byte(token),
		},
	}
	if err := k8sClient.Create(ctx, secret); err != nil {
		return fmt.Errorf("creating child transaction token secret: %w", err)
	}
	return nil
}

func parentOwnerReference(parentTask *corev1alpha1.Task) []metav1.OwnerReference {
	if parentTask == nil || parentTask.UID == "" {
		return nil
	}
	blockOwnerDeletion := true
	return []metav1.OwnerReference{{
		APIVersion:         corev1alpha1.GroupVersion.String(),
		Kind:               "Task",
		Name:               parentTask.Name,
		UID:                parentTask.UID,
		BlockOwnerDeletion: &blockOwnerDeletion,
	}}
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
		if !containsString(parentScopes, child) {
			return fmt.Errorf("child transaction scope %q is not present in parent transaction scopes", child)
		}
	}
	return nil
}

func containsString(values []string, want string) bool {
	return slices.Contains(values, want)
}

func cleanupChildTransactionTokenSecret(ctx context.Context, k8sClient client.Client, childTask *corev1alpha1.Task) {
	if childTask == nil || childTask.Annotations == nil {
		return
	}
	secretName := strings.TrimSpace(childTask.Annotations[labels.AnnotationTransactionTokenSecret])
	if secretName == "" {
		return
	}
	_ = k8sClient.Delete(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: childTask.Namespace}})
}

func childTransactionTokenSecretName(parentName string) string {
	base := labels.SelectorValue(parentName)
	if len(base) > 40 {
		base = base[:40]
	}
	return fmt.Sprintf("%s-txn-%x", base, time.Now().UnixNano())
}

func readWorkerTokenFile(envName, description string) (string, error) {
	path := strings.TrimSpace(os.Getenv(envName))
	if path == "" {
		return "", fmt.Errorf("%s is required", envName)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("failed to read %s file: %w", description, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("%s file %q is empty", description, path)
	}
	return token, nil
}
