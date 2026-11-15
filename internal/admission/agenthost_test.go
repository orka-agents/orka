/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

type capturedAgentHostAuth struct {
	templates, secrets []string
	caller             authenticationv1.UserInfo
	templateErr        error
	secretErr          error
}

func (a *capturedAgentHostAuth) AuthorizeAgentHostTemplate(
	_ context.Context, namespace, name string, caller authenticationv1.UserInfo,
) error {
	a.templates = append(a.templates, namespace+"/"+name)
	a.caller = caller
	return a.templateErr
}

func (a *capturedAgentHostAuth) AuthorizeSecretRead(
	_ context.Context, namespace, name string, caller authenticationv1.UserInfo,
) error {
	a.secrets = append(a.secrets, namespace+"/"+name)
	a.caller = caller
	return a.secretErr
}

func agentHostObject(t *testing.T, secret string) []byte {
	t.Helper()
	return agentHostObjectWithKey(t, secret, "")
}

func agentHostObjectWithKey(t *testing.T, secret, key string) []byte {
	t.Helper()
	host := &corev1alpha1.AgentHost{
		TypeMeta:   metav1.TypeMeta{APIVersion: "core.orka.ai/v1alpha1", Kind: "AgentHost"},
		ObjectMeta: metav1.ObjectMeta{Name: "alice", Namespace: "team"},
		Spec:       corev1alpha1.AgentHostSpec{TemplateRef: corev1alpha1.AgentHostTemplateReference{Name: "copilotd"}},
	}
	if secret != "" {
		host.Spec.GitHubTokenSecretRef = &corev1alpha1.AgentHostSecretKeyReference{Name: secret, Key: key}
	}
	raw, err := json.Marshal(host)
	require.NoError(t, err)
	return raw
}

func agentHostRequest(t *testing.T, operation admissionv1.Operation, secret, oldSecret string) ctrladmission.Request {
	t.Helper()
	request := ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: operation,
		Namespace: "team",
		Object:    runtime.RawExtension{Raw: agentHostObject(t, secret)},
		UserInfo:  authenticationv1.UserInfo{Username: "alice@example.com", Groups: []string{"team"}},
	}}
	if operation == admissionv1.Update {
		request.OldObject = runtime.RawExtension{Raw: agentHostObject(t, oldSecret)}
	}
	return request
}

func newAgentHostValidator(t *testing.T, authorizer AgentHostAuthorizer) *AgentHostValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	return &AgentHostValidator{decoder: ctrladmission.NewDecoder(scheme), authorizer: authorizer}
}

func TestAgentHostCreateChecksTemplateAndSecretAsTheCaller(t *testing.T) {
	authorizer := &capturedAgentHostAuth{}
	response := newAgentHostValidator(t, authorizer).Handle(context.Background(),
		agentHostRequest(t, admissionv1.Create, "alice-github", ""))
	require.True(t, response.Allowed, response.Result)
	require.Equal(t, []string{"team/copilotd"}, authorizer.templates)
	require.Equal(t, []string{"team/alice-github"}, authorizer.secrets)
	require.Equal(t, "alice@example.com", authorizer.caller.Username)
}

func TestAgentHostCreateDeniedWithoutTemplateUse(t *testing.T) {
	authorizer := &capturedAgentHostAuth{templateErr: errors.New("access denied")}
	response := newAgentHostValidator(t, authorizer).Handle(context.Background(),
		agentHostRequest(t, admissionv1.Create, "", ""))
	require.False(t, response.Allowed)
}

func TestAgentHostCannotNameASecretTheCallerCannotRead(t *testing.T) {
	authorizer := &capturedAgentHostAuth{secretErr: errors.New("access denied")}
	validator := newAgentHostValidator(t, authorizer)
	require.False(t, validator.Handle(context.Background(), agentHostRequest(t, admissionv1.Create, "bob-github", "")).Allowed)
	// Pointing a running host at another Secret is checked too.
	require.False(t, validator.Handle(context.Background(),
		agentHostRequest(t, admissionv1.Update, "bob-github", "alice-github")).Allowed)
	require.False(t, validator.Handle(context.Background(),
		agentHostRequest(t, admissionv1.Update, "bob-github", "")).Allowed)
}

func TestAgentHostUpdatesThatSelectNothingNewPass(t *testing.T) {
	authorizer := &capturedAgentHostAuth{templateErr: errors.New("revoked"), secretErr: errors.New("revoked")}
	validator := newAgentHostValidator(t, authorizer)
	for _, request := range []ctrladmission.Request{
		agentHostRequest(t, admissionv1.Update, "alice-github", "alice-github"),
		agentHostRequest(t, admissionv1.Update, "", "alice-github"),
		agentHostRequest(t, admissionv1.Delete, "", ""),
	} {
		require.True(t, validator.Handle(context.Background(), request).Allowed)
	}
	status := agentHostRequest(t, admissionv1.Create, "bob-github", "")
	status.SubResource = "status"
	require.True(t, validator.Handle(context.Background(), status).Allowed)
	require.Empty(t, authorizer.templates)
	require.Empty(t, authorizer.secrets)
}

