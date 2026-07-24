package workspaceprovider

import (
	"context"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

// AdapterMetadata is the immutable identity advertised by a provider adapter.
type AdapterMetadata struct {
	ControllerName string
	Version        string
	Digest         string
	Contracts      []string
	Features       []workspacev1alpha1.ExecutionWorkspaceFeature
}

// ProviderObservation is safe provider/backend metadata suitable for generic status.
type ProviderObservation struct {
	Adapter           workspacev1alpha1.ExecutionWorkspaceAdapterStatus
	Backend           workspacev1alpha1.ExecutionWorkspaceBackendStatus
	SupportedFeatures []workspacev1alpha1.ExecutionWorkspaceFeature
}

// PoolObservation contains provider-independent pool counts.
type PoolObservation struct {
	Available int32
	Allocated int32
	Suspended int32
	Total     int32
}

// WorkspaceObservation contains the provider-neutral result of reconciling a concrete workspace.
type WorkspaceObservation struct {
	State           workspacev1alpha1.ExecutionWorkspaceState
	ExternalID      string
	AttachedEpoch   int64
	ProviderBinding *workspacev1alpha1.ExecutionWorkspaceProviderBindingStatus
	ConnectionRef   *workspacev1alpha1.SecretReference
	Endpoints       []workspacev1alpha1.ExecutionWorkspaceEndpoint
	Disposition     *workspacev1alpha1.ExecutionWorkspaceDisposition
}

// Driver is the provider-specific lifecycle seam exercised by the shared conformance suite.
// Implementations must be idempotent: ReconcileWorkspace may be repeated after ambiguous
// provider responses and must not create duplicate external resources.
type Driver interface {
	Metadata() AdapterMetadata
	ObserveProvider(context.Context, *workspacev1alpha1.ExecutionWorkspaceProvider) (ProviderObservation, error)
	ReconcilePool(context.Context, *workspacev1alpha1.ExecutionWorkspacePool) (PoolObservation, error)
	ReconcileWorkspace(context.Context, *workspacev1alpha1.ExecutionWorkspace) (WorkspaceObservation, error)
}

// AttachmentController is an optional narrow capability for adapters whose privileged
// data-plane attachment control is not performed inside ReconcileWorkspace.
type AttachmentController interface {
	ActivateAttachment(context.Context, *workspacev1alpha1.ExecutionWorkspace) error
	RevokeAttachment(context.Context, *workspacev1alpha1.ExecutionWorkspace, int64) error
}
