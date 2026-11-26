/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/provideridentity"
)

// ACPProviderProxyEntra names the Entra identity and upstream origin that the
// provider auth proxy authenticates with in its Entra mode. Built-in ACP
// runtimes reach models only through that proxy, so it, not the Task's
// Provider, decides where an agent Task's model calls go and with which
// credential.
type ACPProviderProxyEntra struct {
	// Identity is the proxy's --upstream-entra-identity.
	Identity string
	// Endpoint is the proxy's upstream origin, such as
	// https://example.openai.azure.com.
	Endpoint string
}

// serves reports whether the proxy authenticates with the Provider's identity
// to the Provider's origin.
func (c ACPProviderProxyEntra) serves(provider *corev1alpha1.Provider) bool {
	if c.Identity == "" || provider.EntraIdentity() != c.Identity {
		return false
	}
	want, err := provideridentity.Origin(c.Endpoint)
	if err != nil {
		return false
	}
	got, err := provideridentity.Origin(provider.Spec.BaseURL)
	return err == nil && got == want
}

// checkAgentTaskKeylessProvider refuses an agent Task whose Agent names a
// keyless Provider that its execution path cannot authenticate. A built-in
// ACP runtime holds no Provider credential: its model calls go through the
// provider auth proxy, so a keyless Provider is admitted only on the ACP path
// and only when the proxy's Entra mode uses the same identity for the same
// origin. Keyed Providers keep their current handling.
func (r *TaskReconciler) checkAgentTaskKeylessProvider(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent, path agentExecutionPath) error {
	if task.Spec.Type != corev1alpha1.TaskTypeAgent || agent == nil || agent.Spec.ProviderRef == nil {
		return nil
	}
	namespace := agent.Spec.ProviderRef.Namespace
	if namespace == "" {
		namespace = agent.Namespace
	}
	provider := &corev1alpha1.Provider{}
	if err := r.Get(ctx, types.NamespacedName{Name: agent.Spec.ProviderRef.Name, Namespace: namespace}, provider); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("failed to get provider: %w", err)
	}
	if !provider.UsesKeylessAuth() {
		return nil
	}
	if path == agentExecutionPathACP && r.ACPProviderProxyEntra.serves(provider) {
		return nil
	}
	return fmt.Errorf("provider %s uses spec.auth; an agent Task reaches it only through a built-in ACP runtime whose provider auth proxy "+
		"uses the same Entra identity for the same endpoint (--acp-provider-proxy-entra-identity, --acp-provider-proxy-entra-endpoint)", provider.Name)
}