func TestAgentHostFailsClosedWithoutAnAuthorizer(t *testing.T) {
	response := newAgentHostValidator(t, nil).Handle(context.Background(), agentHostRequest(t, admissionv1.Create, "", ""))
	require.False(t, response.Allowed)
}

func TestAgentHostChangingTheSecretKeyIsChecked(t *testing.T) {
	authorizer := &capturedAgentHostAuth{secretErr: errors.New("access denied")}
	validator := newAgentHostValidator(t, authorizer)
	request := agentHostRequest(t, admissionv1.Update, "alice-github", "alice-github")
	request.Object = runtime.RawExtension{Raw: agentHostObjectWithKey(t, "alice-github", "admin-token")}
	require.False(t, validator.Handle(context.Background(), request).Allowed)
	// An explicit default key is the same selection.
	request.Object = runtime.RawExtension{Raw: agentHostObjectWithKey(t, "alice-github", "token")}
	require.True(t, validator.Handle(context.Background(), request).Allowed)
}

const testControllerUsername = "system:serviceaccount:orka-system:orka-controller-manager"

func connectionHostRequest(t *testing.T, username, template string) ctrladmission.Request {
	t.Helper()
	host := &corev1alpha1.AgentHost{
		TypeMeta:   metav1.TypeMeta{APIVersion: "core.orka.ai/v1alpha1", Kind: "AgentHost"},
		ObjectMeta: metav1.ObjectMeta{Name: "gh-alice", Namespace: "team"},
		Spec: corev1alpha1.AgentHostSpec{
			TemplateRef: corev1alpha1.AgentHostTemplateReference{Name: template},
			GitHubTokenFrom: &corev1alpha1.AgentHostTokenSource{
				ConnectionRef: corev1alpha1.LocalObjectReference{Name: "github-alice"},
			},
		},
	}
	raw, err := json.Marshal(host)
	require.NoError(t, err)
	return ctrladmission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create,
		Namespace: "team",
		Object:    runtime.RawExtension{Raw: raw},
		UserInfo:  authenticationv1.UserInfo{Username: username},
	}}
}

func newConnectionHostValidator(t *testing.T, objects ...client.Object) (*AgentHostValidator, *capturedAgentHostAuth) {
	t.Helper()
	validator := newAgentHostValidator(t, &capturedAgentHostAuth{templateErr: errors.New("no template use")})
	validator.controllers = ExecutionModeConfig{ControllerUsernames: []string{testControllerUsername}}
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	validator.reader = fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return validator, validator.authorizer.(*capturedAgentHostAuth)
}

func agentHostProviderObjects(template string) []client.Object {
	return []client.Object{
		&corev1alpha1.Connection{
			ObjectMeta: metav1.ObjectMeta{Name: "github-alice", Namespace: "team"},
			Spec:       corev1alpha1.ConnectionSpec{ProviderRef: corev1alpha1.LocalObjectReference{Name: "github"}},
		},
		&corev1alpha1.ConnectorProvider{
			ObjectMeta: metav1.ObjectMeta{Name: "github", Namespace: "team"},
			Spec: corev1alpha1.ConnectorProviderSpec{
				AgentHost: &corev1alpha1.ConnectorAgentHost{TemplateRef: corev1alpha1.LocalObjectReference{Name: template}},
			},
		},
	}
}

// Only the controller creates a host fed by a Connection, from the template
// its provider names, which needs no template use grant.
func TestAgentHostFromAConnectionIsTheControllersOnly(t *testing.T) {
	validator, authorizer := newConnectionHostValidator(t, agentHostProviderObjects("copilotd")...)
	response := validator.Handle(context.Background(), connectionHostRequest(t, testControllerUsername, "copilotd"))
	require.True(t, response.Allowed, response.Result)
	require.Empty(t, authorizer.templates)

	require.False(t, validator.Handle(context.Background(), connectionHostRequest(t, "alice@example.com", "copilotd")).Allowed,
		"a person cannot point a host at a Connection")
	require.False(t, validator.Handle(context.Background(), connectionHostRequest(t, testControllerUsername, "other")).Allowed,
		"the template must be the one the provider names")

	other, _ := newConnectionHostValidator(t, agentHostProviderObjects("other")...)
	require.False(t, other.Handle(context.Background(), connectionHostRequest(t, testControllerUsername, "copilotd")).Allowed)
	missing, _ := newConnectionHostValidator(t)
	require.False(t, missing.Handle(context.Background(), connectionHostRequest(t, testControllerUsername, "copilotd")).Allowed)
}
