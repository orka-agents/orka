package aitools

import (
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	serializerjson "k8s.io/apimachinery/pkg/runtime/serializer/json"
)

func TestResolveGatewayReplyPolicyThroughKubernetesJSON(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, corev1alpha1.AddToScheme(scheme))
	serializer := serializerjson.NewSerializerWithOptions(serializerjson.DefaultMetaFactory, scheme, scheme, serializerjson.SerializerOptions{})
	for _, tc := range []struct {
		name       string
		agentTools []corev1alpha1.ToolReference
		taskTools  []string
		editTask   func(*corev1alpha1.Task)
		wantReply  bool
	}{
		{name: "nil defaults", wantReply: true},
		{name: "empty Agent tools", agentTools: []corev1alpha1.ToolReference{}},
		{name: "empty Task AI tools", taskTools: []string{}},
		{name: "both empty", agentTools: []corev1alpha1.ToolReference{}, taskTools: []string{}},
		{name: "Agent opt in", agentTools: []corev1alpha1.ToolReference{{Name: GatewayReplyToolName}}, wantReply: true},
		{name: "Task opt in", taskTools: []string{GatewayReplyToolName}, wantReply: true},
		{name: "both opt in", agentTools: []corev1alpha1.ToolReference{{Name: GatewayReplyToolName, Enabled: new(true)}}, taskTools: []string{GatewayReplyToolName}, wantReply: true},
		{name: "Agent empty overrides Task opt in", agentTools: []corev1alpha1.ToolReference{}, taskTools: []string{GatewayReplyToolName}},
		{name: "Task empty overrides Agent opt in", agentTools: []corev1alpha1.ToolReference{{Name: GatewayReplyToolName}}, taskTools: []string{}},
		{name: "Agent disabled overrides Task opt in", agentTools: []corev1alpha1.ToolReference{{Name: GatewayReplyToolName, Enabled: new(false)}}, taskTools: []string{GatewayReplyToolName}},
		{name: "Agent closed list", agentTools: []corev1alpha1.ToolReference{{Name: "web_search"}}, taskTools: []string{GatewayReplyToolName}},
		{name: "Task closed list", agentTools: []corev1alpha1.ToolReference{{Name: GatewayReplyToolName}}, taskTools: []string{"web_search"}},
		{name: "runtime deny overrides opt in", taskTools: []string{GatewayReplyToolName}, editTask: func(task *corev1alpha1.Task) {
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{DisallowedTools: []string{GatewayReplyToolName}}
		}},
		{name: "runtime empty allowlist overrides opt in", taskTools: []string{GatewayReplyToolName}, editTask: func(task *corev1alpha1.Task) {
			task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{AllowedTools: []string{}}
		}},
		{name: "transaction deny overrides opt in", taskTools: []string{GatewayReplyToolName}, editTask: func(task *corev1alpha1.Task) {
			task.Spec.Transaction = &corev1alpha1.TaskTransaction{Context: map[string]string{"allowedTools": "[]"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent := &corev1alpha1.Agent{
				TypeMeta: metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Agent"},
				Spec:     corev1alpha1.AgentSpec{Tools: tc.agentTools},
			}
			task := aiToolTask(nil, nil, tc.taskTools)
			task.TypeMeta = metav1.TypeMeta{APIVersion: corev1alpha1.GroupVersion.String(), Kind: "Task"}
			if tc.editTask != nil {
				tc.editTask(task)
			}
			agentJSON, err := runtime.Encode(serializer, agent)
			require.NoError(t, err)
			taskJSON, err := runtime.Encode(serializer, task)
			require.NoError(t, err)
			var storedAgent corev1alpha1.Agent
			var storedTask corev1alpha1.Task
			require.NoError(t, runtime.DecodeInto(serializer, agentJSON, &storedAgent))
			require.NoError(t, runtime.DecodeInto(serializer, taskJSON, &storedTask))
			// Trusted durable-origin eligibility must not override an explicit empty policy.
			got := ResolveWithGatewayReply(&storedTask, &storedAgent, true)
			if tc.wantReply {
				require.Contains(t, got, GatewayReplyToolName)
			} else {
				require.NotContains(t, got, GatewayReplyToolName,
					"serialization must not turn explicit denial into an implicit grant: Agent=%s Task=%s", agentJSON, taskJSON)
			}
			require.Contains(t, got, "recall_memory")
			require.Equal(t, agent.Spec.Tools, storedAgent.Spec.Tools)
			require.Equal(t, task.Spec.AI.Tools, storedTask.Spec.AI.Tools)
			require.NotContains(t, ResolveWithGatewayReply(&storedTask, &storedAgent, false), GatewayReplyToolName)
			require.NotContains(t, Resolve(&storedTask, &storedAgent), GatewayReplyToolName)
		})
	}
}
