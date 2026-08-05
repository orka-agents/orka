package safesymlink

import "testing"

func TestValidateGraph(t *testing.T) {
	t.Parallel()
	paths := map[string]struct{}{
		".agents/skills/readme": {},
		".claude/skills/readme": {},
	}
	links := map[string]string{
		".claude/skills/readme": "../../.agents/skills/readme",
	}
	if err := ValidateGraph(paths, links, 4096, 4096); err != nil {
		t.Fatalf("safe graph rejected: %v", err)
	}
}

func TestValidateGraphRejectsUnsafeLinks(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name  string
		paths map[string]struct{}
		links map[string]string
	}{
		{name: "absolute", paths: map[string]struct{}{"link": {}}, links: map[string]string{"link": "/etc/passwd"}},
		{name: "escape", paths: map[string]struct{}{"dir/link": {}}, links: map[string]string{"dir/link": "../../outside"}},
		{name: "cycle", paths: map[string]struct{}{"a": {}, "b": {}}, links: map[string]string{"a": "b", "b": "a"}},
		{name: "nested entry", paths: map[string]struct{}{"link": {}, "link/file": {}}, links: map[string]string{"link": "target"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateGraph(test.paths, test.links, 4096, 4096); err == nil {
				t.Fatal("unsafe symlink graph was accepted")
			}
		})
	}
}
