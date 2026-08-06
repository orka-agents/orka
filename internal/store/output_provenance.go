package store

import (
	"context"
	"time"
)

const (
	OutputProducerKubernetesWorker  = "kubernetes-worker"
	OutputProducerHarnessWrapper    = "harness-wrapper"
	OutputProducerControllerHarness = "controller-harness"
	OutputProducerController        = "controller"
	OutputProducerLegacy            = "legacy-unverified"
)

// OutputProvenance binds mutable staging bytes to one Task incarnation and attempt.
type OutputProvenance struct {
	TaskUID               string    `json:"taskUID,omitempty"`
	JobUID                string    `json:"jobUID,omitempty"`
	PodUID                string    `json:"podUID,omitempty"`
	TaskAttempt           int64     `json:"taskAttempt,omitempty"`
	ProducerKind          string    `json:"producerKind,omitempty"`
	RuntimeSessionID      string    `json:"runtimeSessionID,omitempty"`
	TurnID                string    `json:"turnID,omitempty"`
	CorrelationID         string    `json:"correlationID,omitempty"`
	SubmissionNonceDigest string    `json:"submissionNonceDigest,omitempty"`
	StagingGeneration     int64     `json:"stagingGeneration,omitempty"`
	ContentSize           int64     `json:"contentSize,omitempty"`
	ContentSHA256         string    `json:"contentSHA256,omitempty"`
	AcceptedAt            time.Time `json:"acceptedAt,omitempty"`
}

// BoundResult is a mutable staging result with controller-verifiable provenance.
type BoundResult struct {
	Namespace  string           `json:"namespace"`
	TaskName   string           `json:"taskName"`
	Data       []byte           `json:"-"`
	Provenance OutputProvenance `json:"provenance"`
}

// BoundArtifact is a mutable staging artifact with controller-verifiable provenance.
type BoundArtifact struct {
	Namespace   string           `json:"namespace"`
	TaskName    string           `json:"taskName"`
	Filename    string           `json:"filename"`
	ContentType string           `json:"contentType"`
	Data        []byte           `json:"-"`
	Provenance  OutputProvenance `json:"provenance"`
}

// BoundOutputReset authorizes a trusted controller to remove staging rows
// owned by a deleted Task incarnation before recreating the deterministic Task
// name with a different UID. ReplacementTaskUID is never inferred from writer
// input and must be the UID observed by the controller for the replacement Task.
// BoundOutputStore adds attempt-bound staging operations without changing the
// compatibility ResultStore and ArtifactStore interfaces.
type BoundOutputStore interface {
	SaveBoundResult(ctx context.Context, result *BoundResult) error
	GetBoundResult(ctx context.Context, namespace, taskName, taskUID string, attempt int64) (*BoundResult, error)
	SaveBoundArtifact(ctx context.Context, artifact *BoundArtifact) error
	GetBoundArtifact(ctx context.Context, namespace, taskName, filename, taskUID string, attempt int64) (*BoundArtifact, error)
	ListBoundArtifacts(ctx context.Context, namespace, taskName, taskUID string, attempt int64) ([]ArtifactMetadata, error)
}
