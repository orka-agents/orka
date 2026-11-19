/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// ADR 0057: a session-reused Substrate workspace whose profile sets no
// spec.retention.suspendAfterDetach receives a controller default warm-detach
// window when it is materialized. The default is frozen only into the new
// workspace's annotations, with a source marker; it never enters a profile,
// class, or binding hash, and an existing workspace is never backfilled with
// it. Every ADR 0055 rule then applies to the frozen value unchanged.

const (
	// DefaultACPWorkspaceSuspendAfterDetach is the controller's default
	// warm-detach window for new eligible workspaces.
	DefaultACPWorkspaceSuspendAfterDetach = 5 * time.Minute
	// ACPWorkspaceClassDefaultSuspendAfterDetachAnnotation on an
	// ExecutionWorkspaceClass overrides the controller default for new
	// workspaces of that class: a duration such as "10m", or "disabled".
	ACPWorkspaceClassDefaultSuspendAfterDetachAnnotation = "acp.workspace.orka.ai/default-suspend-after-detach"
	// acpWorkspaceDefaultSuspendAfterDetachDisabled opts a class out.
	acpWorkspaceDefaultSuspendAfterDetachDisabled = "disabled"
	// acpWorkspaceSuspendAfterDetachSourceAnnotation records that the frozen
	// window came from the controller default rather than the profile.
	acpWorkspaceSuspendAfterDetachSourceAnnotation = "acp.workspace.orka.ai/suspend-after-detach-source"
	// acpWorkspaceSuspendAfterDetachSourceDefault is its only value.
	acpWorkspaceSuspendAfterDetachSourceDefault = "controller-default"

	substrateWorkspaceProfileGroup = "substrate.workspace.orka.ai"
	substrateWorkspaceProfileKind  = "SubstrateWorkspaceProfile"
)

// ValidateACPWorkspaceDefaultSuspendAfterDetach checks the controller flag:
// zero disables the default, otherwise ADR 0055's bound applies.
func ValidateACPWorkspaceDefaultSuspendAfterDetach(window time.Duration) error {
	if window < 0 || window > maxACPWorkspaceSuspendAfterDetach {
		return fmt.Errorf("the default warm-detach window must be 0 (disabled) or positive and at most %s", maxACPWorkspaceSuspendAfterDetach)
	}
	return nil
}

// acpWorkspaceDefaultSuspendAfterDetachEligible reports a binding whose new
// workspace may receive the default: a session-reused external Substrate
// class that allows Suspend with a DataOnly profile and sets no window itself.
func acpWorkspaceDefaultSuspendAfterDetachEligible(binding *ACPRuntimeWorkspaceBinding) bool {
	if binding == nil || binding.Class == nil {
		return false
	}
	class := binding.Class
	return binding.ReusePolicy == corev1alpha1.WorkspaceReusePolicySession &&
		class.SuspendAfterDetach == "" && class.ControllerName != "" &&
		class.SuspendMode == acpWorkspaceSuspendDataOnly &&
		slices.Contains(class.AllowedOnDetach, string(workspacev1alpha1.WorkspaceOnDetachSuspend)) &&
		class.ParametersRef != nil &&
		class.ParametersRef.Group == substrateWorkspaceProfileGroup &&
		class.ParametersRef.Kind == substrateWorkspaceProfileKind
}

// resolveACPWorkspaceDefaultSuspendAfterDetach returns the default window for
// a binding, or zero. classAnnotations are those of the exact frozen class. A
// window that does not fit ADR 0055's bounds (at most one hour, shorter than
// idleTimeout and maxLifetime) or an invalid class override disables the
// default for the workspace instead of failing the Task.
func resolveACPWorkspaceDefaultSuspendAfterDetach(
	binding *ACPRuntimeWorkspaceBinding,
	classAnnotations map[string]string,
	configured time.Duration,
) time.Duration {
	if !acpWorkspaceDefaultSuspendAfterDetachEligible(binding) {
		return 0
	}
	window := configured
	if raw, present := classAnnotations[ACPWorkspaceClassDefaultSuspendAfterDetachAnnotation]; present {
		raw = strings.TrimSpace(raw)
		if raw == acpWorkspaceDefaultSuspendAfterDetachDisabled {
			return 0
		}
		parsed, err := parseACPWorkspaceSuspendAfterDetach(raw)
		if err != nil {
			return 0
		}
		window = parsed
	}
	if window <= 0 || window > maxACPWorkspaceSuspendAfterDetach {
		return 0
	}
	idleTimeout, idleErr := parseOptionalACPWorkspaceDuration(binding.Class.IdleTimeout)
	maxLifetime, lifetimeErr := parseOptionalACPWorkspaceDuration(binding.Class.MaxLifetime)
	if idleErr != nil || lifetimeErr != nil ||
		validateACPWorkspaceSuspendAfterDetachBounds(window, idleTimeout, maxLifetime) != nil {
		return 0
	}
	return window
}

func parseOptionalACPWorkspaceDuration(raw string) (time.Duration, error) {
	if strings.TrimSpace(raw) == "" {
		return 0, nil
	}
	return time.ParseDuration(raw)
}

// acpWorkspaceDefaultSuspendAfterDetachAnnotations returns the annotations
// that freeze the default window into a workspace about to be created, or nil.
// It reads the exact frozen class for its override; a class that is gone or
// replaced gets no default, and other read errors retry the creation.
func (r *TaskReconciler) acpWorkspaceDefaultSuspendAfterDetachAnnotations(
	ctx context.Context,
	namespace string,
	binding *ACPRuntimeWorkspaceBinding,
) (map[string]string, error) {
	if r.ACPWorkspaceDefaultSuspendAfterDetach <= 0 || !acpWorkspaceDefaultSuspendAfterDetachEligible(binding) {
		return nil, nil
	}
	class := &workspacev1alpha1.ExecutionWorkspaceClass{}
	err := uncachedReader(r.APIReader, r.Client).Get(ctx, types.NamespacedName{Namespace: namespace, Name: binding.Class.Name}, class)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read execution workspace class for its default warm-detach window: %w", err)
	}
	if string(class.UID) != binding.Class.UID {
		return nil, nil
	}
	window := resolveACPWorkspaceDefaultSuspendAfterDetach(binding, class.Annotations, r.ACPWorkspaceDefaultSuspendAfterDetach)
	if window <= 0 {
		return nil, nil
	}
	return map[string]string{
		acpWorkspaceSuspendAfterDetachAnnotation:       window.String(),
		acpWorkspaceSuspendAfterDetachSourceAnnotation: acpWorkspaceSuspendAfterDetachSourceDefault,
	}, nil
}
