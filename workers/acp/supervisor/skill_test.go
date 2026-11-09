package supervisor

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	"github.com/orka-agents/orka/internal/acp/skill"
	"github.com/orka-agents/orka/internal/acp/toolbox"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const supervisorTestSkillMarkdown = "---\nname: %s\ndescription: Review a change.\n---\nRun `make test`.\n"

func supervisorTestSkills() []harnessv2.RuntimeSkill {
	return []harnessv2.RuntimeSkill{
		{Name: "code-review", Image: "registry.example.com/skills/review@" + supervisorTestToolboxDigest, Path: "/skills/code-review"},
		{Name: "release-notes", Image: "registry.example.com/skills/notes@" + supervisorTestToolboxDigest, Path: "/skills/bundle/notes"},
	}
}

// writeSupervisorTestSkillMounts lays out the read-only skill mounts the
// controller binds at <root>/<name>.
func writeSupervisorTestSkillMounts(t *testing.T, root string, skills []harnessv2.RuntimeSkill) {
	t.Helper()
	for _, s := range skills {
		dir := filepath.Join(root, s.Name)
		if err := os.MkdirAll(filepath.Join(dir, "references"), 0o755); err != nil {
			t.Fatal(err)
		}
		markdown := strings.Replace(supervisorTestSkillMarkdown, "%s", s.Name, 1)
		if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(markdown), 0o444); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "references", "guide.md"), []byte("guide\n"), 0o444); err != nil {
			t.Fatal(err)
		}
		// The source is read-only, as the runtime mount is.
		for _, path := range []string{filepath.Join(dir, "references"), dir} {
			if err := os.Chmod(path, 0o555); err != nil {
				t.Fatal(err)
			}
		}
		t.Cleanup(func() {
			_ = os.Chmod(dir, 0o755)
			_ = os.Chmod(filepath.Join(dir, "references"), 0o755)
		})
	}
}

func TestLoadConfigFromEnvSkillsJoinTheProfileDigest(t *testing.T) {
	setLoadConfigEnv(t)
	t.Setenv(EnvSkills, "")
	without, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	skills := supervisorTestSkills()
	encoded := `[{"name":"code-review","image":"` + skills[0].Image + `","path":"/skills/code-review"},` +
		`{"name":"release-notes","image":"` + skills[1].Image + `","path":"/skills/bundle/notes"}]`
	t.Setenv(EnvSkills, encoded)
	with, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if len(with.Skills) != 2 || with.Skills[1].Path != "/skills/bundle/notes" {
		t.Fatalf("skills not loaded: %#v", with.Skills)
	}
	if with.Fence.RuntimeProfileDigest == without.Fence.RuntimeProfileDigest {
		t.Fatal("skills must change the supervisor's runtime profile digest")
	}
	expected := harnessv2.RuntimeProfile{
		ACPProfile: harnessv2.ACPProfileV1, AdapterDigests: acp.BuiltInRuntimeAdapterDigests(providerKindCodex),
		ProviderKind: providerKindCodex, Model: "gpt-test",
		AgentConfigurationDigest: testDigest("agent"), ToolPolicyDigest: testDigest("tool"),
		ApprovalPolicyDigest: testDigest("approval"), MCPConfigurationDigest: testDigest("mcp"),
		WorkspaceIntent: harnessv2.WorkspaceIntentRead, ProxyCredentialRole: "provider", ProxyCredentialScope: "model:gpt-test",
		ResourceClass: "standard", Skills: skills,
	}
	expectedDigest, err := harnessv2.CanonicalProfileDigest(expected)
	if err != nil {
		t.Fatal(err)
	}
	if with.Fence.RuntimeProfileDigest != expectedDigest {
		t.Fatalf("supervisor digest %s != controller digest %s", with.Fence.RuntimeProfileDigest, expectedDigest)
	}
	for _, invalid := range []string{
		`[{"name":"a","image":"registry.example.com/a:latest","path":"/skills/a"}]`,
		`[{"name":"a","image":"` + skills[0].Image + `","path":"/skills/../etc"}]`,
		`[{"name":"a","image":"` + skills[0].Image + `","path":"/skills/a"},{"name":"a","image":"` + skills[0].Image + `","path":"/skills/b"}]`,
	} {
		t.Setenv(EnvSkills, invalid)
		if _, err := LoadConfigFromEnv(); err == nil || !strings.Contains(err.Error(), EnvSkills) {
			t.Fatalf("%s must be rejected: %v", invalid, err)
		}
	}
	t.Setenv(EnvSkills, "[]")
	empty, err := LoadConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if empty.Fence.RuntimeProfileDigest != without.Fence.RuntimeProfileDigest {
		t.Fatal("an empty skill list must digest exactly like an absent one")
	}
}

