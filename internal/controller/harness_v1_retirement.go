/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
)

const defaultHarnessV1RetirementPollInterval = time.Second

// HarnessV1RetirementAttemptInventory exposes the SQLite-backed attempt
// inventory that must be empty before the v1 wrapper may be retired.
type HarnessV1RetirementAttemptInventory interface {
	ListActiveHarnessV1Attempts(context.Context) ([]store.HarnessV1Attempt, error)
}

// HarnessV1RetirementCoordinator closes controller-side v1 execution before a
// chart hook closes or replaces the wrapper. The controller remains the only
// writer: the authenticated hook merely invokes this one-way operation.
type HarnessV1RetirementCoordinator struct {
	Client       client.Client
	APIReader    client.Reader
	Attempts     HarnessV1RetirementAttemptInventory
	PollInterval time.Duration
}

// Retire requests disabled v1 admission, waits for the serialized control
// barrier to publish the exact generation, and then proves every controller
// attempt (including Prepared, Submitting, and SubmittedUnknown) is terminal.
func (r *HarnessV1RetirementCoordinator) Retire(ctx context.Context) error {
	if r == nil || r.Client == nil || r.Attempts == nil {
		return errors.New("harness v1 retirement requires a Kubernetes client and attempt inventory")
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	key := types.NamespacedName{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		control := &corev1alpha1.AgentExecutionControl{}
		if err := reader.Get(ctx, key, control); err != nil {
			return err
		}
		if control.Spec.Backends.V1.DesiredMode == corev1alpha1.AgentExecutionModeDisabled {
			return nil
		}
		base := control.DeepCopy()
		control.Spec.Backends.V1.DesiredMode = corev1alpha1.AgentExecutionModeDisabled
		return r.Client.Patch(
			ctx,
			control,
			client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{}),
		)
	}); err != nil {
		return fmt.Errorf("request disabled harness v1 admission: %w", err)
	}

	ticker := time.NewTicker(r.pollInterval())
	defer ticker.Stop()
	for {
		control := &corev1alpha1.AgentExecutionControl{}
		if err := reader.Get(ctx, key, control); err != nil {
			return fmt.Errorf("read harness v1 retirement control: %w", err)
		}
		if control.Spec.Backends.V1.DesiredMode != corev1alpha1.AgentExecutionModeDisabled {
			return fmt.Errorf(
				"harness v1 retirement request was superseded by desired mode %q",
				control.Spec.Backends.V1.DesiredMode,
			)
		}
		closed := control.Status.ObservedGeneration == control.Generation &&
			control.Status.Backends != nil &&
			control.Status.Backends.V1.EffectiveMode == corev1alpha1.AgentExecutionEffectiveModeDisabled &&
			control.Status.Backends.V1.AdmissionClosedAt != nil &&
			strings.TrimSpace(control.Status.Backends.V1.CutoffInventoryDigest) != ""
		if closed {
			attempts, err := r.Attempts.ListActiveHarnessV1Attempts(ctx)
			if err != nil {
				return fmt.Errorf("list active harness v1 retirement attempts: %w", err)
			}
			if len(attempts) == 0 {
				return nil
			}
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for harness v1 controller retirement: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (r *HarnessV1RetirementCoordinator) pollInterval() time.Duration {
	if r != nil && r.PollInterval > 0 {
		return r.PollInterval
	}
	return defaultHarnessV1RetirementPollInterval
}
