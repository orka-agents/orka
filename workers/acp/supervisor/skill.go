package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/acp/skill"
	"github.com/orka-agents/orka/internal/acp/toolbox"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// skillMountRoot is where the controller binds each skill read-only. Tests
// replace it with a temporary folder.
var skillMountRoot = harnessv2.RuntimeSkillMountRoot

// skillsFromEnv parses the frozen skill list. An absent or empty value means
// no skills and leaves the profile digest unchanged.
func skillsFromEnv() ([]harnessv2.RuntimeSkill, error) {
	raw := strings.TrimSpace(os.Getenv(EnvSkills))
	if raw == "" {
		return nil, nil
	}
	var skills []harnessv2.RuntimeSkill
	if err := json.Unmarshal([]byte(raw), &skills); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvSkills, err)
	}
	if len(skills) == 0 {
		return nil, nil
	}
	if err := harnessv2.ValidateRuntimeSkills(skills); err != nil {
		return nil, fmt.Errorf("%s: %w", EnvSkills, err)
	}
	return skills, nil
}

// Claude receives Orka's skills as one local plugin. Claude Code (2.1.293)
// lists its skills as "<plugin>:<skill>".
const (
	claudeSkillPluginDirName = ".claude-skill-plugin"
	claudeSkillPluginName    = "orka"
)

// claudeSkillPluginRoot is the plugin folder shared by every Claude session of
// this runtime. It sits directly in the session base directory, next to the
// session roots, so no session identity owns it or any folder above it.
func claudeSkillPluginRoot(paths acp.SessionPaths) string {
	return filepath.Join(filepath.Dir(paths.Root), claudeSkillPluginDirName)
}

// nativeSkillsDir returns the folder the provider scans for Orka's Agent
// Skills, given the environment and options the supervisor sets for that
// provider. Providers Orka can't limit to its own skills return an error; the
// controller rejects their skills before planning.
func nativeSkillsDir(provider string, paths acp.SessionPaths) (string, error) {
	switch provider {
	case providerKindClaude:
		// Claude loads no settings file at all (settingSources is empty), so
		// it never reads $CLAUDE_CONFIG_DIR/skills. The skills reach it as a
		// local plugin instead: claude-agent-acp passes the plugins option to
		// Claude Code as --plugin-dir, which loads <root>/skills/<name>/SKILL.md
		// without any setting source.
		return filepath.Join(claudeSkillPluginRoot(paths), "skills"), nil
	case providerKindCodex:
		// Codex (0.160.0) loads user skills from $HOME/.agents/skills; the
		// $CODEX_HOME/skills root is deprecated.
		return filepath.Join(paths.Home, ".agents", "skills"), nil
	case providerKindOpencode:
		// OPENCODE_CONFIG_DIR=$XDG_CONFIG_HOME/opencode; OpenCode (1.19.0)
		// loads skills from $OPENCODE_CONFIG_DIR/skills/**/SKILL.md.
		return filepath.Join(paths.Config, "opencode", "skills"), nil
	default:
		// Copilot CLI (1.0.96) in ACP mode can't be limited to Orka's skills.
		return "", fmt.Errorf("ACP provider %q does not support Orka runtime skills", provider)
	}
}

// skillCheckTimeout and loadSkill are replaced in tests.
var (
	skillCheckTimeout = skill.StartupCheckTimeout
	loadSkill         = skill.Load
)

