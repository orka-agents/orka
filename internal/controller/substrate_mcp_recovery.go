package controller

import (
	"context"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// FindSubstrateMCPRecoveryState names the first Substrate cleanup obligation
// in the watch namespace and reports whether servicing it needs the router
// client identity. SubstrateActorPool finalization is control-only; dedicated
// and pooled MCP Tool finalization builds the router-capable executor.
func FindSubstrateMCPRecoveryState(ctx context.Context, reader client.Reader, namespace string) (state string, routerRequired bool, err error) {
	if reader == nil || strings.TrimSpace(namespace) == "" {
		return "", false, fmt.Errorf("reader and watch namespace are required to discover substrate MCP recovery state")
	}
	tools := &corev1alpha1.ToolList{}
	if err := reader.List(ctx, tools, client.InNamespace(strings.TrimSpace(namespace))); err != nil {
		return "", false, fmt.Errorf("list Tools for disabled substrate recovery: %w", err)
	}
	for i := range tools.Items {
		tool := &tools.Items[i]
		// The finalizer survives backend changes; the mutable MCP spec is
		// not evidence that a prior dedicated or pooled Actor was cleaned up.
		if controllerutil.ContainsFinalizer(tool, substrateMCPToolActorFinalizer) {
			return fmt.Sprintf("Tool %s/%s", tool.Namespace, tool.Name), true, nil
		}
	}
	pools := &corev1alpha1.SubstrateActorPoolList{}
	if err := reader.List(ctx, pools, client.InNamespace(strings.TrimSpace(namespace))); err != nil {
		return "", false, fmt.Errorf("list SubstrateActorPools for disabled substrate recovery: %w", err)
	}
	for i := range pools.Items {
		pool := &pools.Items[i]
		if controllerutil.ContainsFinalizer(pool, substrateActorPoolFinalizer) {
			return fmt.Sprintf("SubstrateActorPool %s/%s", pool.Namespace, pool.Name), false, nil
		}
	}
	return "", false, nil
}
