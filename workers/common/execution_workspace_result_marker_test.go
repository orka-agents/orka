/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package common

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/orka/internal/workerenv"
	"github.com/sozercan/orka/internal/workspace"
)

func TestWorkspaceStdoutResultMarkerDownloadsMarkerWhenStdoutTruncated(t *testing.T) {
	t.Setenv(workerenv.ResultStdout, "true")

	rawResult := `{"kind":"typed-review","payload":"downloaded-result"}`
	marker := workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString([]byte(rawResult))
	recorder := newRecordingWorkspaceExecutor()
	claim, err := recorder.Claim(context.Background(), workspace.ClaimRequest{
		Namespace: "task-ns",
		TaskName:  "task-name",
		Template:  workspace.TemplateRef{Name: "agent-template"},
		Timeout:   time.Second,
	})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if _, err := recorder.Upload(context.Background(), workspace.UploadRequest{
		Ref: claim.Ref,
		Artifacts: []workspace.UploadArtifact{{
			Path: agentSandboxResultMarkerUploadPath,
			Data: []byte(marker + "\n"),
			Mode: 0o600,
		}},
		Timeout: time.Second,
	}); err != nil {
		t.Fatalf("Upload() error = %v", err)
	}

	got, err := workspaceStdoutResultMarker(
		context.Background(),
		recorder,
		claim.Ref,
		time.Second,
		&workspace.ExecResult{
			Stdout:          workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString([]byte("partial")),
			StdoutTruncated: true,
		},
		"",
	)
	if err != nil {
		t.Fatalf("workspaceStdoutResultMarker() error = %v", err)
	}
	if got != marker {
		t.Fatalf("workspaceStdoutResultMarker() = %q, want downloaded marker %q", got, marker)
	}
}

func TestStdoutResultMarkerUsesLastMarkerLine(t *testing.T) {
	first := workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString([]byte("first"))
	last := workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString([]byte("last"))
	got, ok := stdoutResultMarker(strings.Join([]string{"log", first, "more", last}, "\n"))
	if !ok || got != last {
		t.Fatalf("stdoutResultMarker() = %q, %t; want last marker", got, ok)
	}
}

func TestValidateStdoutResultNonce(t *testing.T) {
	data := strings.Join([]string{
		agentSandboxResultTokenPrefix + "nonce-a",
		workerenv.ResultStdoutPrefix + base64.StdEncoding.EncodeToString([]byte("payload")),
	}, "\n")
	if err := validateStdoutResultToken(data, "nonce-a"); err != nil {
		t.Fatalf("validateStdoutResultToken() error = %v", err)
	}
	if err := validateStdoutResultToken(data, "nonce-b"); err == nil || !strings.Contains(err.Error(), "did not match") {
		t.Fatalf("validateStdoutResultToken mismatch error = %v, want mismatch", err)
	}
	err := validateStdoutResultToken(workerenv.ResultStdoutPrefix+"payload", "nonce-a")
	if err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("validateStdoutResultToken missing error = %v, want missing", err)
	}
}

func TestGenerateWorkspaceStdoutResultNonce(t *testing.T) {
	nonce, err := generateWorkspaceStdoutResultToken()
	if err != nil {
		t.Fatalf("generateWorkspaceStdoutResultToken() error = %v", err)
	}
	if len(nonce) != 64 {
		t.Fatalf("token length = %d, want 64", len(nonce))
	}
	for _, ch := range nonce {
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'f') {
			t.Fatalf("nonce = %q, want lowercase hex", nonce)
		}
	}
}
