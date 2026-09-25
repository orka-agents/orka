// Native provider conformance reads credentials only from projected files.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/workspace"
)

const (
	conformanceAtespace = "orka-system"
	conformanceTemplate = "orka-direct"
	credentialBundle    = "/run/substrate-client/credential-bundle.pem"
	proofContents       = "native-data"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	fmt.Println("native direct workspace conformance passed")
}

func randomToken() (string, error) {
	entropy := make([]byte, 32)
	if _, err := rand.Read(entropy); err != nil {
		return "", err
	}
	return hex.EncodeToString(entropy), nil
}

func run() error {
	data, err := os.ReadFile("/run/orka-bootstrap/token")
	if err != nil {
		return fmt.Errorf("bootstrap signing secret is unavailable")
	}
	token, err := randomToken()
	if err != nil {
		return err
	}
	cfg := workspace.SubstrateConfig{
		APIEndpoint: "api.ate-system.svc:443", APICAFile: "/run/substrate-server/trust-bundle.pem",
		APICertFile: credentialBundle, APIKeyFile: credentialBundle, Atespace: conformanceAtespace,
		RouterURL: "http://atenet-router.ate-system.svc", ActorDNSSuffix: "actors.resources.substrate.ate.dev",
		BootstrapToken: strings.TrimSpace(string(data)), HandoffToken: token,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	if report := workspace.DiagnoseSubstrate(ctx, cfg, "orka-acp-infra"); !report.Ready {
		return fmt.Errorf("native diagnostics failed: %+v", report.Checks)
	}
	executor, err := workspace.NewSubstrateExecutor(cfg)
	if err != nil {
		return err
	}
	defer executor.Close() //nolint:errcheck
	claim, err := executor.Claim(ctx, workspace.ClaimRequest{
		Namespace: conformanceAtespace, TaskName: "direct-conformance", ClaimName: "orka-direct-" + token[:12],
		CreateIfMissing: true, Template: workspace.TemplateRef{Namespace: conformanceAtespace, Name: conformanceTemplate},
		Timeout: time.Minute,
	})
	if err != nil {
		return fmt.Errorf("native direct claim: %w", err)
	}
	defer cleanupActor(executor, claim.Ref)
	if err := bootAndSeed(ctx, executor, claim.Ref, token, true); err != nil {
		return err
	}
	if err := verifyProviderConnectivity(ctx, cfg, executor, claim.Ref); err != nil {
		return err
	}
	result, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref:     claim.Ref,
		Command: []string{"sh", "-c", "printf native-data > /workspace/proof; sleep 15; cat /workspace/proof"},
		WorkDir: "/workspace", Timeout: 30 * time.Second,
	})
	if err != nil || result == nil || result.ExitCode != 0 || result.Stdout != proofContents {
		return commandFailure("native direct long command or file write failed", cfg, result, err)
	}
	download, err := executor.Download(ctx, workspace.DownloadRequest{
		Ref: claim.Ref, Paths: []string{"/workspace/proof"}, Timeout: time.Minute,
	})
	if err != nil || len(download.Artifacts) != 1 || string(download.Artifacts[0].Data) != proofContents {
		return commandFailure("native direct file download failed", cfg, nil, err)
	}
	result, err = executor.Exec(ctx, workspace.ExecRequest{
		Ref:     claim.Ref,
		Command: []string{"sh", "-c", "sleep 60"}, Timeout: time.Second,
	})
	if err := verifyCommandTimeout(cfg, result, err); err != nil {
		return err
	}
	return exerciseDataRestore(ctx, cfg, executor, claim.Ref, token[:12])
}

func verifyProviderConnectivity(
	ctx context.Context, cfg workspace.SubstrateConfig,
	executor *workspace.SubstrateWorkspaceExecutor, ref workspace.WorkspaceRef,
) error {
	// Exercise outbound DNS and the provider's native egress path before an
	// agent's retries can obscure the first connection failure. Health is public;
	// no model request or provider credential is involved in this probe.
	result, err := executor.Exec(ctx, workspace.ExecRequest{
		Ref: ref,
		Command: []string{"curl", "--fail", "--silent", "--show-error", "--connect-timeout", "10", "--max-time", "20",
			"--output", "/dev/null", "--write-out", "%{http_code}",
			"http://orka-provider-auth-proxy.orka-system.svc:8080/healthz"},
		Timeout: 30 * time.Second,
	})
	if err != nil || result == nil || result.ExitCode != 0 || result.Stdout != "200" {
		return commandFailure("native Actor could not reach provider proxy health", cfg, result, err)
	}
	fmt.Println("native Actor provider proxy connectivity passed")
	return nil
}

func cleanupActor(executor *workspace.SubstrateWorkspaceExecutor, ref workspace.WorkspaceRef) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	_, _ = executor.Delete(ctx, workspace.DeleteRequest{Ref: ref, SkipScrub: true, Timeout: 2 * time.Minute})
}

func bootAndSeed(
	ctx context.Context, executor *workspace.SubstrateWorkspaceExecutor,
	ref workspace.WorkspaceRef, token string, boot bool,
) error {
	if _, err := executor.WaitReady(ctx, workspace.WaitReadyRequest{
		Ref: ref, Boot: boot, Timeout: 3 * time.Minute,
	}); err != nil {
		return fmt.Errorf("native direct boot: %w", err)
	}
	_, err := executor.Upload(ctx, workspace.UploadRequest{
		Ref: ref, BootstrapHandoff: true, Timeout: time.Minute,
		Artifacts: []workspace.UploadArtifact{{Path: "orka-workspace-handoff-token", Data: []byte(token), Mode: 0o600}},
	})
	return err
}
