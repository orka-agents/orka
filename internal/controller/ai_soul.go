package controller

import (
	"context"
	"reflect"
	"strings"

	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
	"github.com/orka-agents/orka/internal/store"
)

// resolvedAISoul is a prepared input, not a worker-controlled environment value.
type resolvedAISoul struct {
	Prompt     string
	UserPrompt string
	Binding    corev1alpha1.TaskSoulBinding
}

func resolveAISoul(ctx context.Context, reader client.Reader, task *corev1alpha1.Task, agent *corev1alpha1.Agent) (*resolvedAISoul, error) {
	if task == nil || task.Spec.Type != corev1alpha1.TaskTypeAI || agent == nil || agent.Spec.Soul == nil {
		if task != nil && task.Status.SoulBinding != nil {
			return nil, invalidAISoulConfiguration("AI soul configuration was removed after binding")
		}
		return nil, nil
	}
	if err := validateAISoulIntroduction(task); err != nil {
		return nil, err
	}
	if err := validateSoulRuntime(agent); err != nil {
		return nil, invalidAISoulConfiguration("%w", err)
	}
	if task.UID == "" || agent.UID == "" || agent.Generation < 1 || task.Generation < 1 {
		return nil, invalidAISoulConfiguration("AI soul binding requires persistent Task and Agent identities")
	}
	soul, err := agentcontext.ResolveSoul(ctx, reader, agent)
	if err != nil {
		return nil, err
	}
	role := ""
	if task.Spec.AI != nil {
		role = task.Spec.AI.SystemPrompt
	}
	if role == "" {
		role, err = resolveACPSystemPrompt(ctx, reader, agent)
		if err != nil {
			return nil, err
		}
	}
	prompt := agentcontext.Compose(role, soul)
	userPrompt := effectiveAISoulTaskPrompt(task)
	if len(literalKubernetesPrompt(userPrompt)) > maxContainerDeliveredPromptBytes {
		return nil, invalidAISoulConfiguration("AI Task prompt exceeds the safe literal environment limit")
	}

	// Kubernetes expands EnvVar.Value. Count the actual escaped envelope before
	// persisting a binding; the worker receives the original literal bytes.
	if len(literalKubernetesPrompt(prompt)) > maxContainerDeliveredPromptBytes {
		return nil, invalidAISoulConfiguration("composed AI soul prompt exceeds the safe environment limit")
	}
	binding := corev1alpha1.TaskSoulBinding{
		TaskGeneration: task.Generation, AgentUID: string(agent.UID), AgentGeneration: agent.Generation,
		SoulDigest: soul.Digest, PromptDigest: agentcontext.Digest(prompt),
	}
	if task.Status.SoulBinding != nil && *task.Status.SoulBinding != binding {
		return nil, invalidAISoulConfiguration("AI prompt configuration changed after binding; create a new Task")
	}
	return &resolvedAISoul{Prompt: prompt, UserPrompt: userPrompt, Binding: binding}, nil
}

// validateAISoulIntroduction preserves legacy no-soul execution while preventing
// an already-started Task from acquiring its first persona on retry or iteration.
func validateAISoulIntroduction(task *corev1alpha1.Task) error {
	if task.Status.SoulBinding == nil && (task.Status.Attempts > 0 || task.Status.StartTime != nil ||
		task.Status.JobName != "" || task.Status.JobUID != "" || task.Status.Iteration > 0) {
		return invalidAISoulConfiguration("AI Task cannot acquire a soul after execution has started; create a new Task")
	}
	return nil
}

func literalKubernetesPrompt(value string) string {
	return strings.ReplaceAll(value, "$", "$$")
}

func (r *TaskReconciler) prepareAISoul(ctx context.Context, task *corev1alpha1.Task, agent *corev1alpha1.Agent) (*resolvedAISoul, error) {
	if task.Spec.Type != corev1alpha1.TaskTypeAI {
		return nil, nil
	}
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	resolved, err := resolveAISoul(ctx, reader, task, agent)
	if err != nil {
		return nil, err
	}
	var binding *corev1alpha1.TaskSoulBinding
	if resolved != nil {
		binding = &resolved.Binding
	}
	if task.Spec.SessionRef != nil {
		if r.SessionManager == nil {
			if binding != nil {
				return nil, invalidAISoulConfiguration("session manager is required for an AI soul")
			}
		} else if err := r.SessionManager.validateSoulContext(ctx, task, binding); err != nil {
			return nil, err
		}
	}
	if binding == nil || task.Status.SoulBinding != nil {
		return resolved, r.pinAISoulSession(ctx, task, resolved)
	}
	err = retryTaskStatusOnConflict(retry.DefaultBackoff, func() error {
		current := &corev1alpha1.Task{}
		if err := reader.Get(ctx, client.ObjectKeyFromObject(task), current); err != nil {
			return err
		}
		if current.UID != task.UID || current.Generation != task.Generation || !current.DeletionTimestamp.IsZero() {
			return invalidAISoulConfiguration("task identity changed before AI soul binding")
		}
		if err := validateAISoulIntroduction(current); err != nil {
			return err
		}
		if current.Status.SoulBinding != nil {
			if *current.Status.SoulBinding != *binding {
				return invalidAISoulConfiguration("AI soul was bound to a different prompt configuration")
			}
			task.Status = current.Status
			return nil
		}
		base := current.DeepCopy()
		current.Status.SoulBinding = binding
		if err := r.Status().Patch(ctx, current, client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})); err != nil {
			return err
		}
		task.Status = current.Status
		return nil
	})
	if err != nil {
		return resolved, err
	}
	return resolved, r.pinAISoulSession(ctx, task, resolved)
}

