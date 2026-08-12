package safesymlink

import (
	"fmt"
	"testing"
)

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
		{name: "chain cycle", paths: map[string]struct{}{"a": {}, "b": {}, "c": {}}, links: map[string]string{"a": "b", "b": "c", "c": "a"}},
		{name: "nested entry", paths: map[string]struct{}{"link": {}, "link/file": {}}, links: map[string]string{"link": "target"}},
		{name: "link below link", paths: map[string]struct{}{"a": {}, "a/b": {}}, links: map[string]string{"a": "target", "a/b": "other"}},
		{name: "trailing separator entry", paths: map[string]struct{}{"link": {}, "link/": {}}, links: map[string]string{"link": "target"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateGraph(test.paths, test.links, 4096, 4096); err == nil {
				t.Fatal("unsafe symlink graph was accepted")
			}
		})
	}
}

func TestValidateGraphPrefixBoundaries(t *testing.T) {
	t.Parallel()
	paths := map[string]struct{}{
		"a/b":    {},
		"a/bc":   {},
		"ab":     {},
		"A/file": {},
	}
	links := map[string]string{"a/b": "c"}
	if err := ValidateGraph(paths, links, 4096, 4096); err != nil {
		t.Fatalf("entries sharing a link's byte prefix were rejected: %v", err)
	}
}

func TestValidateGraphAcceptsDuplicateTargets(t *testing.T) {
	t.Parallel()
	paths := map[string]struct{}{"first": {}, "second": {}}
	links := map[string]string{"first": "shared", "second": "shared"}
	if err := ValidateGraph(paths, links, 4096, 4096); err != nil {
		t.Fatalf("links sharing one resolved target were rejected: %v", err)
	}
}

func TestValidateGraphAcceptsChainWithRemainder(t *testing.T) {
	t.Parallel()
	paths := map[string]struct{}{"chain": {}, "hop": {}}
	links := map[string]string{"chain": "hop/tail", "hop": "real"}
	if err := ValidateGraph(paths, links, 4096, 4096); err != nil {
		t.Fatalf("safe chained graph rejected: %v", err)
	}
}

func BenchmarkValidateGraph(b *testing.B) {
	paths := make(map[string]struct{}, 22000)
	links := make(map[string]string, 2000)
	for index := range 20000 {
		paths[fmt.Sprintf("src/pkg%03d/file%05d.go", index%200, index)] = struct{}{}
	}
	for index := range 2000 {
		linkPath := fmt.Sprintf("node_modules/mod%04d/link", index)
		paths[linkPath] = struct{}{}
		links[linkPath] = fmt.Sprintf("../../vendor/mod%04d", index)
	}
	b.ReportAllocs()

	for b.Loop() {
		if err := ValidateGraph(paths, links, 4096, 4096); err != nil {
			b.Fatalf("ValidateGraph() error = %v", err)
		}
	}
}
