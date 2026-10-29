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
	return a.authorizeResource(ctx, namespace, "executionworkspaceclasses", className, caller)
}

// AuthorizeCheckpoint checks permission to consume retained workspace data.
func (a WorkspaceClassAuthorizer) AuthorizeCheckpoint(ctx context.Context, namespace, name string, caller authenticationv1.UserInfo) error {
	return a.authorizeResource(ctx, namespace, "executionworkspacecheckpoints", name, caller)
}

// AuthorizeCheckpointSource checks permission to export a workspace's data.
func (a WorkspaceClassAuthorizer) AuthorizeCheckpointSource(ctx context.Context, namespace, name string, caller authenticationv1.UserInfo) error {
	return a.authorizeResource(ctx, namespace, "executionworkspaces", name, caller)
}

func (a WorkspaceClassAuthorizer) authorizeResource(ctx context.Context, namespace, resource, className string, caller authenticationv1.UserInfo) error {
	return authorizeUse(ctx, a.Client, "workspace.orka.ai", resource, "workspace class", namespace, className, caller)
}

// authorizeUse submits a direct SubjectAccessReview for verb use on one
// namespaced object, as the live admission caller.
func authorizeUse(
	ctx context.Context,
	c client.Client,
	group, resource, label, namespace, className string,
	caller authenticationv1.UserInfo,
) error {
	return authorizeVerb(ctx, c, "use", group, resource, label, namespace, className, caller)
}

// authorizeVerb submits a direct SubjectAccessReview for one verb on one
// namespaced object, as the live admission caller.
func authorizeVerb(
	ctx context.Context,
	c client.Client,
	verb, group, resource, label, namespace, className string,
	caller authenticationv1.UserInfo,
) error {
	if c == nil {
		return fmt.Errorf("%s authorizer client is required", label)
	}
	namespace = strings.TrimSpace(namespace)
	className = strings.TrimSpace(className)
	if namespace == "" || className == "" {
		return fmt.Errorf("%s namespace and name are required", label)
	}
	username := strings.TrimSpace(caller.Username)
	if username == "" {
		return fmt.Errorf("%s %s requires admission caller username", label, verb)
	}

	extra := make(map[string]authorizationv1.ExtraValue, len(caller.Extra))
	for key, values := range caller.Extra {
		extra[key] = authorizationv1.ExtraValue(append([]string(nil), values...))
	}
	// Orka's resources are v1alpha1; core resources such as Secrets are v1.
	version := "v1alpha1"
	if group == "" {
		version = "v1"
	}
	review := &authorizationv1.SubjectAccessReview{
		Spec: authorizationv1.SubjectAccessReviewSpec{
			User:   username,
			UID:    caller.UID,
			Groups: append([]string(nil), caller.Groups...),
			Extra:  extra,
			ResourceAttributes: &authorizationv1.ResourceAttributes{
				Namespace: namespace,
				Verb:      verb,
				Group:     group,
				Version:   version,
				Resource:  resource,
				Name:      className,
			},
		},
	}
	if err := c.Create(ctx, review); err != nil {
		return fmt.Errorf("authorize %s %s: %w", label, verb, err)
	}
	if !review.Status.Allowed || review.Status.Denied || review.Status.EvaluationError != "" {
		reason := strings.TrimSpace(review.Status.Reason)
		if reason == "" {
			reason = "access denied"
		}
		return fmt.Errorf("%s %s/%s %s denied: %s", label, namespace, className, verb, reason)
	}
	return nil
}

// AgentHostTemplateAuthorizer checks the live admission caller's use
// permission on an AgentHostTemplate.
type AgentHostTemplateAuthorizer struct {
	Client client.Client
}

// AuthorizeAgentHostTemplate submits a SubjectAccessReview for verb use on the template.
func (a AgentHostTemplateAuthorizer) AuthorizeAgentHostTemplate(
	ctx context.Context,
	namespace, name string,
	caller authenticationv1.UserInfo,
) error {
	return authorizeUse(ctx, a.Client, "core.orka.ai", "agenthosttemplates", "agent host template", namespace, name, caller)
}

// AuthorizeSecretRead checks that the caller may read the Secret an AgentHost
// names as its GitHub token source, so a host can only carry a credential its
// creator could read.
func (a AgentHostTemplateAuthorizer) AuthorizeSecretRead(
	ctx context.Context,
	namespace, name string,
	caller authenticationv1.UserInfo,
) error {
	return authorizeVerb(ctx, a.Client, "get", "", "secrets", "secret", namespace, name, caller)
}
