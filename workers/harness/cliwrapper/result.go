package cliwrapper

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sozercan/orka/workers/common"
)

// FinalizeTurnResult reuses the existing worker structured-result and workspace
// diff finalization behavior. A non-git or empty workDir falls back to the raw
// agent output, matching workers/common.FinalizeResult semantics.
func FinalizeTurnResult(workDir, output string) ([]byte, error) {
	return common.FinalizeResult(workDir, output)
}

// UploadTurnArtifacts reuses the existing worker artifact uploader. It is a
// no-op when /tmp/artifacts is absent.
func UploadTurnArtifacts() error {
	return common.UploadArtifacts()
}

func ShouldFinalizeWorkDir(workDir string) bool {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return false
	}
	if _, err := os.Stat(filepath.Join(workDir, ".git")); err == nil {
		return true
	}
	return false
}
