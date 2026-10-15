//go:build e2e

package e2e

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"slices"
	"time"

	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	gatewayruntime "github.com/orka-agents/orka/internal/gateway"
)

// Preserve the existing functional assertion budgets before event compaction.
// The dedicated CI lane allows the real one-minute maintenance loop to run;
// neither controller clocks nor durable retention records are changed by tests.
const gatewayE2ETerminalRetention = 20 * time.Minute

type gatewayE2ECleanup struct {
	*e2eCleanup
	ingressPending bool
	admittedEvents map[string]types.UID
}

func newGatewayE2ECleanup(apiBaseURL, token string) (*gatewayE2ECleanup, error) {
	cleanup, err := newE2ECleanup(apiBaseURL, token)
	if err != nil {
		return nil, err
	}
	cfg, err := config.GetConfig()
	if err != nil {
		return nil, errors.New("read Gateway cleanup Kubernetes configuration")
	}
	// Gateway provenance protection permits only the existing controller
	// identity to update its Tasks. Use that fixture's token, never a relaxed
	// admission policy or garbage-collector impersonation.
	cfg, err = gatewayE2EControllerConfig(cfg, token)
	if err != nil {
		return nil, err
	}
	cleanup.kube, err = client.New(cfg, client.Options{Scheme: cleanup.kube.Scheme()})
	if err != nil {
		return nil, errors.New("initialize Gateway cleanup controller client")
	}
	return &gatewayE2ECleanup{e2eCleanup: cleanup}, nil
}

// Record uncertainty before sending any ingress request: even a lost response
// may leave an admitted event that has not created its Task yet.
func (c *gatewayE2ECleanup) beginIngress() error {
	if c.ingressPending {
		return errors.New("previous Gateway ingress outcome is still unknown")
	}
	c.ingressPending = true
	return c.checkpoint("request_gateway_ingress")
}

// An empty event ID is allowed only after a confirmed rejection. Accepted and
// duplicate responses both bind the same durable event to its original Task.
func (c *gatewayE2ECleanup) finishIngress(eventID string) error {
	if !c.ingressPending {
		return errors.New("Gateway ingress request was not tracked")
	}
	if eventID != "" {
		if c.admittedEvents == nil {
			c.admittedEvents = map[string]types.UID{}
		}
		if _, exists := c.admittedEvents[eventID]; !exists {
			c.admittedEvents[eventID] = ""
		}
	}
	c.ingressPending = false
	return c.checkpoint("acknowledge_gateway_ingress")
}

func (c *gatewayE2ECleanup) verifyAdmissions() error {
	if c.ingressPending || len(c.admittedEvents) != len(c.report.Tasks) {
		return errors.New("Gateway cleanup lacks complete ingress and Task coverage")
	}
	seen := map[types.UID]bool{}
	for _, uid := range c.admittedEvents {
		if uid == "" || seen[uid] || !slices.ContainsFunc(c.report.Tasks, func(record *e2eTaskCleanupEvidence) bool {
			return record.UID == uid && record.ObserverInstalled
		}) {
			return errors.New("Gateway admitted event lacks its original observed Task")
		}
		seen[uid] = true
	}
	return nil
}

func gatewayE2EControllerConfig(cfg *rest.Config, token string) (*rest.Config, error) {
	if cfg == nil || token == "" {
		return nil, errors.New("Gateway cleanup requires controller authentication")
	}
	controllerConfig := rest.AnonymousClientConfig(cfg)
	controllerConfig.BearerToken = token
	controllerConfig.Timeout = 10 * time.Second
	return controllerConfig, nil
}

func gatewayE2EObserveTask(ctx context.Context, cleanup *gatewayE2ECleanup, task *corev1alpha1.Task) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	if task == nil || task.Namespace != namespace || task.UID == "" || task.Spec.SessionRef == nil || task.Spec.SessionRef.Name == "" {
		return errors.New("Gateway cleanup requires the exact admitted Task and Session identity")
	}
	eventID := task.Annotations[gatewayruntime.TaskGatewayEventAnnotation]
	uid, admitted := cleanup.admittedEvents[eventID]
	if !admitted || uid != "" || task.Labels[gatewayruntime.TaskGatewayEventLabel] != eventID ||
		task.Annotations[gatewayruntime.TaskGatewayNameAnnotation] != gatewayE2EName {
		return errors.New("Gateway Task does not match an unobserved admitted event")
	}
	if slices.ContainsFunc(cleanup.report.Tasks, func(record *e2eTaskCleanupEvidence) bool { return record.Name == task.Name }) {
		return errors.New("Gateway cleanup Task was already observed")
	}
	record := &e2eTaskCleanupEvidence{
		Name: task.Name, Namespace: task.Namespace, UID: task.UID, SessionName: task.Spec.SessionRef.Name,
		ReceiptRequired: task.Spec.Type == corev1alpha1.TaskTypeAgent,
	}
	cleanup.report.Tasks = append(cleanup.report.Tasks, record)
	if err := cleanup.checkpoint("capture_gateway_tasks"); err != nil {
		return err
	}
	if err := cleanup.observeTask(ctx, task.DeepCopy(), record); err != nil {
		return err
	}
	cleanup.admittedEvents[eventID] = task.UID
	return cleanup.checkpoint("capture_gateway_admission")
}

