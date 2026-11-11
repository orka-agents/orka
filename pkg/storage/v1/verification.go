package v1

import (
	"context"

	"github.com/orka-agents/orka/pkg/storage/v1/verification"
)

// VerificationStore retains private, immutable verification inputs and evidence.
// It does not grant Kubernetes authority or execute a candidate. Initialization
// is an idempotent bootstrap operation; every evidence or lifecycle mutation,
// including reads that durably invalidate corrupt evidence, uses a write envelope.
//
// Verification-family refusals may record an incident and invalidate a run.
// Their exact domain error and result are committed with the operation receipt,
// so replay preserves that outcome. Storage-contract errors remain refusals that
// commit nothing. Public Task artifact deletion must not delete these records.
type VerificationStore interface {
	InitializePatchVerificationStore(context.Context) error
	InitializeKubernetesValidationStore(context.Context) error
	CreatePatchVerificationRun(context.Context, verification.Manifest, verification.Binding, map[string][]byte) error
	GetPatchVerificationRun(context.Context, string) (*verification.Record, error)
	GetCompletedReportValidation(context.Context, string) (*verification.Record, error)
	GetPatchVerificationBlob(context.Context, string, string) ([]byte, error)
	RecordPatchVerificationEvidence(context.Context, verification.Binding, verification.ExecutionEvidence) error
	FinalizePatchVerificationRun(context.Context, verification.Binding) (*verification.Record, error)
	CancelPatchVerificationRun(context.Context, verification.Binding) (*verification.Record, error)
	RecoverInterruptedPatchVerificationRun(context.Context, verification.Binding) (*verification.Record, error)

	CreateKubernetesValidationSubmission(context.Context, verification.KubernetesSubmission) error
	GetKubernetesValidationSubmission(context.Context, string, string) (*verification.KubernetesSubmission, error)
	GetKubernetesValidationSubmissionByTask(context.Context, string, string) (*verification.KubernetesSubmission, error)
	GetKubernetesValidationSubmissionByRun(context.Context, string, string) (*verification.KubernetesSubmission, error)
	ListActiveKubernetesValidationSubmissions(context.Context, string) ([]verification.KubernetesSubmission, error)
	CompleteKubernetesValidationSubmission(context.Context, string, string, string) error
	RecordKubernetesValidationTaskUID(context.Context, string, string, string, string) error
	BindKubernetesValidationTasks(context.Context, string, string, string, string, verification.Binding) error
	AttachCancelledKubernetesValidationRun(context.Context, string, string, verification.Binding) error
	RequestKubernetesValidationCancellation(context.Context, string, string) error
	CreateKubernetesValidationDispatch(context.Context, verification.KubernetesDispatch) error
	GetKubernetesValidationDispatch(context.Context, string, string, string) (*verification.KubernetesDispatch, error)
	MarkKubernetesValidationDispatchCreated(context.Context, string, string, string, string) error
	MarkKubernetesValidationDispatchObserved(context.Context, string, string, string, string, string, string) error
	MarkKubernetesValidationDispatchRecorded(context.Context, string, string, string, string, string) error
	MarkKubernetesValidationDispatchFailed(context.Context, string, string, string, string) error
}