// VerifySkills validates every bound skill before the supervisor serves, so a
// skill that can never be projected fails the Pod with a stable reason
// instead of failing each session. The check is bounded: the controller sizes
// the startup probe to outlast it, so a read that stalls is reported with a
// stable reason instead of ending in an unexplained kubelet restart. A stalled
// read cannot be interrupted, and the caller exits on any failure.
func VerifySkills(cfg Config) error {
	if len(cfg.Skills) == 0 {
		return nil
	}
	if _, err := nativeSkillsDir(cfg.Provider.Kind, acp.SessionPaths{Home: "/", Config: "/"}); err != nil {
		return &toolbox.Failure{Reason: skill.ReasonUnsupported, Message: err.Error()}
	}
	// The goroutine can outlive this call after a timeout, so it must not
	// read the replaceable package variables.
	load, mountRoot, timeout := loadSkill, skillMountRoot, skillCheckTimeout
	done := make(chan error, 1)
	go func() {
		for i := range cfg.Skills {
			if _, err := load(filepath.Join(mountRoot, cfg.Skills[i].Name), cfg.Skills[i].Name, skill.DefaultLimits()); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return &toolbox.Failure{Reason: skill.ReasonReadFailed, Message: fmt.Sprintf("skill check did not complete within %s", timeout)}
	}
}

// materializeSessionSkills copies each validated skill into the session's
// native skills folder. It runs before session ownership is finalized, so the
// copies belong to the session identity; the read-only source mount is never
// written. Each skill is read and validated again from the mount, so a
// session never receives content that did not pass every check. Claude's
// skills instead go into the shared plugin folder, see ensureClaudeSkillPlugin.
func materializeSessionSkills(provider string, skills []harnessv2.RuntimeSkill, mountRoot string, paths acp.SessionPaths) error {
	if len(skills) == 0 {
		return nil
	}
	if provider == providerKindClaude {
		return ensureClaudeSkillPlugin(skills, mountRoot, claudeSkillPluginRoot(paths))
	}
	dir, err := nativeSkillsDir(provider, paths)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create native skills folder: %w", err)
	}
	for i := range skills {
		tree, err := skill.Load(filepath.Join(mountRoot, skills[i].Name), skills[i].Name, skill.DefaultLimits())
		if err != nil {
			return err
		}
		if err := tree.WriteTo(filepath.Join(dir, skills[i].Name)); err != nil {
			return err
		}
	}
	return nil
}

// claudeSkillPlugins records the plugin folders this process has built and the
// skills each holds. Every session of a runtime gets the same frozen skills.
var claudeSkillPlugins struct {
	sync.Mutex
	built map[string]string
}

// ensureClaudeSkillPlugin builds Claude's skill plugin once per process. A
// plugin can also carry hooks, agents, commands, and MCP servers, so the
// session child must not be able to change it: the supervisor writes it
// outside every session tree with folders 0755 and files 0644, and no session
// identity owns any part of it or of the folder holding it. Children only read
// it. Each skill is validated from the mount as it is written, and a plugin
// left by an earlier process is replaced rather than trusted.
func ensureClaudeSkillPlugin(skills []harnessv2.RuntimeSkill, mountRoot, root string) error {
	if !filepath.IsAbs(root) || filepath.Clean(root) != root {
		return fmt.Errorf("claude skill plugin folder %q must be a clean absolute path", root)
	}
	names := strings.Join(harnessv2.RuntimeSkillNames(skills), "\x00")
	claudeSkillPlugins.Lock()
	defer claudeSkillPlugins.Unlock()
	if built, ok := claudeSkillPlugins.built[root]; ok && built == names {
		return nil
	}
	build, err := os.MkdirTemp(filepath.Dir(root), claudeSkillPluginDirName+"-build-")
	if err != nil {
		return fmt.Errorf("create claude skill plugin: %w", err)
	}
	defer func() {
		if build != "" {
			_ = os.RemoveAll(build)
		}
	}()
	manifest, err := json.Marshal(map[string]string{"name": claudeSkillPluginName})
	if err != nil {
		return err
	}
	for _, dir := range []string{build, filepath.Join(build, ".claude-plugin"), filepath.Join(build, "skills")} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("create claude skill plugin folder: %w", err)
		}
		if err := os.Chmod(dir, 0o755); err != nil {
			return fmt.Errorf("set claude skill plugin folder mode: %w", err)
		}
	}
	manifestPath := filepath.Join(build, ".claude-plugin", "plugin.json")
	if err := os.WriteFile(manifestPath, manifest, 0o644); err != nil {
		return fmt.Errorf("write claude skill plugin manifest: %w", err)
	}
	if err := os.Chmod(manifestPath, 0o644); err != nil {
		return fmt.Errorf("set claude skill plugin manifest mode: %w", err)
	}
	for i := range skills {
		tree, err := skill.Load(filepath.Join(mountRoot, skills[i].Name), skills[i].Name, skill.DefaultLimits())
		if err != nil {
			return err
		}
		if err := tree.WriteTo(filepath.Join(build, "skills", skills[i].Name)); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(root); err != nil {
		return fmt.Errorf("replace claude skill plugin: %w", err)
	}
	if err := os.Rename(build, root); err != nil {
		return fmt.Errorf("install claude skill plugin: %w", err)
	}
	build = ""
	if claudeSkillPlugins.built == nil {
		claudeSkillPlugins.built = map[string]string{}
	}
	claudeSkillPlugins.built[root] = names
	return nil
}
