/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"context"
	"time"
)

type WorkspaceExecutor interface {
	Claim(ctx context.Context, req ClaimRequest) (*ClaimResult, error)
	WaitReady(ctx context.Context, req WaitReadyRequest) (*ReadyResult, error)
	Exec(ctx context.Context, req ExecRequest) (*ExecResult, error)
	Upload(ctx context.Context, req UploadRequest) (*UploadResult, error)
	Download(ctx context.Context, req DownloadRequest) (*DownloadResult, error)
	Release(ctx context.Context, req ReleaseRequest) (*ReleaseResult, error)
	Delete(ctx context.Context, req DeleteRequest) (*DeleteResult, error)
	Describe(ctx context.Context, req DescribeRequest) (*Description, error)
}

type Phase string

const (
	PhasePending  Phase = "Pending"
	PhaseReady    Phase = "Ready"
	PhaseReleased Phase = "Released"
	PhaseRetained Phase = "Retained"
	PhaseDeleted  Phase = "Deleted"
	PhaseFailed   Phase = "Failed"
)

type WorkspaceRef struct {
	Namespace   string `json:"namespace,omitempty"`
	ClaimName   string `json:"claimName,omitempty"`
	SandboxName string `json:"sandboxName,omitempty"`
	ID          string `json:"id,omitempty"`
}

func (r WorkspaceRef) IsZero() bool {
	return r.Namespace == "" && r.ClaimName == "" && r.SandboxName == "" && r.ID == ""
}

type TemplateRef struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name,omitempty"`
	UID       string `json:"uid,omitempty"`
}

type Placement struct {
	WorkerNamespace string `json:"workerNamespace,omitempty"`
	WorkerPool      string `json:"workerPool,omitempty"`
	WorkerPodName   string `json:"workerPodName,omitempty"`
	PodIP           string `json:"podIP,omitempty"`
}

func (p Placement) IsZero() bool {
	return p.WorkerNamespace == "" && p.WorkerPool == "" && p.WorkerPodName == "" && p.PodIP == ""
}

type Density struct {
	WorkerCount         int    `json:"workerCount,omitempty"`
	ActorCount          int    `json:"actorCount,omitempty"`
	RunningActorCount   int    `json:"runningActorCount,omitempty"`
	SuspendedActorCount int    `json:"suspendedActorCount,omitempty"`
	ActorsPerWorker     string `json:"actorsPerWorker,omitempty"`
}

func (d Density) IsZero() bool {
	return d.WorkerCount == 0 &&
		d.ActorCount == 0 &&
		d.RunningActorCount == 0 &&
		d.SuspendedActorCount == 0 &&
		d.ActorsPerWorker == ""
}

type ClaimRequest struct {
	Namespace         string
	TaskName          string
	ClaimName         string
	CreateIfMissing   bool
	Template          TemplateRef
	ReuseKey          string
	WarmPoolPolicy    string
	Labels            map[string]string
	Annotations       map[string]string
	Timeout           time.Duration
	MaxRequestTimeout time.Duration
}

type ClaimResult struct {
	Ref       WorkspaceRef
	Template  TemplateRef
	ReuseKey  string
	Created   bool
	Reused    bool
	Phase     Phase
	Message   string
	ClaimedAt time.Time
	Placement Placement
}

type WaitReadyRequest struct {
	Ref                   WorkspaceRef
	Template              TemplateRef
	Timeout               time.Duration
	Boot                  bool
	SnapshotRestoreURI    string
	SkipDaemonHealthCheck bool
}

type ReadyResult struct {
	Ref           WorkspaceRef
	Phase         Phase
	Message       string
	ReadyAt       time.Time
	Placement     Placement
	Density       Density
	ResumeLatency time.Duration
}

type ExecRequest struct {
	Ref            WorkspaceRef
	Command        []string
	Env            map[string]string
	WorkDir        string
	Stdin          []byte
	Timeout        time.Duration
	MaxOutputBytes int64
	Resident       bool
	ResidentKey    string
}

type ExecResult struct {
	Ref             WorkspaceRef
	Command         []string
	Stdout          string
	Stderr          string
	ExitCode        int
	StartedAt       time.Time
	FinishedAt      time.Time
	Artifacts       []Artifact
	StdoutTruncated bool
	StderrTruncated bool
}

func (r ExecResult) Duration() time.Duration {
	if r.StartedAt.IsZero() || r.FinishedAt.IsZero() {
		return 0
	}
	return r.FinishedAt.Sub(r.StartedAt)
}

func (r ExecResult) Succeeded() bool {
	return r.ExitCode == 0
}

type UploadRequest struct {
	Ref              WorkspaceRef
	Artifacts        []UploadArtifact
	Timeout          time.Duration
	BootstrapHandoff bool
}

type UploadArtifact struct {
	Path    string
	Data    []byte
	Mode    uint32
	ModTime time.Time
}

type Artifact struct {
	Path    string
	Size    int64
	Digest  string
	Mode    uint32
	ModTime time.Time
}

type UploadResult struct {
	Ref       WorkspaceRef
	Artifacts []Artifact
}

type DownloadRequest struct {
	Ref     WorkspaceRef
	Paths   []string
	Timeout time.Duration
}

type DownloadedArtifact struct {
	Artifact
	Data []byte
}

type DownloadResult struct {
	Ref       WorkspaceRef
	Artifacts []DownloadedArtifact
}

type ReleaseRequest struct {
	Ref                   WorkspaceRef
	Retain                bool
	Reason                string
	Timeout               time.Duration
	SkipScrub             bool
	SnapshotCheckpointURI string
}

type ReleaseResult struct {
	Ref      WorkspaceRef
	Released bool
	Retained bool
	Phase    Phase
	Message  string
}

type DeleteRequest struct {
	Ref       WorkspaceRef
	Reason    string
	Timeout   time.Duration
	SkipScrub bool
}

type DeleteResult struct {
	Ref     WorkspaceRef
	Deleted bool
	Phase   Phase
	Message string
}

type DescribeRequest struct {
	Ref WorkspaceRef
}

type Description struct {
	Ref         WorkspaceRef
	Template    TemplateRef
	ReuseKey    string
	Phase       Phase
	Retained    bool
	Message     string
	CreatedAt   time.Time
	ReadyAt     time.Time
	ReleasedAt  time.Time
	DeletedAt   time.Time
	Placement   Placement
	Density     Density
	Labels      map[string]string
	Annotations map[string]string
	Artifacts   []Artifact
}
