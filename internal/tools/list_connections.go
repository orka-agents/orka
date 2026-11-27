/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package tools

import (
	"context"
	"encoding/json"
	"strings"

	"k8s.io/apimachinery/pkg/api/meta"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/connectors"
)

const ListConnectionsToolName = "list_connections"

const ConnectorSettingsPath = "/settings/connectors"

type ListConnectionsTool struct{}

func (t *ListConnectionsTool) Name() string { return ListConnectionsToolName }

func (t *ListConnectionsTool) Description() string {
	return "List the accounts the person who started this work has linked (for example GitHub), whether each is ready and readOnly or readWrite, " +
		"and the connector providers they could still link. Use it before a tool that acts as the person; when a provider is not linked, " +
		"ask the person to link it with `orka connect <provider> --namespace <namespace>` using the result's namespace, or on the dashboard page " +
		"in settingsPath when the result includes one, " +
		"instead of trying another credential."
}

func (t *ListConnectionsTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{}})
}

type ListConnectionsResult struct {
	Connections  []LinkedConnectionSummary  `json:"connections"`
	Available    []ConnectorProviderSummary `json:"available"`
	Namespace    string                     `json:"namespace,omitempty"`
	SettingsPath string                     `json:"settingsPath,omitempty"`
}

type LinkedConnectionSummary struct {
	Provider        string   `json:"provider"`
	DisplayName     string   `json:"displayName"`
	Mode            string   `json:"mode"`
	State           string   `json:"state"`
	Ready           bool     `json:"ready"`
	LinkedAt        string   `json:"linkedAt,omitempty"`
	Message         string   `json:"message,omitempty"`
	Tools           []string `json:"tools"`
	ProviderMissing bool     `json:"providerMissing,omitempty"`
	Deleting        bool     `json:"deleting,omitempty"`
}

type ConnectorProviderSummary struct {
	Provider    string   `json:"provider"`
	DisplayName string   `json:"displayName"`
	Ready       bool     `json:"ready"`
	Tools       []string `json:"tools"`
}

func (t *ListConnectionsTool) Execute(ctx context.Context, _ json.RawMessage) (string, error) {
	tc := GetToolContext(ctx)
	if tc == nil {
		return ChatToolErrorResult(internalErrorType, "missing tool context", "")
	}
	if tc.AuthorizeConnectorRead != nil {
		if denied := tc.AuthorizeConnectorRead(); denied != nil {
			return ChatToolErrorResult(denied.Type, denied.Message, denied.Suggestion)
		}
	}
	requester := tc.Requester
	if requester == nil || strings.TrimSpace(requester.Issuer) == "" || strings.TrimSpace(requester.Subject) == "" {
		return ChatToolErrorResult("no_identity", "no verified person is attached to this request, so there are no linked accounts to list",
			"Linked accounts belong to a signed-in person (OIDC or context token); service accounts and unverified tasks have none")
	}
	var reader = tc.PolicyReader
	if reader == nil {
		reader = tc.Client
	}
	if reader == nil {
		return ChatToolErrorResult(internalErrorType, "no reader is available for connector providers", "")
	}
	providers := &corev1alpha1.ConnectorProviderList{}
	if err := reader.List(ctx, providers, client.InNamespace(tc.Namespace)); err != nil {
		return classifyChatK8sErr(err)
	}
	owned, err := connectors.ListSubjectConnectionsAuthoritative(ctx, reader, tc.Namespace, requester)
	if err != nil {
		return classifyChatK8sErr(err)
	}
	byProvider := make(map[string][]*corev1alpha1.Connection, len(owned))
	for i := range owned {
		byProvider[owned[i].Spec.ProviderRef.Name] = append(byProvider[owned[i].Spec.ProviderRef.Name], &owned[i])
	}
	result := ListConnectionsResult{
		Connections: []LinkedConnectionSummary{}, Available: []ConnectorProviderSummary{}, Namespace: tc.Namespace,
	}
	if tc.DashboardEnabled {
		result.SettingsPath = ConnectorSettingsPath
	}
	for i := range providers.Items {
		provider := &providers.Items[i]
		displayName := strings.TrimSpace(provider.Spec.DisplayName)
		if displayName == "" {
			displayName = provider.Name
		}
		toolNames := make([]string, 0, len(provider.Spec.Tools))
		for _, tool := range provider.Spec.Tools {
			toolNames = append(toolNames, tool.Name+" ("+string(tool.Class)+")")
		}
		links, linked := byProvider[provider.Name]
		if !linked {
			result.Available = append(result.Available, ConnectorProviderSummary{
				Provider: provider.Name, DisplayName: displayName, Ready: connectors.ProviderAccepted(provider), Tools: toolNames,
			})
			continue
		}
		delete(byProvider, provider.Name)
		for _, connection := range links {
			summary := linkedConnectionSummary(connection, displayName, toolNames, false, tc.DashboardEnabled)
			switch {
			case len(links) > 1:
				summary.Ready = false
				summary.Message = "you hold several links to this provider; disconnect the extra ones " + disconnectGuidance(tc.DashboardEnabled, connection.Namespace) + " before its tools can run"
			case !connectors.ProviderAccepted(provider):
				summary.Ready = false
				summary.Message = "the provider is not accepted right now (its spec changed or its references are invalid); the link cannot be used until it is"
			case summary.Ready && !connectors.ConsentMatchesProvider(connection, provider):
				summary.Ready = false
				summary.Message = "the provider changed since you consented; relink it " + relinkGuidance(tc.DashboardEnabled, connection, requester, summary.Mode) + " before its tools can run"
			case summary.Ready && !connectors.ScopesCover(connection.Status.GrantedScopes, connectors.ScopesForMode(provider, summary.Mode)):
				summary.Ready = false
				summary.Message = "the provider now requires scopes this link was not granted; relink it " + relinkGuidance(tc.DashboardEnabled, connection, requester, summary.Mode) + " before its tools can run"
			}
			result.Connections = append(result.Connections, summary)
		}
	}
	for _, connection := range owned {
		for _, retained := range byProvider[connection.Spec.ProviderRef.Name] {
			if retained.Name == connection.Name {
				result.Connections = append(result.Connections, linkedConnectionSummary(retained, connection.Spec.ProviderRef.Name, []string{}, true, tc.DashboardEnabled))
			}
		}
	}
	return ChatToolSuccess(result)
}

