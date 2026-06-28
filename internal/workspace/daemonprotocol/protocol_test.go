package daemonprotocol

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestWorkspaceDaemonPaths(t *testing.T) {
	if HealthPath != "/healthz" || ExecPath != "/v1/exec" || ExecStatusPrefix != "/v1/exec/" || FilesPath != "/v1/files" || FilesDownloadPath != "/v1/files/download" || ScrubPath != "/v1/scrub" {
		t.Fatalf("unexpected daemon paths")
	}
	if got := ExecStatusPath("exec id/1"); got != "/v1/exec/exec%20id%2F1" {
		t.Fatalf("ExecStatusPath() = %q, want escaped status path", got)
	}
}

func TestWorkspaceDaemonProtocolJSONShape(t *testing.T) {
	started := time.Date(2026, 6, 21, 12, 0, 0, 0, time.UTC)
	finished := started.Add(time.Second)
	payload := struct {
		Exec     ExecRequest     `json:"exec"`
		Result   ExecResponse    `json:"result"`
		Upload   UploadRequest   `json:"upload"`
		Download DownloadRequest `json:"download"`
		Scrub    ScrubRequest    `json:"scrub"`
	}{
		Exec: ExecRequest{
			Command:        []string{"sh", "-c", "echo ok"},
			Env:            map[string]string{"A": "B"},
			WorkDir:        "/workspace",
			Stdin:          []byte("in"),
			TimeoutSeconds: 30,
			MaxOutputBytes: 1024,
			Detach:         true,
			Resident:       true,
			ResidentKey:    "session",
		},
		Result:   ExecResponse{ExecID: "exec-1", Running: true, Stdout: "out", Stderr: "err", ExitCode: 2, StdoutTruncated: true, StderrTruncated: true, StartedAt: started, FinishedAt: finished},
		Upload:   UploadRequest{Files: []UploadFile{{Path: "file", Data: []byte("data"), Mode: 0o600, ModTime: started}}},
		Download: DownloadRequest{Paths: []string{"file"}},
		Scrub:    ScrubRequest{Paths: []string{"/app/runtime-file"}},
	}
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	wantFields := []string{
		`"command"`, `"env"`, `"workDir"`, `"stdin"`, `"timeoutSeconds"`, `"maxOutputBytes"`, `"detach"`, `"resident"`, `"residentKey"`,
		`"execId"`, `"running"`, `"stdout"`, `"stderr"`, `"exitCode"`, `"stdoutTruncated"`, `"stderrTruncated"`, `"startedAt"`, `"finishedAt"`,
		`"files"`, `"path"`, `"data"`, `"mode"`, `"modTime"`, `"paths"`,
	}
	for _, field := range wantFields {
		if !jsonFieldPresent(data, field) {
			t.Fatalf("marshaled protocol payload missing %s: %s", field, data)
		}
	}
}

func jsonFieldPresent(data []byte, field string) bool {
	return json.Valid(data) && strings.Contains(string(data), field)
}
