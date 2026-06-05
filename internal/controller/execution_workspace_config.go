/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
)

const (
	EnvExecutionWorkspaceDefaultProvider = "ORKA_EXECUTION_WORKSPACE_DEFAULT_PROVIDER"

	defaultExecutionWorkspaceProvider = corev1alpha1.WorkspaceProviderAgentSandbox
)

// ExecutionWorkspaceDefaultProviderFromEnv reads the provider-neutral default
// workspace backend. Empty preserves the compatibility default.
func ExecutionWorkspaceDefaultProviderFromEnv(getenv func(string) string) corev1alpha1.WorkspaceProvider {
	if value := strings.TrimSpace(getenv(EnvExecutionWorkspaceDefaultProvider)); value != "" {
		return corev1alpha1.WorkspaceProvider(value)
	}
	return defaultExecutionWorkspaceProvider
}

func executionWorkspaceDefaultProvider(provider corev1alpha1.WorkspaceProvider) corev1alpha1.WorkspaceProvider {
	if provider == "" {
		return defaultExecutionWorkspaceProvider
	}
	return provider
}

// ExecutionWorkspaceRequest is the controller's resolved, validated view of a
// Task execution workspace request. JobBuilder propagates it to workers; the
// worker wrapper owns claim, command execution, status updates, and cleanup.
type ExecutionWorkspaceRequest struct {
	Provider          corev1alpha1.WorkspaceProvider
	RouterURL         string
	TemplateName      string
	TemplateNamespace string
	ClaimNamespace    string
	ClaimName         string
	ReusePolicy       corev1alpha1.WorkspaceReusePolicy
	ReuseKey          string
	CleanupPolicy     corev1alpha1.WorkspaceCleanupPolicy
	Boot              bool
	WarmPoolPolicy    string
	NamespaceStrategy string
	ClaimTimeout      time.Duration
	CommandTimeout    time.Duration

	SubstrateAPIEndpoint               string
	SubstrateAPICAFile                 string
	SubstrateAPIInsecureSkipVerify     bool
	SubstrateRouterURL                 string
	SubstrateActorDNSSuffix            string
	SubstrateBootstrapSecretName       string
	SubstrateBootstrapSecretKey        string
	SubstrateSessionIdentitySecretName string
	SubstrateSessionIdentitySecretKey  string
	SubstrateSessionIdentityRequired   bool
	SubstrateSessionIdentityAudience   string
	SubstrateSessionIdentityAppID      string
	SubstrateSessionIdentityUserID     string
}

// AgentSandboxWorkspaceRequest is kept as a compatibility alias for tests and
// call sites that still use the old agent-sandbox-specific name.
type AgentSandboxWorkspaceRequest = ExecutionWorkspaceRequest

func resolveWorkspaceProvider(ws *corev1alpha1.ExecutionWorkspaceSpec, defaultProvider corev1alpha1.WorkspaceProvider) corev1alpha1.WorkspaceProvider {
	if ws != nil && ws.Provider != "" {
		return ws.Provider
	}
	return executionWorkspaceDefaultProvider(defaultProvider)
}

func supportedWorkspaceProvider(provider corev1alpha1.WorkspaceProvider) bool {
	switch provider {
	case corev1alpha1.WorkspaceProviderAgentSandbox, corev1alpha1.WorkspaceProviderSubstrate:
		return true
	default:
		return false
	}
}

// WorkspaceProviderSupported reports whether provider is a recognized execution
// workspace backend.
func WorkspaceProviderSupported(provider corev1alpha1.WorkspaceProvider) bool {
	return supportedWorkspaceProvider(provider)
}

func deterministicSubstrateSessionActorID(taskNamespace, templateNamespace, templateName, reuseKey string) string {
	parts := []string{
		string(corev1alpha1.WorkspaceProviderSubstrate),
		strings.TrimSpace(taskNamespace),
		strings.TrimSpace(templateNamespace),
		strings.TrimSpace(templateName),
		strings.TrimSpace(reuseKey),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return "orka-s-" + hex.EncodeToString(sum[:])[:32]
}

func deterministicSubstrateTaskActorID(taskUID string, attempt int32) string {
	uid := strings.TrimSpace(taskUID)
	if uid == "" {
		uid = "unknown"
	}
	uid = strings.ToLower(uid)
	sum := sha256.Sum256([]byte(uid))
	uidHash := hex.EncodeToString(sum[:])[:32]
	if attempt <= 0 {
		attempt = 1
	}
	return fmt.Sprintf("orka-t-%s-%d", uidHash, attempt)
}
