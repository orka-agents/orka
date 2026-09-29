/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/tools"
)

// linkedBuiltinAccounts resolves the requester's linked account for catalog
// built-in tools the controller executes on an ACP Task's behalf. It reads
// only the bindings frozen into the Task's execution snapshot: a built-in
// with no frozen link keeps the Task's own credentials, and a frozen link
// that cannot be used now fails the call rather than falling back.
type linkedBuiltinAccounts struct {
	source    outboundaccess.ConnectionCredentialSource
	namespace string
	requester *corev1alpha1.RequestedBy
	frozen    map[string]outboundaccess.FrozenConnection
	// required refuses a catalog built-in with no frozen link instead of
	// reporting it unbound: the broker offers such tools only through the
	// link, so a call without one never runs on other credentials.
	required bool
}

var _ tools.LinkedAccountCredentials = linkedBuiltinAccounts{}

// BuiltinToolCredential implements tools.LinkedAccountCredentials.
func (l linkedBuiltinAccounts) BuiltinToolCredential(ctx context.Context, toolName string) (tools.LinkedAccountCredential, bool, error) {
	class, linked := connectors.BuiltinConnectorToolClass(toolName)
	if !linked {
		return tools.LinkedAccountCredential{}, false, nil
	}
	frozen, bound := l.frozen[outboundaccess.BuiltinConnectionKey(toolName)]
	if !bound || strings.TrimSpace(frozen.UID) == "" {
		if l.required {
			return tools.LinkedAccountCredential{}, false, fmt.Errorf("%s requires the requester's linked account, and none was bound when the task was dispatched", toolName)
		}
		return tools.LinkedAccountCredential{}, false, nil
	}
	if l.source == nil {
		return tools.LinkedAccountCredential{}, false, errors.New("linked accounts are resolved only in the controller")
	}
	if l.requester == nil || strings.TrimSpace(l.requester.Issuer) == "" || strings.TrimSpace(l.requester.Subject) == "" {
		return tools.LinkedAccountCredential{}, false, errors.New("the task carries no verified requester")
	}
	if strings.TrimSpace(frozen.Provider) == "" {
		return tools.LinkedAccountCredential{}, false, fmt.Errorf("the binding frozen for %s names no provider", toolName)
	}
	// The catalog's bound for the call rides with the binding, so the
	// credential source refreshes a token that would expire before the
	// call could finish, whatever the caller's own polling arguments.
	timeout, _ := connectors.BuiltinConnectorToolTimeout(toolName)
	binding := outboundaccess.ToolBinding{
		Name: toolName, Class: corev1alpha1.AgentRuntimeBrokeredToolClass(class), Builtin: true, Timeout: timeout, TimeoutSet: true,
	}
	credential, err := l.source.ResolveConnectionCredential(ctx, outboundaccess.ConnectionCredentialRequest{
		Namespace: l.namespace,
		Provider:  frozen.Provider,
		Issuer:    l.requester.Issuer,
		Subject:   l.requester.Subject,
		Frozen:    frozen,
		Tool:      binding,
	})
	if err != nil {
		return tools.LinkedAccountCredential{}, false, err
	}
	if strings.TrimSpace(credential.AccessToken) == "" {
		return tools.LinkedAccountCredential{}, false, errors.New("the connection credential source returned an empty credential")
	}
	if class == corev1alpha1.ConnectorToolClassWrite && credential.Mode != corev1alpha1.ConnectionModeReadWrite {
		return tools.LinkedAccountCredential{}, false, errors.New("the connection is readOnly; write tools are not available")
	}
	return tools.LinkedAccountCredential{
		AccessToken: credential.AccessToken, Provider: frozen.Provider, ConnectionUID: credential.ConnectionUID,
	}, true, nil
}

// liveLinkedAccounts resolves a signed-in person's linked account for
// catalog built-ins the API executes for them right now (chat and the
// compatibility proxies), where there is no dispatch and so no snapshot:
// the API verified the identity, and the Connection is read live and
// bound as it is at that moment. Only a built-in the person has never
// linked keeps the tool's own credential path, as chat always had. An
// existing link that cannot be used now fails the call rather than
// running on other credentials, and so does a write-class built-in:
// these surfaces execute tools directly, with no approval gate, so a
// linked write is available only through a dispatched Task.
type liveLinkedAccounts struct {
	reader    client.Reader
	registry  *tools.Registry
	source    outboundaccess.ConnectionCredentialSource
	namespace string
	requester *corev1alpha1.RequestedBy
}

// LiveLinkedAccounts returns the linked-account resolver for one signed-in
// person in namespace, or nil when nothing can be resolved (no source,
// no reader, or no personal identity).
func LiveLinkedAccounts(reader client.Reader, registry *tools.Registry, source outboundaccess.ConnectionCredentialSource, namespace string, requester *corev1alpha1.RequestedBy) tools.LinkedAccountCredentials {
	if reader == nil || source == nil || requester == nil ||
		strings.TrimSpace(requester.Issuer) == "" || strings.TrimSpace(requester.Subject) == "" || strings.TrimSpace(namespace) == "" {
		return nil
	}
	return liveLinkedAccounts{reader: reader, registry: registry, source: source, namespace: namespace, requester: requester}
}

