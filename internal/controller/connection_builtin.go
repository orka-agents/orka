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
	binding := outboundaccess.ToolBinding{Name: toolName, Class: corev1alpha1.AgentRuntimeBrokeredToolClass(class), Builtin: true}
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
