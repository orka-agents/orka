package api

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v3"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

func authorizeTaskWorkspaceClassUse(
	ctx context.Context,
	clientset kubernetes.Interface,
	userInfo *UserInfo,
	task *corev1alpha1.Task,
) error {
	if task == nil || task.Spec.Execution == nil || task.Spec.Execution.Workspace == nil ||
		task.Spec.Execution.Workspace.ClassRef == nil {
		return nil
	}
	return authorizeWorkspaceClassUse(
		ctx,
		clientset,
		userInfo,
		task.Namespace,
		task.Spec.Execution.Workspace.ClassRef.Name,
		"task",
		task.Name,
	)
}

func authorizeToolWorkspaceClassUse(
	ctx context.Context,
	clientset kubernetes.Interface,
	userInfo *UserInfo,
	tool *corev1alpha1.Tool,
) error {
	if tool == nil || tool.Spec.MCP == nil || tool.Spec.MCP.Workspace == nil {
		return nil
	}
	return authorizeWorkspaceClassUse(
		ctx,
		clientset,
		userInfo,
		tool.Namespace,
		tool.Spec.MCP.Workspace.ClassRef.Name,
		"tool",
		tool.Name,
	)
}

func authorizeWorkspaceClassUse(
	ctx context.Context,
	clientset kubernetes.Interface,
	userInfo *UserInfo,
	namespace string,
	className string,
	objectKind string,
	objectName string,
) error {
	className = strings.TrimSpace(className)
	if className == "" {
		return nil
	}
	namespace = strings.TrimSpace(namespace)
	username := ""
	if userInfo != nil {
		username = strings.TrimSpace(userInfo.Username)
	}
	if namespace == "" || username == "" || kubernetesClientsetIsNil(clientset) {
		log.Info("workspace class use authorization unavailable",
			"username", username,
			"namespace", namespace,
			"class", className,
			"objectKind", objectKind,
			"object", objectName,
		)
		return fiber.NewError(fiber.StatusForbidden, "not authorized to use workspace class")
	}

	extra := make(map[string]authorizationv1.ExtraValue, len(userInfo.Extra))
	for key, values := range userInfo.Extra {
		extra[key] = authorizationv1.ExtraValue(append([]string(nil), values...))
	}
	review, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   username,
			UID:    userInfo.UID,
			Groups: append([]string(nil), userInfo.Groups...),
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "use",
				Group:     workspacev1alpha1.GroupVersion.Group,
				Version:   workspacev1alpha1.GroupVersion.Version,
				Resource:  "executionworkspaceclasses",
				Name:      className,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		log.Error(err, "workspace class use authorization check failed",
			"username", username,
			"namespace", namespace,
			"class", className,
			"objectKind", objectKind,
			"object", objectName,
		)
		return fiber.NewError(fiber.StatusForbidden, "not authorized to use workspace class")
	}
	if !review.Status.Allowed {
		log.Info("workspace class use authorization denied",
			"username", username,
			"namespace", namespace,
			"class", className,
			"objectKind", objectKind,
			"object", objectName,
			"reason", review.Status.Reason,
		)
		return fiber.NewError(fiber.StatusForbidden, "not authorized to use workspace class")
	}
	return nil
}