func gatewayE2ECaptureRuntimeSession(ctx context.Context, cleanup *gatewayE2ECleanup, task *corev1alpha1.Task) error {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, record := range cleanup.report.Tasks {
		if record.UID != task.UID || record.Name != task.Name || record.Namespace != task.Namespace {
			continue
		}
		e := task.Status.Execution
		if !record.ObserverInstalled || task.Spec.SessionRef == nil || record.SessionName != task.Spec.SessionRef.Name ||
			e == nil || e.RuntimeSessionUID == "" || e.RuntimeSessionGeneration < 1 || e.Attempt < 1 || e.RuntimeInstanceID == "" {
			return errors.New("Gateway runtime cleanup identity is incomplete")
		}
		control, err := cleanup.sessionControl(ctx, record.SessionName)
		if err != nil {
			return err
		}
		if control == nil || control.Spec.SessionUID != e.RuntimeSessionUID {
			return errors.New("Gateway RuntimeSessionControl does not match the admitted runtime")
		}
		record.ReceiptRequired = true
		record.RuntimeSessionUID, record.RuntimeSessionGeneration, record.Attempt = e.RuntimeSessionUID, e.RuntimeSessionGeneration, e.Attempt
		idHash := sha256.Sum256([]byte(e.RuntimeInstanceID))
		record.RuntimeInstanceDigest = "sha256:" + hex.EncodeToString(idHash[:])
		record.RuntimeSessionControlName, record.RuntimeSessionControlUID = control.Name, control.UID
		return cleanup.checkpoint("capture_gateway_runtime")
	}
	return errors.New("Gateway runtime Task was not observed before cleanup")
}

// Gateway Sessions are deliberately hidden from the generic Session API.
// Only normal Gateway maintenance may archive them and request Task deletion.
// The observer retains each Task until the shared strict receipt validator has
// verified runtime retirement and all product finalizers have been released.
func gatewayE2EWaitForRetentionCleanup(ctx context.Context, cleanup *gatewayE2ECleanup) (resultErr error) {
	defer func() {
		cleanup.report.FinishedAt = time.Now().UTC()
		cleanup.report.Passed = resultErr == nil
		if resultErr != nil {
			cleanup.report.Failure = cleanupFailureClass(resultErr)
		}
		resultErr = errors.Join(resultErr, cleanup.save(cleanup.report))
	}()
	if err := cleanup.checkpoint("verify_gateway_admissions"); err != nil {
		return err
	}
	if err := cleanup.verifyAdmissions(); err != nil {
		return err
	}
	if err := cleanup.checkpoint("await_gateway_retention"); err != nil {
		return err
	}
	for _, record := range cleanup.report.Tasks {
		if err := wait.PollUntilContextCancel(ctx, cleanup.interval(), true, func(ctx context.Context) (bool, error) {
			task := &corev1alpha1.Task{}
			if err := cleanup.kube.Get(ctx, client.ObjectKey{Namespace: record.Namespace, Name: record.Name}, task); err != nil {
				return false, safeCleanupError("observe Gateway retention", err)
			}
			if task.UID != record.UID || task.Spec.SessionRef == nil || task.Spec.SessionRef.Name != record.SessionName ||
				!record.ObserverInstalled || !slices.Contains(task.Finalizers, e2eCleanupFinalizer) {
				return false, errors.New("Gateway Task or cleanup observer identity changed")
			}
			if task.DeletionTimestamp.IsZero() {
				return false, nil
			}
			if record.ReceiptRequired && (task.Status.Execution == nil || task.Status.Execution.RuntimeSessionUID == "") {
				return false, errors.New("Gateway ACP Task lost its admitted runtime cleanup identity")
			}
			if record.ReceiptRequired && record.RuntimeSessionControlUID == "" {
				return false, errors.New("Gateway cleanup did not capture its original RuntimeSessionControl")
			}
			if ready, err := gatewayE2EControlAbsent(ctx, cleanup, record); err != nil || !ready {
				return false, err
			}
			// This is an observed controller deletion request, never a synthetic
			// test DELETE that could bypass the retention lifecycle under test.
			record.DeleteRequested = true
			return true, cleanup.checkpoint("observe_gateway_retention")
		}); err != nil {
			return err
		}
		if err := cleanup.finalizeTask(ctx, record); err != nil {
			return err
		}
		if absent, err := gatewayE2EControlAbsent(ctx, cleanup, record); err != nil || !absent {
			return errors.Join(errors.New("Gateway RuntimeSessionControl absence was not preserved"), err)
		}
	}
	return cleanup.checkpoint("complete")
}

func gatewayE2EControlAbsent(ctx context.Context, cleanup *gatewayE2ECleanup, record *e2eTaskCleanupEvidence) (bool, error) {
	if record.RuntimeSessionControlUID == "" {
		return true, nil // Native Tasks have no admitted ACP RuntimeSession.
	}
	control, err := cleanup.sessionControl(ctx, record.SessionName)
	if err != nil {
		return false, err
	}
	if control == nil {
		record.RuntimeSessionControlAbsent = true
		return true, nil
	}
	if control.UID != record.RuntimeSessionControlUID || control.Name != record.RuntimeSessionControlName ||
		control.Spec.SessionUID != record.RuntimeSessionUID {
		return false, errors.New("Gateway RuntimeSessionControl identity changed")
	}
	return false, nil
}
