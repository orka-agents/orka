/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/internal/workspace"
)

func TestWorkspaceInnerEnvStripsExecutionWorkspaceMetadata(t *testing.T) {
	env := workspaceInnerEnv(
		[]string{
			workerenv.ExecutionWorkspaceEnabled + "=true",
			workerenv.ExecutionWorkspaceProvider + "=substrate",
			workerenv.ExecutionWorkspaceTemplateName + "=orka-codex",
			workerenv.ExecutionWorkspaceTemplateNamespace + "=ate-demo",
			workerenv.ExecutionWorkspaceClaimNamespace + "=ate-demo",
			workerenv.ExecutionWorkspaceClaimName + "=actor-1",
			workerenv.ExecutionWorkspaceReusePolicy + "=by-session",
			workerenv.ExecutionWorkspaceReuseKey + "=session-1",
			workerenv.ExecutionWorkspaceCleanupPolicy + "=retain",
			workerenv.ExecutionWorkspaceBoot + "=true",
			workerenv.ExecutionWorkspacePoolName + "=codex-pool",
			workerenv.ExecutionWorkspacePoolNamespace + "=default",
			workerenv.ExecutionWorkspaceSnapshotRestoreURI + "=s3://bucket/restore",
			workerenv.ExecutionWorkspaceSnapshotCheckpointURI + "=s3://bucket/checkpoint",
			workerenv.ExecutionWorkspaceSnapshotOnRelease + "=true",
			workerenv.ExecutionWorkspaceProcessMode + "=resident",
			workerenv.ExecutionWorkspaceResidentKey + "=resident-key",
			workerenv.ExecutionWorkspaceClaimTimeoutSeconds + "=30",
			workerenv.ExecutionWorkspaceCommandTimeoutSeconds + "=600",
			workerenv.ExecutionWorkspaceStatusEndpoint + "=http://controller/internal",
			workerenv.ExecutionWorkspaceDepth + "=4",
			workerenv.SubstrateAPIEndpoint + "=api.ate-system.svc:443",
			workerenv.SubstrateAPICAFile + "=/var/run/orka/substrate/ca.crt",
			workerenv.SubstrateAPIInsecureSkipVerify + "=true",
			workerenv.SubstrateRouterURL + "=http://atenet-router.ate-system.svc",
			workerenv.SubstrateActorDNSSuffix + "=actors.resources.substrate.ate.dev",
			workerenv.SubstrateSessionIdentityToken + "=session-identity-token",
			workerenv.SubstrateSessionIdentityRequired + "=true",
			workerenv.SubstrateSessionIdentityMintCert + "=true",
			workerenv.SubstrateSessionIdentityAudience + "=orka-workspace-daemon,custom-audience",
			workerenv.SubstrateSessionIdentityAppID + "=orka",
			workerenv.SubstrateSessionIdentityUserID + "=orka-worker",
			workerenv.WorkspaceBootstrapToken + "=bootstrap-token",
			workspaceHandoffTokenEnv + "=handoff-token",
			workerenv.AgentSandboxDepth + "=2",
			"USER_DEFINED=value",
		},
		workerenv.ExecutionWorkspaceEnv{Depth: 4},
	)

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
		workspaceHandoffTokenEnv,
	} {
		if _, ok := env[name]; ok {
			t.Fatalf("inner env unexpectedly contains %s", name)
		}
	}
	if env[workerenv.ExecutionWorkspaceEnabled] != workerEnvFalse {
		t.Fatalf("%s = %q, want false", workerenv.ExecutionWorkspaceEnabled, env[workerenv.ExecutionWorkspaceEnabled])
	}
	if env[workerenv.ExecutionWorkspaceDepth] != "5" {
		t.Fatalf("%s = %q, want 5", workerenv.ExecutionWorkspaceDepth, env[workerenv.ExecutionWorkspaceDepth])
	}
	if env[workerenv.AgentSandboxEnabled] != workerEnvFalse {
		t.Fatalf("%s = %q, want false", workerenv.AgentSandboxEnabled, env[workerenv.AgentSandboxEnabled])
	}
	if env[workerenv.AgentSandboxDepth] != "3" {
		t.Fatalf("%s = %q, want 3", workerenv.AgentSandboxDepth, env[workerenv.AgentSandboxDepth])
	}
	if env["USER_DEFINED"] != "value" {
		t.Fatalf("USER_DEFINED = %q, want value", env["USER_DEFINED"])
	}
}
func TestBootstrapWorkspaceHandoffTokenHonorsConfiguredFile(t *testing.T) {
	t.Setenv(workspaceHandoffTokenFileEnv, "/home/worker/custom-handoff-token")
	recorder := newRecordingWorkspaceExecutor()
	claim, err := recorder.Claim(context.Background(), workspace.ClaimRequest{
		Namespace:       "ns",
		CreateIfMissing: true,
		Template:        workspace.TemplateRef{Name: "template"},
		Timeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("claim workspace: %v", err)
	}

	if err := bootstrapWorkspaceHandoffToken(
		context.Background(),
		recorder,
		claim.Ref,
		"handoff-token",
		workerenv.ExecutionWorkspaceEnv{
			Provider:     string(corev1alpha1.WorkspaceProviderSubstrate),
			ClaimTimeout: time.Second,
		},
	); err != nil {
		t.Fatalf("bootstrapWorkspaceHandoffToken returned error: %v", err)
	}

	uploadReqs := recorder.uploadRequests()
	if len(uploadReqs) != 1 || len(uploadReqs[0].Artifacts) != 1 {
		t.Fatalf("upload requests = %#v, want one handoff token artifact", uploadReqs)
	}
	if !uploadReqs[0].BootstrapHandoff {
		t.Fatal("handoff token upload did not request bootstrap auth")
	}
	artifact := uploadReqs[0].Artifacts[0]
	if artifact.Path != "/home/worker/custom-handoff-token" {
		t.Fatalf("handoff token upload path = %q, want configured path", artifact.Path)
	}
	if string(artifact.Data) != "handoff-token" || artifact.Mode != 0o600 {
		t.Fatalf("handoff artifact data/mode = %q/%#o, want token/0600", string(artifact.Data), artifact.Mode)
	}
}

func TestEnsureWorkspaceHandoffTokenNoopsForNonSubstrate(t *testing.T) {
	t.Setenv(workspaceHandoffTokenEnv, "")
	token, err := ensureWorkspaceHandoffToken(workerenv.ExecutionWorkspaceEnv{
		Provider: string(corev1alpha1.WorkspaceProviderAgentSandbox),
	})
	if err != nil {
		t.Fatalf("ensureWorkspaceHandoffToken() error = %v", err)
	}
	if token != "" || strings.TrimSpace(os.Getenv(workspaceHandoffTokenEnv)) != "" {
		t.Fatalf("token/env = %q/%q, want empty", token, os.Getenv(workspaceHandoffTokenEnv))
	}
}

func TestEnsureWorkspaceHandoffTokenReusesTrimmedEnvToken(t *testing.T) {
	t.Setenv(workspaceHandoffTokenEnv, "  existing-token  ")
	token, err := ensureWorkspaceHandoffToken(workerenv.ExecutionWorkspaceEnv{
		Provider: string(corev1alpha1.WorkspaceProviderSubstrate),
	})
	if err != nil {
		t.Fatalf("ensureWorkspaceHandoffToken() error = %v", err)
	}
	if token != "existing-token" {
		t.Fatalf("token = %q, want trimmed existing-token", token)
	}
}

func TestEnsureWorkspaceHandoffTokenGeneratesAndStoresHexToken(t *testing.T) {
	t.Setenv(workspaceHandoffTokenEnv, "")
	token, err := ensureWorkspaceHandoffToken(workerenv.ExecutionWorkspaceEnv{
		Provider: string(corev1alpha1.WorkspaceProviderSubstrate),
	})
	if err != nil {
		t.Fatalf("ensureWorkspaceHandoffToken() error = %v", err)
	}
	if len(token) != 64 {
		t.Fatalf("token length = %d, want 64 hex chars", len(token))
	}
	for _, ch := range token {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			t.Fatalf("token = %q, want lowercase hex", token)
		}
	}
	if got := os.Getenv(workspaceHandoffTokenEnv); got != token {
		t.Fatalf("env token = %q, want generated token", got)
	}
}