// BuiltinToolCredential implements tools.LinkedAccountCredentials.
func (l liveLinkedAccounts) BuiltinToolCredential(ctx context.Context, toolName string) (tools.LinkedAccountCredential, bool, error) {
	class, linked := connectors.BuiltinConnectorToolClass(toolName)
	if !linked {
		return tools.LinkedAccountCredential{}, false, nil
	}
	infos, err := classifyConnectorTools(ctx, l.reader, l.registry, l.namespace, []string{toolName}, connectorScope{builtins: true})
	if err != nil {
		return tools.LinkedAccountCredential{}, false, err
	}
	info, declared := infos[toolName]
	if !declared {
		// No accepted provider declares the tool. A provider that declares
		// it but is not accepted right now (its spec changed and the
		// conditions lag) still owns the person's link: that link is bound
		// but unusable, never a reason to fall back to other credentials.
		return l.unacceptedProviderLink(ctx, toolName)
	}
	connection := &corev1alpha1.Connection{}
	name := connectors.ConnectionName(info.Provider, l.requester.Issuer, l.requester.Subject)
	if err := l.reader.Get(ctx, client.ObjectKey{Namespace: l.namespace, Name: name}, connection); err != nil {
		if apierrors.IsNotFound(err) {
			return tools.LinkedAccountCredential{}, false, nil
		}
		return tools.LinkedAccountCredential{}, false, fmt.Errorf("load connection %q: %w", name, err)
	}
	if class == corev1alpha1.ConnectorToolClassWrite {
		return tools.LinkedAccountCredential{}, false, fmt.Errorf(
			"%s writes through your linked %s account only from a task, where the write waits for approval; it is not available here",
			toolName, info.Provider)
	}
	if !connectionReadyFor(connection, l.requester, info.Provider) {
		return tools.LinkedAccountCredential{}, false, fmt.Errorf(
			"your linked %s account (%s) is not usable right now: relink it under Settings > Connectors before using %s",
			info.Provider, liveConnectionState(connection), toolName)
	}
	bound := linkedBuiltinAccounts{
		source: l.source, namespace: l.namespace, requester: l.requester, required: true,
		frozen: map[string]outboundaccess.FrozenConnection{outboundaccess.BuiltinConnectionKey(toolName): {
			UID: string(connection.UID), Generation: connection.Generation, GrantSequence: connection.Status.GrantSequence, Provider: info.Provider,
		}},
	}
	return bound.BuiltinToolCredential(ctx, toolName)
}

// unacceptedProviderLink reports the person's link to a provider that
// declares toolName without being accepted: an error when such a link
// exists, unbound when no provider declares the tool or none is linked.
func (l liveLinkedAccounts) unacceptedProviderLink(ctx context.Context, toolName string) (tools.LinkedAccountCredential, bool, error) {
	providers := &corev1alpha1.ConnectorProviderList{}
	if err := l.reader.List(ctx, providers, client.InNamespace(l.namespace)); err != nil {
		return tools.LinkedAccountCredential{}, false, fmt.Errorf("list connector providers: %w", err)
	}
	for i := range providers.Items {
		provider := &providers.Items[i]
		if _, declares := connectors.DeclaresBuiltinTool(provider, toolName); !declares {
			continue
		}
		connection := &corev1alpha1.Connection{}
		name := connectors.ConnectionName(provider.Name, l.requester.Issuer, l.requester.Subject)
		if err := l.reader.Get(ctx, client.ObjectKey{Namespace: l.namespace, Name: name}, connection); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return tools.LinkedAccountCredential{}, false, fmt.Errorf("load connection %q: %w", name, err)
		}
		if connection.Spec.Subject.Issuer != l.requester.Issuer || connection.Spec.Subject.Subject != l.requester.Subject ||
			connection.Spec.ProviderRef.Name != provider.Name {
			continue
		}
		return tools.LinkedAccountCredential{}, false, fmt.Errorf(
			"your linked %s account cannot be used right now: the provider is not accepted (it may be mid-reconcile); retry shortly before using %s",
			provider.Name, toolName)
	}
	// A link whose provider was removed keeps its tokens until the person
	// disconnects it. Whether that provider declared toolName can no longer
	// be known, so the link is bound and unusable rather than a reason to
	// run on other credentials.
	owned, err := connectors.ListSubjectConnections(ctx, l.reader, l.namespace, l.requester)
	if err != nil {
		return tools.LinkedAccountCredential{}, false, fmt.Errorf("list the requester's connections: %w", err)
	}
	configured := make(map[string]struct{}, len(providers.Items))
	for i := range providers.Items {
		configured[providers.Items[i].Name] = struct{}{}
	}
	for i := range owned {
		if _, ok := configured[owned[i].Spec.ProviderRef.Name]; ok {
			continue
		}
		return tools.LinkedAccountCredential{}, false, fmt.Errorf(
			"your linked %s account's provider is no longer configured; disconnect it under Settings > Connectors before using %s",
			owned[i].Spec.ProviderRef.Name, toolName)
	}
	return tools.LinkedAccountCredential{}, false, nil
}

// liveConnectionState words why an existing Connection is not usable.
func liveConnectionState(connection *corev1alpha1.Connection) string {
	switch {
	case connection == nil:
		return "missing"
	case !connection.DeletionTimestamp.IsZero():
		return "being deleted"
	case strings.TrimSpace(connection.Status.State) != "":
		return connection.Status.State
	default:
		return "not ready"
	}
}
