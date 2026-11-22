package controller

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/contexttoken"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/outboundaccess"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

func bindVerifiedTaskTransactionAuthority(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
	readScopes []string,
	enforceCredentialAuth bool,
	executor *workerexecutor.ToolExecutor,
) error {
	if reader == nil || task == nil || executor == nil {
		return errors.New("task transaction authority binding requires a reader, Task, and executor")
	}
	transaction := task.Spec.Transaction
	if transaction == nil {
		executor.SetTransactionCredentialAuthority(false, false, "")
		executor.SetTransactionAuthority("", nil)
		executor.SetTransactionExchangeConfig(&workerexecutor.TransactionExchangeConfig{
			TTS: contexttoken.TTSConfig{TokenSource: contexttoken.TTSTokenSourceNone},
		})
		return nil
	}
	scopes := append([]string(nil), transaction.Scopes...)
	if len(scopes) == 0 {
		scopes = strings.Fields(transaction.Scope)
	}
	if enforceCredentialAuth {
		required := make([]string, 0, len(readScopes))
		for _, scope := range readScopes {
			if scope = strings.TrimSpace(scope); scope != "" {
				required = append(required, scope)
			}
		}
		if len(required) == 0 {
			required = []string{outboundaccess.DefaultCredentialReadScope}
		}
		scopeAllowed := slices.ContainsFunc(scopes, func(scope string) bool {
			return slices.Contains(required, scope)
		})
		executor.SetTransactionCredentialAuthority(true, scopeAllowed, strings.TrimSpace(transaction.Context["secret"]))
	} else {
		executor.SetTransactionCredentialAuthority(false, false, "")
	}
	token, err := readTaskOwnedTransactionToken(ctx, reader, task)
	if err != nil {
		return err
	}
	executor.SetTransactionAuthority(token, scopes)
	return nil
}

func readTaskOwnedTransactionToken(
	ctx context.Context,
	reader client.Reader,
	task *corev1alpha1.Task,
) (string, error) {
	secretName := strings.TrimSpace(task.Annotations[labels.AnnotationTransactionTokenSecret])
	if secretName == "" {
		return "", nil
	}
	secret := &corev1.Secret{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: secretName}, secret); err != nil {
		return "", fmt.Errorf("read task transaction-token Secret: %w", err)
	}
	owned := slices.ContainsFunc(secret.OwnerReferences, func(owner metav1.OwnerReference) bool {
		return owner.APIVersion == corev1alpha1.GroupVersion.String() && owner.Kind == taskResourceKind &&
			owner.Name == task.Name && owner.UID == task.UID
	})
	if !owned {
		return "", errors.New("task transaction-token Secret is not owned by the authenticated Task")
	}
	token := strings.TrimSpace(string(secret.Data["token"]))
	if token == "" {
		return "", errors.New("task transaction-token Secret token is missing or empty")
	}
	return token, nil
}
