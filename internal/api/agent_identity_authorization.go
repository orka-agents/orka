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
	// Review the same principal an Agent use check does, so `bind` and `use`
	// grants name one subject.
	spec, reviewable := agentUseReviewSubject(userInfo)
	logValues := []any{"user", spec.User, "agentNamespace", agent.Namespace, "agentName", agent.Name, "identityIssuer", issuer, "identityID", id}
	forbidden := fiber.NewError(fiber.StatusForbidden, "not authorized to bind agent identity "+issuer+"/"+id)
	if !reviewable || strings.TrimSpace(agent.Namespace) == "" || kubernetesClientsetIsNil(clientset) {
		log.Info("agent identity bind authorization unavailable", logValues...)
		return forbidden
	}
	spec.ResourceAttributes = &authorizationv1.ResourceAttributes{
		Namespace: agent.Namespace, Verb: "bind",
		Group: corev1alpha1.GroupVersion.Group, Version: corev1alpha1.GroupVersion.Version,
		Resource: "agentidentities", Subresource: issuer, Name: id,
	}
	review, err := clientset.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
		Spec: spec,
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
