package api

import (
	"context"
	"reflect"
	"strings"

	"github.com/gofiber/fiber/v3"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

func authorizeKubernetesTaskCreate(ctx context.Context, clientset kubernetes.Interface, userInfo *UserInfo, task *corev1alpha1.Task) error {
	if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview || task == nil {
		return nil
	}
	return authorizeKubernetesTaskAction(ctx, clientset, userInfo, task.Namespace, task.Name, "create", "", "task create", "not authorized to create tasks")
}

func authorizeKubernetesApprovalDecision(ctx context.Context, clientset kubernetes.Interface, userInfo *UserInfo, namespace, taskName string) error {
	if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview {
		return nil
	}
	if isWorkerServiceAccount(userInfo.Username) {
		log.Info("approval decision authorization denied for worker service account",
			"username", userInfo.Username,
			"namespace", namespace,
			"task", taskName,
		)
		return fiber.NewError(fiber.StatusForbidden, "not authorized to decide task approvals")
	}
	return authorizeKubernetesTaskAction(ctx, clientset, userInfo, namespace, taskName, "update", "approvals", "approval decision", "not authorized to decide task approvals")
}

func authorizeKubernetesTaskAction(ctx context.Context, clientset kubernetes.Interface, userInfo *UserInfo, namespace, taskName, verb, subresource, action, forbiddenMessage string) error {
	if kubernetesClientsetIsNil(clientset) {
		log.Info(action+" authorization unavailable: missing Kubernetes clientset",
			"username", userInfo.Username,
			"namespace", namespace,
			"task", taskName,
		)
		return fiber.NewError(fiber.StatusForbidden, forbiddenMessage)
	}

	extra := make(map[string]authorizationv1.ExtraValue, len(userInfo.Extra))
	for key, values := range userInfo.Extra {
		extra[key] = authorizationv1.ExtraValue(values)
	}

	review, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   userInfo.Username,
			Groups: userInfo.Groups,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace:   namespace,
				Verb:        verb,
				Group:       corev1alpha1.GroupVersion.Group,
				Resource:    "tasks",
				Subresource: subresource,
				Name:        taskName,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		log.Error(err, action+" authorization check failed",
			"username", userInfo.Username,
			"namespace", namespace,
			"task", taskName,
		)
		return fiber.NewError(fiber.StatusForbidden, forbiddenMessage)
	}
	if !review.Status.Allowed {
		log.Info(action+" authorization denied",
			"username", userInfo.Username,
			"namespace", namespace,
			"task", taskName,
			"reason", review.Status.Reason,
		)
		return fiber.NewError(fiber.StatusForbidden, forbiddenMessage)
	}

	return nil
}

func isWorkerServiceAccount(username string) bool {
	const prefix = "system:serviceaccount:"
	if !strings.HasPrefix(username, prefix) {
		return false
	}
	parts := strings.Split(strings.TrimPrefix(username, prefix), ":")
	if len(parts) != 2 {
		return false
	}
	switch parts[1] {
	case "ai-worker", "container-worker", "vendor-worker":
		return true
	default:
		return false
	}
}

func kubernetesClientsetIsNil(clientset kubernetes.Interface) bool {
	if clientset == nil {
		return true
	}
	value := reflect.ValueOf(clientset)
	return value.Kind() == reflect.Ptr && value.IsNil()
}
