package api

import (
	"context"
	"reflect"
	"strings"
	"sync/atomic"

	"github.com/gofiber/fiber/v3"
	authorizationv1 "k8s.io/api/authorization/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const providerUseVerb = "use"

const providersResource = "providers"

var providerUseAuthorization atomic.Bool

func authorizeKubernetesProviderUse(
	ctx context.Context,
	clientset kubernetes.Interface,
	userInfo *UserInfo,
	namespace string,
	providerName string,
	objectKind string,
	objectName string,
) error {
	providerName = strings.TrimSpace(providerName)
	if !providerUseAuthorization.Load() || providerName == "" {
		return nil
	}
	namespace = strings.TrimSpace(namespace)
	spec, ok := agentUseReviewSubject(userInfo)
	logValues := []any{"reviewedUser", spec.User, "providerNamespace", namespace, "providerName", providerName, "objectKind", objectKind, "object", objectName}
	if namespace == "" || !ok || kubernetesClientsetIsNil(clientset) {
		log.Info("provider use authorization unavailable", logValues...)
		return providerUseForbidden(providerName)
	}

	spec.ResourceAttributes = &authorizationv1.ResourceAttributes{
		Namespace: namespace,
		Verb:      providerUseVerb,
		Group:     corev1alpha1.GroupVersion.Group,
		Version:   corev1alpha1.GroupVersion.Version,
		Resource:  providersResource,
		Name:      providerName,
	}
	review, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: spec,
	}, metav1.CreateOptions{})
	if err != nil {
		log.Error(err, "provider use authorization check failed", logValues...)
		return providerUseForbidden(providerName)
	}
	if review == nil || !review.Status.Allowed || review.Status.Denied || review.Status.EvaluationError != "" {
		log.Info("provider use authorization denied", logValues...)
		return providerUseForbidden(providerName)
	}
	return nil
}

func providerUseForbidden(providerName string) error {
	return fiber.NewError(fiber.StatusForbidden, "not authorized to use provider "+providerName)
}

func providerRefNamespace(ref corev1alpha1.ProviderReference, objectNamespace string) string {
	if namespace := strings.TrimSpace(ref.Namespace); namespace != "" {
		return namespace
	}
	return objectNamespace
}

func authorizeTaskProviderUse(ctx context.Context, reader client.Reader, clientset kubernetes.Interface, userInfo *UserInfo, task *corev1alpha1.Task) error {
	if task == nil || !providerUseAuthorization.Load() {
		return nil
	}
	if task.Spec.AI != nil && task.Spec.AI.ProviderRef != nil {
		ref := *task.Spec.AI.ProviderRef
		if err := authorizeKubernetesProviderUse(ctx, clientset, userInfo,
			providerRefNamespace(ref, task.Namespace), ref.Name, "task", task.Name); err != nil {
			return err
		}
	}
	if task.Spec.Type != corev1alpha1.TaskTypeAI || task.Spec.AgentRef == nil || strings.TrimSpace(task.Spec.AgentRef.Name) == "" {
		return nil
	}
	agentNamespace := agentRefNamespace(*task.Spec.AgentRef, task.Namespace)
	if agentNamespace == task.Namespace {
		return nil
	}
	if reader == nil || reflect.ValueOf(reader).Kind() == reflect.Pointer && reflect.ValueOf(reader).IsNil() {
		log.Info("provider use authorization unavailable: missing reader", "task", task.Name, "namespace", task.Namespace)
		return fiber.NewError(fiber.StatusForbidden, "not authorized to create tasks")
	}
	agent := &corev1alpha1.Agent{}
	if err := reader.Get(ctx, client.ObjectKey{Namespace: agentNamespace, Name: task.Spec.AgentRef.Name}, agent); err != nil {
		if !apierrors.IsNotFound(err) {
			log.Error(err, "provider use authorization could not read the Task's Agent",
				"task", task.Name, "namespace", task.Namespace, "agentNamespace", agentNamespace)
		}
		return fiber.NewError(fiber.StatusForbidden, "not authorized to create tasks")
	}
	if len(corev1alpha1.ProvidersFromAgentInAnotherNamespace(task, agent)) > 0 {
		return fiber.NewError(fiber.StatusForbidden,
			"provider use authorization refuses Providers taken from an Agent in another namespace: "+
				"set the Task's spec.ai.providerRef and use an Agent without model fallbacks, or use an Agent from the Task's namespace")
	}
	return nil
}

