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

var skillMountRoot = harnessv2.RuntimeSkillMountRoot

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

const (
	claudeSkillPluginDirName = ".claude-skill-plugin"
	claudeSkillPluginName    = "orka"
)

func claudeSkillPluginRoot(paths acp.SessionPaths) string {
	return filepath.Join(filepath.Dir(paths.Root), claudeSkillPluginDirName)
}

func nativeSkillsDir(provider string, paths acp.SessionPaths) (string, error) {
	switch provider {
	case providerKindClaude:
		return filepath.Join(claudeSkillPluginRoot(paths), "skills"), nil
	case providerKindCodex:
		return filepath.Join(paths.Home, ".agents", "skills"), nil
	case providerKindOpencode:
		return filepath.Join(paths.Config, "opencode", "skills"), nil
	default:
		return "", fmt.Errorf("ACP provider %q does not support Orka runtime skills", provider)
	}
}

var (
	skillCheckTimeout = skill.StartupCheckTimeout
	loadSkill         = skill.Load
)

func VerifySkills(cfg Config) error {
	if len(cfg.Skills) == 0 {
		return nil
	}
	if _, err := nativeSkillsDir(cfg.Provider.Kind, acp.SessionPaths{Home: "/", Config: "/"}); err != nil {
		return &toolbox.Failure{Reason: skill.ReasonUnsupported, Message: err.Error()}
	}
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

var claudeSkillPlugins struct {
	sync.Mutex
	built map[string]string
}

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
