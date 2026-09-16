/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package main

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"

	"github.com/orka-agents/orka/internal/executionmode"
)

const namespaceModeClaimAttempts = 3

// claimNamespaceMode settles the watched namespace's execution-mode claim
// before any reconciler runs. A namespace that already carries the label must
// name this controller's mode. An unlabeled namespace is claimed by writing
// the label when claim is true; the update carries the namespace's
// resourceVersion, so two controllers racing for one namespace cannot both
// win, and the loser then reads the winner's claim and fails the mode check.
// The ValidatingAdmissionPolicy installed with the release keeps the label
// immutable afterwards, and lets only the controller's own ServiceAccount add
// it to an existing namespace.
func claimNamespaceMode(
	ctx context.Context,
	namespaces typedcorev1.NamespaceInterface,
	name string,
	mode executionmode.Mode,
	claim bool,
) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("a watched namespace is required")
	}
	var lastErr error
	for range namespaceModeClaimAttempts {
		namespace, err := namespaces.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return fmt.Errorf("read controller-mode namespace %q: %w", name, err)
		}
		if strings.TrimSpace(namespace.Labels[executionmode.NamespaceLabel]) != "" || !claim {
			return executionmode.ValidateNamespace(namespace, mode)
		}
		if namespace.Labels == nil {
			namespace.Labels = map[string]string{}
		}
		namespace.Labels[executionmode.NamespaceLabel] = string(mode)
		if _, err := namespaces.Update(ctx, namespace, metav1.UpdateOptions{}); err != nil {
			if apierrors.IsConflict(err) {
				lastErr = err
				continue
			}
			return fmt.Errorf("claim namespace %q for execution mode %q: %w", name, mode, err)
		}
		return nil
	}
	return fmt.Errorf("claim namespace %q for execution mode %q: %w", name, mode, lastErr)
}