func TestBootstrapWorkspaceHandoffTokenNoopsForNonSubstrate(t *testing.T) {
	recorder := newRecordingWorkspaceExecutor()
	claim, err := recorder.Claim(context.Background(), workspace.ClaimRequest{
		Namespace:       "ns",
		CreateIfMissing: true,
		Template:        workspace.TemplateRef{Name: "template"},
	})
	if err != nil {
		t.Fatalf("claim workspace: %v", err)
	}
	if err := bootstrapWorkspaceHandoffToken(
		context.Background(),
		recorder,
		claim.Ref,
		"handoff-token",
		workerenv.ExecutionWorkspaceEnv{Provider: string(corev1alpha1.WorkspaceProviderAgentSandbox)},
	); err != nil {
		t.Fatalf("bootstrapWorkspaceHandoffToken() error = %v", err)
	}
	if got := recorder.uploadRequests(); len(got) != 0 {
		t.Fatalf("upload requests = %#v, want none", got)
	}
}

func TestBootstrapWorkspaceHandoffTokenUsesDefaultTarget(t *testing.T) {
	t.Setenv(workspaceHandoffTokenFileEnv, "")
	recorder := newRecordingWorkspaceExecutor()
	claim, err := recorder.Claim(context.Background(), workspace.ClaimRequest{
		Namespace:       "ns",
		CreateIfMissing: true,
		Template:        workspace.TemplateRef{Name: "template"},
	})
	if err != nil {
		t.Fatalf("claim workspace: %v", err)
	}
	if err := bootstrapWorkspaceHandoffToken(
		context.Background(),
		recorder,
		claim.Ref,
		"handoff-token",
		workerenv.ExecutionWorkspaceEnv{Provider: string(corev1alpha1.WorkspaceProviderSubstrate), ClaimTimeout: time.Second},
	); err != nil {
		t.Fatalf("bootstrapWorkspaceHandoffToken() error = %v", err)
	}
	uploads := recorder.uploadRequests()
	if len(uploads) != 1 || len(uploads[0].Artifacts) != 1 {
		t.Fatalf("upload requests = %#v, want one upload artifact", uploads)
	}
	artifact := uploads[0].Artifacts[0]
	if artifact.Path != workspaceHandoffTokenUploadPath ||
		artifact.Mode != 0o600 ||
		string(artifact.Data) != "handoff-token" {
		t.Fatalf("upload artifact = %#v, want default handoff token upload", artifact)
	}
}

func TestWorkspaceHandoffTokenUploadTargetTrimsCustomPath(t *testing.T) {
	t.Setenv(workspaceHandoffTokenFileEnv, "  /home/worker/custom-token  ")
	if got := workspaceHandoffTokenUploadTarget(); got != "/home/worker/custom-token" {
		t.Fatalf("workspaceHandoffTokenUploadTarget() = %q, want trimmed custom path", got)
	}
}
