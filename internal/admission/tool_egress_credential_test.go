/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type capturedSecretUse struct {
	capturedClassUse
	secrets   []string
	secretErr error
}

func (a *capturedSecretUse) AuthorizeSecret(_ context.Context, namespace, name string, caller authenticationv1.UserInfo) error {
	a.namespace = namespace
	a.caller = caller
	a.secrets = append(a.secrets, name)
	return a.secretErr
}

func egressCredentialTool(secrets ...string) *corev1alpha1.Tool {
	tool := &corev1alpha1.Tool{ObjectMeta: metav1.ObjectMeta{Namespace: admissionTestNamespace, Name: "github-mcp"},
		Spec: corev1alpha1.ToolSpec{MCP: &corev1alpha1.MCPToolServer{SubstrateActor: &corev1alpha1.SubstrateMCPActor{
			TemplateRef: corev1alpha1.WorkspaceTemplateReference{Name: "mcp"},
		}}}}
	for _, name := range secrets {
		tool.Spec.MCP.SubstrateActor.Egress = append(tool.Spec.MCP.SubstrateActor.Egress, corev1alpha1.SubstrateActorEgressRule{
			Scheme: corev1alpha1.SubstrateActorEgressSchemeHTTPS, Host: name + ".example.com",
			Credential: &corev1alpha1.SubstrateActorEgressCredential{Header: "Authorization", Prefix: "Bearer ", SecretRef: corev1alpha1.SecretKeySelector{Name: name, Key: "token"}},
		})
	}
	return tool
}

func TestToolEgressCredentialRequiresCallerSecretAccess(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	caller := authenticationv1.UserInfo{Username: "alice", Groups: []string{"developers"}}

	authorizer := &capturedSecretUse{}
	validator := newWorkspaceClassUseValidator(scheme, authorizer, workspaceClassTool)
	tool := egressCredentialTool("github", "github")
	response := validator.Handle(context.Background(), workspaceClassAdmissionRequest(t, admissionv1.Create, caller, tool, nil, ""))
	require.True(t, response.Allowed, response.Result.Message)
	require.Equal(t, []string{"github"}, authorizer.secrets)
	require.Equal(t, admissionTestNamespace, authorizer.namespace)
	require.Equal(t, caller, authorizer.caller)

	// Every spec update reauthorizes every referenced Secret.
	authorizer.secrets = nil
	updated := egressCredentialTool("github", "slack")
	response = validator.Handle(context.Background(), workspaceClassAdmissionRequest(t, admissionv1.Update, caller, updated, tool, ""))
	require.True(t, response.Allowed, response.Result.Message)
	require.Equal(t, []string{"github", "slack"}, authorizer.secrets)

	denied := &capturedSecretUse{secretErr: errors.New("cannot get secrets")}
	validator = newWorkspaceClassUseValidator(scheme, denied, workspaceClassTool)
	response = validator.Handle(context.Background(), workspaceClassAdmissionRequest(t, admissionv1.Create, caller, tool, nil, ""))
	require.False(t, response.Allowed)
	require.Contains(t, response.Result.Message, "cannot get secrets")

	// An authorizer that cannot check Secrets fails closed.
	validator = newWorkspaceClassUseValidator(scheme, &capturedClassUse{}, workspaceClassTool)
	response = validator.Handle(context.Background(), workspaceClassAdmissionRequest(t, admissionv1.Create, caller, tool, nil, ""))
	require.False(t, response.Allowed)

	// Tools without egress credentials never invoke Secret authorization.
	authorizer = &capturedSecretUse{}
	validator = newWorkspaceClassUseValidator(scheme, authorizer, workspaceClassTool)
	response = validator.Handle(context.Background(), workspaceClassAdmissionRequest(t, admissionv1.Create, caller, egressCredentialTool(), nil, ""))
	require.True(t, response.Allowed, response.Result.Message)
	require.Empty(t, authorizer.secrets)
}
