/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package memorybackend contains startup guards for the MemoryBackend control plane.
package memorybackend

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// ErrPrerequisitesUnavailable marks an absent or schema-incompatible CRD that
// may be intentionally staged before the controller binary during an upgrade.
var ErrPrerequisitesUnavailable = errors.New("memory backend prerequisites unavailable")

const (
	CRDName    = "memorybackends.core.orka.ai"
	CRDGroup   = "core.orka.ai"
	CRDVersion = "v1alpha1"

	fixedNameCELRule         = "self.metadata.name == 'default'"
	protocolImmutableCELRule = "self.protocol == oldSelf.protocol"
	storeImmutableCELRule    = "self.store.name == oldSelf.store.name"
)

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=memorybackends.core.orka.ai,verbs=get

// RequirePrerequisites verifies that the installed MemoryBackend CRD is the
// served/storage schema expected by this controller binary.
func RequirePrerequisites(ctx context.Context, reader client.Reader) error {
	if reader == nil {
		return fmt.Errorf("kubernetes API reader is required")
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := reader.Get(ctx, client.ObjectKey{Name: CRDName}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: MemoryBackend CRD %s is not installed", ErrPrerequisitesUnavailable, CRDName)
		}
		return fmt.Errorf("read MemoryBackend CRD %s: %w", CRDName, err)
	}
	if crd.Spec.Group != CRDGroup || crd.Spec.Names.Kind != "MemoryBackend" ||
		crd.Spec.Names.Plural != "memorybackends" || crd.Spec.Scope != apiextensionsv1.NamespaceScoped {
		return fmt.Errorf("%w: MemoryBackend CRD has an unexpected group, names, or scope", ErrPrerequisitesUnavailable)
	}
	if crd.Annotations[corev1alpha1.MemoryBackendSchemaAnnotation] != corev1alpha1.MemoryBackendSchemaVersion {
		return fmt.Errorf(
			"%w: MemoryBackend CRD is missing %s=%s",
			ErrPrerequisitesUnavailable,
			corev1alpha1.MemoryBackendSchemaAnnotation,
			corev1alpha1.MemoryBackendSchemaVersion,
		)
	}
	if !crdEstablished(crd) {
		return fmt.Errorf("%w: MemoryBackend CRD is not Established", ErrPrerequisitesUnavailable)
	}

	version := slices.IndexFunc(crd.Spec.Versions, func(candidate apiextensionsv1.CustomResourceDefinitionVersion) bool {
		return candidate.Name == CRDVersion
	})
	if version < 0 || !crd.Spec.Versions[version].Served || !crd.Spec.Versions[version].Storage {
		return fmt.Errorf("%w: MemoryBackend CRD must serve and store %s", ErrPrerequisitesUnavailable, CRDVersion)
	}
	contract := crd.Spec.Versions[version]
	if contract.Subresources == nil || contract.Subresources.Status == nil {
		return fmt.Errorf("%w: MemoryBackend CRD %s is missing the status subresource", ErrPrerequisitesUnavailable, CRDVersion)
	}
	if contract.Schema == nil || contract.Schema.OpenAPIV3Schema == nil {
		return fmt.Errorf("%w: MemoryBackend CRD %s is missing its OpenAPI schema", ErrPrerequisitesUnavailable, CRDVersion)
	}
	rootSchema := contract.Schema.OpenAPIV3Schema
	if !containsCELRule(rootSchema.XValidations, fixedNameCELRule) {
		return fmt.Errorf("%w: MemoryBackend CRD is missing the fixed-name CEL rule", ErrPrerequisitesUnavailable)
	}
	specSchema, found := rootSchema.Properties["spec"]
	if !found || !containsCELRule(specSchema.XValidations, protocolImmutableCELRule) ||
		!containsCELRule(specSchema.XValidations, storeImmutableCELRule) {
		return fmt.Errorf("%w: MemoryBackend CRD is missing immutable protocol/store CEL rules", ErrPrerequisitesUnavailable)
	}
	return nil
}

// WaitForPrerequisites retries staging races and API failures until ctx expires.
func WaitForPrerequisites(ctx context.Context, reader client.Reader, interval time.Duration) error {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	var lastErr error
	for {
		if err := RequirePrerequisites(ctx, reader); err == nil {
			return nil
		} else {
			lastErr = err
		}
		timer := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return lastErr
		case <-timer.C:
		}
	}
}

// PrerequisiteErrorIsTransient reports whether an error is an operational/API
// failure rather than a missing or incompatible staged CRD.
func PrerequisiteErrorIsTransient(err error) bool {
	if err == nil {
		return false
	}
	type multiUnwrapper interface {
		Unwrap() []error
	}
	if multi, ok := err.(multiUnwrapper); ok {
		return slices.ContainsFunc(multi.Unwrap(), PrerequisiteErrorIsTransient)
	}
	return !errors.Is(err, ErrPrerequisitesUnavailable)
}

func containsCELRule(rules []apiextensionsv1.ValidationRule, wanted string) bool {
	return slices.ContainsFunc(rules, func(rule apiextensionsv1.ValidationRule) bool {
		return rule.Rule == wanted
	})
}

func crdEstablished(crd *apiextensionsv1.CustomResourceDefinition) bool {
	return crd != nil && slices.ContainsFunc(crd.Status.Conditions, func(condition apiextensionsv1.CustomResourceDefinitionCondition) bool {
		return condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue
	})
}
