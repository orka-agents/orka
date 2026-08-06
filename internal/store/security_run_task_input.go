package store

import (
	"context"
	"time"
)

// SecurityRunTaskInput is the immutable initial Task input accepted for one run stage.
type SecurityRunTaskInput struct {
	RunUID         string    `json:"runUID"`
	Namespace      string    `json:"namespace"`
	RepositoryScan string    `json:"repositoryScan"`
	ScanRunID      string    `json:"scanRunID"`
	Stage          string    `json:"stage"`
	SourceVersion  int64     `json:"sourceVersion"`
	Content        string    `json:"content"`
	ContentDigest  string    `json:"contentDigest"`
	RecordDigest   string    `json:"recordDigest"`
	CreatedAt      time.Time `json:"createdAt"`
}

// SecurityRunTaskInputStore persists immutable per-run, per-stage initial Task inputs.
type SecurityRunTaskInputStore interface {
	CreateScanRunWithTaskInput(ctx context.Context, run *ScanRun, input *SecurityRunTaskInput) error
	SaveSecurityRunTaskInput(ctx context.Context, input *SecurityRunTaskInput) (bool, error)
	GetSecurityRunTaskInput(ctx context.Context, namespace, runUID, stage string) (*SecurityRunTaskInput, error)
}
