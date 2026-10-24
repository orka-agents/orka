package v2

import "testing"

func TestValidateWorkspaceRelativePath(t *testing.T) {
	for _, valid := range []string{"a.go", "pkg/util/a.go", ".github/workflows/ci.yml", "a..b"} {
		if err := ValidateWorkspaceRelativePath(valid); err != nil {
			t.Errorf("%q: %v", valid, err)
		}
	}
	for _, invalid := range []string{"", "/abs/a.go", "../a.go", "..", ".", "a/../b", "a//b", "a/", "./a", "a\x00b"} {
		if err := ValidateWorkspaceRelativePath(invalid); err == nil {
			t.Errorf("%q was accepted", invalid)
		}
	}
}
