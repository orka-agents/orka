/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"fmt"
	"net/http"
	"reflect"

	admissionv1 "k8s.io/api/admission/v1"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	kindAgentExecutionControl = "AgentExecutionControl"
	kindAgentExecutionPolicy  = "AgentExecutionPolicy"
)

// ControlPolicyValidator restricts desired backend modes and v1 compatibility
// policy specs to configured administrator groups. Controller-owned status
// updates remain available to the exact controller identity through RBAC and
// the status subresource.
type ControlPolicyValidator struct {
	decoder ctrladmission.Decoder
	config  CoexistenceConfig
}

func (v *ControlPolicyValidator) Handle(_ context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.SubResource == statusSubresource {
		return ctrladmission.Allowed("controller-owned subresource write")
	}
	if req.SubResource != "" {
		return ctrladmission.Errored(http.StatusBadRequest, fmt.Errorf("unexpected subresource %q", req.SubResource))
	}
	if req.Operation != admissionv1.Create && req.Operation != admissionv1.Update && req.Operation != admissionv1.Delete {
		return ctrladmission.Allowed("not a backend control or policy spec write")
	}
	if req.Kind.Kind != kindAgentExecutionControl && req.Kind.Kind != kindAgentExecutionPolicy {
		return ctrladmission.Errored(
			http.StatusBadRequest,
			fmt.Errorf("unexpected kind %q for the execution control/policy webhook", req.Kind.Kind),
		)
	}
	if req.Operation == admissionv1.Create {
		if !v.config.admin(req.UserInfo.Groups) {
			return ctrladmission.Denied(req.Kind.Kind + " creation is restricted to configured admin groups")
		}
		return ctrladmission.Allowed("admin-authored execution control or policy")
	}
	if req.Operation == admissionv1.Delete {
		if !v.config.admin(req.UserInfo.Groups) {
			return ctrladmission.Denied(req.Kind.Kind + " deletion is restricted to configured admin groups")
		}
		return ctrladmission.Allowed("admin-authorized execution control or policy deletion")
	}

	changed, err := v.specChanged(req)
	if err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, err)
	}
	if changed && !v.config.admin(req.UserInfo.Groups) {
		return ctrladmission.Denied(req.Kind.Kind + " spec updates are restricted to configured admin groups")
	}
	return ctrladmission.Allowed("no unauthorized execution control or policy spec change")
}

func (v *ControlPolicyValidator) specChanged(req ctrladmission.Request) (bool, error) {
	switch req.Kind.Kind {
	case kindAgentExecutionControl:
		object := &corev1alpha1.AgentExecutionControl{}
		oldObject := &corev1alpha1.AgentExecutionControl{}
		if err := v.decoder.Decode(req, object); err != nil {
			return false, fmt.Errorf("decode AgentExecutionControl: %w", err)
		}
		if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
			return false, fmt.Errorf("decode old AgentExecutionControl: %w", err)
		}
		return !reflect.DeepEqual(oldObject.Spec, object.Spec), nil
	case kindAgentExecutionPolicy:
		object := &corev1alpha1.AgentExecutionPolicy{}
		oldObject := &corev1alpha1.AgentExecutionPolicy{}
		if err := v.decoder.Decode(req, object); err != nil {
			return false, fmt.Errorf("decode AgentExecutionPolicy: %w", err)
		}
		if err := v.decoder.DecodeRaw(req.OldObject, oldObject); err != nil {
			return false, fmt.Errorf("decode old AgentExecutionPolicy: %w", err)
		}
		return !reflect.DeepEqual(oldObject.Spec, object.Spec), nil
	default:
		return false, fmt.Errorf("unexpected kind %q for the execution control/policy webhook", req.Kind.Kind)
	}
}
