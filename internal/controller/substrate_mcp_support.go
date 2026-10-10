// Copyright (c) 2026. MIT License - see LICENSE file for details.

package controller

import (
	"context"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const substrateActorListenPort int32 = 80

func substrateActorPoolReference(ref *corev1alpha1.SubstrateActorPoolReference, defaultNamespace string) (string, string) {
	if ref == nil {
		return "", ""
	}
	name := strings.TrimSpace(ref.Name)
	if name == "" {
		return "", ""
	}
	namespace := strings.TrimSpace(ref.Namespace)
	if namespace == "" {
		namespace = defaultNamespace
	}
	return name, namespace
}

func resolveSubstrateActorPoolReference(
	ctx context.Context,
	reader client.Reader,
	poolNamespace string,
	poolName string,
	templateNamespace string,
	templateName string,
) (*corev1alpha1.SubstrateActorPool, error) {
	if reader == nil {
		return nil, fmt.Errorf("substrate actor poolRef %q/%q cannot be resolved without a Kubernetes client", poolNamespace, poolName)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	pool := &corev1alpha1.SubstrateActorPool{}
	if err := reader.Get(ctx, types.NamespacedName{Namespace: poolNamespace, Name: poolName}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("substrate actor poolRef %q not found in namespace %q", poolName, poolNamespace)
		}
		return nil, fmt.Errorf("failed to resolve substrate actor poolRef %q in namespace %q: %w", poolName, poolNamespace, err)
	}
	if !pool.DeletionTimestamp.IsZero() {
		return nil, fmt.Errorf("substrate actor poolRef %q in namespace %q is deleting", poolName, poolNamespace)
	}
	if err := validateSubstrateActorPoolTargetActors(poolNamespace, poolName, pool.Spec.TargetActors, false); err != nil {
		return nil, err
	}
	if !controllerutil.ContainsFinalizer(pool, substrateActorPoolFinalizer) {
		updater, ok := reader.(client.Client)
		if !ok {
			return nil, fmt.Errorf("substrate actor poolRef %q in namespace %q cannot persist cleanup finalizer", poolName, poolNamespace)
		}
		patch := client.MergeFrom(pool.DeepCopy())
		controllerutil.AddFinalizer(pool, substrateActorPoolFinalizer)
		if err := updater.Patch(ctx, pool, patch); err != nil {
			return nil, fmt.Errorf("failed to persist cleanup finalizer for substrate actor poolRef %q in namespace %q: %w", poolName, poolNamespace, err)
		}
	}
	poolTemplateNamespace := strings.TrimSpace(pool.Spec.TemplateRef.Namespace)
	if poolTemplateNamespace == "" {
		poolTemplateNamespace = pool.Namespace
	}
	poolTemplateName := strings.TrimSpace(pool.Spec.TemplateRef.Name)
	if poolTemplateNamespace != strings.TrimSpace(templateNamespace) || poolTemplateName != strings.TrimSpace(templateName) {
		return nil, fmt.Errorf(
			"substrate actor poolRef %q in namespace %q uses template %s/%s, want %s/%s",
			poolName,
			poolNamespace,
			poolTemplateNamespace,
			poolTemplateName,
			strings.TrimSpace(templateNamespace),
			strings.TrimSpace(templateName),
		)
	}
	return pool, nil
}
