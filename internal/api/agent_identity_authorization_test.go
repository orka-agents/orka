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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func identityBindClientset(allow bool) (*kubefake.Clientset, *[]authorizationv1.ResourceAttributes) {
	clientset, seen, _ := identityBindClientsetWithSubjects(allow)
	return clientset, seen
}

func identityBindClientsetWithSubjects(allow bool) (*kubefake.Clientset, *[]authorizationv1.ResourceAttributes, *[]authorizationv1.SubjectAccessReviewSpec) {
	seen := &[]authorizationv1.ResourceAttributes{}
	subjects := &[]authorizationv1.SubjectAccessReviewSpec{}
	clientset := kubefake.NewClientset()
	clientset.PrependReactor("create", "subjectaccessreviews", func(action k8stesting.Action) (bool, runtime.Object, error) {
		review := action.(k8stesting.CreateAction).GetObject().(*authorizationv1.SubjectAccessReview)
		*seen = append(*seen, *review.Spec.ResourceAttributes)
		subject := review.Spec
		subject.ResourceAttributes = nil
		*subjects = append(*subjects, subject)
		review.Status.Allowed = allow
		return true, review, nil
	})
	return clientset, seen, subjects
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

func TestOnlyTheRequesterApprovesOnTasksThatActForThem(t *testing.T) {
	task := func(actsForRequester bool) *corev1alpha1.Task {
		return &corev1alpha1.Task{
			ObjectMeta: metav1.ObjectMeta{Namespace: "team-a", Name: "work"},
			Spec:       corev1alpha1.TaskSpec{RequestedBy: &corev1alpha1.RequestedBy{Issuer: "https://login.example", Subject: "alice"}},
			Status: corev1alpha1.TaskStatus{AgentExecutionBinding: &corev1alpha1.AgentExecutionBinding{
				Agent: &corev1alpha1.AgentExecutionAgentRef{Identity: &corev1alpha1.AgentExecutionIdentityRef{
					Issuer: "entra", ID: "id", Instance: "entra:t/b", ActsForRequester: actsForRequester,
				}},
			}},
		}
	}
	alice := &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://login.example", Subject: "alice"}
	bob := &UserInfo{AuthType: AuthTypeOIDC, Issuer: "https://login.example", Subject: "bob"}
	admin := &UserInfo{AuthType: AuthTypeTokenReview, Username: "system:admin"}

	require.NoError(t, requireRequesterApprovesDelegatedWrite(task(true), alice))
	requireForbidden(t, requireRequesterApprovesDelegatedWrite(task(true), bob))
	requireForbidden(t, requireRequesterApprovesDelegatedWrite(task(true), admin))
	require.NoError(t, requireRequesterApprovesDelegatedWrite(task(false), bob), "other Tasks keep the existing rules")
	require.NoError(t, requireRequesterApprovesDelegatedWrite(&corev1alpha1.Task{}, bob))
}

func TestAgentIdentityBindReviewsTheSameSubjectAsAgentUse(t *testing.T) {
	agent := identityBoundAgent("33333333-3333-3333-3333-33333333aaaa")
	for name, tc := range map[string]struct {
		user *UserInfo
		want authorizationv1.SubjectAccessReviewSpec
	}{
		"Kubernetes username is kept exactly": {
			user: &UserInfo{Username: " alice ", Groups: []string{"admins"}, AuthType: AuthTypeTokenReview},
			want: authorizationv1.SubjectAccessReviewSpec{User: " alice ", Groups: []string{"admins"}, Extra: map[string]authorizationv1.ExtraValue{}},
		},
		"OIDC caller is its issuer and subject, not its display name": {
			user: &UserInfo{Username: "Alice", Issuer: "https://issuer.example", Subject: "sub-1",
				Groups: []string{"admins"}, AuthType: AuthTypeOIDC},
			want: authorizationv1.SubjectAccessReviewSpec{User: "https://issuer.example#sub-1",
				Groups: []string{"https://issuer.example#admins"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			wantSpec, ok := agentUseReviewSubject(tc.user)
			require.True(t, ok)
			require.Equal(t, tc.want, wantSpec)
			clientset, _, subjects := identityBindClientsetWithSubjects(true)
			require.NoError(t, authorizeAgentIdentityBind(context.Background(), clientset, tc.user, agent, nil))
			require.Equal(t, []authorizationv1.SubjectAccessReviewSpec{tc.want}, *subjects)
		})
	}
	clientset, seen := identityBindClientset(true)
	requireForbidden(t, authorizeAgentIdentityBind(context.Background(), clientset,
		&UserInfo{Username: "Alice", AuthType: AuthTypeOIDC}, agent, nil)) // no issuer and subject: no reviewable identity
	require.Empty(t, *seen)
}