func TestNativeSkillsDirPerRuntime(t *testing.T) {
	paths := acp.SessionPaths{Root: "/sessions/s", Home: "/sessions/s/home", Config: "/sessions/s/xdg/config"}
	want := map[string]string{
		providerKindClaude:   "/sessions/s/home/.claude/skills",
		providerKindCodex:    "/sessions/s/home/.agents/skills",
		providerKindOpencode: "/sessions/s/xdg/config/opencode/skills",
	}
	for provider, dir := range want {
		got, err := nativeSkillsDir(provider, paths)
		if err != nil || got != dir {
			t.Errorf("%s skills folder = %q, %v; want %q", provider, got, err, dir)
		}
		if !strings.HasPrefix(got, paths.Root+"/") {
			t.Errorf("%s skills folder %q is outside the session root, so it would not be owned by the session identity", provider, got)
		}
	}
	for _, provider := range []string{providerKindCopilot, providerKindAgentKit, providerKindFoundry, "unknown"} {
		if _, err := nativeSkillsDir(provider, paths); err == nil {
			t.Errorf("%s must have no native skills folder", provider)
		}
	}
}

// TestNativeSkillsDirMatchesProviderEnvironment ties each placement to the
// environment the supervisor gives the provider, so a change to the
// provider's config folder cannot silently strand the projected skills.
func TestNativeSkillsDirMatchesProviderEnvironment(t *testing.T) {
	paths := acp.SessionPaths{Root: "/sessions/s", Home: "/sessions/s/home", Config: "/sessions/s/xdg/config"}
	cases := map[string]struct {
		envName string
		suffix  string
	}{
		providerKindClaude:   {envName: "CLAUDE_CONFIG_DIR", suffix: "skills"},
		providerKindOpencode: {envName: "OPENCODE_CONFIG_DIR", suffix: "skills"},
	}
	for provider, tc := range cases {
		profile, err := providerProfile(provider, "openai/gpt-test", harnessv2.WorkspaceIntentWrite, &harnessv2.ModelTokenLimits{Context: 2000, Output: 1000})
		if err != nil {
			t.Fatal(err)
		}
		request := harnessv2.CreateRuntimeSessionRequest{}
		env, err := profile.EnvironmentForSession(request, paths, ProviderProxyBinding{BaseURL: "http://127.0.0.1:1", Credential: strings.Repeat("c", 32)})
		if err != nil && provider != providerKindOpencode {
			t.Fatal(err)
		}
		if provider == providerKindOpencode && err != nil {
			// OpenCode needs a full request to render its configuration; its
			// config folder is fixed by the session paths.
			env = map[string]string{"OPENCODE_CONFIG_DIR": filepath.Join(paths.Config, "opencode")}
		}
		dir, err := nativeSkillsDir(provider, paths)
		if err != nil {
			t.Fatal(err)
		}
		if want := filepath.Join(env[tc.envName], tc.suffix); dir != want {
			t.Errorf("%s skills folder %q does not follow %s=%q", provider, dir, tc.envName, env[tc.envName])
		}
	}
}

