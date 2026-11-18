//go:build linux

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func compileValidationGitFixture(t *testing.T, source, destination string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "cc", "-std=c11", "-O2", "-Wall", "-Wextra", "-Werror",
		"-static", "-fstack-protector-strong", "-D_FORTIFY_SOURCE=2", "-Wl,-z,relro,-z,now",
		"-o", destination, source).CombinedOutput()
	require.NoError(t, err, string(output))
}

func validationGitProbeEnvironment(t *testing.T, home string) []string {
	t.Helper()
	var processes unix.Rlimit
	require.NoError(t, unix.Getrlimit(unix.RLIMIT_NPROC, &processes))
	return []string{
		"PATH=/usr/bin:/bin", "HOME=" + home,
		"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		fmt.Sprintf("ORKA_TEST_NPROC_SOFT=%d", processes.Cur),
		fmt.Sprintf("ORKA_TEST_NPROC_HARD=%d", processes.Max),
		"ORKA_TEST_LARGE_ENV=" + strings.Repeat("x", 96<<10),
	}
}

func TestValidationGitNativeLauncher(t *testing.T) {
	directory := t.TempDir()
	launcher, probe := filepath.Join(directory, "launcher"), filepath.Join(directory, "probe")
	compileValidationGitFixture(t, "gitlauncher/launcher.c", launcher)
	compileValidationGitFixture(t, "testdata/validation_git_probe.c", probe)
	for _, args := range [][]string{nil, {"/usr/bin/git"}, {"/bin/sh", "-c", "echo should-not-run"}} {
		command := exec.Command(launcher, args...)
		output, err := command.CombinedOutput()
		var exit *exec.ExitError
		require.ErrorAs(t, err, &exit)
		require.Equal(t, 125, exit.ExitCode())
		require.Equal(t, "validation Git launcher unavailable\n", string(output))
	}
	for range 8 {
		command := exec.Command(launcher, "/usr/bin/git", "-c", "alias.probe=!exec \""+probe+"\"", "probe")
		command.Dir = directory
		command.Env = validationGitProbeEnvironment(t, directory)
		output, err := command.CombinedOutput()
		require.NoError(t, err, string(output))
		require.Equal(t, "NATIVE_LIMITS_OK\n", string(output))
	}
}

func TestProductionValidationGitLauncherContainer(t *testing.T) {
	if os.Getenv("ORKA_TEST_VALIDATION_GIT_CHILD") == "1" {
		var processes unix.Rlimit
		require.NoError(t, unix.Getrlimit(unix.RLIMIT_NPROC, &processes))
		t.Setenv("ORKA_TEST_NPROC_SOFT", fmt.Sprint(processes.Cur))
		t.Setenv("ORKA_TEST_NPROC_HARD", fmt.Sprint(processes.Max))
		t.Setenv("ORKA_TEST_LARGE_ENV", strings.Repeat("x", 96<<10))
		err := runBoundedValidationGit([]string{
			"/usr/bin/git", "-c", "alias.probe=!exec /fixture/probe", "probe",
		})
		t.Fatalf("bounded exec returned instead of replacing the process: %v", err)
	}
	image := os.Getenv("ORKA_VALIDATION_LIVE_TOOL_IMAGE")
	if image == "" {
		t.Skip("set ORKA_VALIDATION_LIVE_TOOL_IMAGE for production-shaped Git launcher conformance")
	}
	require.True(t, strings.Contains(image, "@sha256:") || strings.HasPrefix(image, "sha256:"),
		"production launcher conformance requires an immutable local image")
	directory := t.TempDir()
	require.NoError(t, os.Chmod(directory, 0755))
	launcher, probe := filepath.Join(directory, "launcher"), filepath.Join(directory, "probe")
	compileValidationGitFixture(t, "gitlauncher/launcher.c", launcher)
	compileValidationGitFixture(t, "testdata/validation_git_probe.c", probe)
	executable, err := os.Executable()
	require.NoError(t, err)
	content, err := os.ReadFile(executable)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(filepath.Join(directory, "general.test"), content, 0555))
	name := fmt.Sprintf("orka-git-conformance-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "rm", "--force", name).Run()
	})
	args := []string{
		"run", "--rm", "--name", name, "--pull", "never", "--network", "none", "--read-only",
		"--user", "1000:1000", "--cap-drop", "ALL", "--security-opt", "no-new-privileges=true",
		"--cpus", "1", "--memory", "512m", "--memory-swap", "512m", "--pids-limit", "512",
		"--mount", "type=bind,src=" + directory + ",dst=/fixture,readonly",
		"--mount", "type=bind,src=" + launcher + ",dst=/usr/local/libexec/orka-validation-git,readonly",
		"--tmpfs", "/workspace:rw,exec,nosuid,nodev,size=256m,mode=0700,uid=1000,gid=1000",
		"--env", "HOME=/workspace", "--env", "GIT_CONFIG_NOSYSTEM=1", "--env", "GIT_CONFIG_GLOBAL=/dev/null",
		"--env", "ORKA_TEST_VALIDATION_GIT_CHILD=1", "--entrypoint", "/bin/sh", image,
		"-ceu", `i=0; while [ "$i" -lt 32 ]; do ` +
			`/fixture/general.test -test.run=^TestProductionValidationGitLauncherContainer$; i=$((i + 1)); done`,
	}
	ctx, cancel := context.WithTimeout(t.Context(), 90*time.Second)
	defer cancel()
	output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput()
	require.NoError(t, err, string(output))
	require.Equal(t, 32, strings.Count(string(output), "NATIVE_LIMITS_OK\n"))
}
