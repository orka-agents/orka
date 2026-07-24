package controller

import (
	"context"
	"fmt"
	"strings"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// WorkspaceClassAuthorizer performs the explicit Kubernetes `use`
// authorization check required before an admission request may select an
// ExecutionWorkspaceClass. Callers must pass AdmissionRequest.UserInfo from the
// live Kubernetes admission request, never identity copied from a Task spec.
type WorkspaceClassAuthorizer struct {
	Client client.Client
}

// Authorize submits a direct SubjectAccessReview for verb use on the class.
func (a WorkspaceClassAuthorizer) Authorize(
	ctx context.Context,
	namespace string,
	className string,
	caller authenticationv1.UserInfo,
) error {
	if a.Client == nil {
		return fmt.Errorf("workspace class authorizer client is required")
	}
	namespace = strings.TrimSpace(namespace)
	className = strings.TrimSpace(className)
	if namespace == "" || className == "" {
		return fmt.Errorf("workspace class namespace and name are required")
	}
	username := strings.TrimSpace(caller.Username)
	if username == "" {
		return fmt.Errorf("workspace class use requires admission caller username")
	}

	extra := make(map[string]authorizationv1.ExtraValue, len(caller.Extra))
	for key, values := range caller.Extra {
		extra[key] = authorizationv1.ExtraValue(append([]string(nil), values...))
	}
	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   username,
			UID:    caller.UID,
			Groups: append([]string(nil), caller.Groups...),
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      "use",
				Group:     "workspace.orka.ai",
				Version:   "v1alpha1",
				Resource:  "executionworkspaceclasses",
				Name:      className,
			},
		},
	}
	if err := a.Client.Create(ctx, review); err != nil {
		return fmt.Errorf("authorize workspace class use: %w", err)
	}
	if !review.Status.Allowed {
		reason := strings.TrimSpace(review.Status.Reason)
		if reason == "" {
			reason = "access denied"
		}
		return fmt.Errorf("workspace class %s/%s use denied: %s", namespace, className, reason)
	}
	return nil
}
