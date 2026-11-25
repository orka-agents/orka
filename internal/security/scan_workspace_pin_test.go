package security

import (
	"reflect"
	"strings"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const (
	pinTestHead   = "0123456789abcdef0123456789abcdef01234567"
	pinTestOther  = "fedcba9876543210fedcba9876543210fedcba98"
	pinTestSHA256 = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
)

func pinTestWorkspace(branch, ref string) *corev1alpha1.WorkspaceConfig {
	return &corev1alpha1.WorkspaceConfig{
		Intent:  corev1alpha1.WorkspaceIntentRead,
		GitRepo: "https://github.com/example/repo.git",
		Branch:  branch,
		Ref:     ref,
		SubPath: "services/api",
	}
}

func TestPinScanWorkspace(t *testing.T) {
	tests := []struct {
		name      string
		workspace *corev1alpha1.WorkspaceConfig
		revision  string
		wantRef   string
	}{
		{name: "branch scan pins to the head commit", workspace: pinTestWorkspace("main", ""), revision: pinTestHead, wantRef: pinTestHead},
		{name: "a tag ref is replaced by the commit", workspace: pinTestWorkspace("", "v1.2.0"), revision: pinTestHead, wantRef: pinTestHead},
		{name: "SHA-256 object IDs pin", workspace: pinTestWorkspace("main", ""), revision: pinTestSHA256, wantRef: pinTestSHA256},
		{name: "surrounding whitespace is trimmed", workspace: pinTestWorkspace("main", ""), revision: " " + pinTestHead + "\n", wantRef: pinTestHead},
		{name: "an empty revision leaves the branch", workspace: pinTestWorkspace("main", ""), revision: "", wantRef: ""},
		{name: "a short SHA is not a pin", workspace: pinTestWorkspace("main", ""), revision: pinTestHead[:12], wantRef: ""},
		{name: "a placeholder is not a pin", workspace: pinTestWorkspace("main", "v1"), revision: "head456", wantRef: "v1"},
		{name: "uppercase hex is not a canonical object ID", workspace: pinTestWorkspace("main", ""), revision: strings.ToUpper(pinTestHead), wantRef: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			branch := tt.workspace.Branch
			got := PinScanWorkspace(tt.workspace, tt.revision)
			if got.Ref != tt.wantRef {
				t.Fatalf("Ref = %q, want %q", got.Ref, tt.wantRef)
			}
			if got.Branch != branch || got.SubPath != "services/api" || got.GitRepo != "https://github.com/example/repo.git" {
				t.Fatalf("pin changed the logical target: %#v", got)
			}
		})
	}
	if PinScanWorkspace(nil, pinTestHead) != nil {
		t.Fatal("PinScanWorkspace(nil) returned a workspace")
	}
}

func TestUnpinnedScanWorkspaceRestoresTheScanRef(t *testing.T) {
	branchScan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{Branch: "main"}}
	tagScan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{Ref: "v1.2.0"}}
	shaScan := &corev1alpha1.RepositoryScan{Spec: corev1alpha1.RepositoryScanSpec{Branch: "main", Ref: pinTestHead}}
	tests := []struct {
		name      string
		scan      *corev1alpha1.RepositoryScan
		workspace *corev1alpha1.WorkspaceConfig
		revision  string
		wantRef   string
	}{
		{name: "pinned branch scan", scan: branchScan, workspace: pinTestWorkspace("main", pinTestHead), revision: pinTestHead, wantRef: ""},
		{name: "pinned tag scan", scan: tagScan, workspace: pinTestWorkspace("", pinTestHead), revision: pinTestHead, wantRef: "v1.2.0"},
		{name: "a scan already pinned to that commit", scan: shaScan, workspace: pinTestWorkspace("main", pinTestHead), revision: pinTestHead, wantRef: pinTestHead},
		{name: "an unpinned workspace from an earlier release", scan: branchScan, workspace: pinTestWorkspace("main", ""), revision: pinTestHead, wantRef: ""},
		{name: "a workspace pinned to another commit keeps it", scan: branchScan, workspace: pinTestWorkspace("main", pinTestOther), revision: pinTestHead, wantRef: pinTestOther},
		{name: "no run revision keeps the workspace", scan: branchScan, workspace: pinTestWorkspace("main", pinTestHead), revision: "", wantRef: pinTestHead},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := *tt.workspace
			got := UnpinnedScanWorkspace(tt.scan, tt.workspace, tt.revision)
			if got.Ref != tt.wantRef {
				t.Fatalf("Ref = %q, want %q", got.Ref, tt.wantRef)
			}
			if got.Branch != before.Branch || got.SubPath != before.SubPath || got.GitRepo != before.GitRepo {
				t.Fatalf("unpin changed the target: %#v", got)
			}
			if !reflect.DeepEqual(*tt.workspace, before) {
				t.Fatalf("unpin mutated the Task workspace: %#v, want %#v", tt.workspace, before)
			}
		})
	}
}
