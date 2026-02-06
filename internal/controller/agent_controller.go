/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/sozercan/mercan/api/v1alpha1"
)

// AgentReconciler reconciles a Agent object
type AgentReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=core.mercan.ai,resources=agents,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core.mercan.ai,resources=agents/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=core.mercan.ai,resources=agents/finalizers,verbs=update
// +kubebuilder:rbac:groups=core.mercan.ai,resources=tasks,verbs=list
// +kubebuilder:rbac:groups=core.mercan.ai,resources=providers,verbs=get
// +kubebuilder:rbac:groups=core.mercan.ai,resources=tools,verbs=get
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get

// Reconcile validates the Agent configuration and updates its status.
func (r *AgentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	// Fetch the Agent
	agent := &corev1alpha1.Agent{}
	if err := r.Get(ctx, req.NamespacedName, agent); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	logger.Info("Reconciling Agent", "agent", agent.Name)

	// Validate the agent configuration
	validationErr := r.validateAgent(ctx, agent)

	// Count active tasks referencing this agent
	activeTasks, err := r.countActiveTasks(ctx, agent)
	if err != nil {
		logger.Error(err, "Failed to count active tasks")
		// Non-fatal: continue with status update
	}

	// Update status
	return r.updateStatus(ctx, agent, activeTasks, validationErr)
}

// validateAgent validates the Agent's referenced resources exist and config is coherent.
func (r *AgentReconciler) validateAgent(ctx context.Context, agent *corev1alpha1.Agent) error {
	// Runtime and providerRef are mutually exclusive
	if agent.Spec.Runtime != nil && agent.Spec.ProviderRef != nil {
		return fmt.Errorf("runtime and providerRef are mutually exclusive")
	}

	// For non-runtime agents, validate model config
	if agent.Spec.Runtime == nil {
		if agent.Spec.ProviderRef == nil && (agent.Spec.Model == nil || agent.Spec.Model.Provider == "") {
			return fmt.Errorf("either providerRef or model.provider must be specified")
		}
	}

	// Validate providerRef if set
	if agent.Spec.ProviderRef != nil {
		provider := &corev1alpha1.Provider{}
		ns := agent.Namespace
		if agent.Spec.ProviderRef.Namespace != "" {
			ns = agent.Spec.ProviderRef.Namespace
		}
		key := client.ObjectKey{Name: agent.Spec.ProviderRef.Name, Namespace: ns}
		if err := r.Get(ctx, key, provider); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("referenced provider %q not found", agent.Spec.ProviderRef.Name)
			}
			return fmt.Errorf("failed to get provider %q: %w", agent.Spec.ProviderRef.Name, err)
		}
		if !provider.Status.Ready {
			return fmt.Errorf("referenced provider %q is not ready", agent.Spec.ProviderRef.Name)
		}
	}

	// Validate secretRef if set
	if agent.Spec.SecretRef != nil {
		secret := &corev1.Secret{}
		key := client.ObjectKey{Name: agent.Spec.SecretRef.Name, Namespace: agent.Namespace}
		if err := r.Get(ctx, key, secret); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("referenced secret %q not found", agent.Spec.SecretRef.Name)
			}
			return fmt.Errorf("failed to get secret %q: %w", agent.Spec.SecretRef.Name, err)
		}
	}

	// Validate referenced tools exist
	for _, toolRef := range agent.Spec.Tools {
		if toolRef.Enabled != nil && !*toolRef.Enabled {
			continue
		}
		tool := &corev1alpha1.Tool{}
		key := client.ObjectKey{Name: toolRef.Name, Namespace: agent.Namespace}
		if err := r.Get(ctx, key, tool); err != nil {
			if errors.IsNotFound(err) {
				// Tool might be a built-in; only warn, don't fail
				continue
			}
			return fmt.Errorf("failed to check tool %q: %w", toolRef.Name, err)
		}
	}

	// Validate referenced skills (ConfigMaps) exist
	for _, skillRef := range agent.Spec.Skills {
		cm := &corev1.ConfigMap{}
		key := client.ObjectKey{Name: skillRef.ConfigMapRef.Name, Namespace: agent.Namespace}
		if err := r.Get(ctx, key, cm); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("referenced skill ConfigMap %q not found", skillRef.ConfigMapRef.Name)
			}
			return fmt.Errorf("failed to get skill ConfigMap %q: %w", skillRef.ConfigMapRef.Name, err)
		}
	}

	// Validate systemPrompt configMapRef if set
	if agent.Spec.SystemPrompt != nil && agent.Spec.SystemPrompt.ConfigMapRef != nil {
		cm := &corev1.ConfigMap{}
		key := client.ObjectKey{Name: agent.Spec.SystemPrompt.ConfigMapRef.Name, Namespace: agent.Namespace}
		if err := r.Get(ctx, key, cm); err != nil {
			if errors.IsNotFound(err) {
				return fmt.Errorf("systemPrompt ConfigMap %q not found", agent.Spec.SystemPrompt.ConfigMapRef.Name)
			}
			return fmt.Errorf("failed to get systemPrompt ConfigMap %q: %w", agent.Spec.SystemPrompt.ConfigMapRef.Name, err)
		}
		if _, ok := cm.Data[agent.Spec.SystemPrompt.ConfigMapRef.Key]; !ok {
			return fmt.Errorf("key %q not found in systemPrompt ConfigMap %q", agent.Spec.SystemPrompt.ConfigMapRef.Key, agent.Spec.SystemPrompt.ConfigMapRef.Name)
		}
	}

	// Validate coordination config
	if agent.Spec.Coordination != nil && agent.Spec.Coordination.Enabled {
		for _, allowed := range agent.Spec.Coordination.AllowedAgents {
			ns := agent.Namespace
			if allowed.Namespace != "" {
				ns = allowed.Namespace
			}
			delegateAgent := &corev1alpha1.Agent{}
			key := client.ObjectKey{Name: allowed.Name, Namespace: ns}
			if err := r.Get(ctx, key, delegateAgent); err != nil {
				if errors.IsNotFound(err) {
					return fmt.Errorf("coordination target agent %q not found", allowed.Name)
				}
				return fmt.Errorf("failed to check coordination target agent %q: %w", allowed.Name, err)
			}
		}
	}

	return nil
}

