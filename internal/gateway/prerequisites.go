/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package gateway

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"time"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ErrGatewayPrerequisitesUnavailable marks missing or incompatible CRDs that may be
// intentionally staged during an upgrade. Other errors are transient/operational.
var ErrGatewayPrerequisitesUnavailable = errors.New("gateway prerequisites unavailable")

const (
	TaskCRDName                       = "tasks.core.orka.ai"
	GatewayClassCRDName               = "gatewayclasses.gateway.orka.ai"
	GatewayCRDName                    = "gateways.gateway.orka.ai"
	GatewayBindingCRDName             = "gatewaybindings.gateway.orka.ai"
	GatewayCRDGroup                   = "gateway.orka.ai"
	GatewayCRDVersion                 = "v1alpha1"
	TaskSessionCutoffSchemaAnnotation = "gateway.orka.ai/session-cutoff-schema"
	TaskSessionCutoffSchemaVersion    = "v1"
)

// +kubebuilder:rbac:groups=apiextensions.k8s.io,resources=customresourcedefinitions,resourceNames=tasks.core.orka.ai;gatewayclasses.gateway.orka.ai;gateways.gateway.orka.ai;gatewaybindings.gateway.orka.ai,verbs=get

// RequireGatewayPrerequisites verifies every CRD contract needed before gateway controllers start.
func RequireGatewayPrerequisites(ctx context.Context, reader client.Reader) error {
	if reader == nil {
		return fmt.Errorf("kubernetes API reader is required")
	}
	var errs []error
	if err := RequireTaskSessionCutoffSchema(ctx, reader); err != nil {
		errs = append(errs, err)
	}
	for _, requirement := range []struct {
		name  string
		kind  string
		scope apiextensionsv1.ResourceScope
	}{
		{name: GatewayClassCRDName, kind: "GatewayClass", scope: apiextensionsv1.ClusterScoped},
		{name: GatewayCRDName, kind: "Gateway", scope: apiextensionsv1.NamespaceScoped},
		{name: GatewayBindingCRDName, kind: "GatewayBinding", scope: apiextensionsv1.NamespaceScoped},
	} {
		if err := requireServedGatewayCRD(ctx, reader, requirement.name, requirement.kind, requirement.scope); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RequireTaskSessionCutoffSchema verifies the installed Task CRD preserves gateway transcript policy fields.
func RequireTaskSessionCutoffSchema(ctx context.Context, reader client.Reader) error {
	if reader == nil {
		return fmt.Errorf("kubernetes API reader is required")
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := reader.Get(ctx, client.ObjectKey{Name: TaskCRDName}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: task CRD %s is not installed", ErrGatewayPrerequisitesUnavailable, TaskCRDName)
		}
		return fmt.Errorf("read Task CRD %s: %w", TaskCRDName, err)
	}
	if crd.Annotations[TaskSessionCutoffSchemaAnnotation] != TaskSessionCutoffSchemaVersion {
		return fmt.Errorf(
			"%w: task CRD %s is missing %s=%s",
			ErrGatewayPrerequisitesUnavailable,
			TaskCRDName,
			TaskSessionCutoffSchemaAnnotation,
			TaskSessionCutoffSchemaVersion,
		)
	}
	if !crdEstablished(crd) {
		return fmt.Errorf("%w: task CRD %s is not Established", ErrGatewayPrerequisitesUnavailable, TaskCRDName)
	}
	return nil
}

func requireServedGatewayCRD(
	ctx context.Context, reader client.Reader, name, kind string, scope apiextensionsv1.ResourceScope,
) error {
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := reader.Get(ctx, client.ObjectKey{Name: name}, crd); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("%w: gateway CRD %s is not installed", ErrGatewayPrerequisitesUnavailable, name)
		}
		return fmt.Errorf("read Gateway CRD %s: %w", name, err)
	}
	if crd.Spec.Group != GatewayCRDGroup || crd.Spec.Names.Kind != kind || crd.Spec.Scope != scope {
		return fmt.Errorf("%w: gateway CRD %s has an unexpected group, kind, or scope", ErrGatewayPrerequisitesUnavailable, name)
	}
	served := slices.ContainsFunc(crd.Spec.Versions, func(version apiextensionsv1.CustomResourceDefinitionVersion) bool {
		return version.Name == GatewayCRDVersion && version.Served
	})
	if !served {
		return fmt.Errorf("%w: gateway CRD %s does not serve %s", ErrGatewayPrerequisitesUnavailable, name, GatewayCRDVersion)
	}
	if !crdEstablished(crd) {
		return fmt.Errorf("%w: gateway CRD %s is not Established", ErrGatewayPrerequisitesUnavailable, name)
	}
	return nil
}

func crdEstablished(crd *apiextensionsv1.CustomResourceDefinition) bool {
	return crd != nil && slices.ContainsFunc(crd.Status.Conditions, func(condition apiextensionsv1.CustomResourceDefinitionCondition) bool {
		return condition.Type == apiextensionsv1.Established && condition.Status == apiextensionsv1.ConditionTrue
	})
}

// WaitForGatewayPrerequisites retries transient and staging races until ctx expires.
func WaitForGatewayPrerequisites(
	ctx context.Context, reader client.Reader, interval time.Duration,
) error {
	if interval <= 0 {
		interval = 500 * time.Millisecond
	}
	var lastErr error
	for {
		if err := RequireGatewayPrerequisites(ctx, reader); err == nil {
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

// GatewayPrerequisiteErrorIsTransient reports whether any leaf error came from
// the Kubernetes API or local configuration rather than a missing/stale CRD.
func GatewayPrerequisiteErrorIsTransient(err error) bool {
	if err == nil {
		return false
	}
	type multiUnwrapper interface {
		Unwrap() []error
	}
	if multi, ok := err.(multiUnwrapper); ok {
		return slices.ContainsFunc(multi.Unwrap(), GatewayPrerequisiteErrorIsTransient)
	}
	return !errors.Is(err, ErrGatewayPrerequisitesUnavailable)
}
