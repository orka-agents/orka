package acp

import (
	"slices"
	"testing"
)

func TestOpenCodeDefaultAllowedToolsReturnsCopy(t *testing.T) {
	want := []string{"Read", "Write", "Edit", "Bash", "Glob", "Grep"}
	first := OpenCodeDefaultAllowedTools()
	if !slices.Equal(first, want) {
		t.Fatalf("defaults = %#v, want %#v", first, want)
	}
	first[0] = "changed"
	if got := OpenCodeDefaultAllowedTools(); !slices.Equal(got, want) {
		t.Fatalf("defaults shared mutable storage: %#v", got)
	}
}

func TestNormalizeOpenCodeToolPolicy(t *testing.T) {
	allowed, disallowed, allowBash := NormalizeOpenCodeToolPolicy(
		true,
		OpenCodeDefaultAllowedTools(),
		nil,
		true,
	)
	if want := []string{"glob", "read"}; !slices.Equal(allowed, want) {
		t.Fatalf("read-intent allowed = %#v, want %#v", allowed, want)
	}
	if want := []string{"apply_patch", "bash", "edit", "grep", "write"}; !slices.Equal(disallowed, want) {
		t.Fatalf("read-intent disallowed = %#v, want %#v", disallowed, want)
	}
	if allowBash {
		t.Fatal("read-intent policy retained bash")
	}

	allowed, _, allowBash = NormalizeOpenCodeToolPolicy(false, []string{"Edit"}, nil, false)
	if want := []string{"apply_patch", "edit", "write"}; !slices.Equal(allowed, want) {
		t.Fatalf("mutation aliases = %#v, want %#v", allowed, want)
	}
	if allowBash {
		t.Fatal("write-intent policy changed explicit bash denial")
	}
	if got := OpenCodeEffectiveAllowedTools([]string{"bash", "read", "write"}, []string{"write"}, false); !slices.Equal(got, []string{"read"}) {
		t.Fatalf("effective tools = %#v, want read only", got)
	}
	if got := NormalizeOpenCodeAuthorizationTools([]string{"Read", "Edit", "Bash"}); !slices.Equal(got, []string{"apply_patch", "bash", "edit", "read", "write"}) {
		t.Fatalf("authorization tools = %#v, want public names normalized with mutation aliases", got)
	}

	explicitEmpty := []string{}
	allowed, _, _ = NormalizeOpenCodeToolPolicy(false, explicitEmpty, nil, false)
	if allowed == nil || len(allowed) != 0 {
		t.Fatalf("explicit empty policy = %#v, want non-nil empty", allowed)
	}
}

func TestBuiltInRuntimeNativeToolPolicy(t *testing.T) {
	for _, provider := range []string{"codex", "claude", "copilot"} {
		if !IsBuiltInRuntimeNativeTool(provider, "WebSearch") || !IsBuiltInRuntimeNativeTool(provider, "bash") {
			t.Fatalf("%s native tool policy omitted a reviewed tool", provider)
		}
		if IsBuiltInRuntimeNativeTool(provider, "custom_tool") {
			t.Fatalf("%s native tool policy accepted a custom tool", provider)
		}
	}
	if !IsBuiltInRuntimeNativeTool("opencode", "apply_patch") || IsBuiltInRuntimeNativeTool("opencode", "WebSearch") {
		t.Fatal("OpenCode native tool policy does not match the reviewed runtime surface")
	}

	tools := BuiltInRuntimeNativeToolNames("opencode")
	if len(tools) == 0 {
		t.Fatal("OpenCode native tool policy is empty")
	}
	tools[0] = "changed"
	if IsBuiltInRuntimeNativeTool("opencode", "changed") {
		t.Fatal("BuiltInRuntimeNativeToolNames returned mutable policy storage")
	}
}

func TestNormalizeBuiltInRuntimeToolPolicy(t *testing.T) {
	allowed, disallowed, allowBash := NormalizeBuiltInRuntimeToolPolicy("claude", nil, []string{"Write"}, true)
	if !slices.Equal(allowed, []string{"Bash", "Edit", "Glob", "Grep", "Read", "WebFetch", "WebSearch"}) {
		t.Fatalf("deny-only allowed tools = %#v", allowed)
	}
	if !slices.Equal(disallowed, []string{"Write"}) || !allowBash {
		t.Fatalf("deny-only metadata changed: disallowed=%#v allowBash=%v", disallowed, allowBash)
	}

	allowed, _, allowBash = NormalizeBuiltInRuntimeToolPolicy("claude", nil, nil, false)
	if !slices.Equal(allowed, []string{"Edit", "Glob", "Grep", "Read", "WebFetch", "WebSearch", "Write"}) || allowBash {
		t.Fatalf("bash-disabled policy = %#v allowBash=%v", allowed, allowBash)
	}

	explicitEmpty := []string{}
	allowed, _, _ = NormalizeBuiltInRuntimeToolPolicy("claude", explicitEmpty, nil, true)
	if allowed == nil || len(allowed) != 0 {
		t.Fatalf("explicit empty allowlist = %#v, want explicit deny-all", allowed)
	}

	if got := BuiltInRuntimeEffectiveAllowedTools([]string{"Read", "Write", "Bash"}, []string{"Write"}, false); !slices.Equal(got, []string{"Read"}) {
		t.Fatalf("effective narrowed tools = %#v, want Read only", got)
	}
	if got := BuiltInRuntimeEffectiveAllowedTools(nil, []string{"Write"}, true); got != nil {
		t.Fatalf("unrestricted effective tools = %#v, want nil sentinel", got)
	}
	if BuiltInRuntimeEffectiveAllowBash(nil, []string{"bAsH"}, true) {
		t.Fatal("case-insensitive Bash deny did not close the effective Bash gate")
	}
	if !BuiltInRuntimeEffectiveAllowBash(nil, []string{"Write"}, true) {
		t.Fatal("non-Bash deny unexpectedly closed the effective Bash gate")
	}
	if BuiltInRuntimeEffectiveAllowBash([]string{"Read"}, nil, true) {
		t.Fatal("explicit allowlist without Bash unexpectedly kept the Bash gate open")
	}
	if !BuiltInRuntimeEffectiveAllowBash([]string{"Read", "bAsH"}, nil, true) {
		t.Fatal("explicit allowlist containing Bash did not keep the Bash gate open")
	}
	if !BuiltInRuntimeEffectiveAllowBash([]string{"Bash(git status:*)"}, nil, true) {
		t.Fatal("scoped Bash allowlist rule did not keep the Bash gate open")
	}
	if got := BuiltInRuntimeEffectiveAllowedTools([]string{"Read", "Bash(git status:*)"}, nil, false); !slices.Equal(got, []string{"Read"}) {
		t.Fatalf("allowBash=false effective tools = %#v, want scoped Bash removed", got)
	}
	if got := BuiltInRuntimeEffectiveAllowedTools([]string{"Read", "Bash(git status:*)"}, []string{"Bash"}, true); !slices.Equal(got, []string{"Read"}) {
		t.Fatalf("bare Bash deny effective tools = %#v, want scoped Bash removed", got)
	}
}
