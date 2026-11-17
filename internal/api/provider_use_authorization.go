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

// providerUseVerb is the SubjectAccessReview verb that authorizes sending
// work through a Provider's credential, as opposed to reading or editing the
// Provider itself.
const providerUseVerb = "use"

const providersResource = "providers"

// providerUseAuthorization is set once from ServerConfig when the server
// starts, like agentUseAuthorization.
var providerUseAuthorization atomic.Bool

// authorizeKubernetesProviderUse checks that the caller may use the named Provider. It
// is a no-op unless Provider use authorization is enabled. The caller is
// reviewed as the same principal Agent use reviews.
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

// providerRefNamespace returns the namespace a ProviderReference resolves in
// for an object in objectNamespace: its own namespace when set, otherwise the
// object's. The controller resolves a Task's Provider this way, including a
// reference it takes from the Task's Agent.
func providerRefNamespace(ref corev1alpha1.ProviderReference, objectNamespace string) string {
	if namespace := strings.TrimSpace(ref.Namespace); namespace != "" {
		return namespace
	}
	return objectNamespace
}

// authorizeTaskProviderUse checks the Providers a new Task chooses. A Task's
// own spec.ai.providerRef is always checked. Providers a same-namespace Agent
// names were checked when the Agent named them, and running the Agent is
// covered by Agent use.
//
// An Agent from another namespace is different: it may sit outside the
// namespaces whose admission checked its Providers, its namespace-less
// providerRef and fallbacks resolve in the Task's namespace, and it can change
// after this check. So a Task that would take a Provider from such an Agent
// is refused, here and again by the controller each time the Task runs.
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
		// A missing Agent is refused too: one created before the Task runs
		// could name Providers in the Task's namespace.
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

// authorizeAgentProviderRefsUse checks every Provider the Agent newly names:
// its providerRef and each fallback, which resolve in the Agent's namespace
// when it runs there. Providers the previous version already named were
// checked when they were added, so an unrelated edit by someone without use
// on them still succeeds. Names are compared exactly, as admission does.
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

// agentProviderKeys lists the namespace/name of every Provider an Agent names,
// resolved in the Agent's namespace.
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

// authorizeChatProviderUse checks a Provider a chat request resolved, before
// its credential is read. A chat sends the caller's request straight to the
// Provider, so every Provider it resolves is checked: one it named, its
// Agent's, a fallback, or a default.
func authorizeChatProviderUse(c fiber.Ctx, clientset kubernetes.Interface, objectKind string, provider ProviderResolutionInfo) error {
	if !providerUseAuthorization.Load() {
		return nil
	}
	if c == nil {
		return providerUseForbidden(provider.Name)
	}
	return authorizeKubernetesProviderUse(c.Context(), clientset, GetUserInfo(c), provider.Namespace, provider.Name, objectKind, "")
}

// authorizeToolAgentCreateProviderUse checks an Agent a chat or compatibility
// tool creates. The tool writes with Orka's identity, which admission trusts,
// so the caller's use of every Provider the Agent names is checked here. A
// same-named stored Agent is not treated as a previous version: it could be
// deleted before the create, which would then add Providers nobody checked.
func authorizeToolAgentCreateProviderUse(ctx context.Context, clientset kubernetes.Interface, userInfo *UserInfo, agent *corev1alpha1.Agent) error {
	return authorizeAgentProviderRefsUse(ctx, clientset, userInfo, agent, nil)
}

// authorizeToolAgentUpdateProviderUse checks an Agent a chat or compatibility
// tool updates, against the stored version it replaces. The tool's update
// carries the resourceVersion it read, so it fails if that version changed.
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
		// Only the version this update replaces counts as previous.
		if stored.UID == agent.UID && stored.ResourceVersion == agent.ResourceVersion {
			previous = stored
		}
	case !apierrors.IsNotFound(err):
		log.Error(err, "provider use authorization could not read the stored Agent", "namespace", agent.Namespace, "agent", agent.Name)
		return fiber.NewError(fiber.StatusForbidden, "not authorized to write agents")
	}
	return authorizeAgentProviderRefsUse(ctx, clientset, userInfo, agent, previous)
}
