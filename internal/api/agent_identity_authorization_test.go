package api

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func identityBindClientset(allow bool) (*kubefake.Clientset, *[]authorizationv1.ResourceAttributes) {
	seen := &[]authorizationv1.ResourceAttributes{}
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		*seen = append(*seen, *review.Spec.ResourceAttributes)
		review.Status.Allowed = allow
		return true, review, nil
	})
	return clientset, seen
}

func identityBoundAgent(id string) *corev1alpha1.Agent {
	agent := &corev1alpha1.Agent{ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "reviewer"}}
	if id != "" {
		agent.Spec.Identity = &corev1alpha1.AgentIdentity{Entra: &corev1alpha1.EntraAgentIdentity{AgentIdentityID: id}}
	}
	return agent
}

func TestAgentIdentityBindRequiresBindOnTheExactIdentity(t *testing.T) {
	user := &UserInfo{Username: "alice", Groups: []string{"admins"}, AuthType: AuthTypeTokenReview}
	denied, seen := identityBindClientset(false)
	requireForbidden(t, authorizeAgentIdentityBind(context.Background(), denied, user,
		identityBoundAgent("33333333-3333-3333-3333-33333333AAAA"), nil))
	require.Equal(t, []authorizationv1.ResourceAttributes{{
		Namespace: "team-a", Verb: "bind", Group: "core.orka.ai", Version: "v1alpha1",
		Resource: "agentidentities", Subresource: "entra", Name: "33333333-3333-3333-3333-33333333aaaa",
	}}, *seen)

	allowed, _ := identityBindClientset(true)
	require.NoError(t, authorizeAgentIdentityBind(context.Background(), allowed, user,
		identityBoundAgent("33333333-3333-3333-3333-33333333aaaa"), nil))
	requireForbidden(t, authorizeAgentIdentityBind(context.Background(), nil, user,
		identityBoundAgent("33333333-3333-3333-3333-33333333aaaa"), nil))
}

func TestAgentIdentityBindSkipsUnchangedAndRemovedIdentities(t *testing.T) {
	user := &UserInfo{Username: "alice", AuthType: AuthTypeTokenReview}
	clientset, seen := identityBindClientset(false)
	bound := identityBoundAgent("33333333-3333-3333-3333-33333333aaaa")
	require.NoError(t, authorizeAgentIdentityBind(context.Background(), clientset, user,
		identityBoundAgent("33333333-3333-3333-3333-33333333AAAA"), bound), "keeping the identity needs no grant")
	require.NoError(t, authorizeAgentIdentityBind(context.Background(), clientset, user, identityBoundAgent(""), bound),
		"removing the identity needs no grant")
	require.Empty(t, *seen)
	requireForbidden(t, authorizeAgentIdentityBind(context.Background(), clientset, user,
		identityBoundAgent("44444444-4444-4444-4444-444444444444"), bound))
}

func TestOnlyTheRequesterApprovesDelegatedWrites(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	delegated := &corev1alpha1.OutboundAccessPolicy{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "storage-as-requester"},
		Spec: corev1alpha1.OutboundAccessPolicySpec{AgentIdentity: &corev1alpha1.AgentIdentityOutboundAccess{
			Mode:      corev1alpha1.OutboundAgentIdentityModeDelegated,
			Entra:     &corev1alpha1.EntraOutboundTarget{Resource: "https://storage.azure.com"},
			Delegated: &corev1alpha1.DelegatedAgentIdentity{ProviderRef: corev1alpha1.LocalObjectReference{Name: "entra"}},
		}},
	}
	self := delegated.DeepCopy()
	self.Name = "graph-as-agent"
	self.Spec.AgentIdentity.Mode = corev1alpha1.OutboundAgentIdentityModeSelf
	self.Spec.AgentIdentity.Delegated = nil
	tool := func(name, policy string) *corev1alpha1.Tool {
		return &corev1alpha1.Tool{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: name},
			Spec: corev1alpha1.ToolSpec{HTTP: &corev1alpha1.HTTPExecution{
				URL: "https://example.invalid", OutboundAccessPolicyRef: &corev1alpha1.LocalObjectReference{Name: policy},
			}},
		}
	}
	kube := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		delegated, self, tool("write_blob", "storage-as-requester"), tool("send_mail", "graph-as-agent"),
	).Build()
	h := NewHandlers(HandlersConfig{Client: kube})
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "work"},
		Spec:       corev1alpha1.TaskSpec{RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://login.example", Subject: "alice"}},
	}
	alice := &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://login.example", Subject: "alice"}
	bob := &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://login.example", Subject: "bob"}
	admin := &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:admin"}

	require.NoError(t, h.requireRequesterApprovesDelegatedWrite(context.Background(), task, "write_blob", alice))
	requireForbidden(t, h.requireRequesterApprovesDelegatedWrite(context.Background(), task, "write_blob", bob))
	requireForbidden(t, h.requireRequesterApprovesDelegatedWrite(context.Background(), task, "write_blob", admin))
	require.NoError(t, h.requireRequesterApprovesDelegatedWrite(context.Background(), task, "send_mail", bob),
		"a Self-mode call is approved under the existing rules")
	require.NoError(t, h.requireRequesterApprovesDelegatedWrite(context.Background(), task, "unknown_tool", bob))
}
