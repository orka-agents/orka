package controller

import (
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	"github.com/orka-agents/orka/internal/store"
)

type acpWorkspaceCredentialBlockedError struct {
	role                    store.PromptCredentialRole
	label                   string
	expectedResourceVersion string
}

func (e *acpWorkspaceCredentialBlockedError) Error() string {
	if e == nil {
		return "frozen workspace credential changed or became unavailable after queue"
	}
	return fmt.Sprintf("frozen %s changed or became unavailable after queue", e.label)
}

func asACPWorkspaceCredentialBlockedError(err error) (*acpWorkspaceCredentialBlockedError, bool) {
	var blocked *acpWorkspaceCredentialBlockedError
	if !errors.As(err, &blocked) {
		return nil, false
	}
	return blocked, true
}

func classifyACPWorkspaceCredentialFreezeError(
	err error,
	role store.PromptCredentialRole,
	label, expectedResourceVersion string,
) error {
	if frozenWorkspaceCredentialUnavailable(err) {
		return &acpWorkspaceCredentialBlockedError{
			role: role, label: label, expectedResourceVersion: expectedResourceVersion,
		}
	}
	return err
}

func frozenWorkspaceCredentialUnavailable(err error) bool {
	if apierrors.IsNotFound(err) {
		return true
	}
	message := err.Error()
	return strings.Contains(message, "credential Secret key ") ||
		(strings.HasPrefix(message, "secret key ") && strings.HasSuffix(message, " is missing or invalid")) ||
		strings.Contains(message, "credential Secret resourceVersion is unavailable")
}

func promptAttemptFreezesCredential(
	attempt *store.PromptAttempt,
	blocked *acpWorkspaceCredentialBlockedError,
) bool {
	if attempt == nil || blocked == nil {
		return false
	}
	for _, binding := range attempt.CredentialBindings {
		if binding.Role != blocked.role {
			continue
		}
		return blocked.expectedResourceVersion != "" && binding.ResourceVersion == blocked.expectedResourceVersion
	}
	return blocked.expectedResourceVersion == ""
}
