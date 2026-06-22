/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/internal/workspace"
)

func forwardWorkspaceStdoutResultMarker(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	timeout time.Duration,
	result *workspace.ExecResult,
	expectedMarkerNonce string,
) error {
	if !workerenv.IsTrue(os.Getenv(workerenv.ResultStdout)) {
		return nil
	}
	marker, err := workspaceStdoutResultMarker(ctx, executor, ref, timeout, result, expectedMarkerNonce)
	if err != nil {
		return err
	}
	if marker == "" {
		return fmt.Errorf(
			"%s is true but inner worker did not write %s",
			workerenv.ResultStdout,
			workerenv.ResultStdoutPrefix,
		)
	}
	fmt.Println(marker)
	return nil
}

func forwardWorkspaceStdoutResultMarkerIfPresent(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	timeout time.Duration,
	result *workspace.ExecResult,
	expectedMarkerNonce string,
) {
	if !workerenv.IsTrue(os.Getenv(workerenv.ResultStdout)) {
		return
	}
	marker, err := workspaceStdoutResultMarker(ctx, executor, ref, timeout, result, expectedMarkerNonce)
	if err != nil {
		fmt.Fprintf(os.Stderr, "warning: failed to forward stdout result marker: %v\n", err)
		return
	}
	if marker != "" {
		fmt.Println(marker)
	}
}

func workspaceStdoutResultMarker(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	timeout time.Duration,
	result *workspace.ExecResult,
	expectedMarkerNonce string,
) (string, error) {
	if !workerenv.IsTrue(os.Getenv(workerenv.ResultStdout)) {
		return "", nil
	}

	if result != nil {
		if result.StdoutTruncated {
			marker, downloadErr := downloadStdoutResultMarker(ctx, executor, ref, timeout, expectedMarkerNonce)
			if marker != "" {
				return marker, nil
			}
			if downloadErr != nil {
				return "", fmt.Errorf("download stdout result marker after truncated stdout: %w", downloadErr)
			}
			return "", fmt.Errorf(
				"inner worker stdout was truncated before %s could be forwarded and marker file was not available",
				workerenv.ResultStdoutPrefix,
			)
		}
		if marker, ok := stdoutResultMarker(result.Stdout); ok {
			return marker, nil
		}
	}
	marker, downloadErr := downloadStdoutResultMarker(ctx, executor, ref, timeout, expectedMarkerNonce)
	if marker != "" {
		return marker, nil
	}
	if downloadErr != nil && !workspace.IsKind(downloadErr, workspace.ErrorKindNotFound) {
		return "", fmt.Errorf("download stdout result marker: %w", downloadErr)
	}
	return "", nil
}

func downloadStdoutResultMarker(
	ctx context.Context,
	executor workspace.WorkspaceExecutor,
	ref workspace.WorkspaceRef,
	timeout time.Duration,
	expectedMarkerNonce string,
) (string, error) {
	if executor == nil || ref.IsZero() {
		return "", workspace.NewError(
			"download",
			workspace.ErrorKindNotFound,
			"workspace reference is unavailable",
			false,
			nil,
		)
	}
	result, err := executor.Download(ctx, workspace.DownloadRequest{
		Ref:     ref,
		Paths:   []string{agentSandboxResultMarkerUploadPath},
		Timeout: timeout,
	})
	if err != nil {
		return "", err
	}
	for _, artifact := range result.Artifacts {
		if artifact.Path != agentSandboxResultMarkerUploadPath {
			continue
		}
		data := string(artifact.Data)
		if err := validateStdoutResultToken(data, expectedMarkerNonce); err != nil {
			return "", err
		}
		if marker, ok := stdoutResultMarker(data); ok {
			return marker, nil
		}
		return "", fmt.Errorf("downloaded stdout result marker did not contain %s", workerenv.ResultStdoutPrefix)
	}
	return "", workspace.NewError(
		"download",
		workspace.ErrorKindNotFound,
		"stdout result marker artifact not found",
		false,
		nil,
	)
}
