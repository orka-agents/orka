/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package admission

import (
	"context"
	"net/http"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// AgentHostWebhookPath validates what an AgentHost selects: its template and
// its GitHub token Secret.
const AgentHostWebhookPath = "/validate-core-orka-ai-v1alpha1-agenthost"

// AgentHostAuthorizer checks the live admission caller's permissions for an AgentHost.
type AgentHostAuthorizer interface {
	// AuthorizeAgentHostTemplate checks use on one AgentHostTemplate.
	AuthorizeAgentHostTemplate(context.Context, string, string, authenticationv1.UserInfo) error
	// AuthorizeSecretRead checks get on one Secret.
	AuthorizeSecretRead(context.Context, string, string, authenticationv1.UserInfo) error
}

// RegisterAgentHostWebhook registers the fail-closed AgentHost selection
// handler. reader resolves the Connection a controller-made host names, and
// controllers are the identities that may make one.
func RegisterAgentHostWebhook(server webhook.Server, scheme *runtime.Scheme, authorizer AgentHostAuthorizer,
	reader client.Reader, controllers ExecutionModeConfig) {
	server.Register(AgentHostWebhookPath, &ctrladmission.Webhook{
		Handler: &AgentHostValidator{
			decoder: ctrladmission.NewDecoder(scheme), authorizer: authorizer,
			reader: reader, controllers: controllers.normalized(),
		},
	})
}

// AgentHostValidator authorizes an AgentHost's template and token Secret
// using AdmissionRequest.UserInfo, never identity from the submitted object.
type AgentHostValidator struct {
	decoder     ctrladmission.Decoder
	authorizer  AgentHostAuthorizer
	reader      client.Reader
	controllers ExecutionModeConfig
}

// Handle implements admission.Handler.
func (v *AgentHostValidator) Handle(ctx context.Context, req ctrladmission.Request) ctrladmission.Response {
	if req.SubResource != "" || (req.Operation != admissionv1.Create && req.Operation != admissionv1.Update) {
		return ctrladmission.Allowed("not an AgentHost selection")
	}
	host := &corev1alpha1.AgentHost{}
	if err := v.decoder.Decode(req, host); err != nil {
		return ctrladmission.Errored(http.StatusBadRequest, err)
	}
	var previous *corev1alpha1.AgentHost
	if req.Operation == admissionv1.Update {
		previous = &corev1alpha1.AgentHost{}
		if err := v.decoder.DecodeRaw(req.OldObject, previous); err != nil {
			return ctrladmission.Errored(http.StatusBadRequest, err)
		}
	}
	namespace := requestNamespace(req.Namespace, host.Namespace)
	// githubTokenFrom is immutable, so only creation sets it.
	if host.Spec.GitHubTokenFrom != nil && previous == nil {
		return v.admitConnectionHost(ctx, namespace, host, req.UserInfo)
	}
	// templateRef is immutable, so only creation selects a template. Updates
	// that change nothing selected, such as the controller's finalizer
	// removal, must pass after the caller's permission is revoked.
	checkTemplate := previous == nil
	secret := ""
	if ref := host.Spec.GitHubTokenSecretRef; ref != nil {
		if previous == nil || !sameSecretKey(previous.Spec.GitHubTokenSecretRef, ref) {
			secret = strings.TrimSpace(ref.Name)
		}
	}
	if !checkTemplate && secret == "" {
		return ctrladmission.Allowed("no AgentHost selection changed")
	}
	if v.authorizer == nil {
		return ctrladmission.Denied("agent host authorizer is unavailable")
	}
	if checkTemplate {
		name := strings.TrimSpace(host.Spec.TemplateRef.Name)
		if name == "" {
			return ctrladmission.Denied("templateRef.name is required")
		}
		if err := v.authorizer.AuthorizeAgentHostTemplate(ctx, namespace, name, req.UserInfo); err != nil {
			return ctrladmission.Denied("agent host template use authorization failed: " + err.Error())
		}
	}
	if secret != "" {
		// The controller copies the Secret into the host, so only someone who
		// may read it may name it.
		if err := v.authorizer.AuthorizeSecretRead(ctx, namespace, secret, req.UserInfo); err != nil {
			return ctrladmission.Denied("agent host GitHub token Secret authorization failed: " + err.Error())
		}
	}
	return ctrladmission.Allowed("agent host selection authorized")
}

// admitConnectionHost admits a host fed by a Connection: only the controller
// creates one, and only from the template the Connection's provider names.
// Naming it there is the operator's grant, so no template use check applies.
func (v *AgentHostValidator) admitConnectionHost(ctx context.Context, namespace string, host *corev1alpha1.AgentHost,
	user authenticationv1.UserInfo) ctrladmission.Response {
	if !v.controllers.controller(user.Username) {
		return ctrladmission.Denied("only the Orka controller may create an AgentHost that takes its token from a Connection")
	}
	if v.reader == nil {
		return ctrladmission.Denied("agent host Connection reader is unavailable")
	}
	connection := &corev1alpha1.Connection{}
	key := types.NamespacedName{Namespace: namespace, Name: host.Spec.GitHubTokenFrom.ConnectionRef.Name}
	if err := v.reader.Get(ctx, key, connection); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrladmission.Denied("the AgentHost's Connection does not exist")
		}
		return ctrladmission.Errored(http.StatusInternalServerError, err)
	}
	provider := &corev1alpha1.ConnectorProvider{}
	if err := v.reader.Get(ctx, types.NamespacedName{Namespace: namespace, Name: connection.Spec.ProviderRef.Name}, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrladmission.Denied("the Connection's ConnectorProvider does not exist")
		}
		return ctrladmission.Errored(http.StatusInternalServerError, err)
	}
	if provider.Spec.AgentHost == nil || provider.Spec.AgentHost.TemplateRef.Name != host.Spec.TemplateRef.Name {
		return ctrladmission.Denied("the Connection's ConnectorProvider does not serve agent hosts from this template")
	}
	return ctrladmission.Allowed("agent host fed by a Connection authorized")
}

// sameSecretKey reports whether two references select the same Secret data.
// A different key in the same Secret is a new selection too.
func sameSecretKey(a, b *corev1alpha1.AgentHostSecretKeyReference) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Name == b.Name && secretKeyOrDefault(a.Key) == secretKeyOrDefault(b.Key)
}

func secretKeyOrDefault(key string) string {
	if key == "" {
		return "token"
	}
	return key
}
