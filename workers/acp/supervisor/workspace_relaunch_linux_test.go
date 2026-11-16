//go:build linux

package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
)

// By an idle adapter restart the session tree is the child's, and a
// write-intent workspace is its private 0700 directory. The supervisor has no
// DAC_OVERRIDE, so the restart's workspace check must run as the child or it
// would refuse every clean session. Run as root, ideally with the runtime's
// capabilities (CHOWN, KILL, SETGID, SETUID), to cover that.
func TestRelaunchWorkspaceValidationActsAsTheSessionChild(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root, as the runtime's supervisor runs")
	}
	const child = 4243
	base := t.TempDir()
	// The child reaches its tree through supervisor-owned ancestors it can
	// only search, as under the runtime's session base directory.
	for _, dir := range []string{base, filepath.Dir(base)} {
		if err := os.Chmod(dir, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	root := filepath.Join(base, "session")
	paths := acp.SessionPaths{Root: root, Workspace: filepath.Join(root, "workspace")}
	writeTestFile(t, filepath.Join(paths.Workspace, "AGENTS.md"), "# agents")
	for _, dir := range []string{root, paths.Workspace} {
		if err := os.Chmod(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := acp.FinalizeSessionOwnership(root, child, child); err != nil {
		t.Fatal(err)
	}
	// Without DAC_OVERRIDE the test's cleanup must reclaim the tree.
	t.Cleanup(func() { _ = acp.ReclaimSessionOwnership(root) })

	validate := validateWorkspaceAsSessionChild(rejectOpenCodeProjectConfiguration, paths, child, child)
	if err := validate(); err != nil {
		t.Fatalf("clean child-owned workspace was rejected on relaunch: %v", err)
	}
	if err := rejectOpenCodeProjectConfiguration(paths); err != nil {
		if !errors.Is(err, os.ErrPermission) {
			t.Fatalf("supervisor-identity check = %v, want a permission error or success", err)
		}
		t.Log("supervisor lacks DAC_OVERRIDE: its own identity cannot inspect the workspace")
	}

	if err := asSessionChild(child, child, func() error {
		return os.WriteFile(filepath.Join(paths.Workspace, "opencode.json"), []byte("{}"), 0o600)
	}); err != nil {
		t.Fatal(err)
	}
	rejected, ok := errors.AsType[*workspaceRejectedError](validate())
	if !ok || rejected.publicMessage != acp.OpenCodeProjectConfigurationRejectedMessage {
		t.Fatalf("child-written OpenCode configuration on relaunch = %v, want the OpenCode rejection", validate())
	}
}
