package supervisor

import (
	"testing"

	"github.com/orka-agents/orka/internal/acp"
)

const testSessionRoot = "/sessions/abc"

func TestSessionWorkspaceOutsideRoot(t *testing.T) {
	cases := []struct {
		name      string
		root      string
		workspace string
		outside   bool
	}{
		{name: "inside root", root: testSessionRoot, workspace: testSessionRoot + "/workspace", outside: false},
		{name: "root itself", root: testSessionRoot, workspace: testSessionRoot, outside: false},
		{name: "durable volume", root: testSessionRoot, workspace: "/workspace-data/ws-uid-1", outside: true},
		{name: "sibling prefix is not containment", root: testSessionRoot, workspace: testSessionRoot + "def/workspace", outside: true},
		{name: "trailing slash root", root: testSessionRoot + "/", workspace: testSessionRoot + "/workspace", outside: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sessionWorkspaceOutsideRoot(acp.SessionPaths{Root: tc.root, Workspace: tc.workspace})
			if got != tc.outside {
				t.Fatalf("sessionWorkspaceOutsideRoot(%q, %q) = %v, want %v", tc.root, tc.workspace, got, tc.outside)
			}
		})
	}
}

// A resumed lineage's checkpoint mismatch may be wiped only under the
// controller's exact prior-identity assertion for a verified publication
// transition; every other foreign identity stays fail-closed.
func TestDurableResumeTransitionAuthorized(t *testing.T) {
	t.Parallel()
	if durableResumeTransitionAuthorized("github.com/o/source", "abc", "") {
		t.Fatal("a mismatch without a prior-identity assertion must stay fail-closed")
	}
	if durableResumeTransitionAuthorized("github.com/o/source", "abc", "github.com/other/repo") {
		t.Fatal("a checkpoint bound to a foreign identity must not be wiped")
	}
	if !durableResumeTransitionAuthorized("github.com/o/source", "abc", "github.com/o/source") {
		t.Fatal("the asserted prior identity must authorize the transition")
	}
	if !durableResumeTransitionAuthorized("github.com/O/Source", "abc", "github.com/o/source") {
		t.Fatal("GitHub identities compare case-insensitively")
	}
}
