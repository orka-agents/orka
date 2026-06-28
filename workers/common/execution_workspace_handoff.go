/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strconv"
	"strings"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/internal/workspace"
)

func ensureWorkspaceHandoffToken(workspaceEnv workerenv.ExecutionWorkspaceEnv) (string, error) {
	if strings.TrimSpace(workspaceEnv.Provider) != string(corev1alpha1.WorkspaceProviderSubstrate) {
		return "", nil
	}
	if token := strings.TrimSpace(os.Getenv(workspaceHandoffTokenEnv)); token != "" {
		return token, nil
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", fmt.Errorf("generate workspace handoff token: %w", err)
	}
	token := hex.EncodeToString(random[:])
	if err := os.Setenv(workspaceHandoffTokenEnv, token); err != nil {
		return "", fmt.Errorf("store workspace handoff token in environment: %w", err)
	}
	return token, nil
}

func bootstrapWorkspaceHandoffToken(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	token string,
	workspaceEnv workerenv.ExecutionWorkspaceEnv,
) error {
	if strings.TrimSpace(workspaceEnv.Provider) != string(corev1alpha1.WorkspaceProviderSubstrate) {
		return nil
	}
	if _, err := executor.Upload(ctx, workspace.UploadRequest{
		Ref:              ref,
		BootstrapHandoff: true,
		Artifacts: []workspace.UploadArtifact{{
			Path: workspaceHandoffTokenUploadTarget(),
			Data: []byte(token),
			Mode: 0o600,
		}},
		Timeout: workspaceEnv.ClaimTimeout,
	}); err != nil {
		return fmt.Errorf("stage workspace handoff token: %w", err)
	}
	return nil
}
func workspaceHandoffTokenUploadTarget() string {
	if custom := strings.TrimSpace(os.Getenv(workspaceHandoffTokenFileEnv)); custom != "" {
		return custom
	}
	return workspaceHandoffTokenUploadPath
}
func workspaceInnerEnv(environ []string, workspaceEnv workerenv.ExecutionWorkspaceEnv) map[string]string {
	env := environToMap(environ)
	depth := workspaceEnv.Depth
	scrubInnerExecutionWorkspaceEnv(env)
	env[workerenv.ExecutionWorkspaceEnabled] = workerEnvFalse
	env[workerenv.ExecutionWorkspaceDepth] = strconv.Itoa(depth + 1)
	legacyDepth := agentSandboxDepth(env[workerenv.AgentSandboxDepth])
	env[workerenv.AgentSandboxEnabled] = workerEnvFalse
	env[workerenv.AgentSandboxDepth] = strconv.Itoa(legacyDepth + 1)
	delete(env, workerenv.ServiceAccountToken)
	delete(env, workerenv.ServiceAccountTokenPath)
	delete(env, workspaceHandoffTokenEnv)
	delete(env, workspaceBootstrapTokenEnv)
	return env
}

func scrubInnerExecutionWorkspaceEnv(env map[string]string) {
	for _, name := range []string{
		workerenv.ExecutionWorkspaceProvider,
		workerenv.ExecutionWorkspaceTemplateName,
		workerenv.ExecutionWorkspaceTemplateNamespace,
		workerenv.ExecutionWorkspaceClaimNamespace,
		workerenv.ExecutionWorkspaceClaimName,
		workerenv.ExecutionWorkspaceReusePolicy,
		workerenv.ExecutionWorkspaceReuseKey,
		workerenv.ExecutionWorkspaceCleanupPolicy,
		workerenv.ExecutionWorkspaceBoot,
		workerenv.ExecutionWorkspacePoolName,
		workerenv.ExecutionWorkspacePoolNamespace,
		workerenv.ExecutionWorkspaceSnapshotRestoreURI,
		workerenv.ExecutionWorkspaceSnapshotCheckpointURI,
		workerenv.ExecutionWorkspaceSnapshotOnRelease,
		workerenv.ExecutionWorkspaceProcessMode,
		workerenv.ExecutionWorkspaceResidentKey,
		workerenv.ExecutionWorkspaceClaimTimeoutSeconds,
		workerenv.ExecutionWorkspaceCommandTimeoutSeconds,
		workerenv.ExecutionWorkspaceStatusEndpoint,
		workerenv.SubstrateAPIEndpoint,
		workerenv.SubstrateAPICAFile,
		workerenv.SubstrateAPIInsecureSkipVerify,
		workerenv.SubstrateRouterURL,
		workerenv.SubstrateActorDNSSuffix,
		workerenv.SubstrateSessionIdentityToken,
		workerenv.SubstrateSessionIdentityRequired,
		workerenv.SubstrateSessionIdentityMintCert,
		workerenv.SubstrateSessionIdentityAudience,
		workerenv.SubstrateSessionIdentityAppID,
		workerenv.SubstrateSessionIdentityUserID,
		workerenv.WorkspaceBootstrapToken,
	} {
		delete(env, name)
	}
}
