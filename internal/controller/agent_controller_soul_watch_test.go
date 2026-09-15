package controller

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/workqueue"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/agentcontext"
)

const (
	agentSoulWatchMapName = "agent-soul"
	agentSoulWatchMapKey  = "SOUL.md"
	agentSoulWatchText    = "Be direct and careful."
)

func newAgentSoulWatchReconciler(t *testing.T, objects ...client.Object) *AgentReconciler {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	c := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&corev1alpha1.Agent{}).
		WithIndex(&corev1alpha1.Agent{}, agentSoulConfigMapDependenciesField, agentSoulConfigMapDependencyIndex).
		WithObjects(objects...).Build()
	return &AgentReconciler{Client: c, Scheme: scheme}
}

func agentForSoulWatch(name string) *corev1alpha1.Agent {
	agent := baseAgent(name)
	agent.Generation = 7
	agent.Spec.Soul = &corev1alpha1.SoulSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: agentSoulWatchMapName, Key: agentSoulWatchMapKey},
		Digest:       agentcontext.Digest(agentSoulWatchText),
	}
	return agent
}

func TestAgentSoulConfigMapWatchMapping(t *testing.T) {
	matching := agentForSoulWatch("matching")
	otherKey := agentForSoulWatch("same-map-other-key")
	otherKey.Spec.Soul.ConfigMapRef.Key = "another-soul.md"
	otherNamespace := agentForSoulWatch("other-namespace")
	otherNamespace.Namespace = "another-namespace"
	otherMap := agentForSoulWatch("other-map")
	otherMap.Spec.Soul.ConfigMapRef.Name = "another-map"
	inline := baseAgent("inline")
	inline.Spec.Soul = &corev1alpha1.SoulSource{Inline: agentSoulWatchText}
	roleOnly := baseAgent("role-only")
	roleOnly.Spec.SystemPrompt = &corev1alpha1.PromptSource{
		ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: agentSoulWatchMapName, Key: agentSoulWatchMapKey},
	}
	skillOnly := baseAgent("skill-only")
	skillOnly.Spec.Skills = []corev1alpha1.SkillReference{{ConfigMapRef: roleOnly.Spec.SystemPrompt.ConfigMapRef.DeepCopy()}}
	r := newAgentSoulWatchReconciler(t, matching, otherKey, otherNamespace, otherMap, inline, roleOnly, skillOnly, baseAgent("no-soul"))
	configMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: agentSoulWatchMapName, Namespace: testNS}}
	want := []reconcile.Request{
		{NamespacedName: client.ObjectKeyFromObject(matching)},
		{NamespacedName: client.ObjectKeyFromObject(otherKey)},
	}
	ctx := context.Background()
	h := handler.EnqueueRequestsFromMapFunc(r.agentsForSoulConfigMap)

	for _, operation := range []string{"create", "update", "delete", "generic"} {
		t.Run(operation, func(t *testing.T) {
			queue := workqueue.NewTypedRateLimitingQueue(workqueue.DefaultTypedControllerRateLimiter[reconcile.Request]())
			t.Cleanup(queue.ShutDown)
			switch operation {
			case "create":
				h.Create(ctx, event.CreateEvent{Object: configMap}, queue)
			case "update":
				updated := configMap.DeepCopy()
				updated.Data = map[string]string{agentSoulWatchMapKey: "changed"}
				h.Update(ctx, event.UpdateEvent{ObjectOld: configMap, ObjectNew: updated}, queue)
			case "delete":
				h.Delete(ctx, event.DeleteEvent{Object: configMap}, queue)
			case "generic":
				h.Generic(ctx, event.GenericEvent{Object: configMap}, queue)
			}
			require.Equal(t, len(want), queue.Len())
			var got []reconcile.Request
			for queue.Len() > 0 {
				request, shutdown := queue.Get()
				require.False(t, shutdown)
				got = append(got, request)
				queue.Done(request)
			}
			require.ElementsMatch(t, want, got)
		})
	}

	for _, object := range []client.Object{
		nil,
		&corev1.Pod{ObjectMeta: configMap.ObjectMeta},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: agentSoulWatchMapName}},
		&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Namespace: testNS}},
	} {
		require.Empty(t, r.agentsForSoulConfigMap(ctx, object))
	}
}

