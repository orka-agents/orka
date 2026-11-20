package v2

import (
	"strings"
	"testing"
)

const testSkillImage = "registry.example.com/skills/review@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestRuntimeSkillValidate(t *testing.T) {
	valid := RuntimeSkill{Name: "code-review", Image: testSkillImage, Path: DefaultRuntimeSkillPath("code-review")}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid skill: %v", err)
	}
	if valid.Path != "/skills/code-review" || valid.MountPath() != "/opt/orka-skills/code-review" {
		t.Fatalf("default path %q, mount path %q", valid.Path, valid.MountPath())
	}
	cases := map[string]RuntimeSkill{
		"empty name":            {Name: "", Image: testSkillImage, Path: "/skills/a"},
		"uppercase name":        {Name: "Review", Image: testSkillImage, Path: "/skills/a"},
		"leading hyphen":        {Name: "-review", Image: testSkillImage, Path: "/skills/a"},
		"double hyphen":         {Name: "code--review", Image: testSkillImage, Path: "/skills/a"},
		"traversal name":        {Name: "../etc", Image: testSkillImage, Path: "/skills/a"},
		"long name":             {Name: strings.Repeat("a", 65), Image: testSkillImage, Path: "/skills/a"},
		"tag instead of digest": {Name: "a", Image: "registry.example.com/skills/review:1", Path: "/skills/a"},
		"tag and digest":        {Name: "a", Image: "registry.example.com/skills/review:1@sha256:" + strings.Repeat("a", 64), Path: "/skills/a"},
		"docker hub shorthand":  {Name: "a", Image: "review@sha256:" + strings.Repeat("a", 64), Path: "/skills/a"},
		"sha512 digest":         {Name: "a", Image: "registry.example.com/skills/review@sha512:" + strings.Repeat("a", 128), Path: "/skills/a"},
		"missing path":          {Name: "a", Image: testSkillImage},
		"root path":             {Name: "a", Image: testSkillImage, Path: "/"},
		"skills root only":      {Name: "a", Image: testSkillImage, Path: "/skills"},
		"outside skills":        {Name: "a", Image: testSkillImage, Path: "/etc/a"},
		"proc":                  {Name: "a", Image: testSkillImage, Path: "/proc/self"},
		"dot dot":               {Name: "a", Image: testSkillImage, Path: "/skills/../etc"},
		"dot":                   {Name: "a", Image: testSkillImage, Path: "/skills/./a"},
		"trailing slash":        {Name: "a", Image: testSkillImage, Path: "/skills/a/"},
		"double slash":          {Name: "a", Image: testSkillImage, Path: "/skills//a"},
		"relative":              {Name: "a", Image: testSkillImage, Path: "skills/a"},
		"control character":     {Name: "a", Image: testSkillImage, Path: "/skills/a\nb"},
		"long path":             {Name: "a", Image: testSkillImage, Path: "/skills/" + strings.Repeat("a", 260)},
	}
	for name, skill := range cases {
		t.Run(name, func(t *testing.T) {
			if err := skill.Validate(); err == nil {
				t.Fatalf("%#v must be rejected", skill)
			}
		})
	}
	nested := RuntimeSkill{Name: "a", Image: testSkillImage, Path: "/skills/bundle/v1.2/a_b"}
	if err := nested.Validate(); err != nil {
		t.Fatalf("nested path: %v", err)
	}
}

func TestValidateRuntimeSkillsLimitsAndDuplicates(t *testing.T) {
	if err := ValidateRuntimeSkills(nil); err != nil {
		t.Fatalf("nil skills: %v", err)
	}
	skills := make([]RuntimeSkill, 0, MaxRuntimeSkills+1)
	for i := range MaxRuntimeSkills + 1 {
		name := "s" + string(rune('a'+i))
		skills = append(skills, RuntimeSkill{Name: name, Image: testSkillImage, Path: DefaultRuntimeSkillPath(name)})
	}
	if err := ValidateRuntimeSkills(skills[:MaxRuntimeSkills]); err != nil {
		t.Fatalf("maximum skills: %v", err)
	}
	if err := ValidateRuntimeSkills(skills); err == nil {
		t.Fatal("more than the maximum skills must be rejected")
	}
	duplicate := []RuntimeSkill{skills[0], {Name: skills[0].Name, Image: testSkillImage, Path: "/skills/other"}}
	if err := ValidateRuntimeSkills(duplicate); err == nil || !strings.Contains(err.Error(), "repeats the name") {
		t.Fatalf("duplicate names: %v", err)
	}
}

func TestRuntimeProfileSkillDigest(t *testing.T) {
	base := testToolboxProfile(nil)
	baseDigest, err := CanonicalProfileDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	empty := base
	empty.Skills = []RuntimeSkill{}
	if digest, err := CanonicalProfileDigest(empty); err != nil || digest != baseDigest {
		t.Fatalf("an empty skill list must keep the digest of profiles that predate skills: %v", err)
	}
	withSkills := func(skills ...RuntimeSkill) ProfileDigest {
		t.Helper()
		profile := base
		profile.Skills = skills
		if err := profile.Validate(); err != nil {
			t.Fatal(err)
		}
		digest, err := CanonicalProfileDigest(profile)
		if err != nil {
			t.Fatal(err)
		}
		return digest
	}
	a := RuntimeSkill{Name: "a", Image: testSkillImage, Path: "/skills/a"}
	b := RuntimeSkill{Name: "b", Image: testSkillImage, Path: "/skills/b"}
	digests := map[ProfileDigest]string{baseDigest: "none"}
	for name, digest := range map[string]ProfileDigest{
		"one":   withSkills(a),
		"two":   withSkills(a, b),
		"order": withSkills(b, a),
		"path":  withSkills(RuntimeSkill{Name: "a", Image: testSkillImage, Path: "/skills/other"}),
		"image": withSkills(RuntimeSkill{Name: "a", Image: strings.Replace(testSkillImage, "0123", "4567", 1), Path: "/skills/a"}),
	} {
		if previous, duplicate := digests[digest]; duplicate {
			t.Errorf("%s produced the same digest as %s", name, previous)
		}
		digests[digest] = name
	}
	invalid := base
	invalid.Skills = []RuntimeSkill{{Name: "a", Image: "registry.example.com/skills/a:latest", Path: "/skills/a"}}
	if err := invalid.Validate(); err == nil {
		t.Fatal("profile validation must reject an unpinned skill image")
	}
}

func TestSkillMountRootIsReservedFromToolboxes(t *testing.T) {
	if err := ValidateRuntimeToolboxMountPath(RuntimeSkillMountRoot); err == nil {
		t.Fatal("a toolbox must not shadow the skill mount root")
	}
}
