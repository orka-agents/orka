/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/sozercan/orka/internal/workerenv"

	"github.com/sozercan/orka/workers/common"
)

const workspaceDir = "/workspace"

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

func run() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer cancel()

	if len(os.Args) > 1 && os.Args[1] == "--prepare-workspace-only" {
		return prepareWorkspace(ctx)
	}

	workDir, err := prepareWorkspaceIfConfigured(ctx)
	if err != nil {
		return err
	}

	// Get command from arguments or environment
	var command []string
	if len(os.Args) > 1 {
		command = os.Args[1:]
	} else {
		cmdStr := os.Getenv(workerenv.Command)
		if cmdStr == "" {
			return fmt.Errorf("no command specified")
		}
		command = strings.Fields(cmdStr)
	}

	if len(command) == 0 {
		return fmt.Errorf("command cannot be empty")
	}

	// Execute the command and print output to stdout/stderr.
	// The controller captures pod logs and writes them to a result ConfigMap.
	var stdout, stderr bytes.Buffer

	cmd := exec.CommandContext(ctx, command[0], command[1:]...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	cmd.Env = os.Environ()
	if workDir != "" {
		cmd.Dir = workDir
	}

	err = cmd.Run()

	if stdout.Len() > 0 {
		fmt.Print(stdout.String())
	}
	if stderr.Len() > 0 {
		fmt.Fprint(os.Stderr, stderr.String())
	}

	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			_ = submitResult(workDir, stdout.String()+stderr.String())
			os.Exit(exitErr.ExitCode())
		}
		return err
	}

	if err := submitResult(workDir, stdout.String()+stderr.String()); err != nil {
		return err
	}

	return nil
}

func prepareWorkspaceIfConfigured(ctx context.Context) (string, error) {
	if os.Getenv(workerenv.GitRepo) == "" {
		return "", nil
	}
	if _, err := os.Stat(filepath.Join(workspaceDir, ".git")); err == nil {
		return workspaceRoot(), nil
	}
	if err := prepareWorkspace(ctx); err != nil {
		return "", err
	}
	return workspaceRoot(), nil
}

func prepareWorkspace(ctx context.Context) error {
	cfg, err := common.LoadWorkspaceConfig()
	if err != nil {
		return err
	}
	if cfg.GitRepo == "" {
		return nil
	}

	common.SetupGitCredentials()
	if _, err := os.Stat(filepath.Join(workspaceDir, ".git")); os.IsNotExist(err) {
		if err := common.CloneRepo(ctx, cfg, workspaceDir); err != nil {
			return err
		}
	} else if err != nil {
		return fmt.Errorf("stat workspace: %w", err)
	}
	if err := common.PrepareWorkspace(workspaceDir); err != nil {
		return err
	}
	return common.EnsureWorkspaceArtifactsLink(workspaceDir)
}

func workspaceRoot() string {
	if subPath := os.Getenv(workerenv.WorkspaceSubpath); subPath != "" {
		return filepath.Join(workspaceDir, subPath)
	}
	return workspaceDir
}

func submitResult(workDir, output string) error {
	if os.Getenv(workerenv.ResultEndpoint) == "" && os.Getenv(workerenv.ControllerURL) == "" {
		return nil
	}
	resultDir := ""
	if workDir != "" {
		resultDir = workspaceDir
	}
	resultBytes, err := common.FinalizeResult(resultDir, output)
	if err != nil {
		return err
	}
	if err := common.SubmitResult(resultBytes); err != nil {
		return err
	}
	return common.UploadArtifacts()
}