func TestMaterializeSessionSkillsCopiesIntoNativeFolder(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skills are projected only in Linux runtime Pods")
	}
	mounts := t.TempDir()
	skills := supervisorTestSkills()
	writeSupervisorTestSkillMounts(t, mounts, skills)
	for _, provider := range []string{providerKindClaude, providerKindCodex, providerKindOpencode} {
		t.Run(provider, func(t *testing.T) {
			paths, err := acp.PrepareSessionPaths(t.TempDir(), "session")
			if err != nil {
				t.Fatal(err)
			}
			if err := materializeSessionSkills(provider, skills, mounts, paths); err != nil {
				t.Fatalf("materialize: %v", err)
			}
			dir, err := nativeSkillsDir(provider, paths)
			if err != nil {
				t.Fatal(err)
			}
			for _, s := range skills {
				data, err := os.ReadFile(filepath.Join(dir, s.Name, "SKILL.md"))
				if err != nil || !strings.Contains(string(data), "name: "+s.Name) {
					t.Fatalf("%s SKILL.md = %q, %v", s.Name, data, err)
				}
				if _, err := os.Stat(filepath.Join(dir, s.Name, "references", "guide.md")); err != nil {
					t.Fatalf("%s supporting file: %v", s.Name, err)
				}
			}
			// The projection never writes to the read-only source.
			if entries, _ := os.ReadDir(filepath.Join(mounts, skills[0].Name)); len(entries) != 2 {
				t.Fatalf("source mount changed: %v", entries)
			}
		})
	}
	for _, provider := range []string{providerKindCopilot, providerKindAgentKit} {
		if err := materializeSessionSkills(provider, skills, mounts, acp.SessionPaths{Root: "/x", Home: "/x/home", Config: "/x/c"}); err == nil {
			t.Fatalf("%s must fail closed", provider)
		}
	}
	if err := materializeSessionSkills(providerKindAgentKit, nil, mounts, acp.SessionPaths{}); err != nil {
		t.Fatalf("no skills must be a no-op for every provider: %v", err)
	}
}

func TestMaterializeSessionSkillsRejectsUnsafeMount(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skills are projected only in Linux runtime Pods")
	}
	mounts := t.TempDir()
	skills := supervisorTestSkills()[:1]
	dir := filepath.Join(mounts, skills[0].Name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(strings.Replace(supervisorTestSkillMarkdown, "%s", skills[0].Name, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc", filepath.Join(dir, "etc")); err != nil {
		t.Fatal(err)
	}
	paths, err := acp.PrepareSessionPaths(t.TempDir(), "session")
	if err != nil {
		t.Fatal(err)
	}
	err = materializeSessionSkills(providerKindClaude, skills, mounts, paths)
	var failure *toolbox.Failure
	if !errors.As(err, &failure) || failure.Reason != skill.ReasonUnsupportedFileType {
		t.Fatalf("symlinked skill content = %v, want %s", err, skill.ReasonUnsupportedFileType)
	}
}

func TestVerifySkillsReportsStableFailure(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("skills are verified only in Linux runtime Pods")
	}
	original := skillMountRoot
	t.Cleanup(func() { skillMountRoot = original })
	skillMountRoot = t.TempDir()
	skills := supervisorTestSkills()
	cfg := Config{Provider: ProviderProfile{Kind: providerKindCodex}, Skills: skills}
	writeSupervisorTestSkillMounts(t, skillMountRoot, skills[:1])
	err := VerifySkills(cfg)
	var failure *toolbox.Failure
	if !errors.As(err, &failure) || failure.Reason != skill.ReasonMissingMount || !strings.Contains(failure.Message, "release-notes") {
		t.Fatalf("missing skill mount = %v", err)
	}
	if line := toolbox.FormatFailureLine(failure); !strings.HasPrefix(line, "FAIL reason=SKILL_MISSING_MOUNT msg=") {
		t.Fatalf("failure line %q", line)
	}
	writeSupervisorTestSkillMounts(t, skillMountRoot, skills[1:])
	if err := VerifySkills(cfg); err != nil {
		t.Fatalf("valid skills: %v", err)
	}
	for _, provider := range []string{providerKindCopilot, providerKindAgentKit} {
		cfg.Provider.Kind = provider
		if err := VerifySkills(cfg); !errors.As(err, &failure) || failure.Reason != skill.ReasonUnsupported {
			t.Fatalf("unsupported provider %s = %v", provider, err)
		}
	}
	if err := VerifySkills(Config{Provider: ProviderProfile{Kind: providerKindAgentKit}}); err != nil {
		t.Fatalf("no skills: %v", err)
	}
}

