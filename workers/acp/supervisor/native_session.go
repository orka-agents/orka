package supervisor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/opencodestate"
	"github.com/orka-agents/orka/internal/workspacedelta"
)

const nativeSessionSetupTimeout = 30 * time.Second

func (s *Server) supportsNativeSessions() bool {
	// Agent-sandbox keeps the durable directory under the stable Session UID.
	// It does not need the optional key used for independent checkpoint restores.
	// The controller restricts native continuity to its selected Session class.
	return s.cfg.Capabilities.SupportsNativeSessionRestore &&
		s.cfg.Provider.Kind == providerKindOpencode && acp.OpenCodeVersion == "1.18.9" &&
		s.cfg.Provider.AdapterName == openCodeAdapterName() &&
		s.cfg.Provider.AdapterDigest == openCodeAdapterDigest() &&
		s.cfg.DurableWorkspaceDir != ""
}

// captureNativeSession runs inside workspace validation's proven freeze and
// ownership barrier. Failed or incomplete captures leave the Task's ordinary
// read-validation result intact and cannot create a usable saved copy.
func (s *Server) captureNativeSession(ctx context.Context, state *sessionState) (*harnessv2.NativeSessionSnapshot, string) {
	s.mu.Lock()
	if !s.supportsNativeSessions() || state.workspaceIntent != harnessv2.WorkspaceIntentRead ||
		!sessionWorkspaceOutsideRoot(state.paths) {
		s.mu.Unlock()
		return nil, "unsupported_runtime"
	}
	if state.prompt == nil || state.prompt.settlement == nil ||
		state.prompt.settlement.Outcome != harnessv2.PromptOutcomeSucceeded ||
		state.prompt.providerDrainTimedOut || state.prompt.remoteSettlementUnproven || len(state.permissions) != 0 {
		s.mu.Unlock()
		return nil, "conversation_not_settled"
	}
	paths, descriptor, model, runtime := state.paths, state.descriptor, state.profile.Model, state.runtime
	s.mu.Unlock()
	if runtime == nil || runtime.Process().VerifyNoDescendants() != nil {
		return nil, "background_processes"
	}
	ctx, cancel := context.WithTimeout(ctx, nativeSessionSetupTimeout)
	defer cancel()
	data, err := opencodestate.Capture(ctx, nativeSessionDBPath(paths), descriptor.ProviderSessionID, paths.Workspace, model)
	if err != nil {
		return nil, "conversation_data_ineligible"
	}
	snapshot, err := workspacedelta.CaptureContext(ctx, paths.Workspace, s.baselineCaptureOptions())
	if err != nil {
		return nil, "workspace_state_unavailable"
	}
	result := &harnessv2.NativeSessionSnapshot{
		SessionUID: descriptor.RuntimeSessionUID, ProviderKind: providerKindOpencode,
		ProviderVersion: acp.OpenCodeVersion, ProviderSessionID: descriptor.ProviderSessionID,
		ProfileDigest: descriptor.RuntimeProfileDigest, WorkingDirectory: paths.Workspace,
		WorkspaceStateDigest: snapshot.ManifestDigest(), Data: data, DataDigest: harnessv2.NativeSessionDataDigest(data),
	}
	if err := result.Validate(); err != nil {
		return nil, "conversation_data_ineligible"
	}
	return result, "saved_pending_session_finalization"
}

func nativeSessionDBPath(paths acp.SessionPaths) string {
	return filepath.Join(paths.Data, "opencode", "opencode.db")
}