// countActiveTasks counts tasks that reference this agent and are not complete.
func (r *AgentReconciler) countActiveTasks(ctx context.Context, agent *corev1alpha1.Agent) (int32, error) {
	taskList := &corev1alpha1.TaskList{}
	if err := r.List(ctx, taskList, client.InNamespace(agent.Namespace)); err != nil {
		return 0, err
	}

	var count int32
	for i := range taskList.Items {
		task := &taskList.Items[i]
		if task.Spec.AgentRef != nil && task.Spec.AgentRef.Name == agent.Name {
			phase := task.Status.Phase
			if phase != corev1alpha1.TaskPhaseSucceeded && phase != corev1alpha1.TaskPhaseFailed {
				count++
			}
		}
	}
	return count, nil
}

// updateStatus updates the Agent's status with validation results and active task count.
func (r *AgentReconciler) updateStatus(ctx context.Context, agent *corev1alpha1.Agent, activeTasks int32, validationErr error) (ctrl.Result, error) {
	now := metav1.Now()

	agent.Status.ActiveTasks = activeTasks

	condition := metav1.Condition{
		Type:               "Ready",
		LastTransitionTime: now,
		ObservedGeneration: agent.Generation,
	}

	if validationErr != nil {
		condition.Status = metav1.ConditionFalse
		condition.Reason = "ValidationFailed"
		condition.Message = validationErr.Error()
	} else {
		condition.Status = metav1.ConditionTrue
		condition.Reason = "ValidationSucceeded"
		condition.Message = "Agent configuration is valid"
	}

	meta.SetStatusCondition(&agent.Status.Conditions, condition)

	if err := r.Status().Update(ctx, agent); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1alpha1.Agent{}).
		Named("agent").
		Complete(r)
}
