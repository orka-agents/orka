//go:build linux

package supervisor

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

// Claude's skill plugin can carry hooks and MCP servers, so the session child
// must be able to read it but never change, add to, or replace it. Run as
// root, ideally with the runtime's capabilities, to cover that.
func TestClaudeSkillPluginIsReadOnlyForTheSessionChild(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root, as the runtime's supervisor runs")
	}
	const child = 4244
	mounts := t.TempDir()
	skills := supervisorTestSkills()
	writeSupervisorTestSkillMounts(t, mounts, skills)
	base := filepath.Join(t.TempDir(), "sessions")
	paths, err := acp.PrepareSessionPaths(base, "session")
	if err != nil {
		t.Fatal(err)
	}
	// Session roots sit under supervisor-owned folders children only search.
	for _, dir := range []string{filepath.Dir(base), filepath.Dir(filepath.Dir(base))} {
		if err := os.Chmod(dir, 0o711); err != nil {
			t.Fatal(err)
		}
	}
	if err := materializeSessionSkills(providerKindClaude, skills, mounts, paths); err != nil {
		t.Fatal(err)
	}
	if err := acp.FinalizeSessionOwnership(paths.Root, child, child); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = acp.ReclaimSessionOwnership(paths.Root) })
	plugin := claudeSkillPluginRoot(paths)
	skillFile := filepath.Join(plugin, "skills", skills[0].Name, "SKILL.md")
	err = filepath.WalkDir(plugin, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		if stat := info.Sys().(*syscall.Stat_t); stat.Uid != 0 || info.Mode().Perm()&0o022 != 0 {
			t.Errorf("%s owner %d mode %v; want root-owned and writable by root only", path, stat.Uid, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var read []byte
	attempts := map[string]func() error{
		"add a hook":         func() error { return os.Mkdir(filepath.Join(plugin, "hooks"), 0o755) },
		"add a manifest":     func() error { return os.WriteFile(filepath.Join(plugin, ".mcp.json"), []byte("{}"), 0o644) },
		"edit a skill":       func() error { return os.WriteFile(skillFile, []byte("changed"), 0o644) },
		"add a skill":        func() error { return os.Mkdir(filepath.Join(plugin, "skills", "extra"), 0o755) },
		"replace the plugin": func() error { return os.Rename(plugin, plugin+".old") },
		"remove a skill":     func() error { return os.RemoveAll(filepath.Join(plugin, "skills", skills[0].Name)) },
	}
	for name, attempt := range attempts {
		if err := asSessionChild(child, child, attempt); err == nil {
			t.Errorf("the session child could %s", name)
		}
	}
	if err := asSessionChild(child, child, func() (err error) {
		read, err = os.ReadFile(skillFile)
		return err
	}); err != nil || !strings.Contains(string(read), "name: "+skills[0].Name) {
		t.Fatalf("the session child cannot read its skill: %q, %v", read, err)
	}
}
