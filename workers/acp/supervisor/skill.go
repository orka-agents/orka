package supervisor

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// nativeSkillsDir returns the folder, inside one session's private tree, that
// the provider scans for user-level Agent Skills, given the environment the
// supervisor sets for that provider. Providers Orka can't limit to its own
// skills return an error; the controller rejects their skills before planning.
func nativeSkillsDir(provider string, paths acp.SessionPaths) (string, error) {
	switch provider {
	case providerKindClaude:
		// CLAUDE_CONFIG_DIR=$HOME/.claude; Claude Code (2.1.293) loads user
		// skills from $CLAUDE_CONFIG_DIR/skills/<name>/SKILL.md, keyed by the
		// folder name, because the adapter keeps the "user" setting source.
		return filepath.Join(paths.Home, ".claude", "skills"), nil
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

// VerifySkills validates every bound skill before the supervisor serves, so a
// skill that can never be projected fails the Pod with a stable reason
// instead of failing each session.
func VerifySkills(cfg Config) error {
	if len(cfg.Skills) == 0 {
		return nil
	}
	if _, err := nativeSkillsDir(cfg.Provider.Kind, acp.SessionPaths{Home: "/", Config: "/"}); err != nil {
		return &toolbox.Failure{Reason: skill.ReasonUnsupported, Message: err.Error()}
	}
	for i := range cfg.Skills {
		if _, err := skill.Load(filepath.Join(skillMountRoot, cfg.Skills[i].Name), cfg.Skills[i].Name, skill.DefaultLimits()); err != nil {
			return err
		}
	}
	return nil
}

// materializeSessionSkills copies each validated skill into the session's
// native skills folder. It runs before session ownership is finalized, so the
// copies belong to the session identity; the read-only source mount is never
// written. Each skill is read and validated again from the mount, so a
// session never receives content that did not pass every check.
func materializeSessionSkills(provider string, skills []harnessv2.RuntimeSkill, mountRoot string, paths acp.SessionPaths) error {
	if len(skills) == 0 {
		return nil
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