// TestMaterializedSkillsBelongToTheSessionIdentity proves the projection plus
// session ownership finalization hands each copy to the child's own UID/GID
// while the source stays untouched. It needs root, as the runtime Pod does.
func TestMaterializedSkillsBelongToTheSessionIdentity(t *testing.T) {
	if runtime.GOOS != "linux" || os.Geteuid() != 0 {
		t.Skip("requires root to assign a distinct session identity")
	}
	mounts := t.TempDir()
	skills := supervisorTestSkills()
	writeSupervisorTestSkillMounts(t, mounts, skills)
	paths, err := acp.PrepareSessionPaths(t.TempDir(), "session")
	if err != nil {
		t.Fatal(err)
	}
	if err := materializeSessionSkills(providerKindClaude, skills, mounts, paths); err != nil {
		t.Fatal(err)
	}
	const uid, gid = 20042, 20042
	if err := acp.FinalizeSessionOwnership(paths.Root, uid, gid); err != nil {
		t.Fatal(err)
	}
	dir, err := nativeSkillsDir(providerKindClaude, paths)
	if err != nil {
		t.Fatal(err)
	}
	err = filepath.WalkDir(dir, func(path string, _ os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := os.Lstat(path)
		if err != nil {
			return err
		}
		stat := info.Sys().(*syscall.Stat_t)
		if stat.Uid != uid || stat.Gid != gid {
			t.Errorf("%s owned by %d:%d, want %d:%d", path, stat.Uid, stat.Gid, uid, gid)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(filepath.Join(mounts, skills[0].Name, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if stat := info.Sys().(*syscall.Stat_t); stat.Uid == uid || info.Mode().Perm() != 0o444 {
		t.Fatalf("source skill changed ownership or mode: uid=%d mode=%v", stat.Uid, info.Mode())
	}
}

func TestClaudeSkillProjectionExposesOnlyOrkaSkills(t *testing.T) {
	paths := acp.SessionPaths{Home: "/sessions/private/home"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "test-auth-token"}
	claude, err := providerProfile(providerKindClaude, "claude-test", harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	options := func(t *testing.T, allowed, disallowed []string, unrestricted bool, skills []harnessv2.RuntimeSkill) map[string]any {
		t.Helper()
		request := testProviderProjectionRequest(t, providerKindClaude, "claude-test", "", "", allowed, disallowed, unrestricted)
		request.Profile.Skills = skills
		projection, err := claude.ProjectSession(request, paths, proxy)
		if err != nil {
			t.Fatal(err)
		}
		return projection.NewSessionMeta["claudeCode"].(map[string]any)["options"].(map[string]any)
	}
	restricted := options(t, []string{providerToolRead, providerToolGrep}, []string{providerToolBash}, false, supervisorTestSkills())
	tools := restricted["tools"].([]string)
	if !slices.Contains(tools, claudeSkillToolName) || !slices.Contains(tools, providerToolRead) || slices.Contains(tools, providerToolBash) {
		t.Fatalf("restricted tools with skills = %v", tools)
	}
	if !slices.Equal(restricted["skills"].([]string), []string{"code-review", "release-notes"}) {
		t.Fatalf("skills filter = %#v", restricted["skills"])
	}
	settings := restricted["settings"].(map[string]any)
	if settings["disableSkillShellExecution"] != true {
		t.Fatalf("settings = %#v", settings)
	}
	unrestricted := options(t, nil, nil, true, supervisorTestSkills())
	if _, present := unrestricted["tools"]; present || !slices.Equal(unrestricted["skills"].([]string), []string{"code-review", "release-notes"}) {
		t.Fatalf("unrestricted options with skills = %#v", unrestricted)
	}
	without := options(t, []string{providerToolRead}, nil, false, nil)
	if slices.Contains(without["tools"].([]string), claudeSkillToolName) {
		t.Fatal("a session without skills must not gain the Skill tool")
	}
	for _, key := range []string{"skills", "settings"} {
		if _, present := without[key]; present {
			t.Fatalf("a session without skills must not set %s: %#v", key, without)
		}
	}
}

func TestOpenCodeSkillPermissionAllowsOnlyOrkaSkills(t *testing.T) {
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace", Config: "/sessions/private/xdg/config"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "placeholder"}
	permission := func(t *testing.T, skills []harnessv2.RuntimeSkill) any {
		t.Helper()
		request := harnessv2.CreateRuntimeSessionRequest{MCPConfiguration: harnessv2.MCPPolicyConfiguration{
			ToolPolicy: harnessv2.MCPToolPolicy{AllowedToolNames: []string{"Read"}},
		}}
		request.Profile.Skills = skills
		data, err := openCodeSessionConfig("openai/gpt-test", testOpenCodeModelLimits(), harnessv2.WorkspaceIntentRead, request, paths, proxy)
		if err != nil {
			t.Fatal(err)
		}
		var config map[string]any
		if err := json.Unmarshal(data, &config); err != nil {
			t.Fatal(err)
		}
		return config["permission"].(map[string]any)["skill"]
	}
	if got := permission(t, nil); got != openCodePermissionDeny {
		t.Fatalf("skill permission without skills = %#v", got)
	}
	got, ok := permission(t, supervisorTestSkills()).(map[string]any)
	if !ok || len(got) != 2 || got["code-review"] != openCodePermissionAllow || got["release-notes"] != openCodePermissionAllow {
		t.Fatalf("skill permission with skills = %#v", got)
	}
}

// jsObjectKeyOrder orders keys as JavaScript enumerates an object parsed from
// JSON: canonical array-index keys ascending, then the rest in source order.
func jsObjectKeyOrder(keys []string) []string {
	var indices, others []string
	for _, key := range keys {
		if n, err := strconv.ParseUint(key, 10, 32); err == nil && n < 1<<32-1 && strconv.FormatUint(n, 10) == key {
			indices = append(indices, key)
		} else {
			others = append(others, key)
		}
	}
	sort.Slice(indices, func(i, j int) bool {
		a, _ := strconv.ParseUint(indices[i], 10, 32)
		b, _ := strconv.ParseUint(indices[j], 10, 32)
		return a < b
	})
	return append(indices, others...)
}

func TestOpenCodeSkillPermissionAllowsNumericSkillNames(t *testing.T) {
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace", Config: "/sessions/private/xdg/config"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "placeholder"}
	request := harnessv2.CreateRuntimeSessionRequest{MCPConfiguration: harnessv2.MCPPolicyConfiguration{
		ToolPolicy: harnessv2.MCPToolPolicy{AllowedToolNames: []string{"Read"}},
	}}
	request.Profile.Skills = []harnessv2.RuntimeSkill{
		{Name: "123", Image: "registry.example.com/skills/numeric@" + supervisorTestToolboxDigest, Path: "/skills/numeric"},
		{Name: "code-review", Image: "registry.example.com/skills/review@" + supervisorTestToolboxDigest, Path: "/skills/code-review"},
	}
	data, err := openCodeSessionConfig("openai/gpt-test", testOpenCodeModelLimits(), harnessv2.WorkspaceIntentRead, request, paths, proxy)
	if err != nil {
		t.Fatal(err)
	}
	// OpenCode flattens the top-level permissions first ("*" sorts first),
	// then each permission's own rules in JavaScript key order, and the last
	// matching rule decides.
	topLevel := openCodeConfigRules(t, data, "*")
	skillRules := openCodeConfigRules(t, data, "skill")
	keys := make([]string, 0, len(skillRules))
	actions := map[string]string{}
	for _, rule := range skillRules {
		keys = append(keys, rule[0])
		actions[rule[0]] = rule[1]
	}
	decide := func(name string) string {
		action := topLevel[0][1]
		for _, key := range jsObjectKeyOrder(keys) {
			if key == "*" || key == name {
				action = actions[key]
			}
		}
		return action
	}
	for name, want := range map[string]string{"123": openCodePermissionAllow, "code-review": openCodePermissionAllow, "builtin-skill": openCodePermissionDeny} {
		if got := decide(name); got != want {
			t.Fatalf("OpenCode skill %q resolves to %s, want %s (rules %v)", name, got, want, skillRules)
		}
	}
}

// openCodeConfigRules returns one permission's rules in the order OpenCode
// reads them from the config file (JSON object order), as its fromConfig
// does: a string is a single "*" rule, an object is one rule per key.
func openCodeConfigRules(t *testing.T, config []byte, permission string) [][2]string {
	t.Helper()
	var root struct {
		Permission map[string]json.RawMessage `json:"permission"`
	}
	if err := json.Unmarshal(config, &root); err != nil {
		t.Fatal(err)
	}
	raw, ok := root.Permission[permission]
	if !ok {
		t.Fatalf("permission %s is not configured", permission)
	}
	var action string
	if json.Unmarshal(raw, &action) == nil {
		return [][2]string{{"*", action}}
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	if _, err := decoder.Token(); err != nil {
		t.Fatal(err)
	}
	var rules [][2]string
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		value, err := decoder.Token()
		if err != nil {
			t.Fatal(err)
		}
		rules = append(rules, [2]string{key.(string), value.(string)})
	}
	return rules
}

// openCodeEvaluate mirrors OpenCode 1.19.0 Permission.evaluate: the last
// rule whose wildcard matches wins. Configured rules follow OpenCode's
// defaults, and the configured "*" rule always matches, so the defaults
// never decide.
func openCodeEvaluate(rules [][2]string, pattern string) string {
	action := "ask"
	for _, rule := range rules {
		var expression strings.Builder
		for _, r := range rule[0] {
			switch r {
			case '*':
				expression.WriteString(".*")
			case '?':
				expression.WriteString(".")
			default:
				expression.WriteString(regexp.QuoteMeta(string(r)))
			}
		}
		if regexp.MustCompile("(?s)^" + expression.String() + "$").MatchString(pattern) {
			action = rule[1]
		}
	}
	return action
}

// TestOpenCodeExternalDirectoryAllowsOnlyProjectedSkillFolders follows
// OpenCode's read tool: a file outside the workspace asks external_directory
// with the glob <parent folder>/*. A loaded skill's supporting files must be
// readable while every other folder outside the workspace stays denied.
func TestOpenCodeExternalDirectoryAllowsOnlyProjectedSkillFolders(t *testing.T) {
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace", Config: "/sessions/private/xdg/config"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "placeholder"}
	config := func(t *testing.T, skills []harnessv2.RuntimeSkill) []byte {
		t.Helper()
		request := harnessv2.CreateRuntimeSessionRequest{MCPConfiguration: harnessv2.MCPPolicyConfiguration{
			ToolPolicy: harnessv2.MCPToolPolicy{AllowedToolNames: []string{"Read"}},
		}}
		request.Profile.Skills = skills
		data, err := openCodeSessionConfig("openai/gpt-test", testOpenCodeModelLimits(), harnessv2.WorkspaceIntentRead, request, paths, proxy)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	if rules := openCodeConfigRules(t, config(t, nil), "external_directory"); !slices.Equal(rules, [][2]string{{"*", openCodePermissionDeny}}) {
		t.Fatalf("external_directory without skills = %v", rules)
	}
	data := config(t, supervisorTestSkills())
	rules := openCodeConfigRules(t, data, "external_directory")
	readGlob := func(file string) string { return filepath.Join(filepath.Dir(file), "*") }
	skillsDir := "/sessions/private/xdg/config/opencode/skills"
	for _, file := range []string{
		skillsDir + "/code-review/SKILL.md",
		skillsDir + "/code-review/references/guide.md",
		skillsDir + "/release-notes/scripts/deep/check.sh",
	} {
		if got := openCodeEvaluate(rules, readGlob(file)); got != openCodePermissionAllow {
			t.Errorf("reading %s after loading its skill = %s, want allow (rules %v)", file, got, rules)
		}
	}
	for _, file := range []string{
		skillsDir + "/README.md",
		skillsDir + "/code-reviewer/SKILL.md",
		skillsDir + "/builtin/SKILL.md",
		"/sessions/private/xdg/config/opencode/opencode.json",
		"/sessions/other/xdg/config/opencode/skills/code-review/SKILL.md",
		"/etc/passwd",
	} {
		if got := openCodeEvaluate(rules, readGlob(file)); got != openCodePermissionDeny {
			t.Errorf("reading %s = %s, want deny (rules %v)", file, got, rules)
		}
	}
	if got := openCodeConfigRules(t, data, "read"); openCodeEvaluate(got, skillsDir+"/code-review/references/guide.md") != openCodePermissionAllow {
		t.Fatalf("read permission must allow the skill's supporting files: %v", got)
	}
}

// TestOpenCodeMutationToolsCannotRewriteProjectedSkills follows OpenCode's
// edit, write, and apply_patch tools: after external_directory they ask the
// "edit" permission with the file path relative to the project worktree, which
// is the workspace for a Git repository and "/" otherwise. The skill folders
// are readable through external_directory, so the edit rules are what keep
// those tools from rewriting a projected skill.
func TestOpenCodeMutationToolsCannotRewriteProjectedSkills(t *testing.T) {
	paths := acp.SessionPaths{Workspace: "/sessions/private/workspace", Config: "/sessions/private/xdg/config"}
	proxy := ProviderProxyBinding{BaseURL: "http://127.0.0.1:43210/_orka/provider/session", Credential: "placeholder"}
	config := func(t *testing.T, intent harnessv2.WorkspaceIntent, tools []string, skills []harnessv2.RuntimeSkill) []byte {
		t.Helper()
		request := harnessv2.CreateRuntimeSessionRequest{MCPConfiguration: harnessv2.MCPPolicyConfiguration{
			ToolPolicy: harnessv2.MCPToolPolicy{AllowedToolNames: tools},
		}}
		request.Profile.Skills = skills
		data, err := openCodeSessionConfig("openai/gpt-test", testOpenCodeModelLimits(), intent, request, paths, proxy)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	mutating := []string{"Read", "Edit", "Write"}
	data := config(t, harnessv2.WorkspaceIntentWrite, mutating, supervisorTestSkills())
	rules := openCodeConfigRules(t, data, openCodeToolEdit)
	skillsDir := "/sessions/private/xdg/config/opencode/skills"
	for _, worktree := range []string{paths.Workspace, "/"} {
		relative := func(file string) string {
			value, err := filepath.Rel(worktree, file)
			if err != nil {
				t.Fatal(err)
			}
			return value
		}
		for _, file := range []string{
			skillsDir + "/code-review/SKILL.md",
			skillsDir + "/code-review/references/guide.md",
			skillsDir + "/release-notes/scripts/deep/check.sh",
			skillsDir + "/release-notes/new-file.md",
		} {
			// The tools reach this check: the skill folder passes external_directory.
			if got := openCodeEvaluate(openCodeConfigRules(t, data, "external_directory"), filepath.Join(filepath.Dir(file), "*")); got != openCodePermissionAllow {
				t.Fatalf("external_directory for %s = %s, want allow", file, got)
			}
			if got := openCodeEvaluate(rules, relative(file)); got != openCodePermissionDeny {
				t.Errorf("editing %s from worktree %s = %s, want deny (rules %v)", file, worktree, got, rules)
			}
		}
		for _, file := range []string{
			paths.Workspace + "/main.go",
			paths.Workspace + "/code-review/SKILL.md",
			paths.Workspace + "/.config/opencode/skills/code-review/SKILL.md",
			paths.Workspace + "/xdg/config/opencode/skills/code-review/SKILL.md",
		} {
			if got := openCodeEvaluate(rules, relative(file)); got != openCodePermissionAllow {
				t.Errorf("editing workspace file %s from worktree %s = %s, want allow (rules %v)", file, worktree, got, rules)
			}
		}
	}
	// OpenCode hides a tool whose last rule is a blanket deny; the edit tools
	// must stay available for the workspace.
	if last := rules[len(rules)-1]; rules[0] != [2]string{"*", openCodePermissionAllow} || last[0] == "*" {
		t.Fatalf("edit rules must open with the blanket allow and end on a skill folder: %v", rules)
	}

	plain := func(action string) [][2]string { return [][2]string{{"*", action}} }
	for name, tc := range map[string]struct {
		data []byte
		want string
	}{
		"no skills":       {config(t, harnessv2.WorkspaceIntentWrite, mutating, nil), openCodePermissionAllow},
		"read intent":     {config(t, harnessv2.WorkspaceIntentRead, mutating, supervisorTestSkills()), openCodePermissionDeny},
		"mutation denied": {config(t, harnessv2.WorkspaceIntentWrite, []string{"Read"}, supervisorTestSkills()), openCodePermissionDeny},
	} {
		if got := openCodeConfigRules(t, tc.data, openCodeToolEdit); !slices.Equal(got, plain(tc.want)) {
			t.Errorf("%s: edit rules = %v, want a plain %s", name, got, tc.want)
		}
	}
}

// TestOpenCodeSkillEditRulesFailClosed covers a skill folder whose deny rule
// OpenCode would order before the blanket allow or rewrite.
func TestOpenCodeSkillEditRulesFailClosed(t *testing.T) {
	for _, skillsDir := range []string{"/!sessions/config/opencode/skills", "/~/config/opencode/skills"} {
		if rules, err := openCodeSkillEditRules("/workspace", skillsDir, []string{"code-review"}); err == nil {
			t.Errorf("skills folder %s produced rules %v, want an error", skillsDir, rules)
		}
	}
}

// TestVerifySkillsBoundsAStalledRead keeps a read that never returns from
// holding the supervisor past the startup probe without a stable reason.
func TestVerifySkillsBoundsAStalledRead(t *testing.T) {
	originalTimeout, originalLoad := skillCheckTimeout, loadSkill
	t.Cleanup(func() { skillCheckTimeout, loadSkill = originalTimeout, originalLoad })
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	skillCheckTimeout = 20 * time.Millisecond
	loadSkill = func(string, string, skill.Limits) (*skill.Tree, error) {
		select {
		case <-release:
		case <-time.After(10 * time.Second):
		}
		return nil, nil
	}
	err := VerifySkills(Config{Provider: ProviderProfile{Kind: providerKindCodex}, Skills: supervisorTestSkills()})
	var failure *toolbox.Failure
	if !errors.As(err, &failure) || failure.Reason != skill.ReasonReadFailed || !strings.Contains(failure.Message, "did not complete within 20ms") {
		t.Fatalf("stalled skill check = %v, want %s", err, skill.ReasonReadFailed)
	}
}