// prepareNativeRestore imports selected conversation rows into a database
// initialized by the pinned binary. No saved configuration, credentials, SQL
// schema, permissions, or workspace files are imported.
func (s *Server) prepareNativeRestore(
	ctx context.Context, request harnessv2.CreateRuntimeSessionRequest, process acp.ProcessConfig,
) (*acp.SessionRestore, *harnessv2.NativeSessionRestoration, error) {
	if request.NativeRestore == nil {
		return nil, nil, nil
	}
	result := &harnessv2.NativeSessionRestoration{
		SnapshotID: request.NativeRestore.SnapshotID, Method: "reconstructed", Reason: "incompatible_saved_copy",
	}
	if !s.supportsNativeSessions() {
		return nil, nil, errors.New("native session restoration is not supported by this runtime")
	}
	saved := request.NativeRestore.Snapshot
	if saved.ProviderVersion != acp.OpenCodeVersion || saved.WorkingDirectory != process.Paths.Workspace ||
		!sessionWorkspaceOutsideRoot(process.Paths) {
		return nil, result, nil
	}
	if err := acp.ReclaimSessionOwnership(process.Paths.Workspace); err != nil {
		return nil, nil, errors.New("native workspace ownership reclaim failed")
	}
	snapshot, err := workspacedelta.CaptureContext(ctx, process.Paths.Workspace, s.baselineCaptureOptions())
	if restoreErr := acp.FinalizeSessionOwnership(process.Paths.Workspace, process.UID, process.GID); restoreErr != nil {
		return nil, nil, errors.New("native workspace ownership restore failed")
	}
	if err != nil || snapshot.ManifestDigest() != saved.WorkspaceStateDigest {
		result.Reason = "workspace_state_mismatch"
		return nil, result, nil
	}
	if err := s.initializeNativeSessionDatabase(ctx, process); err != nil {
		return nil, nil, err
	}
	if err := acp.ReclaimSessionOwnership(process.Paths.Root); err != nil {
		return nil, nil, errors.New("native conversation ownership reclaim failed")
	}
	err = opencodestate.Restore(ctx, nativeSessionDBPath(process.Paths), saved.Data,
		saved.ProviderSessionID, process.Paths.Workspace, request.Profile.Model)
	if err != nil {
		// Import is transactional. Discard the private database before a new
		// conversation is created, even if a corrupt copy left partial data.
		if removeErr := os.RemoveAll(filepath.Join(process.Paths.Data, "opencode")); removeErr != nil {
			return nil, nil, errors.New("native conversation cleanup failed")
		}
		result.Reason = "invalid_saved_copy"
	}
	if ownershipErr := acp.FinalizeSessionOwnership(process.Paths.Root, process.UID, process.GID); ownershipErr != nil {
		return nil, nil, errors.New("native conversation ownership restore failed")
	}
	if err != nil {
		return nil, result, nil
	}
	return &acp.SessionRestore{
		SessionID: saved.ProviderSessionID, ModelID: openCodeProviderID + "/" + request.Profile.Model, ModeID: "build",
	}, result, nil
}

func (s *Server) initializeNativeSessionDatabase(ctx context.Context, config acp.ProcessConfig) error {
	ctx, cancel := context.WithTimeout(ctx, nativeSessionSetupTimeout)
	defer cancel()
	config.Args = []string{"--pure", "db", "SELECT 1", "--format", "json"}
	process, err := acp.StartProcess(config)
	if err != nil {
		return errors.New("native database initialization could not start")
	}
	waitErr := process.Wait(ctx)
	cleanupCtx, cleanupCancel := context.WithTimeout(context.WithoutCancel(ctx), acp.DefaultStopGrace*2)
	defer cleanupCancel()
	proof, cleanupErr := process.Stop(cleanupCtx, acp.DefaultStopGrace)
	if cleanupErr != nil || !proof.Proven {
		s.poisonPool("native_database_cleanup_unproven")
		return errors.New("native database initialization cleanup could not be proven")
	}
	if waitErr != nil {
		return errors.New("native database initialization failed")
	}
	return nil
}

// A restore timeout may have opened a native session. A fresh conversation is
// allowed only after the ACP client proves the failed process and descendants
// are gone, and the imported private data has been removed.
func (s *Server) startNativeRuntimeSession(
	ctx context.Context, cfg acp.RuntimeSessionConfig, result *harnessv2.NativeSessionRestoration,
) (*acp.RuntimeSession, error) {
	runtimeSession, err := acp.NewRuntimeSession(ctx, cfg)
	if err == nil {
		if cfg.Restore != nil {
			result.Method, result.Reason = runtimeSession.RestoreMethod(), "saved_conversation"
		}
		return runtimeSession, nil
	}
	if cfg.Restore == nil {
		return nil, err
	}
	var restoreErr *acp.SessionRestoreError
	if !errors.As(err, &restoreErr) || !restoreErr.Cleanup.Proven || restoreErr.CleanupErr != nil {
		s.poisonPool("native_restore_cleanup_unproven")
		return nil, errors.New("native restoration cleanup could not be proven")
	}
	if reclaimErr := acp.ReclaimSessionOwnership(cfg.Process.Paths.Root); reclaimErr != nil {
		return nil, errors.New("native restoration cleanup ownership failed")
	}
	if removeErr := os.RemoveAll(filepath.Join(cfg.Process.Paths.Data, "opencode")); removeErr != nil {
		return nil, errors.New("native restoration data cleanup failed")
	}
	if ownershipErr := acp.FinalizeSessionOwnership(cfg.Process.Paths.Root, cfg.Process.UID, cfg.Process.GID); ownershipErr != nil {
		return nil, errors.New("native restoration cleanup ownership failed")
	}
	result.Method, result.Reason = "reconstructed", "restore_failed_process_stopped"
	cfg.Restore = nil
	runtimeSession, err = acp.NewRuntimeSession(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("create reconstructed conversation: %w", err)
	}
	return runtimeSession, nil
}
