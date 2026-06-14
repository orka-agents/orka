package cliwrapper

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/sozercan/orka/internal/workerenv"
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
func UploadTurnArtifacts(turn TurnContext) error {
	restoreTaskName := setTemporaryEnv(workerenv.TaskName, turn.TaskName)
	defer restoreTaskName()
	restoreTaskNamespace := setTemporaryEnv(workerenv.TaskNamespace, turn.Namespace)
	defer restoreTaskNamespace()
	return common.UploadArtifacts()
}

func setTemporaryEnv(key, value string) func() {
	previous, hadPrevious := os.LookupEnv(key)
	if strings.TrimSpace(value) == "" {
		return func() {}
	}
	_ = os.Setenv(key, value)
	return func() {
		if hadPrevious {
			_ = os.Setenv(key, previous)
			return
		}
		_ = os.Unsetenv(key)
	}
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