// Pin before Job creation: final transcript/result/lock-release failures cannot
// make a used Session appear unestablished or let its next Task change persona.
func (r *TaskReconciler) pinAISoulSession(ctx context.Context, task *corev1alpha1.Task, prepared *resolvedAISoul) error {
	if prepared == nil || task.Spec.SessionRef == nil {
		return nil
	}
	if _, gateway, err := r.SessionManager.gatewayEventForTask(ctx, task); err != nil {
		return err
	} else if gateway {
		return nil // Canonical Gateway projection owns its revision and lock.
	}
	writer, ok := r.SessionManager.store.(store.SessionSoulWriter)
	if !ok {
		return invalidAISoulConfiguration("session store does not support durable soul revision pinning")
	}
	return writer.EnsureSessionSoulWithLock(ctx, task.Namespace, task.Spec.SessionRef.Name,
		task.Name, string(task.UID), agentcontext.SessionDigest(&prepared.Binding))
}

func (m *SessionManager) validateSoulContext(ctx context.Context, task *corev1alpha1.Task, binding *corev1alpha1.TaskSoulBinding) error {
	reader, ok := m.store.(store.SessionSoulReader)
	if !ok {
		if binding == nil {
			return nil // Preserve no-soul behavior for legacy Session store adapters.
		}
		return invalidAISoulConfiguration("session store does not support soul revision metadata")
	}
	state, err := reader.ReadSessionSoul(ctx, task.Namespace, task.Spec.SessionRef.Name, task.Name, string(task.UID))
	if err != nil {
		return err
	}
	digest := agentcontext.SessionDigest(binding)
	if state.Established {
		if state.Digest != digest {
			return invalidAISoulConfiguration("session soul configuration does not match; create a new Session")
		}
		return nil
	}
	if digest == "" {
		return nil
	}
	if state.MessageCount == 0 {
		if !task.Spec.SessionRef.Append {
			return invalidAISoulConfiguration("a new AI soul Session must append its initial turn")
		}
		return nil
	}
	if event, gateway, err := m.gatewayEventForTask(ctx, task); err != nil {
		return err
	} else if gateway && state.FirstMessageID == store.GatewayUserMessageID(event.ID) {
		return nil
	}
	return invalidAISoulConfiguration("existing Session has no pinned soul revision; create a new Session")
}

func validatePreparedAISoul(task *corev1alpha1.Task, agent *corev1alpha1.Agent, prepared *resolvedAISoul) error {
	if agent == nil || agent.Spec.Soul == nil {
		if prepared != nil || task.Status.SoulBinding != nil {
			return invalidAISoulConfiguration("AI soul configuration is missing")
		}
		return nil
	}
	if prepared == nil || task.Status.SoulBinding == nil || !reflect.DeepEqual(task.Status.SoulBinding, &prepared.Binding) ||
		prepared.Binding.TaskGeneration != task.Generation || prepared.Binding.AgentUID != string(agent.UID) ||
		prepared.Binding.AgentGeneration != agent.Generation || prepared.Binding.PromptDigest != agentcontext.Digest(prepared.Prompt) || prepared.UserPrompt != effectiveAISoulTaskPrompt(task) {
		return invalidAISoulConfiguration("AI Job requires the exact controller-bound soul prompt")
	}
	return nil
}

func validateSoulRuntime(agent *corev1alpha1.Agent) error {
	return agentcontext.ValidateSoulRuntime(agent)
}

func effectiveAISoulTaskPrompt(task *corev1alpha1.Task) string {
	if task.Spec.AI != nil && task.Spec.AI.Prompt != "" {
		return task.Spec.AI.Prompt
	}
	return task.Spec.Prompt
}