func linkedConnectionSummary(connection *corev1alpha1.Connection, displayName string, toolNames []string, providerMissing, dashboard bool) LinkedConnectionSummary {
	summary := LinkedConnectionSummary{
		Provider: connection.Spec.ProviderRef.Name, DisplayName: displayName, Mode: connection.Spec.Mode, State: connection.Status.State,
		Ready: !providerMissing && connectors.ConnectionLinked(connection) && connection.DeletionTimestamp.IsZero(), Tools: toolNames,
		ProviderMissing: providerMissing,
	}
	if summary.Mode == "" {
		summary.Mode = corev1alpha1.ConnectionModeReadOnly
	}
	if summary.State == "" {
		summary.State = corev1alpha1.ConnectionStatePending
	}
	if connection.Status.LinkedAt != nil {
		summary.LinkedAt = connection.Status.LinkedAt.UTC().Format("2006-01-02T15:04:05Z")
	}
	if condition := meta.FindStatusCondition(connection.Status.Conditions, corev1alpha1.ConnectionConditionReady); condition != nil && condition.Status != "True" {
		summary.Message = condition.Message
	}
	if providerMissing {
		summary.Message = "the provider is no longer configured; this link cannot be used and should be disconnected " + disconnectGuidance(dashboard, connection.Namespace)
	}
	if !connection.DeletionTimestamp.IsZero() {
		summary.Deleting = true
		summary.State = "Disconnecting"
		summary.Message = "this link is being disconnected; its tokens are being revoked and it will disappear"
	}
	return summary
}

func relinkGuidance(dashboard bool, connection *corev1alpha1.Connection, requester *corev1alpha1.RequestedBy, mode string) string {
	if dashboard {
		return "under " + ConnectorSettingsPath
	}
	provider := connection.Spec.ProviderRef.Name
	in := cliNamespaceFlag(connection.Namespace)
	command := "orka connect " + provider + in
	if mode == corev1alpha1.ConnectionModeReadWrite {
		command += " --mode " + mode
	}
	if connection.Name != connectors.ConnectionName(provider, requester.Issuer, requester.Subject) {
		return "by removing it with `orka connection delete " + connection.Name + in + "` and, once `orka connection list" + in +
			"` no longer shows it, running `" + command + "`"
	}
	return "with `" + command + "`"
}

func disconnectGuidance(dashboard bool, namespace string) string {
	if dashboard {
		return "under " + ConnectorSettingsPath
	}
	in := cliNamespaceFlag(namespace)
	return "with `orka connection delete <name>" + in + "` (`orka connection list" + in + "` shows the names)"
}

func cliNamespaceFlag(namespace string) string {
	if strings.TrimSpace(namespace) == "" {
		return ""
	}
	return " --namespace " + namespace
}
