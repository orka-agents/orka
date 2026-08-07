package controller

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/harness"
	"github.com/orka-agents/orka/internal/store"
)

const (
	harnessV1ReasonLegacyCleanupOnly  = "LegacyCleanupOnly"
	harnessV1LegacyCleanupOnlyMessage = "legacy harness v1 state was retired without retry or continuation"
)

// reconcileLegacyCleanupHarnessV1Task settles historical v1 state without
// loading the synthetic migration snapshot or its intentionally absent
// binding reservation. In particular, this path never calls StartTurn,
// StreamFrames, or retry creation.
func (d *HarnessV1Dispatcher) reconcileLegacyCleanupHarnessV1Task(
	ctx context.Context,
	task *corev1alpha1.Task,
) error {
	if legacyCleanupBinding(task, corev1alpha1.AgentRuntimeContractHarnessV1) == nil {
		return nil
	}
	attempts, err := d.Attempts.ListHarnessV1AttemptsByTask(ctx, task.Namespace, string(task.UID))
	if err != nil {
		return err
	}
	if len(attempts) == 0 {
		return nil
	}

	historicalBindingDigest := attempts[0].BindingDigest
	if err := store.ValidateCanonicalDigest("legacy cleanup harness v1 binding digest", historicalBindingDigest); err != nil {
		return err
	}
	for i := range attempts {
		attempt := &attempts[i]
		if attempt.Namespace != task.Namespace || attempt.TaskName != task.Name ||
			attempt.TaskUID != string(task.UID) {
			return errors.New("legacy cleanup harness v1 attempt identity does not match the Task")
		}
		if attempt.BindingDigest != historicalBindingDigest {
			return errors.New("legacy cleanup harness v1 attempts span multiple binding lineages")
		}
	}

	fence, err := d.Epochs.CurrentFence(ctx)
	if err != nil {
		return err
	}
	for i := range attempts {
		attempt := &attempts[i]
		if !store.IsTerminalHarnessV1AttemptState(attempt.State) {
			settled, settleErr := d.settleLegacyCleanupHarnessV1Attempt(ctx, task, attempt, fence)
			if settleErr != nil {
				return settleErr
			}
			attempts[i] = *settled
			attempt = &attempts[i]
		}
		if err := d.settleHarnessV1RuntimeSessionRecord(ctx, task, attempt); err != nil {
			return err
		}
	}

	latest := &attempts[len(attempts)-1]
	if !store.IsTerminalHarnessV1AttemptState(latest.State) {
		return fmt.Errorf("legacy cleanup harness v1 attempt %d did not reach a terminal state", latest.Attempt)
	}
	return d.projectLegacyCleanupHarnessV1Attempt(ctx, task, latest)
}

func (d *HarnessV1Dispatcher) settleLegacyCleanupHarnessV1Attempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
	fence store.ControllerEpochFence,
) (*store.HarnessV1Attempt, error) {
	switch attempt.State {
	case store.HarnessV1AttemptPrepared:
		target := store.HarnessV1AttemptRejected
		if !task.DeletionTimestamp.IsZero() || task.Status.Phase == corev1alpha1.TaskPhaseCancelled {
			target = store.HarnessV1AttemptCancelled
		}
		return d.transitionAttempt(ctx, attempt, fence, target, "legacy-cleanup-pre-submit", store.HarnessV1AttemptUpdates{
			TerminalReason: new(harnessV1ReasonLegacyCleanupOnly),
		})
	case store.HarnessV1AttemptSubmitting, store.HarnessV1AttemptSubmittedUnknown:
		if d.legacyCleanupTurnDefinitivelyRejected(ctx, attempt) {
			return d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptRejected,
				"legacy-cleanup-durable-rejected", store.HarnessV1AttemptUpdates{
					TerminalReason: new(harnessV1ReasonLegacyCleanupOnly),
				})
		}
		if attempt.State == store.HarnessV1AttemptSubmitting {
			var err error
			attempt, err = d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptSubmittedUnknown,
				"legacy-cleanup-submission-unknown", store.HarnessV1AttemptUpdates{})
			if err != nil {
				return nil, err
			}
		}
		return d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptOutcomeUnknown,
			"legacy-cleanup-outcome-unknown", store.HarnessV1AttemptUpdates{
				TerminalReason: new(harnessV1ReasonLegacyCleanupOnly),
			})
	case store.HarnessV1AttemptAccepted, store.HarnessV1AttemptRunning,
		store.HarnessV1AttemptCancelRequested, store.HarnessV1AttemptSettling:
		return d.transitionAttempt(ctx, attempt, fence, store.HarnessV1AttemptOutcomeUnknown,
			"legacy-cleanup-outcome-unknown", store.HarnessV1AttemptUpdates{
				TerminalReason: new(harnessV1ReasonLegacyCleanupOnly),
			})
	default:
		return nil, fmt.Errorf("unsupported nonterminal legacy cleanup harness v1 state %q", attempt.State)
	}
}