func authorizeAgentProviderRefsUse(ctx context.Context, clientset kubernetes.Interface, userInfo *UserInfo, agent, previous *corev1alpha1.Agent) error {
	if agent == nil || !providerUseAuthorization.Load() {
		return nil
	}
	existing := map[string]bool{}
	if previous != nil {
		for _, key := range agentProviderKeys(previous) {
			existing[key] = true
		}
	}
	for _, key := range agentProviderKeys(agent) {
		if existing[key] {
			continue
		}
		existing[key] = true
		namespace, name, _ := strings.Cut(key, "/")
		if err := authorizeKubernetesProviderUse(ctx, clientset, userInfo, namespace, name, "agent", agent.Name); err != nil {
			return err
		}
	}
	return nil
}

func agentProviderKeys(agent *corev1alpha1.Agent) []string {
	var keys []string
	if ref := agent.Spec.ProviderRef; ref != nil && strings.TrimSpace(ref.Name) != "" {
		namespace := ref.Namespace
		if namespace == "" {
			namespace = agent.Namespace
		}
		keys = append(keys, namespace+"/"+ref.Name)
	}
	if agent.Spec.Model != nil {
		for _, fallback := range agent.Spec.Model.Fallbacks {
			if strings.TrimSpace(fallback.ProviderRef) != "" {
				keys = append(keys, agent.Namespace+"/"+fallback.ProviderRef)
			}
		}
	}
	return keys
}

func authorizeChatProviderUse(c fiber.Ctx, clientset kubernetes.Interface, objectKind string, provider ProviderResolutionInfo) error {
	if !providerUseAuthorization.Load() {
		return nil
	}
	if c == nil {
		return providerUseForbidden(provider.Name)
	}
	return authorizeKubernetesProviderUse(c.Context(), clientset, GetUserInfo(c), provider.Namespace, provider.Name, objectKind, "")
}

func authorizeToolAgentCreateProviderUse(ctx context.Context, clientset kubernetes.Interface, userInfo *UserInfo, agent *corev1alpha1.Agent) error {
	return authorizeAgentProviderRefsUse(ctx, clientset, userInfo, agent, nil)
}

func authorizeToolAgentUpdateProviderUse(ctx context.Context, reader client.Reader, clientset kubernetes.Interface, userInfo *UserInfo, agent *corev1alpha1.Agent) error {
	if agent == nil || !providerUseAuthorization.Load() {
		return nil
	}
	if reader == nil || reflect.ValueOf(reader).Kind() == reflect.Pointer && reflect.ValueOf(reader).IsNil() {
		return fiber.NewError(fiber.StatusForbidden, "not authorized to write agents")
	}
	var previous *corev1alpha1.Agent
	stored := &corev1alpha1.Agent{}
	switch err := reader.Get(ctx, client.ObjectKey{Namespace: agent.Namespace, Name: agent.Name}, stored); {
	case err == nil:
		if stored.UID == agent.UID && stored.ResourceVersion == agent.ResourceVersion {
			previous = stored
		}
	case !apierrors.IsNotFound(err):
		log.Error(err, "provider use authorization could not read the stored Agent", "namespace", agent.Namespace, "agent", agent.Name)
		return fiber.NewError(fiber.StatusForbidden, "not authorized to write agents")
	}
	return authorizeAgentProviderRefsUse(ctx, clientset, userInfo, agent, previous)
}
