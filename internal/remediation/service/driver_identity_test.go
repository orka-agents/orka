package service

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/source"
)

type blockedDriverSource struct {
	SourceReader
	resolve func(context.Context, string, string) (source.Target, error)
}

func (s blockedDriverSource) Resolve(ctx context.Context, repository, ref string) (source.Target, error) {
	return s.resolve(ctx, repository, ref)
}

func replaceBlockedDriverRun(t *testing.T, f *serviceFixture, request contract.Request) (*run, *corev1alpha1.Task) {
	t.Helper()
	old, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.NoError(t, f.client.Delete(t.Context(), old.object))
	var tasks corev1alpha1.TaskList
	require.NoError(t, f.client.List(t.Context(), &tasks))
	for index := range tasks.Items {
		task := &tasks.Items[index]
		task.Finalizers = nil
		require.NoError(t, f.client.Update(t.Context(), task))
		require.NoError(t, f.client.Delete(t.Context(), task))
		require.NoError(t, f.results.DeleteResult(t.Context(), task.Namespace, task.Name))
	}
	var jobs batchv1.JobList
	require.NoError(t, f.client.List(t.Context(), &jobs))
	for index := range jobs.Items {
		require.NoError(t, f.client.Delete(t.Context(), &jobs.Items[index]))
	}
	_, err = f.service.Submit(t.Context(), "private", "subject", request)
	require.NoError(t, err)
	fresh, err := f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	require.NotEqual(t, old.object.UID, fresh.object.UID)
	fresh.state.Deadline = old.state.Deadline.Add(time.Hour)
	require.NoError(t, f.service.save(t.Context(), fresh))
	var parent *corev1alpha1.Task
	for range 5 {
		fresh, err = f.service.load(t.Context(), request.RequestID)
		require.NoError(t, err)
		var ready bool
		parent, ready, err = f.service.ensureTask(t.Context(), fresh, parentRole, "")
		require.NoError(t, err)
		if ready {
			break
		}
	}
	require.NotNil(t, parent)
	parent.Status.Phase, parent.Status.Message = corev1alpha1.TaskPhasePending, "replacement parent"
	require.NoError(t, f.client.Status().Update(t.Context(), parent))
	fresh, err = f.service.load(t.Context(), request.RequestID)
	require.NoError(t, err)
	return fresh, parent
}

func TestDriverCannotMutateRecreatedRun(t *testing.T) {
	for _, boundary := range []string{"source", "backend"} {
		for _, outcome := range []string{"success", "failure", "cancel", "deadline"} {
			t.Run(boundary+"-"+outcome, func(t *testing.T) {
				t.Parallel()
				f := newServiceFixture(t)
				dispatch := &recreatedRunDispatchGate{Reader: f.service.config.Reader}
				f.service.config.Reader = dispatch
				if outcome == "deadline" {
					f.service.config.RunTimeout = 3 * time.Second
				}
				entered := make(chan context.Context, 1)
				release := make(chan struct{})
				var releaseOnce sync.Once
				var blockOnce sync.Once
				block := func(ctx context.Context) error {
					first := false
					blockOnce.Do(func() { first = true })
					if !first {
						return nil
					}
					entered <- ctx
					<-release
					switch outcome {
					case "failure":
						return errors.New("synthetic old operation failure")
					case "cancel":
						return context.Canceled
					case "deadline":
						return context.DeadlineExceeded
					default:
						return nil
					}
				}

				if boundary == "source" {
					reader := f.service.config.Source
					f.service.config.Source = blockedDriverSource{SourceReader: reader, resolve: func(ctx context.Context, repository, ref string) (source.Target, error) {
						if err := block(ctx); err != nil {
							return source.Target{}, err
						}
						return reader.Resolve(ctx, repository, ref)
					}}
				} else {
					f.backend.beforeStart = func(ctx context.Context, _ contract.ExecutionRequest) error { return block(ctx) }
				}
				f.start()
				t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
				request := f.request(contract.Validate)
				_, err := f.service.Submit(t.Context(), "private", "subject", request)
				require.NoError(t, err)
				var oldContext context.Context
				select {
				case oldContext = <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("old operation did not block")
				}
				f.service.mu.Lock()
				oldCancel := f.service.active[request.RequestID]
				f.service.mu.Unlock()
				require.NotNil(t, oldCancel)
				oldIdentity, ok := oldContext.Value(runIdentityContextKey{}).(runIdentity)
				require.True(t, ok)
				require.NotEmpty(t, oldIdentity.uid)
				// Close before creation: a later active=false label cannot revoke
				// a replacement already captured by the coordinator's listing.
				dispatch.blockReplacement(oldIdentity)
				fresh, parent := replaceBlockedDriverRun(t, f, request)
				expectedState, expectedVersion := fresh.object.Data["state"], fresh.object.ResourceVersion
				expectedParent := parent.DeepCopy()
				switch outcome {
				case "cancel":
					oldCancel()
				case "deadline":
					deadline, ok := oldContext.Deadline()
					require.True(t, ok)
					timer := time.NewTimer(max(time.Until(deadline), 0))
					<-timer.C
				}
				releaseOnce.Do(func() { close(release) })
				require.Eventually(t, func() bool {
					f.service.mu.Lock()
					defer f.service.mu.Unlock()
					_, active := f.service.active[request.RequestID]
					return !active
				}, 5*time.Second, 5*time.Millisecond, "old drive did not release its slot")
				after, err := f.service.load(t.Context(), request.RequestID)
				require.NoError(t, err)
				require.Equal(t, fresh.object.UID, after.object.UID)
				require.Equal(t, expectedState, after.object.Data["state"], "old completion modified the new run")
				require.Equal(t, expectedVersion, after.object.ResourceVersion)
				observedParent := new(corev1alpha1.Task)
				require.NoError(t, f.client.Get(t.Context(), client.ObjectKeyFromObject(parent), observedParent))
				require.Equal(t, expectedParent, observedParent, "old driver mutated replacement resources")
				dispatch.allowReplacement()
				status := f.awaitStatus(request.RequestID, func(status *contract.Status) bool { return contract.TerminalPhase(status.Phase) })
				require.Equal(t, contract.PhaseSucceeded, status.Phase)
				require.Equal(t, contract.OutcomeReproduced, status.Outcome)
				require.Equal(t, string(parent.UID), status.TaskUID)
			})
		}
	}
}
