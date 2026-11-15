package api

import (
	"context"
	"reflect"
	"slices"
	"strings"

	"github.com/gofiber/fiber/v3"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func authorizeKubernetesTaskCreate(ctx context.Context, reader client.Reader, clientset kubernetes.Interface, userInfo *UserInfo, task *corev1alpha1.Task) error {
	if task == nil {
		return nil
	}
	if err := refuseWorkerRootTaskForIdentityAgent(ctx, reader, userInfo, task); err != nil {
		return err
	}
	if err := authorizeTaskWorkspaceClassUse(ctx, clientset, userInfo, task); err != nil {
		return err
	}
	if err := authorizeTaskAgentUse(ctx, clientset, userInfo, task); err != nil {
		return err
	}
	if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview {
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
			UID:    userInfo.UID,
			Groups: userInfo.Groups,
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: task.Namespace,
				Verb:      externalToolCreateVerb,
				Group:     corev1alpha1.GroupVersion.Group,
				Resource:  externalToolTaskResource,
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
	if review == nil || !review.Status.Allowed || review.Status.Denied || review.Status.EvaluationError != "" {
		log.Info("task create authorization denied",
			"username", userInfo.Username,
			"namespace", task.Namespace,
			"task", task.Name,
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
	return value.Kind() == reflect.Pointer && value.IsNil()
}

// authorizeCoreResourceAction enforces Kubernetes RBAC for orka.ai core-group
// resource reads and writes (RuntimePool, AgentRuntime, Session, Provider): a
// TokenReview-authenticated identity must pass a SubjectAccessReview for the
// exact verb, resource, and name before the controller client acts on its
// behalf. It is a no-op for non-TokenReview auth (context tokens carry their
// own scope checks).
func (h *Handlers) authorizeCoreResourceAction(c fiber.Ctx, verb, resource, namespace, name string) error {
	return authorizeKubernetesResourceAction(
		c.Context(), h.clientset, GetUserInfo(c), namespace, verb, corev1alpha1.GroupVersion.Group, resource, name,
	)
}

func authorizeKubernetesResourceAction(
	ctx context.Context,
	clientset kubernetes.Interface,
	userInfo *UserInfo,
	namespace, verb, group, resource, name string,
) error {
	if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview {
		return nil
	}
	if kubernetesClientsetIsNil(clientset) {
		return fiber.NewError(fiber.StatusForbidden, "not authorized")
	}
	extra := make(map[string]authorizationv1.ExtraValue, len(userInfo.Extra))
	for key, values := range userInfo.Extra {
		extra[key] = authorizationv1.ExtraValue(values)
	}
	resource, subresource, _ := strings.Cut(resource, "/")
	review, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User: userInfo.Username, UID: userInfo.UID, Groups: userInfo.Groups, Extra: extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace, Verb: verb, Group: group, Resource: resource, Subresource: subresource, Name: name,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil || review == nil || !review.Status.Allowed || review.Status.Denied || review.Status.EvaluationError != "" {
		return fiber.NewError(fiber.StatusForbidden, "not authorized")
	}
	return nil
}

// workerServiceAccountNames are the ServiceAccount names worker Pods run as
// (--ai-worker-service-account-name and --vendor-worker-service-account-name),
// the same names Task provenance admission trusts as workers.
var workerServiceAccountNames = []string{"orka-ai-worker", "orka-vendor-worker"}

// SetWorkerServiceAccountNames records the worker ServiceAccount names the
// controller runs worker Pods as. An empty list keeps the defaults.
func SetWorkerServiceAccountNames(names []string) {
	var kept []string
	for _, name := range names {
		if name = strings.TrimSpace(name); name != "" {
			kept = append(kept, name)
		}
	}
	if len(kept) > 0 {
		workerServiceAccountNames = kept
	}
}

// refuseWorkerRootTaskForIdentityAgent applies Task provenance admission's
// worker rule to API callers: the API writes Tasks as the controller, which
// admission trusts, so a worker posting a Task here would otherwise bind an
// identity-bound Agent as a root Task. Workers hold Agent use broadly; they
// reach such an Agent only as a delegated child of their own Task.
func refuseWorkerRootTaskForIdentityAgent(ctx context.Context, reader client.Reader, userInfo *UserInfo, task *corev1alpha1.Task) error {
	if task.Spec.AgentRef == nil || strings.TrimSpace(task.Spec.AgentRef.Name) == "" || !isWorkerServiceAccount(userInfo) {
		return nil
	}
	if owner := metav1.GetControllerOf(task); owner != nil && owner.APIVersion == corev1alpha1.GroupVersion.String() && owner.Kind == "Task" {
		return nil
	}
	if reader == nil || reflect.ValueOf(reader).Kind() == reflect.Pointer && reflect.ValueOf(reader).IsNil() {
		return fiber.NewError(fiber.StatusForbidden, "not authorized to create tasks")
	}
	agent := &corev1alpha1.Agent{}
	key := client.ObjectKey{Namespace: agentRefNamespace(*task.Spec.AgentRef, task.Namespace), Name: task.Spec.AgentRef.Name}
	if err := reader.Get(ctx, key, agent); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fiber.NewError(fiber.StatusInternalServerError, "failed to read the Task's Agent")
	}
	if agent.HoldsIdentity() {
		return fiber.NewError(fiber.StatusForbidden, "a worker may name an identity-bound Agent only in a Task delegated from its own Task")
	}
	return nil
}

// isWorkerServiceAccount reports a caller authenticated as a worker
// ServiceAccount. Only a TokenReview caller can be one: the Agent use check
// judges OIDC and context-token callers by issuer and subject, so their
// display name, whatever it looks like, carries no worker's broad use grant.
func isWorkerServiceAccount(userInfo *UserInfo) bool {
	if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview || parseServiceAccountNamespace(userInfo.Username) == "" {
		return false
	}
	name := userInfo.Username[strings.LastIndex(userInfo.Username, ":")+1:]
	return slices.Contains(workerServiceAccountNames, name)
}
