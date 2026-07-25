package workspaceprovider

import (
	"fmt"
	"slices"

	workspacev1alpha1 "github.com/orka-agents/orka/api/workspace/v1alpha1"
)

// ValidateDeletedDisposition verifies that every cleanup category reached a
// terminal state and that policy-controlled resources match the workspace's
// deletion policy.
func ValidateDeletedDisposition(
	disposition *workspacev1alpha1.ExecutionWorkspaceDisposition,
	policy workspacev1alpha1.ExecutionWorkspaceDeletionPolicy,
) error {
	if disposition == nil {
		return fmt.Errorf("cleanup disposition is missing")
	}
	if err := requireTerminalDisposition(
		"compute",
		disposition.Compute,
		workspacev1alpha1.DispositionDeleted,
		workspacev1alpha1.DispositionNotApplicable,
	); err != nil {
		return err
	}
	if err := requireTerminalDisposition(
		"accessCredentials",
		disposition.AccessCredentials,
		workspacev1alpha1.DispositionRevoked,
		workspacev1alpha1.DispositionDeleted,
		workspacev1alpha1.DispositionNotApplicable,
	); err != nil {
		return err
	}
	if err := requireTerminalDisposition(
		"ephemeralSecrets",
		disposition.EphemeralSecrets,
		workspacev1alpha1.DispositionDeleted,
		workspacev1alpha1.DispositionNotApplicable,
	); err != nil {
		return err
	}
	if err := requireTerminalDisposition(
		"workspaceData",
		disposition.WorkspaceData,
		workspacev1alpha1.DispositionDeleted,
		workspacev1alpha1.DispositionRetained,
		workspacev1alpha1.DispositionNotApplicable,
	); err != nil {
		return err
	}
	if err := requirePolicyDisposition(
		"persistentVolumes",
		disposition.PersistentVolumes,
		policy.PersistentVolumes,
	); err != nil {
		return err
	}
	if err := requirePolicyDisposition(
		"checkpoints",
		disposition.Checkpoints,
		policy.Checkpoints,
	); err != nil {
		return err
	}
	return requirePolicyDisposition(
		"providerResources",
		disposition.ProviderResources,
		policy.ProviderResources,
	)
}

// ValidateInteractiveDeletedDisposition applies the generic terminal cleanup
// contract and additionally requires affirmative cleanup of interactive access
// credentials. Interactive workspaces cannot report those credentials as not
// applicable because every attachment carries a bearer credential.
func ValidateInteractiveDeletedDisposition(
	disposition *workspacev1alpha1.ExecutionWorkspaceDisposition,
	policy workspacev1alpha1.ExecutionWorkspaceDeletionPolicy,
) error {
	if err := ValidateDeletedDisposition(disposition, policy); err != nil {
		return err
	}
	if disposition.AccessCredentials != workspacev1alpha1.DispositionRevoked &&
		disposition.AccessCredentials != workspacev1alpha1.DispositionDeleted {
		return fmt.Errorf(
			"accessCredentials disposition %q does not confirm interactive credential cleanup",
			disposition.AccessCredentials,
		)
	}
	return nil
}

func requireTerminalDisposition(
	category string,
	state workspacev1alpha1.ExecutionWorkspaceDispositionState,
	allowed ...workspacev1alpha1.ExecutionWorkspaceDispositionState,
) error {
	if slices.Contains(allowed, state) {
		return nil
	}
	return fmt.Errorf("%s disposition %q is not terminal", category, state)
}

func requirePolicyDisposition(
	category string,
	state workspacev1alpha1.ExecutionWorkspaceDispositionState,
	action workspacev1alpha1.WorkspaceDeletionAction,
) error {
	if state == workspacev1alpha1.DispositionNotApplicable {
		return nil
	}
	var expected workspacev1alpha1.ExecutionWorkspaceDispositionState
	switch action {
	case workspacev1alpha1.WorkspaceDeletionActionDelete:
		expected = workspacev1alpha1.DispositionDeleted
	case workspacev1alpha1.WorkspaceDeletionActionRetain:
		expected = workspacev1alpha1.DispositionRetained
	default:
		return fmt.Errorf("%s deletion action %q is unsupported", category, action)
	}
	if state != expected {
		return fmt.Errorf(
			"%s disposition %q does not satisfy %q policy",
			category,
			state,
			action,
		)
	}
	return nil
}
