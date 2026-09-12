/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

const (
	// TaskWorkspaceClassUseWebhookPath validates Task workspace class selection.
	TaskWorkspaceClassUseWebhookPath = "/validate-core-orka-ai-v1alpha1-task-workspace-class-use"
	// ToolWorkspaceClassUseWebhookPath validates Tool workspace class selection.
	ToolWorkspaceClassUseWebhookPath = "/validate-core-orka-ai-v1alpha1-tool-workspace-class-use"
	CheckpointSourceUseWebhookPath   = "/validate-workspace-orka-ai-v1alpha1-checkpoint-source-use"
)

// WorkspaceClassUseAuthorizer checks the live admission caller's permission to
// use one namespaced ExecutionWorkspaceClass.
type WorkspaceClassUseAuthorizer interface {
	Authorize(context.Context, string, string, authenticationv1.UserInfo) error
}

type workspaceClassResource int

const (
	workspaceClassTask workspaceClassResource = iota
	workspaceClassTool
	workspaceCheckpointSource
)

// RegisterWorkspaceClassUseWebhooks registers fail-closed Task and Tool class
// selection handlers. The corresponding ValidatingWebhookConfiguration decides
// whether Kubernetes routes requests to these endpoints.
func RegisterWorkspaceClassUseWebhooks(
	server webhook.Server,
	scheme *runtime.Scheme,
	authorizer WorkspaceClassUseAuthorizer,
) {
	server.Register(TaskWorkspaceClassUseWebhookPath, &ctrladmission.Webhook{
		Handler: newWorkspaceClassUseValidator(scheme, authorizer, workspaceClassTask),
	})
	server.Register(ToolWorkspaceClassUseWebhookPath, &ctrladmission.Webhook{
		Handler: newWorkspaceClassUseValidator(scheme, authorizer, workspaceClassTool),
	})
	server.Register(CheckpointSourceUseWebhookPath, &ctrladmission.Webhook{
		Handler: newWorkspaceClassUseValidator(scheme, authorizer, workspaceCheckpointSource),
	})
}

// WorkspaceClassUseValidator authorizes newly selected workspace classes using
// AdmissionRequest.UserInfo rather than forgeable fields from the submitted object.
type WorkspaceClassUseValidator struct {
	decoder    ctrladmission.Decoder
	authorizer WorkspaceClassUseAuthorizer
	resource   workspaceClassResource
}

func newWorkspaceClassUseValidator(
	scheme *runtime.Scheme,
	authorizer WorkspaceClassUseAuthorizer,
	resource workspaceClassResource,
) *WorkspaceClassUseValidator {
	return &WorkspaceClassUseValidator{
		decoder:    ctrladmission.NewDecoder(scheme),
		authorizer: authorizer,
		resource:   resource,
	}
}

// Handle implements admission.Handler.
func (v *WorkspaceClassUseValidator) Handle(
	ctx context.Context,
	req ctrladmission.Request,
) ctrladmission.Response {
	if req.SubResource != "" || (req.Operation != admissionv1.Create && req.Operation != admissionv1.Update) {
		return ctrladmission.Allowed("not a workspace class selection write")
	}
	if v.resource == workspaceCheckpointSource {
		// The CRD makes the source immutable. Only creation introduces access;
		// controller finalizer updates must remain possible after revocation.
		if req.Operation != admissionv1.Create {
			return ctrladmission.Allowed("checkpoint source is immutable")
		}
		checkpoint := &workspacev1alpha1.ExecutionWorkspaceCheckpoint{}
		if err := v.decoder.Decode(req, checkpoint); err != nil {
			return ctrladmission.Errored(http.StatusBadRequest, err)
		}
		authorizer, ok := v.authorizer.(interface {
			AuthorizeCheckpointSource(context.Context, string, string, authenticationv1.UserInfo) error
		})
		if !ok {
			return ctrladmission.Denied("workspace checkpoint source authorizer is unavailable")
		}
		if err := authorizer.AuthorizeCheckpointSource(ctx, requestNamespace(req.Namespace, checkpoint.Namespace), checkpoint.Spec.WorkspaceRef.Name, req.UserInfo); err != nil {
			return ctrladmission.Denied("workspace checkpoint source use authorization failed")
		}
		return ctrladmission.Allowed("workspace checkpoint source use authorized")
	}

	className, namespace, err := v.decodeSelection(req)
	if err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, err)
	}
	if className == "" {
		return ctrladmission.Allowed("no workspace class selected")
	}
	if v.authorizer == nil {
		return ctrladmission.Errored(
			http.StatusInternalServerError,
			fmt.Errorf("workspace class use authorizer is unavailable"),
		)
	}
	if err := v.authorizer.Authorize(ctx, namespace, className, req.UserInfo); err != nil {
		return ctrladmission.Denied("workspace class use authorization failed: " + err.Error())
	}
	if v.resource == workspaceClassTask {
		task := &corev1alpha1.Task{}
		if err := v.decoder.Decode(req, task); err != nil {
			return ctrladmission.Errored(http.StatusBadRequest, err)
		}
		if task.Spec.Execution != nil && task.Spec.Execution.Workspace != nil && task.Spec.Execution.Workspace.RestoreFrom != nil {
			authorizer, ok := v.authorizer.(interface {
				AuthorizeCheckpoint(context.Context, string, string, authenticationv1.UserInfo) error
			})
			if !ok {
				return ctrladmission.Denied("workspace checkpoint use authorizer is unavailable")
			}
			if err := authorizer.AuthorizeCheckpoint(ctx, namespace, task.Spec.Execution.Workspace.RestoreFrom.Name, req.UserInfo); err != nil {
				return ctrladmission.Denied("workspace checkpoint use authorization failed")
			}
		}
	}
	return ctrladmission.Allowed("workspace class use authorized")
}

func (v *WorkspaceClassUseValidator) decodeSelection(
	req ctrladmission.Request,
) (string, string, error) {
	switch v.resource {
	case workspaceClassTask:
		value := &corev1alpha1.Task{}
		if err := v.decoder.Decode(req, value); err != nil {
			return "", "", fmt.Errorf("decode Task workspace class selection: %w", err)
		}
		className := ""
		if value.Spec.Execution != nil && value.Spec.Execution.Workspace != nil &&
			value.Spec.Execution.Workspace.ClassRef != nil {
			className = value.Spec.Execution.Workspace.ClassRef.Name
		}
		return strings.TrimSpace(className), requestNamespace(req.Namespace, value.Namespace), nil
	case workspaceClassTool:
		value := &corev1alpha1.Tool{}
		if err := v.decoder.Decode(req, value); err != nil {
			return "", "", fmt.Errorf("decode Tool workspace class selection: %w", err)
		}
		className := ""
		if value.Spec.MCP != nil && value.Spec.MCP.Workspace != nil {
			className = value.Spec.MCP.Workspace.ClassRef.Name
		}
		return strings.TrimSpace(className), requestNamespace(req.Namespace, value.Namespace), nil
	default:
		return "", "", fmt.Errorf("unsupported workspace class admission resource")
	}
}

func requestNamespace(requestNamespace, objectNamespace string) string {
	if namespace := strings.TrimSpace(requestNamespace); namespace != "" {
		return namespace
	}
	return strings.TrimSpace(objectNamespace)
}
