package controller

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

const maxAgentExecutionReadinessExamples = 8

// AgentExecutionClassificationReadiness proves that bridge classification is
// closed before the coexistence controller is advertised as ready. The check
// uses the uncached API reader: cached absence must never authorize execution.
//
// A transiently new Task may make readiness false until the controller writes
// its immutable binding. That is intentional and fail-closed; reconciliation
// continues while the Pod is unready.
type AgentExecutionClassificationReadiness struct {
	APIReader      client.Reader
	WatchNamespace string
}

// AgentExecutionClassificationGate is the execution authority corresponding
// to the readiness inventory. Unlike readiness, the gate is safe to consult
// from binding and long-running mutating runnables: it requires a persisted
// Sealed marker for the exact current AgentExecutionControl incarnation and
// generation, rather than treating Pod readiness as authorization.
type AgentExecutionClassificationGate struct {
	APIReader client.Reader
}

// Check performs the uncached, exact control-incarnation gate check.
func (g *AgentExecutionClassificationGate) Check(ctx context.Context) error {
	if g == nil || g.APIReader == nil {
		return fmt.Errorf("agent execution classification gate API reader is required")
	}
	control := &corev1alpha1.AgentExecutionControl{}
	if err := g.APIReader.Get(ctx, client.ObjectKey{
		Namespace: corev1alpha1.AgentExecutionControlNamespace,
		Name:      corev1alpha1.AgentExecutionControlName,
	}, control); err != nil {
		return fmt.Errorf("read AgentExecutionControl classification gate: %w", err)
	}
	classification := control.Status.Classification
	if classification == nil {
		return fmt.Errorf("agent execution classification is Open: no sealed inventory marker")
	}
	if classification.State != corev1alpha1.AgentExecutionClassificationSealed {
		return fmt.Errorf("agent execution classification is %s, not Sealed", classification.State)
	}
	if classification.ControlUID == "" || classification.ControlUID != control.UID ||
		classification.ControlGeneration < 1 || classification.ControlGeneration != control.Generation {
		return fmt.Errorf(
			"agent execution classification seal is stale: sealed control %s generation %d, current control %s generation %d",
			classification.ControlUID, classification.ControlGeneration, control.UID, control.Generation,
		)
	}
	if strings.TrimSpace(classification.InventoryID) == "" ||
		!canonicalSHA256Digest(classification.InventoryDigest) || classification.ObservedAt.IsZero() {
		return fmt.Errorf("agent execution classification seal is incomplete")
	}
	return nil
}

// ReadyzChecker returns the controller-runtime readiness hook.
func (c *AgentExecutionClassificationReadiness) ReadyzChecker() func(*http.Request) error {
	return func(request *http.Request) error {
		ctx := context.Background()
		if request != nil {
			ctx = request.Context()
		}
		return c.Check(ctx)
	}
}

