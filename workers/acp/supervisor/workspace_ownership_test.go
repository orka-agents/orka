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
