package workspaceprovider

import (
	"testing"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

func TestValidateDeletedDispositionMatrix(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		disposition *workspacev1alpha1.ExecutionWorkspaceDisposition
		wantError   bool
	}{
		{name: "missing", wantError: true},
		{
			name: "compute active",
			disposition: dispositionReviewWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.Compute = workspacev1alpha1.DispositionActive
			}),
			wantError: true,
		},
		{
			name: "credentials pending",
			disposition: dispositionReviewWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.AccessCredentials = workspacev1alpha1.DispositionPending
			}),
			wantError: true,
		},
		{
			name: "ephemeral secrets failed",
			disposition: dispositionReviewWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.EphemeralSecrets = workspacev1alpha1.DispositionFailed
			}),
			wantError: true,
		},
		{
			name: "workspace data active",
			disposition: dispositionReviewWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.WorkspaceData = workspacev1alpha1.DispositionActive
			}),
			wantError: true,
		},
		{
			name: "persistent volume policy mismatch",
			disposition: dispositionReviewWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.PersistentVolumes = workspacev1alpha1.DispositionDeleted
			}),
			wantError: true,
		},
		{
			name: "checkpoint policy mismatch",
			disposition: dispositionReviewWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.Checkpoints = workspacev1alpha1.DispositionRetained
			}),
			wantError: true,
		},
		{
			name: "provider resource policy mismatch",
			disposition: dispositionReviewWith(func(disposition *workspacev1alpha1.ExecutionWorkspaceDisposition) {
				disposition.ProviderResources = workspacev1alpha1.DispositionRetained
			}),
			wantError: true,
		},
		{
			name: "not applicable terminal categories",
			disposition: &workspacev1alpha1.ExecutionWorkspaceDisposition{
				Compute:           workspacev1alpha1.DispositionNotApplicable,
				AccessCredentials: workspacev1alpha1.DispositionNotApplicable,
				EphemeralSecrets:  workspacev1alpha1.DispositionNotApplicable,
				WorkspaceData:     workspacev1alpha1.DispositionNotApplicable,
				PersistentVolumes: workspacev1alpha1.DispositionNotApplicable,
				Checkpoints:       workspacev1alpha1.DispositionNotApplicable,
				ProviderResources: workspacev1alpha1.DispositionNotApplicable,
			},
		},
		{name: "policy compliant", disposition: dispositionReviewValid()},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			err := ValidateDeletedDisposition(tt.disposition, dispositionReviewPolicy())
			if tt.wantError && err == nil {
				t.Fatal("ValidateDeletedDisposition succeeded, want error")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("ValidateDeletedDisposition error = %v", err)
			}
		})
	}
}

func TestValidateInteractiveDeletedDispositionRequiresCredentialCleanup(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		credentials workspacev1alpha1.ExecutionWorkspaceDispositionState
		wantError   bool
	}{
		{name: "revoked", credentials: workspacev1alpha1.DispositionRevoked},
		{name: "deleted", credentials: workspacev1alpha1.DispositionDeleted},
		{name: "not applicable", credentials: workspacev1alpha1.DispositionNotApplicable, wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			disposition := dispositionReviewValid()
			disposition.AccessCredentials = tt.credentials
			err := ValidateInteractiveDeletedDisposition(disposition, dispositionReviewPolicy())
			if tt.wantError && err == nil {
				t.Fatal("ValidateInteractiveDeletedDisposition succeeded, want error")
			}
			if !tt.wantError && err != nil {
				t.Fatalf("ValidateInteractiveDeletedDisposition error = %v", err)
			}
		})
	}
}

func dispositionReviewValid() *workspacev1alpha1.ExecutionWorkspaceDisposition {
	return &workspacev1alpha1.ExecutionWorkspaceDisposition{
		Compute:           workspacev1alpha1.DispositionDeleted,
		AccessCredentials: workspacev1alpha1.DispositionRevoked,
		EphemeralSecrets:  workspacev1alpha1.DispositionDeleted,
		WorkspaceData:     workspacev1alpha1.DispositionDeleted,
		PersistentVolumes: workspacev1alpha1.DispositionRetained,
		Checkpoints:       workspacev1alpha1.DispositionDeleted,
		ProviderResources: workspacev1alpha1.DispositionDeleted,
	}
}

func dispositionReviewWith(
	mutate func(*workspacev1alpha1.ExecutionWorkspaceDisposition),
) *workspacev1alpha1.ExecutionWorkspaceDisposition {
	disposition := dispositionReviewValid()
	mutate(disposition)
	return disposition
}

func dispositionReviewPolicy() workspacev1alpha1.ExecutionWorkspaceDeletionPolicy {
	return workspacev1alpha1.ExecutionWorkspaceDeletionPolicy{
		ProviderResources: workspacev1alpha1.WorkspaceDeletionActionDelete,
		PersistentVolumes: workspacev1alpha1.WorkspaceDeletionActionRetain,
		Checkpoints:       workspacev1alpha1.WorkspaceDeletionActionDelete,
	}
}
