package api

import (
	"context"
	"strings"
	"sync/atomic"

	"github.com/gofiber/fiber/v3"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// agentUseVerb is the SubjectAccessReview verb that authorizes running work
// as an Agent, as opposed to reading or editing the Agent itself.
const agentUseVerb = "use"

// agentUseAuthorization is set once from ServerConfig when the server starts.
// It is process-wide so that every API path that creates a Task or another
// object naming an Agent enforces it without having to thread a setting
// through each caller.
var agentUseAuthorization atomic.Bool

// authorizeAgentUse checks that the caller may use the named Agent. It is a
// no-op unless Agent use authorization is enabled.
func authorizeAgentUse(
	ctx context.Context,
	clientset kubernetes.Interface,
	userInfo *UserInfo,
	namespace string,
	agentName string,
	objectKind string,
	objectName string,
) error {
	agentName = strings.TrimSpace(agentName)
	if !agentUseAuthorization.Load() || agentName == "" {
		return nil
	}
	namespace = strings.TrimSpace(namespace)
	username := ""
	if userInfo != nil {
		username = strings.TrimSpace(userInfo.Username)
	}
	logValues := []any{"user", username, "agentNamespace", namespace, "agentName", agentName, "objectKind", objectKind, "object", objectName}
	if namespace == "" || username == "" || kubernetesClientsetIsNil(clientset) {
		log.Info("agent use authorization unavailable", logValues...)
		return agentUseForbidden(agentName)
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
				Verb:      agentUseVerb,
				Group:     corev1alpha1.GroupVersion.Group,
				Version:   corev1alpha1.GroupVersion.Version,
				Resource:  "agents",
				Name:      agentName,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		log.Error(err, "agent use authorization check failed", logValues...)
		return agentUseForbidden(agentName)
	}
	if review == nil || !review.Status.Allowed || review.Status.Denied || review.Status.EvaluationError != "" {
		log.Info("agent use authorization denied", logValues...)
		return agentUseForbidden(agentName)
	}
	return nil
}

func agentUseForbidden(agentName string) error {
	return fiber.NewError(fiber.StatusForbidden, "not authorized to use agent "+agentName)
}

// agentRefNamespace returns the namespace an AgentReference resolves in: its
// own namespace when set, otherwise the referring object's.
func agentRefNamespace(ref corev1alpha1.AgentReference, objectNamespace string) string {
	if namespace := strings.TrimSpace(ref.Namespace); namespace != "" {
		return namespace
	}
	return objectNamespace
}

func authorizeTaskAgentUse(ctx context.Context, clientset kubernetes.Interface, userInfo *UserInfo, task *corev1alpha1.Task) error {
	if task == nil || task.Spec.AgentRef == nil {
		return nil
	}
	return authorizeAgentUse(ctx, clientset, userInfo,
		agentRefNamespace(*task.Spec.AgentRef, task.Namespace), task.Spec.AgentRef.Name, "task", task.Name)
}

// authorizeAgentRefsUse checks every Agent the object newly names. Agents the
// previous version already named were authorized when they were added, so an
// unrelated edit by someone without use on them still succeeds.
func authorizeAgentRefsUse(
	ctx context.Context,
	clientset kubernetes.Interface,
	userInfo *UserInfo,
	namespace string,
	objectKind string,
	objectName string,
	refs []corev1alpha1.AgentReference,
	previous []corev1alpha1.AgentReference,
) error {
	key := func(ref corev1alpha1.AgentReference) string {
		return agentRefNamespace(ref, namespace) + "/" + strings.TrimSpace(ref.Name)
	}
	existing := make(map[string]bool, len(previous))
	for _, ref := range previous {
		existing[key(ref)] = true
	}
	checked := make(map[string]bool, len(refs))
	for _, ref := range refs {
		if strings.TrimSpace(ref.Name) == "" || existing[key(ref)] || checked[key(ref)] {
			continue
		}
		checked[key(ref)] = true
		if err := authorizeAgentUse(ctx, clientset, userInfo, agentRefNamespace(ref, namespace), ref.Name, objectKind, objectName); err != nil {
			return err
		}
	}
	return nil
}

func repositoryMonitorSpecAgentRefs(spec corev1alpha1.RepositoryMonitorSpec) []corev1alpha1.AgentReference {
	return repositoryMonitorAgentRefs(&corev1alpha1.RepositoryMonitor{Spec: spec})
}

func repositoryScanSpecAgentRefs(spec corev1alpha1.RepositoryScanSpec) []corev1alpha1.AgentReference {
	refs := []corev1alpha1.AgentReference{spec.AnalysisAgentRef}
	if spec.PatchAgentRef != nil {
		refs = append(refs, *spec.PatchAgentRef)
	}
	return refs
}
