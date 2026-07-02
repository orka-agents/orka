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

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/workspace/statusrules"
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
	Provider              corev1alpha1.WorkspaceProvider
	RouterURL             string
	TemplateName          string
	TemplateNamespace     string
	ClaimNamespace        string
	ClaimName             string
	ReusePolicy           corev1alpha1.WorkspaceReusePolicy
	ReuseKey              string
	CleanupPolicy         corev1alpha1.WorkspaceCleanupPolicy
	Boot                  bool
	PoolName              string
	PoolNamespace         string
	PoolTargetActors      int32
	SnapshotRestoreURI    string
	SnapshotCheckpointURI string
	SnapshotOnRelease     bool
	ProcessMode           corev1alpha1.ExecutionWorkspaceProcessMode
	ResidentKey           string
	WarmPoolPolicy        string
	NamespaceStrategy     string
	ClaimTimeout          time.Duration
	CommandTimeout        time.Duration

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
	SubstrateSessionIdentityMintCert   bool
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
	return statusrules.IsSupportedProvider(provider)
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

func deterministicSubstratePoolActorID(prefix string, ordinal int) string {
	return fmt.Sprintf("%s-%05d", strings.Trim(strings.TrimSpace(prefix), "-"), ordinal)
}

func deterministicSubstratePoolActorOrdinal(target int32, parts ...string) int {
	if target <= 0 {
		return 0
	}
	trimmedParts := make([]string, 0, len(parts))
	for _, part := range parts {
		trimmedParts = append(trimmedParts, strings.TrimSpace(part))
	}
	sum := sha256.Sum256([]byte(strings.Join(trimmedParts, "\x00")))
	ordinal := 0
	for _, b := range sum {
		ordinal = (ordinal*256 + int(b)) % int(target)
	}
	return ordinal
}

func executionWorkspaceSnapshot(ws *corev1alpha1.ExecutionWorkspaceSpec) (string, string, bool) {
	if ws == nil || ws.Snapshot == nil {
		return "", "", false
	}
	return strings.TrimSpace(ws.Snapshot.RestoreURI),
		strings.TrimSpace(ws.Snapshot.CheckpointURI),
		ws.Snapshot.CheckpointOnRelease
}

func executionWorkspaceHibernation(
	ws *corev1alpha1.ExecutionWorkspaceSpec,
) (corev1alpha1.ExecutionWorkspaceProcessMode, string) {
	if ws == nil || ws.Hibernation == nil {
		return corev1alpha1.ExecutionWorkspaceProcessModeFresh, ""
	}
	mode := ws.Hibernation.ProcessMode
	if mode == "" {
		mode = corev1alpha1.ExecutionWorkspaceProcessModeFresh
	}
	return mode, strings.TrimSpace(ws.Hibernation.ResidentKey)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if trimmed := strings.TrimSpace(value); trimmed != "" {
			return trimmed
		}
	}
	return ""
}
