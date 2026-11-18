/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package repocheck

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// scaffoldMarkers lists every kubebuilder scaffold marker in the repository.
// kubebuilder's create api, create webhook and edit commands insert code at
// these comments, so each one must survive refactors.
var scaffoldMarkers = []struct{ file, marker string }{
	{"cmd/main.go", "// +kubebuilder:scaffold:imports"},
	{"cmd/main.go", "// +kubebuilder:scaffold:scheme"},
	{"cmd/main.go", "// +kubebuilder:scaffold:builder"},
	{"config/crd/kustomization.yaml", "# +kubebuilder:scaffold:crdkustomizeresource"},
	{"config/crd/kustomization.yaml", "# +kubebuilder:scaffold:crdkustomizewebhookpatch"},
	{"config/default/kustomization.yaml", "# +kubebuilder:scaffold:crdkustomizecainjectionns"},
	{"config/default/kustomization.yaml", "# +kubebuilder:scaffold:crdkustomizecainjectionname"},
	{"config/samples/kustomization.yaml", "# +kubebuilder:scaffold:manifestskustomizesamples"},
	{"internal/controller/suite_test.go", "// +kubebuilder:scaffold:imports"},
	{"internal/controller/suite_test.go", "// +kubebuilder:scaffold:scheme"},
	{"test/e2e/e2e_test.go", "// +kubebuilder:scaffold:e2e-metrics-webhooks-readiness"},
	{"test/e2e/e2e_test.go", "// +kubebuilder:scaffold:e2e-webhooks-checks"},
}

// scaffoldLine matches a whole-line scaffold marker comment.
var scaffoldLine = regexp.MustCompile(`^(//|#) \+kubebuilder:scaffold:\S+$`)

// scaffoldLines returns the trimmed scaffold marker lines in data.
func scaffoldLines(data []byte) []string {
	var lines []string
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if scaffoldLine.MatchString(line) {
			lines = append(lines, line)
		}
	}
	return lines
}

func TestKubebuilderScaffoldMarkersExist(t *testing.T) {
	root := repoRoot(t)
	for _, want := range scaffoldMarkers {
		data, err := os.ReadFile(filepath.Join(root, want.file))
		if err != nil {
			t.Errorf("%s: %v; it holds the %q kubebuilder scaffold marker. "+
				"Restore the file, or move the marker and update scaffoldMarkers in hack/repocheck/scaffold_test.go.",
				want.file, err, want.marker)
			continue
		}
		if !slices.Contains(scaffoldLines(data), want.marker) {
			t.Errorf("%s: the %q line is missing. kubebuilder inserts generated code at this comment, "+
				"so never delete it. Put it back on its own line where it was: "+
				"git show origin/main:%s | grep -n -- '%s'",
				want.file, want.marker, want.file, strings.TrimLeft(want.marker, "/# "))
		}
	}
}

// TestKubebuilderScaffoldMarkerListIsComplete keeps scaffoldMarkers in step
// with the tree, so a marker added by a kubebuilder upgrade is protected too.
func TestKubebuilderScaffoldMarkerListIsComplete(t *testing.T) {
	root := repoRoot(t)
	listed := map[string]bool{}
	for _, m := range scaffoldMarkers {
		listed[m.file+"\x00"+m.marker] = true
	}
	for _, f := range trackedFiles(t, root) {
		if f.path == "AGENTS.md" || f.path == "hack/repocheck/scaffold_test.go" || f.mode != "100644" && f.mode != "100755" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(root, f.path))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(data, []byte("+kubebuilder:scaffold:")) {
			continue
		}
		for _, marker := range scaffoldLines(data) {
			if !listed[f.path+"\x00"+marker] {
				t.Errorf("%s: scaffold marker %q is not listed; add {%q, %q} to scaffoldMarkers in hack/repocheck/scaffold_test.go.",
					f.path, marker, f.path, marker)
			}
		}
	}
}
