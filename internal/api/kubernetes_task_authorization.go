package api

import (
	"context"
	"errors"
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
	if err := authorizeTaskProviderUse(ctx, reader, clientset, userInfo, task); err != nil {
		return err
	}
	if err := authorizeTaskMemoryConsolidation(ctx, clientset, userInfo, task); err != nil {
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
	allowed, err := kubernetesResourceActionAllowed(ctx, clientset, userInfo, namespace, verb, group, resource, name)
	if err != nil || !allowed {
		return fiber.NewError(fiber.StatusForbidden, "not authorized")
	}
	return nil
}

// errKubernetesAccessReviewUnavailable reports a SubjectAccessReview that did
// not return a usable answer, as opposed to an explicit denial.
var errKubernetesAccessReviewUnavailable = errors.New("kubernetes access review is unavailable")

// kubernetesResourceActionAllowed is authorizeKubernetesResourceAction's
// three-way form for read-side hints: it reports allowed, denied, or (with an
// error) that the review gave no usable answer. Callers that enforce access
// must use authorizeKubernetesResourceAction, which refuses in both cases.
func kubernetesResourceActionAllowed(
	ctx context.Context,
	clientset kubernetes.Interface,
	userInfo *UserInfo,
	namespace, verb, group, resource, name string,
) (bool, error) {
	if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview {
		return true, nil
	}
	if kubernetesClientsetIsNil(clientset) {
		return false, nil
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
	if err != nil || review == nil {
		return false, errKubernetesAccessReviewUnavailable
	}
	if review.Status.Denied {
		return false, nil
	}
	if review.Status.EvaluationError != "" {
		return false, errKubernetesAccessReviewUnavailable
	}
	return review.Status.Allowed, nil
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

// memoriesResource is the Kubernetes resource name of durable memories.
const memoriesResource = "memories"

// authorizeTaskMemoryConsolidation checks, as the original caller, the grants
// a memory consolidation run needs: it reads every user's Sessions and the
// namespace's memories, and in Propose mode creates memory proposals. The API
// creates Tasks with its own identity, so the admission policy alone would
// check the controller rather than the caller. It fails closed for any caller
// it cannot review.
func authorizeTaskMemoryConsolidation(ctx context.Context, clientset kubernetes.Interface, userInfo *UserInfo, task *corev1alpha1.Task) error {
	if task == nil || task.Spec.AI == nil || task.Spec.AI.MemoryConsolidation == nil {
		return nil
	}
	denied := fiber.NewError(fiber.StatusForbidden,
		"not authorized to create a memory consolidation run: it needs list and get on sessions, list on memories, "+
			"and create on memoryproposals in Propose mode")
	username := ""
	if userInfo != nil {
		username = strings.TrimSpace(userInfo.Username)
	}
	if username == "" || strings.TrimSpace(task.Namespace) == "" || kubernetesClientsetIsNil(clientset) {
		return denied
	}
	checks := []authorizationv1.ResourceAttributes{
		{Verb: apiObjectList, Resource: usageSessionResource},
		{Verb: gatewayVerbGet, Resource: usageSessionResource},
		{Verb: apiObjectList, Resource: memoriesResource},
	}
	if task.Spec.AI.MemoryConsolidation.Mode == corev1alpha1.MemoryConsolidationPropose {
		checks = append(checks, authorizationv1.ResourceAttributes{Verb: externalToolCreateVerb, Resource: "memoryproposals"})
	}
	extra := make(map[string]authorizationv1.ExtraValue, len(userInfo.Extra))
	for key, values := range userInfo.Extra {
		extra[key] = authorizationv1.ExtraValue(append([]string(nil), values...))
	}
	for _, check := range checks {
		check.Namespace = task.Namespace
		check.Group = corev1alpha1.GroupVersion.Group
		check.Version = corev1alpha1.GroupVersion.Version
		review, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User: username, UID: userInfo.UID, Groups: append([]string(nil), userInfo.Groups...), Extra: extra,
				ResourceAttributes: &check,
			},
		}, metav1.CreateOptions{})
		if err != nil || review == nil || !review.Status.Allowed || review.Status.Denied || review.Status.EvaluationError != "" {
			log.Info("memory consolidation run authorization denied", "username", username, "namespace", task.Namespace,
				"task", task.Name, "verb", check.Verb, "resource", check.Resource)
			return denied
		}
	}
	return nil
}
