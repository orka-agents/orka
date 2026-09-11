/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package harness

import "testing"

func TestWrapperGitHostAllowed(t *testing.T) {
	for _, host := range []string{"github.com", "GITLAB.COM", " bitbucket.org "} {
		if !WrapperGitHostAllowed(host, "") {
			t.Fatalf("default wrapper host %q rejected", host)
		}
	}
	if WrapperGitHostAllowed("codeberg.org", "") {
		t.Fatal("unconfigured wrapper host accepted")
	}
	if !WrapperGitHostAllowed("codeberg.org", " git.example.com, CODEBERG.ORG ") {
		t.Fatal("configured wrapper host rejected")
	}
}
