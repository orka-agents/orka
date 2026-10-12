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

// ListConnectionsToolName is the tool that shows a person their linked
// accounts and the providers they could still connect.
const ListConnectionsToolName = "list_connections"

// ConnectorSettingsPath is where a person links accounts in the dashboard.
const ConnectorSettingsPath = "/settings/connectors"

// ListConnectionsTool lists the requester's Connections and the connector
// providers available to link. It reads through the controller (never the
// caller's own Kubernetes permissions) and reports only the Connections
// whose subject is the verified requester; it never returns token material.
type ListConnectionsTool struct{}

// Name returns the tool name.
func (t *ListConnectionsTool) Name() string { return ListConnectionsToolName }

// Description returns the tool description.
func (t *ListConnectionsTool) Description() string {
	return "List the accounts the person who started this work has linked (for example GitHub), whether each is ready and readOnly or readWrite, " +
		"and the connector providers they could still link. Use it before a tool that acts as the person; when a provider is not linked, " +
		"ask the person to connect it in the dashboard at " + ConnectorSettingsPath + " instead of trying another credential."
}

// Parameters returns the JSON schema for tool parameters.
func (t *ListConnectionsTool) Parameters() json.RawMessage {
	return mustMarshalSchema(map[string]any{jsonSchemaTypeField: jsonSchemaTypeObject, jsonSchemaPropertiesField: map[string]any{}})
}

// ListConnectionsResult is the tool's result.
type ListConnectionsResult struct {
	Connections  []LinkedConnectionSummary  `json:"connections"`
	Available    []ConnectorProviderSummary `json:"available"`
	SettingsPath string                     `json:"settingsPath"`
}

// LinkedConnectionSummary is one of the requester's Connections.
type LinkedConnectionSummary struct {
	Provider    string   `json:"provider"`
	DisplayName string   `json:"displayName"`
	Mode        string   `json:"mode"`
	State       string   `json:"state"`
	Ready       bool     `json:"ready"`
	LinkedAt    string   `json:"linkedAt,omitempty"`
	Message     string   `json:"message,omitempty"`
	Tools       []string `json:"tools"`
	// ProviderMissing marks a link whose ConnectorProvider was removed.
	ProviderMissing bool `json:"providerMissing,omitempty"`
	// Deleting marks a link whose disconnect is still finishing.
	Deleting bool `json:"deleting,omitempty"`
}

// ConnectorProviderSummary is a provider the requester has not linked.
type ConnectorProviderSummary struct {
	Provider    string   `json:"provider"`
	DisplayName string   `json:"displayName"`
	Ready       bool     `json:"ready"`
	Tools       []string `json:"tools"`
}

// Execute lists the requester's Connections and the remaining providers.
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
	// The person's Connections are listed on their own, so a link whose
	// provider was removed (it keeps its tokens until disconnected) is
	// still reported, as unusable, rather than silently dropped. Ownership
	// comes from spec.subject, not the index label, the same listing
	// credential resolution uses, so a link created outside the API or with
	// its label stripped is never reported as missing.
	owned, err := connectors.ListSubjectConnectionsAuthoritative(ctx, reader, tc.Namespace, requester)
	if err != nil {
		return classifyChatK8sErr(err)
	}
	// Every link is kept: a person can hold several Connections to one
	// provider (created outside the API under their own names), and
	// credential resolution refuses the tool while duplicates exist, so
	// each one is reported rather than one hiding the rest.
	byProvider := make(map[string][]*corev1alpha1.Connection, len(owned))
	for i := range owned {
		byProvider[owned[i].Spec.ProviderRef.Name] = append(byProvider[owned[i].Spec.ProviderRef.Name], &owned[i])
	}
	result := ListConnectionsResult{
		Connections: []LinkedConnectionSummary{}, Available: []ConnectorProviderSummary{}, SettingsPath: ConnectorSettingsPath,
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
			summary := linkedConnectionSummary(connection, displayName, toolNames, false)
			switch {
			case len(links) > 1:
				// Resolution refuses the tool while the person holds more
				// than one link to the provider; each is reported unusable.
				summary.Ready = false
				summary.Message = "you hold several links to this provider; disconnect the extra ones under " + ConnectorSettingsPath + " before its tools can run"
			case !connectors.ProviderAccepted(provider):
				// Credential resolution refuses a link whose provider is not
				// accepted, so the listing says so instead of advertising a
				// link the Connection's own conditions have not caught up on.
				summary.Ready = false
				summary.Message = "the provider is not accepted right now (its spec changed or its references are invalid); the link cannot be used until it is"
			case summary.Ready && !connectors.ConsentMatchesProvider(connection, provider):
				// The same checks credential resolution applies: a provider
				// changed since consent refuses the token at once, before the
				// Connection's conditions catch up.
				summary.Ready = false
				summary.Message = "the provider changed since you consented; relink it under " + ConnectorSettingsPath + " before its tools can run"
			case summary.Ready && !connectors.ScopesCover(connection.Status.GrantedScopes, connectors.ScopesForMode(provider, summary.Mode)):
				summary.Ready = false
				summary.Message = "the provider now requires scopes this link was not granted; relink it under " + ConnectorSettingsPath + " before its tools can run"
			}
			result.Connections = append(result.Connections, summary)
		}
	}
	for _, connection := range owned {
		for _, retained := range byProvider[connection.Spec.ProviderRef.Name] {
			if retained.Name == connection.Name {
				result.Connections = append(result.Connections, linkedConnectionSummary(retained, connection.Spec.ProviderRef.Name, []string{}, true))
			}
		}
	}
	return ChatToolSuccess(result)
}

// linkedConnectionSummary describes one of the person's links. A link
// whose provider is gone is never ready and says so.
func linkedConnectionSummary(connection *corev1alpha1.Connection, displayName string, toolNames []string, providerMissing bool) LinkedConnectionSummary {
	summary := LinkedConnectionSummary{
		Provider: connection.Spec.ProviderRef.Name, DisplayName: displayName, Mode: connection.Spec.Mode, State: connection.Status.State,
		Ready: !providerMissing && connectors.ConnectionLinked(connection) && connection.DeletionTimestamp.IsZero(), Tools: toolNames,
		ProviderMissing: providerMissing,
	}
	if summary.Mode == "" {
		summary.Mode = corev1alpha1.ConnectionModeReadOnly
	}
	// A Connection the controller has not reconciled yet has no state;
	// the API view calls that Pending, and so does this listing.
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
		summary.Message = "the provider is no longer configured; this link cannot be used and should be disconnected under " + ConnectorSettingsPath
	}
	// A disconnect in progress (the finalizer revokes tokens first, and may
	// retry) is neither Ready nor something to relink.
	if !connection.DeletionTimestamp.IsZero() {
		summary.Deleting = true
		summary.State = "Disconnecting"
		summary.Message = "this link is being disconnected; its tokens are being revoked and it will disappear"
	}
	return summary
}
