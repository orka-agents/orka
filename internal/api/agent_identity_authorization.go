package api

import (
	"context"
	"strings"

	"github.com/gofiber/fiber/v3"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// authorizeAgentIdentityBind requires the `bind` verb on
// agentidentities/<issuer> when a write sets an Agent identity that the
// previous version did not hold. Removing an identity needs no grant.
func authorizeAgentIdentityBind(
	ctx context.Context,
	clientset kubernetes.Interface,
	userInfo *UserInfo,
	agent *corev1alpha1.Agent,
	previous *corev1alpha1.Agent,
) error {
	issuer, id, ok := agent.IdentityRef()
	if !ok {
		return nil
	}
	if previousIssuer, previousID, had := previous.IdentityRef(); had && previousIssuer == issuer && previousID == id {
		return nil
	}
	username := ""
	if userInfo != nil {
		username = strings.TrimSpace(userInfo.Username)
	}
	logValues := []any{"user", username, "agentNamespace", agent.Namespace, "agentName", agent.Name, "identityIssuer", issuer, "identityID", id}
	forbidden := fiber.NewError(fiber.StatusForbidden, "not authorized to bind agent identity "+issuer+"/"+id)
	if username == "" || strings.TrimSpace(agent.Namespace) == "" || kubernetesClientsetIsNil(clientset) {
		log.Info("agent identity bind authorization unavailable", logValues...)
		return forbidden
	}
	extra := make(map[string]authorizationv1.ExtraValue, len(userInfo.Extra))
	for key, values := range userInfo.Extra {
		extra[key] = authorizationv1.ExtraValue(append([]string(nil), values...))
	}
	review, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User: username, UID: userInfo.UID, Groups: append([]string(nil), userInfo.Groups...), Extra: extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: agent.Namespace, Verb: "bind",
				Group: corev1alpha1.GroupVersion.Group, Version: corev1alpha1.GroupVersion.Version,
				Resource: "agentidentities", Subresource: issuer, Name: id,
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		log.Error(err, "agent identity bind authorization check failed", logValues...)
		return forbidden
	}
	if review == nil || !review.Status.Allowed || review.Status.Denied || review.Status.EvaluationError != "" {
		log.Info("agent identity bind authorization denied", logValues...)
		return forbidden
	}
	return nil
}

// requireRequesterApprovesDelegatedWrite lets only the Task's requester
// approve a call on a Task whose binding froze a Delegated agentIdentity
// policy, because such a call can act as that person. The decision rests on
// the write-once binding, never on the live Tool or policy, which an editor
// could change and restore around an approval. Anyone authorized to decide
// approvals may still decline.
func requireRequesterApprovesDelegatedWrite(task *corev1alpha1.Task, userInfo *UserInfo) error {
	if task == nil || task.Status.AgentExecutionBinding == nil || task.Status.AgentExecutionBinding.Agent == nil {
		return nil
	}
	identity := task.Status.AgentExecutionBinding.Agent.Identity
	if identity == nil || !identity.ActsForRequester {
		return nil
	}
	requester := task.Spec.RequestedBy
	if requester == nil || userInfo == nil ||
		(userInfo.AuthType != AuthTypeOIDC && userInfo.AuthType != AuthTypeContextToken) ||
		strings.TrimSpace(userInfo.Issuer) == "" || userInfo.Issuer != requester.Issuer || userInfo.Subject != requester.Subject {
		return fiber.NewError(fiber.StatusForbidden, "only the Task's requester can approve calls on a Task that acts on their behalf")
	}
	return nil
}
