package api

import (
	"context"
	"reflect"

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
	if kubernetesClientsetIsNil(clientset) {
		log.Info("task create authorization unavailable: missing Kubernetes clientset",
			"username", userInfo.Username,
			"namespace", task.Namespace,
			"task", task.Name,
		)
		return fiber.NewError(fiber.StatusForbidden, "not authorized to create tasks")
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
				Namespace: task.Namespace,
				Verb:      "create",
				Group:     corev1alpha1.GroupVersion.Group,
				Resource:  "tasks",
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		log.Error(err, "task create authorization check failed",
			"username", userInfo.Username,
			"namespace", task.Namespace,
			"task", task.Name,
		)
		return fiber.NewError(fiber.StatusForbidden, "not authorized to create tasks")
	}
	if !review.Status.Allowed {
		log.Info("task create authorization denied",
			"username", userInfo.Username,
			"namespace", task.Namespace,
			"task", task.Name,
			"reason", review.Status.Reason,
		)
		return fiber.NewError(fiber.StatusForbidden, "not authorized to create tasks")
	}

	return nil
}

func kubernetesClientsetIsNil(clientset kubernetes.Interface) bool {
	if clientset == nil {
		return true
	}
	value := reflect.ValueOf(clientset)
	return value.Kind() == reflect.Ptr && value.IsNil()
}
