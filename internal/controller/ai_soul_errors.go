package controller

import (
	"errors"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
)

type permanentAISoulConfigurationError struct{ err error }

func (e *permanentAISoulConfigurationError) Error() string { return e.err.Error() }
func (e *permanentAISoulConfigurationError) Unwrap() error { return e.err }

func invalidAISoulConfiguration(format string, args ...any) error {
	return &permanentAISoulConfigurationError{err: fmt.Errorf(format, args...)}
}

// Only invalid declarations or pinned-identity mismatches terminalize a Task.
// Reader, Session-store, and status-write failures remain reconciliation errors.
func isPermanentAISoulConfigurationError(err error) bool {
	var permanent *permanentAISoulConfigurationError
	return errors.As(err, &permanent) || agentcontext.IsInvalidSource(err) ||
		isPermanentACPAgentConfigurationError(err) || errors.Is(err, store.ErrSessionConfigurationMismatch)
}

func aiSoulTaskChanged(task *corev1alpha1.Task) error {
	return apierrors.NewConflict(corev1alpha1.GroupVersion.WithResource("tasks").GroupResource(), task.Name,
		errors.New("task changed before AI soul preparation; reconcile the current task"))
}
