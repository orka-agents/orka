package service

import (
	"testing"

	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/controller"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/workerenv"
)

func TestServiceRejectsAgentSoulConfiguration(t *testing.T) {
	for _, test := range []struct {
		name string
		soul *corev1alpha1.SoulSource
	}{
		{name: "empty", soul: &corev1alpha1.SoulSource{}},
		{name: "inline", soul: &corev1alpha1.SoulSource{Inline: "Use a conversational persona."}},
		{name: "ConfigMap", soul: &corev1alpha1.SoulSource{
			ConfigMapRef: &corev1alpha1.ConfigMapKeySelector{Name: "persona", Key: "soul"},
			Digest:       pv.Digest([]byte("Use a conversational persona.")),
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			agent := new(corev1alpha1.Agent)
			require.NoError(t, fixture.client.Get(t.Context(), client.ObjectKey{
				Namespace: "private", Name: "fixture-agent",
			}, agent))
			agent.Spec.Soul = test.soul
			require.NoError(t, fixture.client.Update(t.Context(), agent))

			require.ErrorIs(t, fixture.service.CheckReady(t.Context()), contract.ErrInvalid)
			created := 0
			fixture.createHook = func(client.Object) error {
				created++
				return nil
			}
			status, err := fixture.service.Submit(t.Context(), "private", "subject", fixture.request(contract.Generate))
			require.ErrorIs(t, err, contract.ErrInvalid)
			require.Nil(t, status)
			require.Zero(t, created, "unsupported soul configuration must not persist a run or create work")
		})
	}
}

func TestServiceWithoutSoulKeepsNativeRolePrompt(t *testing.T) {
	fixture := newServiceFixture(t)
	request := fixture.request(contract.Generate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	run, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	run.state.Parent = taskOperation{Name: "parent", UID: "parent-uid"}
	task := fixture.service.newTask(run, "checks", "Produce the synthetic check proposal.")
	task.UID, task.Generation = "role-task-uid", 1
	agent := new(corev1alpha1.Agent)
	require.NoError(t, fixture.client.Get(t.Context(), client.ObjectKey{
		Namespace: "private", Name: "fixture-agent",
	}, agent))
	require.Nil(t, agent.Spec.Soul)

	builder := controller.NewJobBuilder(fixture.client)
	builder.AIWorkerImage = fixture.service.config.AIWorkerImage
	builder.ControllerURL = "http://127.0.0.1:8080"
	job, err := builder.Build(t.Context(), task, agent, nil)
	require.NoError(t, err)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	require.Empty(t, job.Spec.Template.Spec.InitContainers)
	environment := make(map[string]string)
	for _, variable := range job.Spec.Template.Spec.Containers[0].Env {
		environment[variable.Name] = variable.Value
	}
	require.Equal(t, nativeSystemPrompt, environment[workerenv.AISystemPrompt])
	require.Equal(t, task.Spec.Prompt, environment[workerenv.AIPrompt])
	require.Empty(t, environment[workerenv.AITools])
	require.Equal(t, boolFalse, environment[workerenv.MemoryToolsAutoEnable])
	require.Equal(t, boolFalse, environment[workerenv.MemoryContextEnabled])
}

func TestBoundedNativeRoleDoesNotInheritChildCoordination(t *testing.T) {
	fixture, run := boundedRunFixture(t)
	run.state.Parent = taskOperation{Name: "parent", UID: "parent-uid"}
	task := fixture.service.newTask(run, "select", "Produce the synthetic source selection.")
	task.UID, task.Generation = "role-task-uid", 1
	agent := new(corev1alpha1.Agent)
	require.NoError(t, fixture.client.Get(t.Context(), client.ObjectKey{
		Namespace: "private", Name: "fixture-agent",
	}, agent))
	builder := controller.NewJobBuilder(fixture.client)
	builder.AIWorkerImage = fixture.service.config.AIWorkerImage
	builder.ControllerURL = "http://127.0.0.1:8080"
	job, err := builder.Build(t.Context(), task, agent, nil)
	require.NoError(t, err)
	require.Len(t, job.Spec.Template.Spec.Containers, 1)
	environment := make(map[string]string)
	coordinationEntries := 0
	for _, variable := range job.Spec.Template.Spec.Containers[0].Env {
		environment[variable.Name] = variable.Value
		if variable.Name == workerenv.CoordinationEnabled {
			coordinationEntries++
		}
	}
	getenv := func(name string) string { return environment[name] }
	worker := workerenv.ParseAIWorkerEnv(getenv)
	coordination := workerenv.ParseCoordinationEnv(getenv)
	require.Equal(t, "1", getenv(contract.BoundedModelRequestsEnv))
	require.Equal(t, string(run.object.UID), getenv(contract.ModelBudgetRunEnv))
	require.Equal(t, "select", getenv(contract.ModelBudgetRoleEnv))
	require.Equal(t, run.state.Parent.Name, getenv(workerenv.ParentTask))
	require.False(t, coordination.Enabled)
	require.Equal(t, 1, coordinationEntries)
	require.False(t, coordination.AutonomousMode)
	require.Empty(t, worker.Tools)
	require.Empty(t, worker.Fallbacks)
	require.Empty(t, getenv(workerenv.SessionName))
	require.Equal(t, boolFalse, getenv(workerenv.MemoryToolsAutoEnable))
	require.Equal(t, boolFalse, getenv(workerenv.MemoryContextEnabled))
	require.Equal(t, task.Name, worker.TaskName)
	require.Equal(t, task.Namespace, worker.TaskNamespace)
	require.Equal(t, string(task.UID), worker.TaskUID)
}
