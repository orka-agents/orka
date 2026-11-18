package service

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	batchv1 "k8s.io/api/batch/v1"
	networkingv1 "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	kubernetesfake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/orka-agents/orka/internal/remediation/executor"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type pausedAdmission struct {
	request contract.ExecutionRequest
	receipt contract.ExecutionReceipt
}

type pauseAfterReceiptBackend struct {
	contract.ExecutionBackend
	admitted       chan pausedAdmission
	starts         atomic.Int32
	pauseAtReceipt bool
}

func (b *pauseAfterReceiptBackend) EvidenceScope() contract.EvidenceScope {
	return b.ExecutionBackend.(contract.EvidenceScopedBackend).EvidenceScope()
}

func (b *pauseAfterReceiptBackend) Start(ctx context.Context, request contract.ExecutionRequest, accepted func(contract.ExecutionReceipt) error) (*pv.Record, error) {
	b.starts.Add(1)
	return b.ExecutionBackend.Start(ctx, request, func(receipt contract.ExecutionReceipt) error {
		if err := accepted(receipt); err != nil {
			return err
		}
		b.admitted <- pausedAdmission{request: request, receipt: receipt}
		if b.pauseAtReceipt {
			<-ctx.Done()
			return ctx.Err()
		}
		return nil
	})
}

func newRealDeletionBackend(t *testing.T, c client.Client) *executor.Backend {
	t.Helper()
	require.NoError(t, networkingv1.AddToScheme(c.Scheme()))
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	backend, err := executor.New(c, c, kubernetesfake.NewClientset(), sqlite.NewStore(db, ":memory:"), c.Scheme(), executor.Config{
		Enabled: true, Namespace: "private",
		GeneralWorkerImage: "example/general@" + pv.Digest([]byte("g")), GeneralWorkerImageID: pv.Digest([]byte("gc")),
		HelperImage: "example/helper@" + pv.Digest([]byte("h")), HelperImageID: pv.Digest([]byte("hc")),
		ToolImage: "example/tool@" + pv.Digest([]byte("t")), ToolImageID: pv.Digest([]byte("tc")),
		Platform: "linux/amd64", Profile: pv.LocalServices, LocalServices: true,
		CanaryHost: "10.0.0.2", CanaryPort: 80, NodeSelectorKey: "orka.ai/validation", NodeSelectorValue: "qualified",
		ReconcileInterval: time.Millisecond, RunTimeout: time.Minute, CleanupTimeout: time.Second,
	})
	require.NoError(t, err)
	return backend
}

func TestRealBackendDeletedPreparationSettlesCoordinator(t *testing.T) {
	for _, scenario := range []struct {
		name         string
		deleteParent bool
		withJob      bool
	}{
		{name: "preparation Task deleted"},
		{name: "parent and preparation Tasks deleted", deleteParent: true},
		{name: "preparation deleted with Job and inputs", withJob: true},
		{name: "parent and preparation deleted with Job and inputs", deleteParent: true, withJob: true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			f := newServiceFixture(t)
			jobCreated := make(chan struct{})
			var jobOnce sync.Once
			f.afterCreateHook = func(object client.Object) error {
				if job, ok := object.(*batchv1.Job); ok && len(job.Spec.Template.Spec.Containers) != 0 &&
					job.Spec.Template.Spec.Containers[0].Name == "prepare" {
					jobOnce.Do(func() { close(jobCreated) })
				}
				return nil
			}
			backend := newRealDeletionBackend(t, f.client)
			pausing := &pauseAfterReceiptBackend{
				ExecutionBackend: backend, admitted: make(chan pausedAdmission, 1), pauseAtReceipt: !scenario.withJob,
			}
			config := f.service.config
			config.Backend = pausing
			var err error
			f.service, err = New(config)
			require.NoError(t, err)
			stop := f.start()
			request := f.request(contract.Validate)
			_, err = f.service.Submit(t.Context(), "private", "subject", request)
			require.NoError(t, err)
			var admitted pausedAdmission
			select {
			case admitted = <-pausing.admitted:
			case <-time.After(5 * time.Second):
				t.Fatal("real backend receipt was not persisted")
			}
			if scenario.withJob {
				select {
				case <-jobCreated:
				case <-time.After(5 * time.Second):
					t.Fatal("real backend preparation Job was not created")
				}
			}
			stop()
			saved, err := f.service.load(t.Context(), request.RequestID)
			require.NoError(t, err)
			require.Equal(t, admitted.receipt, *saved.state.Executions["original"].Receipt)

			preparation := new(corev1alpha1.Task)
			key := client.ObjectKey{Namespace: admitted.receipt.Namespace, Name: admitted.receipt.TaskName}
			require.NoError(t, f.client.Get(t.Context(), key, preparation))
			require.Equal(t, admitted.receipt.TaskUID, string(preparation.UID))
			preparation.Finalizers = nil
			require.NoError(t, f.client.Update(t.Context(), preparation))
			require.NoError(t, f.client.Delete(t.Context(), preparation))
			if scenario.deleteParent {
				parent := new(corev1alpha1.Task)
				require.NoError(t, f.client.Get(t.Context(), client.ObjectKey{
					Namespace: "private", Name: saved.state.Parent.Name,
				}, parent))
				require.NoError(t, f.client.Delete(t.Context(), parent))
				require.NoError(t, f.client.Get(t.Context(), client.ObjectKeyFromObject(parent), parent))
				_, handled, err := f.service.ReconcileRemediationTask(t.Context(), parent)
				require.NoError(t, err)
				require.False(t, handled)
				parent.Finalizers = nil
				require.NoError(t, f.client.Update(t.Context(), parent))
			} else {
				_, err := f.service.Cancel(t.Context(), "private", request.RequestID)
				require.NoError(t, err)
			}
			f.service, err = New(config)
			require.NoError(t, err)
			f.start()
			require.EventuallyWithT(t, func(c *assert.CollectT) {
				current, err := f.service.load(t.Context(), request.RequestID)
				require.NoError(c, err)
				require.Equal(c, contract.PhaseCancelled, current.state.Status.Phase)
				require.True(c, current.state.Settled)
				require.Equal(c, admitted.receipt, *current.state.Executions["original"].Receipt)
				require.True(c, current.state.Executions["original"].Attempted)
				require.False(c, current.state.Executions["original"].Rejected)
				require.NoError(c, EnsureNoActiveRuns(t.Context(), f.client, "private"))
			}, 5*time.Second, 5*time.Millisecond)
			require.NoError(t, f.service.CheckReady(t.Context()))
			require.EqualValues(t, 1, pausing.starts.Load())

			replay, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err = backend.Start(replay, admitted.request, func(contract.ExecutionReceipt) error {
				t.Error("cancelled admission replay produced a new receipt")
				return contract.ErrConflict
			})
			require.Error(t, err)
			require.True(t, apierrors.IsNotFound(f.client.Get(t.Context(), key, new(corev1alpha1.Task))),
				"cancelled preparation identity was recreated")
			var jobs batchv1.JobList
			require.NoError(t, f.client.List(t.Context(), &jobs, client.InNamespace("private")))
			for _, job := range jobs.Items {
				for _, owner := range job.OwnerReferences {
					require.NotEqual(t, admitted.receipt.TaskUID, string(owner.UID), "preparation Job survived proved cleanup")
				}
			}
		})
	}
}
