// Package daemonprotocol defines the private Orka Workspace Daemon HTTP
// protocol shared by the worker-side daemon and WorkspaceExecutor adapters.
package daemonprotocol

import (
	"net/url"
	"strings"
	"time"
)

const (
	HealthPath        = "/healthz"
	ExecPath          = "/v1/exec"
	ExecStatusPrefix  = ExecPath + "/"
	FilesPath         = "/v1/files"
	FilesDownloadPath = "/v1/files/download"
	ScrubPath         = "/v1/scrub"
)

type ExecRequest struct {
	Command        []string          `json:"command"`
	Env            map[string]string `json:"env,omitempty"`
	WorkDir        string            `json:"workDir,omitempty"`
	Stdin          []byte            `json:"stdin,omitempty"`
	TimeoutSeconds int64             `json:"timeoutSeconds,omitempty"`
	MaxOutputBytes int64             `json:"maxOutputBytes,omitempty"`
	Detach         bool              `json:"detach,omitempty"`
	Resident       bool              `json:"resident,omitempty"`
	ResidentKey    string            `json:"residentKey,omitempty"`
}

type ExecResponse struct {
	ExecID          string    `json:"execId,omitempty"`
	Running         bool      `json:"running,omitempty"`
	Stdout          string    `json:"stdout"`
	Stderr          string    `json:"stderr"`
	ExitCode        int       `json:"exitCode"`
	StdoutTruncated bool      `json:"stdoutTruncated"`
	StderrTruncated bool      `json:"stderrTruncated"`
	StartedAt       time.Time `json:"startedAt"`
	FinishedAt      time.Time `json:"finishedAt"`
}

type UploadRequest struct {
	Files []UploadFile `json:"files"`
}

type UploadFile struct {
	Path    string    `json:"path"`
	Data    []byte    `json:"data"`
	Mode    uint32    `json:"mode,omitempty"`
	ModTime time.Time `json:"modTime,omitempty"`
}

type UploadResponse struct {
	Artifacts []Artifact `json:"artifacts"`
}

type DownloadRequest struct {
	Paths []string `json:"paths,omitempty"`
}

type DownloadResponse struct {
	Artifacts []DownloadedArtifact `json:"artifacts"`
}

type ScrubRequest struct {
	Paths []string `json:"paths"`
}

type Artifact struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Digest  string    `json:"digest"`
	Mode    uint32    `json:"mode"`
	ModTime time.Time `json:"modTime"`
}

type DownloadedArtifact struct {
	Artifact
	Data []byte `json:"data"`
}

func ExecStatusPath(execID string) string {
	return ExecStatusPrefix + url.PathEscape(strings.TrimSpace(execID))
}