// Check performs one closed-world classification inventory over the owned
// namespace scope.
//
//nolint:gocyclo // One inventory pass keeps every fail-closed classification check visible together.
func (c *AgentExecutionClassificationReadiness) Check(ctx context.Context) error {
	if c == nil || c.APIReader == nil {
		return fmt.Errorf("agent execution classification API reader is required")
	}
	if err := (&AgentExecutionClassificationGate{APIReader: c.APIReader}).Check(ctx); err != nil {
		return err
	}
	options := []client.ListOption{}
	if namespace := strings.TrimSpace(c.WatchNamespace); namespace != "" {
		options = append(options, client.InNamespace(namespace))
	}

	issues := make([]string, 0)
	agents := &corev1alpha1.AgentList{}
	if err := c.APIReader.List(ctx, agents, options...); err != nil {
		return fmt.Errorf("list Agents for execution classification: %w", err)
	}
	for i := range agents.Items {
		agent := &agents.Items[i]
		runtime := agent.Spec.Runtime
		if runtime != nil && runtime.Type != "" && runtime.ContractVersion == nil {
			issues = appendReadinessIssue(issues, fmt.Sprintf(
				"Agent %s/%s has an unclassified built-in runtime", agent.Namespace, agent.Name,
			))
		}
	}

	runtimes := &corev1alpha1.AgentRuntimeList{}
	if err := c.APIReader.List(ctx, runtimes, options...); err != nil {
		return fmt.Errorf("list AgentRuntimes for execution classification: %w", err)
	}
	for i := range runtimes.Items {
		runtime := &runtimes.Items[i]
		if runtime.Spec.ContractVersion == nil {
			issues = appendReadinessIssue(issues, fmt.Sprintf(
				"AgentRuntime %s/%s has no contract classification", runtime.Namespace, runtime.Name,
			))
		}
	}

	tasks := &corev1alpha1.TaskList{}
	if err := c.APIReader.List(ctx, tasks, options...); err != nil {
		return fmt.Errorf("list Tasks for execution classification: %w", err)
	}
	referencedSessions := make(map[client.ObjectKey]struct{})
	for i := range tasks.Items {
		task := &tasks.Items[i]
		if task.Spec.Type != corev1alpha1.TaskTypeAgent {
			continue
		}
		classifications := 0
		if task.Status.AgentExecutionBinding != nil {
			classifications++
		}
		if task.Status.AgentExecutionNoExecution != nil {
			classifications++
		}
		if task.Status.AgentExecutionQuarantine != nil {
			classifications++
		}
		if classifications > 1 {
			issues = appendReadinessIssue(issues, fmt.Sprintf(
				"Task %s/%s has conflicting execution classifications", task.Namespace, task.Name,
			))
			continue
		}
		if binding := task.Status.AgentExecutionBinding; binding != nil {
			conflict := (binding.ContractVersion == corev1alpha1.AgentRuntimeContractHarnessV1 &&
				(task.Status.Execution != nil || task.Status.Delivery != nil)) ||
				(binding.ContractVersion == corev1alpha1.AgentRuntimeContractHarnessV2 && task.Status.HarnessRuntime != nil)
			if conflict {
				issues = appendReadinessIssue(issues, fmt.Sprintf(
					"Task %s/%s binding conflicts with route-specific status evidence", task.Namespace, task.Name,
				))
				continue
			}
		}
		if task.Status.AgentExecutionNoExecution != nil &&
			(task.Status.HarnessRuntime != nil || task.Status.Execution != nil || task.Status.Delivery != nil) {
			issues = appendReadinessIssue(issues, fmt.Sprintf(
				"Task %s/%s no-execution disposition conflicts with route-specific status evidence", task.Namespace, task.Name,
			))
			continue
		}
		if !agentTaskNeedsMigrationClassification(task) {
			continue
		}
		if classifications == 0 {
			issues = appendReadinessIssue(issues, fmt.Sprintf(
				"Task %s/%s has no binding, no-execution disposition, or quarantine", task.Namespace, task.Name,
			))
			continue
		}
		if task.Spec.SessionRef != nil && task.Status.AgentExecutionNoExecution == nil {
			referencedSessions[client.ObjectKey{
				Namespace: task.Namespace,
				Name:      task.Spec.SessionRef.Name,
			}] = struct{}{}
		}
	}

	if len(referencedSessions) != 0 {
		controls := &corev1alpha1.RuntimeSessionControlList{}
		if err := c.APIReader.List(ctx, controls, options...); err != nil {
			return fmt.Errorf("list RuntimeSessionControls for lineage classification: %w", err)
		}
		bySession := make(map[client.ObjectKey]*corev1alpha1.RuntimeSessionControl, len(controls.Items))
		for i := range controls.Items {
			control := &controls.Items[i]
			bySession[client.ObjectKey{Namespace: control.Namespace, Name: control.Spec.SessionName}] = control
		}
		for key := range referencedSessions {
			control := bySession[key]
			if control == nil {
				// A missing control is the sealed absent-control classification for
				// a Session that has not reached first use. The dispatcher creates
				// the authoritative control and establishes lineage atomically with
				// the first mutation Lease. Once a control exists, readiness remains
				// fail-closed until it has lineage or an immutable block below.
				continue
			}
			lineageClassified := control.Status.Lineage != nil
			ambiguityBlocked := control.Status.Availability == "ReconciliationBlocked" &&
				strings.TrimSpace(control.Status.BlockedReason) != ""
			if !lineageClassified && !ambiguityBlocked {
				issues = appendReadinessIssue(issues, fmt.Sprintf(
					"referenced Session %s/%s has no lineage or immutable reconciliation block", key.Namespace, key.Name,
				))
			}
		}
	}

	if len(issues) != 0 {
		return fmt.Errorf("agent execution classification is incomplete: %s", strings.Join(issues, "; "))
	}
	return nil
}

func appendReadinessIssue(issues []string, issue string) []string {
	if len(issues) >= maxAgentExecutionReadinessExamples {
		return issues
	}
	return append(issues, issue)
}

func canonicalSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+64 || !strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, char := range value[len("sha256:"):] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}
