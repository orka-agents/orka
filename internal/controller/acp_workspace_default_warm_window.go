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

const (
	DefaultACPWorkspaceSuspendAfterDetach                = time.Duration(0)
	ACPWorkspaceClassDefaultSuspendAfterDetachAnnotation = "acp.workspace.orka.ai/default-suspend-after-detach"
	acpWorkspaceDefaultSuspendAfterDetachDisabled        = "disabled"
	acpWorkspaceSuspendAfterDetachSourceAnnotation       = "acp.workspace.orka.ai/suspend-after-detach-source"
	acpWorkspaceSuspendAfterDetachSourceDefault          = "controller-default"

	substrateWorkspaceProfileGroup = "substrate.workspace.orka.ai"
	substrateWorkspaceProfileKind  = "SubstrateWorkspaceProfile"
)

func ValidateACPWorkspaceDefaultSuspendAfterDetach(window time.Duration) error {
	if window < 0 || window > maxACPWorkspaceSuspendAfterDetach {
		return fmt.Errorf("the default warm-detach window must be 0 (disabled) or positive and at most %s", maxACPWorkspaceSuspendAfterDetach)
	}
	return nil
}

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

func (r *TaskReconciler) acpWorkspaceDefaultSuspendAfterDetachAnnotations(
	ctx context.Context,
	namespace string,
	binding *ACPRuntimeWorkspaceBinding,
) (map[string]string, error) {
	if !acpWorkspaceDefaultSuspendAfterDetachEligible(binding) {
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