func TestAgentSoulConfigMapWatchReferenceChanges(t *testing.T) {
	ctx := context.Background()
	agent := agentForSoulWatch("retargeted")
	r := newAgentSoulWatchReconciler(t, agent)
	original := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: agentSoulWatchMapName, Namespace: testNS}}
	replacement := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "replacement", Namespace: testNS}}
	want := []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(agent)}}
	require.Equal(t, want, r.agentsForSoulConfigMap(ctx, original))
	require.Empty(t, r.agentsForSoulConfigMap(ctx, replacement))

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(agent), agent))
	agent.Spec.Soul.ConfigMapRef.Name = replacement.Name
	require.NoError(t, r.Update(ctx, agent))
	require.Empty(t, r.agentsForSoulConfigMap(ctx, original))
	require.Equal(t, want, r.agentsForSoulConfigMap(ctx, replacement))

	agent.Spec.Soul = nil
	require.NoError(t, r.Update(ctx, agent))
	require.Empty(t, r.agentsForSoulConfigMap(ctx, replacement))
}

func TestAgentSoulConfigMapWatchReadinessTransitions(t *testing.T) {
	ctx := context.Background()
	agent := agentForSoulWatch("readiness")
	r := newAgentSoulWatchReconciler(t, agent)
	request := reconcile.Request{NamespacedName: client.ObjectKeyFromObject(agent)}
	configMap := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: agentSoulWatchMapName, Namespace: testNS},
		Data:       map[string]string{agentSoulWatchMapKey: agentSoulWatchText},
	}
	assertReady := func(want bool, message string) {
		t.Helper()
		stored := &corev1alpha1.Agent{}
		require.NoError(t, r.Get(ctx, request.NamespacedName, stored))
		require.Equal(t, want, stored.Status.Ready)
		require.Equal(t, agent.Generation, stored.Generation)
		condition := meta.FindStatusCondition(stored.Status.Conditions, "Ready")
		require.NotNil(t, condition)
		require.Equal(t, agent.Generation, condition.ObservedGeneration)
		if want {
			require.Equal(t, metav1.ConditionTrue, condition.Status)
			require.Equal(t, reasonValidationSucceeded, condition.Reason)
		} else {
			require.Equal(t, metav1.ConditionFalse, condition.Status)
			require.Equal(t, reasonValidationFailed, condition.Reason)
			require.Contains(t, condition.Message, message)
		}
	}
	reconcileMap := func() {
		t.Helper()
		requests := r.agentsForSoulConfigMap(ctx, configMap)
		require.Equal(t, []reconcile.Request{request}, requests)
		for _, mapped := range requests {
			result, err := r.Reconcile(ctx, mapped)
			require.NoError(t, err)
			require.Zero(t, result.RequeueAfter)
		}
	}

	_, err := r.Reconcile(ctx, request)
	require.NoError(t, err)
	assertReady(false, "soul ConfigMap was not found")

	require.NoError(t, r.Create(ctx, configMap))
	reconcileMap()
	assertReady(true, "")

	configMap.Data[agentSoulWatchMapKey] = "changed persona"
	require.NoError(t, r.Update(ctx, configMap))
	reconcileMap()
	assertReady(false, "does not match its expected digest")

	configMap.Data[agentSoulWatchMapKey] = agentSoulWatchText
	require.NoError(t, r.Update(ctx, configMap))
	reconcileMap()
	assertReady(true, "")

	delete(configMap.Data, agentSoulWatchMapKey)
	require.NoError(t, r.Update(ctx, configMap))
	reconcileMap()
	assertReady(false, "does not contain the selected key")

	configMap.Data[agentSoulWatchMapKey] = agentSoulWatchText
	require.NoError(t, r.Update(ctx, configMap))
	reconcileMap()
	assertReady(true, "")

	require.NoError(t, r.Delete(ctx, configMap))
	reconcileMap()
	assertReady(false, "soul ConfigMap was not found")

	configMap.ResourceVersion = ""
	configMap.UID = ""
	require.NoError(t, r.Create(ctx, configMap))
	reconcileMap()
	assertReady(true, "")
}

