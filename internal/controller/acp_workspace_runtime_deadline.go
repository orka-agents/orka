package controller

import (
	"context"
	"fmt"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

// newWorkspaceRuntimeContext applies the concrete workspace's frozen lifetime
// to active prompt execution. Deleting its RuntimePool closes admission but
// waits for controller work to settle; the prompt must therefore enter the
// normal deadline cancellation path before that pool drain can complete.
func (d *ACPDispatcher) newWorkspaceRuntimeContext(ctx context.Context, pool *corev1alpha1.RuntimePool) (context.Context, context.CancelFunc, error) {
	unchanged := func() (context.Context, context.CancelFunc, error) {
		return ctx, func() {}, nil
	}
	if pool == nil || pool.Spec.ExecutionWorkspace == nil {
		return unchanged()
	}
	name := strings.TrimSpace(pool.Labels[acpExecutionWorkspaceLinkLabel])
	if name == "" {
		return unchanged()
	}
	reader := d.APIReader
	if reader == nil {
		reader = d.Client
	}
	workspace := &workspacev1alpha1.ExecutionWorkspace{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: pool.Namespace, Name: name}, workspace); err != nil {
		return nil, nil, fmt.Errorf("read execution workspace lifetime: %w", err)
	}
	if workspace.UID == "" || pool.Annotations[acpExecutionWorkspaceUIDAnnotation] != string(workspace.UID) ||
		workspace.Annotations[acpExecutionWorkspacePoolAnnotation] != pool.Name {
		return nil, nil, fmt.Errorf("execution workspace lifetime does not match the exact RuntimePool binding")
	}
	lifetime := workspace.Spec.Lifecycle.MaxLifetime
	if lifetime == nil {
		return unchanged()
	}
	if lifetime.Duration <= 0 || workspace.CreationTimestamp.IsZero() {
		return nil, nil, fmt.Errorf("execution workspace lifetime is not valid")
	}
	runtimeCtx, cancel := context.WithDeadline(ctx, workspace.CreationTimestamp.Add(lifetime.Duration))
	return runtimeCtx, cancel, nil
}
