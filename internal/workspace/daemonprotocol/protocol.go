// Package daemonprotocol preserves the legacy provider-routed client surface while
// the public DTOs live in pkg/workspaceagent. New adapters must import the public
// package directly.
package daemonprotocol

import workspaceagent "github.com/orka-agents/orka/pkg/workspaceagent"

const (
	HealthPath        = workspaceagent.LegacyHealthPath
	ExecPath          = workspaceagent.ExecPath
	ExecStatusPrefix  = workspaceagent.ExecStatusPrefix
	FilesPath         = workspaceagent.FilesPath
	FilesDownloadPath = workspaceagent.FilesDownloadPath
	ScrubPath         = workspaceagent.ScrubPath
)

type ExecRequest = workspaceagent.ExecRequest
type ExecResponse = workspaceagent.ExecResponse
type UploadRequest = workspaceagent.UploadRequest
type UploadFile = workspaceagent.UploadFile
type UploadResponse = workspaceagent.UploadResponse
type DownloadRequest = workspaceagent.DownloadRequest
type DownloadResponse = workspaceagent.DownloadResponse
type ScrubRequest = workspaceagent.ScrubRequest
type Artifact = workspaceagent.Artifact
type DownloadedArtifact = workspaceagent.DownloadedArtifact

func ExecStatusPath(execID string) string {
	return workspaceagent.ExecStatusPath(execID)
}