func TestAgentSoulConfigMapWatchCopilotRoleDependencies(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*corev1alpha1.Agent)
		want   bool
	}{
		{name: "inline soul", want: true},
		{name: "ConfigMap soul", mutate: func(agent *corev1alpha1.Agent) { agent.Spec.Soul = agentForSoulWatch("source").Spec.Soul }, want: true},
		{name: "shared role and soul ConfigMap", mutate: func(agent *corev1alpha1.Agent) {
			agent.Spec.Soul = agentForSoulWatch("source").Spec.Soul
			agent.Spec.Soul.ConfigMapRef.Name = copilotInstructionsRoleMapName
		}, want: true},
		{name: "another namespace", mutate: func(agent *corev1alpha1.Agent) { agent.Namespace = "another-namespace" }},
		{name: "no soul", mutate: func(agent *corev1alpha1.Agent) { agent.Spec.Soul = nil }},
		{name: "AI", mutate: func(agent *corev1alpha1.Agent) { agent.Spec.Runtime = nil }},
		{name: "Claude", mutate: func(agent *corev1alpha1.Agent) { agent.Spec.Runtime.Type = corev1alpha1.AgentRuntimeClaude }},
		{name: "legacy Copilot", mutate: func(agent *corev1alpha1.Agent) {
			agent.Spec.Runtime.ContractVersion = new(corev1alpha1.AgentRuntimeContractHarnessV1)
		}},
		{name: "unclassified Copilot", mutate: func(agent *corev1alpha1.Agent) { agent.Spec.Runtime.ContractVersion = nil }},
		{name: "runtimeRef", mutate: func(agent *corev1alpha1.Agent) {
			agent.Spec.Runtime.Type = ""
			agent.Spec.Runtime.RuntimeRef = &corev1alpha1.AgentRuntimeReference{Name: "external"}
			agent.Spec.Runtime.ContractVersion = nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			agent := copilotSoulInstructionsAgent("role-dependent")
			agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: copilotInstructionsRoleMapName, Key: copilotInstructionsRoleMapKey}}
			if test.mutate != nil {
				test.mutate(agent)
			}
			r := newAgentSoulWatchReconciler(t, agent)
			roleMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: copilotInstructionsRoleMapName, Namespace: testNS}}
			requests := r.agentsForSoulConfigMap(context.Background(), roleMap)
			if test.want {
				want := []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(agent)}}
				require.Equal(t, want, requests)
				dependencies := []string{roleMap.Name}
				if ref := agent.Spec.Soul.ConfigMapRef; ref != nil {
					soulMap := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: ref.Name, Namespace: agent.Namespace}}
					require.Equal(t, want, r.agentsForSoulConfigMap(context.Background(), soulMap))
					if ref.Name != roleMap.Name {
						dependencies = append(dependencies, ref.Name)
					}
				}
				require.ElementsMatch(t, dependencies, agentSoulConfigMapDependencyIndex(agent))
			} else {
				require.Empty(t, requests)
			}
		})
	}
}

func TestAgentSoulConfigMapWatchCopilotRoleReferenceChanges(t *testing.T) {
	ctx := context.Background()
	agent := copilotSoulInstructionsAgent("role-retargeted")
	agent.Spec.SystemPrompt = &corev1alpha1.PromptSource{ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: copilotInstructionsRoleMapName, Key: copilotInstructionsRoleMapKey}}
	r := newAgentSoulWatchReconciler(t, agent)
	original := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: copilotInstructionsRoleMapName, Namespace: testNS}}
	replacement := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: "replacement-role", Namespace: testNS}}
	want := []reconcile.Request{{NamespacedName: client.ObjectKeyFromObject(agent)}}
	require.Equal(t, want, r.agentsForSoulConfigMap(ctx, original))
	require.Empty(t, r.agentsForSoulConfigMap(ctx, replacement))

	require.NoError(t, r.Get(ctx, client.ObjectKeyFromObject(agent), agent))
	agent.Spec.SystemPrompt.ConfigMapRef.Name = replacement.Name
	require.NoError(t, r.Update(ctx, agent))
	require.Empty(t, r.agentsForSoulConfigMap(ctx, original))
	require.Equal(t, want, r.agentsForSoulConfigMap(ctx, replacement))

	agent.Spec.Soul = nil
	require.NoError(t, r.Update(ctx, agent))
	require.Empty(t, r.agentsForSoulConfigMap(ctx, replacement))
}
