/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	kontxttoken "github.com/aramase/kontxt/pkg/token"
	sdktts "github.com/aramase/kontxt/sdk/tts"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/labels"
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
	token, err := sdktts.NewClient(ttsURL).Exchange(ctx, &sdktts.ExchangeRequest{
		SubjectToken:     subjectToken,
		SubjectTokenType: subjectTokenType,
		Scope:            scope,
		RequestDetails:   requestDetails,
	})
	if err != nil {
		return fmt.Errorf("exchanging child transaction token: %w", err)
	}

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
