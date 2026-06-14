package cliwrapper

import (
	"fmt"
	"os"
	"os/exec"
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
func ClearTurnArtifacts() {
	_ = os.RemoveAll("/tmp/artifacts")
}

func UploadTurnArtifacts(turn TurnContext) error {
	restoreTaskName := setTemporaryEnv(workerenv.TaskName, turn.TaskName)
	defer restoreTaskName()
	restoreTaskNamespace := setTemporaryEnv(workerenv.TaskNamespace, turn.Namespace)
	defer restoreTaskNamespace()
	err := common.UploadArtifacts()
	return err
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
	return exec.Command("git", "-C", workDir, "rev-parse", "--show-toplevel").Run() == nil
}

func CleanFinalizedWorkDir(workDir string) error {
	workDir = strings.TrimSpace(workDir)
	if workDir == "" {
		return nil
	}
	rootOut, err := exec.Command("git", "-C", workDir, "rev-parse", "--show-toplevel").Output()
	if err != nil {
		return nil
	}
	repoRoot := strings.TrimSpace(string(rootOut))
	if repoRoot == "" {
		return nil
	}
	cleanPath, err := filepath.Abs(workDir)
	if err != nil {
		return fmt.Errorf("clean finalized workdir path: %w", err)
	}
	repoRoot, err = filepath.Abs(repoRoot)
	if err != nil {
		return fmt.Errorf("clean finalized repo root path: %w", err)
	}
	relPath, err := filepath.Rel(repoRoot, cleanPath)
	if err != nil || strings.HasPrefix(relPath, "..") || filepath.IsAbs(relPath) {
		return fmt.Errorf("clean finalized workdir %q is outside repository root %q", cleanPath, repoRoot)
	}
	if relPath == "." {
		if out, err := exec.Command("git", "-C", repoRoot, "reset", "--hard", "HEAD").CombinedOutput(); err != nil {
			return fmt.Errorf("clean finalized workdir reset: %w: %s", err, strings.TrimSpace(string(out)))
		}
		if out, err := exec.Command("git", "-C", repoRoot, "clean", "-fd").CombinedOutput(); err != nil {
			return fmt.Errorf("clean finalized workdir clean: %w: %s", err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	if out, err := exec.Command("git", "-C", repoRoot, "reset", "HEAD", "--", relPath).CombinedOutput(); err != nil {
		return fmt.Errorf("clean finalized workdir unstage: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("git", "-C", repoRoot, "checkout", "--", relPath).CombinedOutput(); err != nil {
		return fmt.Errorf("clean finalized workdir checkout: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if out, err := exec.Command("git", "-C", repoRoot, "clean", "-fd", "--", relPath).CombinedOutput(); err != nil {
		return fmt.Errorf("clean finalized workdir clean: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
