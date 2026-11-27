package v2

import (
	"fmt"
	"path"
	"regexp"
	"strings"
)

const (
	MaxRuntimeSkills          = 8
	MaxRuntimeSkillNameBytes  = 64
	MaxRuntimeSkillPathBytes  = 256
	MaxRuntimeSkillImageBytes = MaxRuntimeToolboxImageBytes

	RuntimeSkillSourceRoot    = "/skills"
	RuntimeSkillMountRoot     = "/opt/" + RuntimeSkillMountRootName
	RuntimeSkillMountRootName = "orka-skills"

	MaxRuntimeSkillEntries       = 256
	MaxRuntimeSkillTotalBytes    = 4 << 20
	MaxRuntimeSkillFileBytes     = 1 << 20
	MaxRuntimeSkillDepth         = 8
	MaxRuntimeSkillMarkdownBytes = 64 << 10
	RuntimeSkillMarkdownName     = "SKILL.md"
)

var (
	runtimeSkillNamePattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	runtimeSkillPathPattern = regexp.MustCompile(`^/skills(/[A-Za-z0-9_][A-Za-z0-9._-]*)+$`)
)

type RuntimeSkill struct {
	Name  string `json:"name"`
	Image string `json:"image"`
	Path  string `json:"path"`
}

func DefaultRuntimeSkillPath(name string) string {
	return RuntimeSkillSourceRoot + "/" + name
}

func (s RuntimeSkill) MountPath() string {
	return RuntimeSkillMountRoot + "/" + s.Name
}

func (s RuntimeSkill) Validate() error {
	if err := ValidateRuntimeSkillName(s.Name); err != nil {
		return err
	}
	if err := validateRuntimeImageReference("skill", s.Image); err != nil {
		return err
	}
	return ValidateRuntimeSkillPath(s.Path)
}

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

func CloneRuntimeSkills(skills []RuntimeSkill) []RuntimeSkill {
	if skills == nil {
		return nil
	}
	return append([]RuntimeSkill(nil), skills...)
}

func RuntimeSkillNames(skills []RuntimeSkill) []string {
	names := make([]string, 0, len(skills))
	for i := range skills {
		names = append(names, strings.TrimSpace(skills[i].Name))
	}
	return names
}
