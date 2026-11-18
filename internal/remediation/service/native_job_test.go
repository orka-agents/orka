package service

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/remediation/contract"
)

func TestServiceRequiresPinnedAIWorkerImage(t *testing.T) {
	fixture := newServiceFixture(t)
	for _, image := range []string{
		"", "registry.example/ai-worker:latest", "registry.example/ai-worker",
		"registry.example/ai-worker@sha256:short",
		"registry.example/ai-worker@sha512:" + strings.Repeat("3", 128),
		"https://registry.example/ai-worker@sha256:" + strings.Repeat("3", 64),
		"registry.example/ai-worker@sha256:" + strings.Repeat("A", 64),
		fixture.service.config.AIWorkerImage + "\n",
	} {
		config := fixture.service.config
		config.AIWorkerImage = image
		service, err := New(config)
		require.ErrorIs(t, err, contract.ErrInvalid)
		require.Nil(t, service)
	}
	config := fixture.service.config
	config.AIWorkerImage = "registry.example/ai-worker:version@sha256:" + strings.Repeat("3", 64)
	_, err := New(config)
	require.NoError(t, err)
}

func TestServiceFreezesAIWorkerImageAuthority(t *testing.T) {
	fixture := newServiceFixture(t)
	request := fixture.request(contract.Generate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	saved, err := fixture.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.Equal(t, fixture.service.config.AIWorkerImage, saved.state.Authority.AIWorkerImage)
	config := fixture.service.config
	config.AIWorkerImage = "registry.example/ai-worker@sha256:" + strings.Repeat("4", 64)
	restarted, err := New(config)
	require.NoError(t, err)
	require.ErrorIs(t, restarted.CheckReady(t.Context()), contract.ErrConflict)
	require.NoError(t, fixture.service.CheckReady(t.Context()))
}

func TestServiceRejectsUnexpectedProposalJobImage(t *testing.T) {
	for _, image := range []string{
		"registry.example/ai-worker:latest",
		"registry.example/ai-worker@sha256:" + strings.Repeat("4", 64),
	} {
		t.Run(image, func(t *testing.T) {
			fixture := newServiceFixture(t)
			fixture.jobImage = image
			fixture.start()
			request := fixture.request(contract.Generate)
			_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool {
				return contract.TerminalPhase(status.Phase)
			})
			require.Equal(t, contract.PhaseFailed, status.Phase)
			require.Equal(t, "agent-configuration-changed", status.Reason)
			require.False(t, status.PatchReady)
			fixture.results.mu.Lock()
			require.Equal(t, 1, fixture.results.readsByName["private/"+stateName(request.RequestID)+"-select"],
				"only the pre-creation stale-result check may read the proposal result")
			fixture.results.mu.Unlock()
			fixture.backend.mu.Lock()
			require.Empty(t, fixture.backend.requests)
			fixture.backend.mu.Unlock()
		})
	}
}

func TestServiceRequiresObservedProposalJob(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.skipJobs = true
	fixture.start()
	request := fixture.request(contract.Validate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool {
		return contract.TerminalPhase(status.Phase)
	})
	require.Equal(t, contract.PhaseNeedsInput, status.Phase)
	require.Equal(t, "identity-conflict", status.Reason)
	fixture.results.mu.Lock()
	require.Equal(t, 1, fixture.results.readsByName["private/"+stateName(request.RequestID)+"-select"])
	fixture.results.mu.Unlock()
}

func TestServiceRejectsForeignProposalJobOwner(t *testing.T) {
	fixture := newServiceFixture(t)
	fixture.createHook = func(object client.Object) error {
		if job, ok := object.(*batchv1.Job); ok {
			job.OwnerReferences[0].UID = "different-task-uid"
		}
		return nil
	}
	fixture.start()
	request := fixture.request(contract.Validate)
	_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool {
		return contract.TerminalPhase(status.Phase)
	})
	require.Equal(t, contract.PhaseNeedsInput, status.Phase)
	fixture.results.mu.Lock()
	require.Equal(t, 1, fixture.results.readsByName["private/"+stateName(request.RequestID)+"-select"])
	fixture.results.mu.Unlock()
}

func TestServiceProposalJobIdentityFencesResultRead(t *testing.T) {
	for _, replace := range []bool{false, true} {
		name := "retained observation survives TTL cleanup"
		if replace {
			name = "same-name replacement cannot provide results"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newServiceFixture(t)
			fixture.results.onRead = func() {
				var tasks corev1alpha1.TaskList
				require.NoError(t, fixture.client.List(t.Context(), &tasks, client.MatchingLabels{roleLabel: "select"}))
				require.Len(t, tasks.Items, 1)
				task := &tasks.Items[0]
				job := new(batchv1.Job)
				require.NoError(t, fixture.client.Get(t.Context(), client.ObjectKey{Namespace: task.Namespace, Name: task.Status.JobName}, job))
				require.NoError(t, fixture.client.Delete(t.Context(), job))
				if replace {
					job.UID, job.ResourceVersion = "", ""
					require.NoError(t, fixture.client.Create(t.Context(), job))
				}
			}
			fixture.start()
			request := fixture.request(contract.Validate)
			_, err := fixture.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			status := fixture.awaitStatus(request.RequestID, func(status *contract.Status) bool {
				return contract.TerminalPhase(status.Phase)
			})
			saved, err := fixture.service.load(t.Context(), request.RequestID)
			require.NoError(t, err)
			observation := saved.state.Agents["select"].Job
			require.NotNil(t, observation)
			require.Equal(t, fixture.service.config.AIWorkerImage, observation.Image)
			if replace {
				require.Equal(t, contract.PhaseNeedsInput, status.Phase)
				require.Equal(t, "identity-conflict", status.Reason)
				require.True(t, saved.state.Agents["select"].Output.empty())
			} else {
				require.Equal(t, contract.PhaseSucceeded, status.Phase)
				require.Equal(t, contract.OutcomeReproduced, status.Outcome)
			}
		})
	}
}
