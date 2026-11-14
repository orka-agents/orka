package v2

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

// Skill limits are part of the immutable runtime profile contract: controller
// planning, RuntimePool projection, the deployed template, the dispatcher, and
// the supervisor enforce the same shape.
const (
	MaxRuntimeSkills          = 8
	MaxRuntimeSkillNameBytes  = 64
	MaxRuntimeSkillPathBytes  = 256
	MaxRuntimeSkillImageBytes = MaxRuntimeToolboxImageBytes

	// RuntimeSkillSourceRoot is the only folder inside a skill image that a
	// skill may be read from. Keeping skills under one dedicated folder keeps
	// the copier away from kernel, device, and credential mounts.
	RuntimeSkillSourceRoot = "/skills"
	// RuntimeSkillMountRoot is where the runtime container sees each skill
	// read-only, at RuntimeSkillMountRoot/<name>. The supervisor copies from
	// there into each session's native skills folder.
	RuntimeSkillMountRoot = "/opt/" + RuntimeSkillMountRootName
	// RuntimeSkillMountRootName is the /opt folder reserved for skills.
	RuntimeSkillMountRootName = "orka-skills"

	// Content limits for one skill folder. The copier enforces the entry,
	// byte, and depth limits in copy mode and the supervisor enforces all of
	// them again before it projects a skill into a session.
	MaxRuntimeSkillEntries       = 256
	MaxRuntimeSkillTotalBytes    = 4 << 20
	MaxRuntimeSkillFileBytes     = 1 << 20
	MaxRuntimeSkillDepth         = 8
	MaxRuntimeSkillMarkdownBytes = 64 << 10
	// RuntimeSkillMarkdownName is the required instruction file at the root
	// of every skill folder.
	RuntimeSkillMarkdownName = "SKILL.md"
)

var (
	// runtimeSkillNamePattern follows the Agent Skills name rules: lowercase
	// letters, digits, and single hyphens, never leading or trailing.
	runtimeSkillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	// runtimeSkillPathPattern keeps the in-image source under /skills with
	// clean components that can be neither "." nor "..".
	runtimeSkillPathPattern = regexp.MustCompile(`^/skills(/[A-Za-z0-9_][A-Za-z0-9._-]*)+$`)
)

// RuntimeSkill is one Agent Skill delivered read-only from a digest-pinned
// image. Path is always explicit in the profile so the frozen digest covers
// the effective source folder, not a default that could change.
type RuntimeSkill struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Path  string `json:"path"`
}

// DefaultRuntimeSkillPath is the in-image folder used when an Agent omits
// the skill path.
func DefaultRuntimeSkillPath(name string) string {
	return RuntimeSkillSourceRoot + "/" + name
}

// MountPath is the read-only folder the skill is bound to in the runtime
// container.
func (s RuntimeSkill) MountPath() string {
	return RuntimeSkillMountRoot + "/" + s.Name
}

// Validate checks one skill's shape. Registry allowlisting, feature gating,
// and runtime support are controller policy and live outside the profile.
func (s RuntimeSkill) Validate() error {
	if err := ValidateRuntimeSkillName(s.Name); err != nil {
		return err
	}
	if err := validateRuntimeImageReference("skill", s.Image); err != nil {
		return err
	}
	return ValidateRuntimeSkillPath(s.Path)
}

// ValidateRuntimeSkillName applies the Agent Skills name rules.
func ValidateRuntimeSkillName(name string) error {
	if name == "" {
		return fmt.Errorf("skill name is required")
	}
	if len(name) > MaxRuntimeSkillNameBytes {
		return fmt.Errorf("skill name %q exceeds %d bytes", name, MaxRuntimeSkillNameBytes)
	}
	if !runtimeSkillNamePattern.MatchString(name) {
		return fmt.Errorf("skill name %q must use lowercase letters, digits, and single hyphens, and must not start or end with a hyphen", name)
	}
	return nil
}

// ValidateRuntimeSkillPath accepts a clean absolute folder under /skills.
func ValidateRuntimeSkillPath(source string) error {
	if source == "" {
		return fmt.Errorf("skill path is required")
	}
	if len(source) > MaxRuntimeSkillPathBytes {
		return fmt.Errorf("skill path exceeds %d bytes", MaxRuntimeSkillPathBytes)
	}
	if !runtimeSkillPathPattern.MatchString(source) || path.Clean(source) != source {
		return fmt.Errorf("skill path %q must be a clean folder under %s (letters, digits, '.', '_', '-'; no '.' or '..' components)", source, RuntimeSkillSourceRoot)
	}
	return nil
}

// ValidateRuntimeSkills validates each skill and rejects duplicate names,
// which would collide in every runtime's skills folder. A nil or empty list
// is valid and means no skills.
func ValidateRuntimeSkills(skills []RuntimeSkill) error {
	if len(skills) > MaxRuntimeSkills {
		return fmt.Errorf("%d skills declared; at most %d are allowed", len(skills), MaxRuntimeSkills)
	}
	seen := make(map[string]int, len(skills))
	for i := range skills {
		if err := skills[i].Validate(); err != nil {
			return fmt.Errorf("skills[%d]: %w", i, err)
		}
		if previous, duplicate := seen[skills[i].Name]; duplicate {
			return fmt.Errorf("skills[%d] repeats the name %q of skills[%d]", i, skills[i].Name, previous)
		}
		seen[skills[i].Name] = i
	}
	return nil
}

// CloneRuntimeSkills copies a skill list, preserving nil.
func CloneRuntimeSkills(skills []RuntimeSkill) []RuntimeSkill {
	if skills == nil {
		return nil
	}
	return append([]RuntimeSkill(nil), skills...)
}

// RuntimeSkillNames returns the declared skill names in order.
func RuntimeSkillNames(skills []RuntimeSkill) []string {
	names := make([]string, 0, len(skills))
	for i := range skills {
		names = append(names, strings.TrimSpace(skills[i].Name))
	}
	return names
}