// legacyCleanupTurnDefinitivelyRejected uses only the endpoint and exact
// Secret identity already frozen on the historical attempt. Missing, stale,
// or unreachable authority is not retried and is conservatively classified as
// OutcomeUnknown by the caller.
func (d *HarnessV1Dispatcher) legacyCleanupTurnDefinitivelyRejected(
	ctx context.Context,
	attempt *store.HarnessV1Attempt,
) bool {
	if attempt == nil || strings.TrimSpace(attempt.BackendEndpoint) == "" ||
		strings.TrimSpace(attempt.AuthSecretNamespace) == "" || strings.TrimSpace(attempt.AuthSecretName) == "" ||
		strings.TrimSpace(attempt.AuthSecretKey) == "" || strings.TrimSpace(attempt.AuthSecretUID) == "" ||
		strings.TrimSpace(attempt.AuthSecretResourceVersion) == "" || d.APIReader == nil {
		return false
	}
	auth := &corev1.Secret{}
	if err := d.APIReader.Get(ctx, types.NamespacedName{
		Namespace: attempt.AuthSecretNamespace,
		Name:      attempt.AuthSecretName,
	}, auth); err != nil || string(auth.UID) != attempt.AuthSecretUID ||
		auth.ResourceVersion != attempt.AuthSecretResourceVersion {
		return false
	}
	bearer := strings.TrimSpace(string(auth.Data[attempt.AuthSecretKey]))
	if bearer == "" {
		return false
	}
	factory := d.clientFactory
	if factory == nil {
		factory = func(endpoint, bearer string, httpClient *http.Client) (harnessV1ProtocolClient, error) {
			options := []harness.ClientOption{harness.WithBearerToken(bearer)}
			if httpClient != nil {
				options = append(options, harness.WithHTTPClient(httpClient))
			}
			return harness.NewClient(endpoint, options...)
		}
	}
	protocolClient, err := factory(attempt.BackendEndpoint, bearer, d.HTTPClient)
	if err != nil {
		return false
	}
	status, err := protocolClient.DurableTurnStatus(ctx, harness.HarnessTurnID(attempt.TurnID))
	return err == nil && status != nil && status.State == harness.DurableTurnRejected &&
		status.TurnID == attempt.TurnID && status.TaskUID == attempt.TaskUID &&
		status.Attempt == attempt.Attempt && status.RequestDigest == attempt.RequestDigest
}

func (d *HarnessV1Dispatcher) projectLegacyCleanupHarnessV1Attempt(
	ctx context.Context,
	task *corev1alpha1.Task,
	attempt *store.HarnessV1Attempt,
) error {
	key := types.NamespacedName{Namespace: task.Namespace, Name: task.Name}
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &corev1alpha1.Task{}
		if err := d.Client.Get(ctx, key, latest); err != nil {
			return client.IgnoreNotFound(err)
		}
		if latest.UID != task.UID ||
			legacyCleanupBinding(latest, corev1alpha1.AgentRuntimeContractHarnessV1) == nil {
			return errors.New("task cleanup binding changed before harness v1 terminal projection")
		}
		if latest.Status.HarnessRuntime != nil && latest.Status.HarnessRuntime.Attempt > attempt.Attempt {
			return nil
		}
		if harnessV1AttemptProjectionMatches(latest, attempt) {
			return nil
		}

		base := latest.DeepCopy()
		now := metav1.Now()
		message := harnessV1LegacyCleanupOnlyMessage
		projected := harnessV1RuntimeProjection(latest, attempt, message)
		projected.LastTransitionTime = &now
		latest.Status.HarnessRuntime = projected
		_, _, phase := harnessV1TaskProjection(attempt.State)
		latest.Status.Attempts = attempt.Attempt
		latest.Status.Phase = phase
		latest.Status.Message = message
		latest.Status.CompletionTime = &now
		return d.Client.Status().Patch(ctx, latest, client.MergeFrom(base))
	})
}
