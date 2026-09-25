package controller

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// FindSubstrateMCPRecoveryState discovers MCP resources whose persisted
// cleanup finalizers can still own native Actors. Unused resources created
// while Substrate is disabled do not require provider recovery credentials.
func FindSubstrateMCPRecoveryState(ctx context.Context, reader client.Reader, namespace string) (string, error) {
	if reader == nil || strings.TrimSpace(namespace) == "" {
		return "", fmt.Errorf("reader and watch namespace are required to discover substrate MCP recovery state")
	}
	pools := &corev1alpha1.SubstrateActorPoolList{}
	if err := reader.List(ctx, pools, client.InNamespace(strings.TrimSpace(namespace))); err != nil {
		return "", fmt.Errorf("list SubstrateActorPools for disabled substrate recovery: %w", err)
	}
	for i := range pools.Items {
		pool := &pools.Items[i]
		if controllerutil.ContainsFinalizer(pool, substrateActorPoolFinalizer) {
			return fmt.Sprintf("SubstrateActorPool %s/%s", pool.Namespace, pool.Name), nil
		}
	}
	tools := &corev1alpha1.ToolList{}
	if err := reader.List(ctx, tools, client.InNamespace(strings.TrimSpace(namespace))); err != nil {
		return "", fmt.Errorf("list Tools for disabled substrate recovery: %w", err)
	}
	for i := range tools.Items {
		tool := &tools.Items[i]
		// The finalizer survives backend changes; the mutable MCP spec is
		// not evidence that a prior dedicated or pooled Actor was cleaned up.
		if controllerutil.ContainsFinalizer(tool, substrateMCPToolActorFinalizer) {
			return fmt.Sprintf("Tool %s/%s", tool.Namespace, tool.Name), nil
		}
	}
	return "", nil
}

// FindSubstrateRecoveryJournal discovers provider cleanup obligations that
// outlive their source RuntimePools. Inspect labels only: even a damaged
// journal requires recovery configuration rather than silently losing cleanup.
func FindSubstrateRecoveryJournal(ctx context.Context, reader client.Reader, namespace string) (string, error) {
	if reader == nil || strings.TrimSpace(namespace) == "" {
		return "", fmt.Errorf("reader and controller namespace are required to discover substrate recovery journals")
	}
	for _, label := range []string{substrateCatalogLabel, substrateTemplateBindingLabel} {
		list := &corev1.ConfigMapList{}
		if err := reader.List(ctx, list, client.InNamespace(strings.TrimSpace(namespace)), client.MatchingLabels{
			label: substrateOwnedLabelValue,
		}, client.Limit(1)); err != nil {
			return "", fmt.Errorf("list substrate recovery journals: %w", err)
		}
		if len(list.Items) > 0 {
			return fmt.Sprintf("recovery ConfigMap %s/%s", list.Items[0].Namespace, list.Items[0].Name), nil
		}
	}
	return "", nil
}
