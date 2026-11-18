package executor

import (
	"context"
	"errors"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestResolvedWorkerImageIDsAreRequiredNormalizedAndFrozen(t *testing.T) {
	f := newFixture(t, true)
	config := f.backend.config
	config.GeneralWorkerImageID = "containerd://" + config.GeneralWorkerImageID
	config.HelperImageID = "example/helper@" + config.HelperImageID
	normalized, err := New(f.backend.client, f.backend.reader, f.backend.kubeClient, f.store, f.backend.scheme, config)
	require.NoError(t, err)
	require.Equal(t, f.backend.config.GeneralWorkerImageID, normalized.config.GeneralWorkerImageID)
	require.Equal(t, f.backend.config.HelperImageID, normalized.config.HelperImageID)
	require.Equal(t, f.backend.ConfigurationDigest(), normalized.ConfigurationDigest())
	for _, change := range []func(*Config){
		func(c *Config) { c.GeneralWorkerImageID = "" },
		func(c *Config) { c.GeneralWorkerImageID = "invalid" },
		func(c *Config) { c.HelperImageID = "" },
		func(c *Config) { c.HelperImageID = "invalid" },
	} {
		changed := f.backend.config
		change(&changed)
		backend, err := New(f.backend.client, f.backend.reader, f.backend.kubeClient, f.store, f.backend.scheme, changed)
		require.ErrorIs(t, err, contract.ErrInvalid)
		require.Nil(t, backend)
	}
	var receipt contract.ExecutionReceipt
	_, err = f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error {
		receipt = r
		return nil
	})
	require.NoError(t, err)
	for _, change := range []func(*Config){
		func(c *Config) { c.GeneralWorkerImageID = pv.Digest([]byte("changed preparation identity")) },
		func(c *Config) { c.HelperImageID = pv.Digest([]byte("changed helper identity")) },
	} {
		changed := f.backend.config
		change(&changed)
		backend, err := New(f.backend.client, f.backend.reader, f.backend.kubeClient, f.store, f.backend.scheme, changed)
		require.NoError(t, err)
		require.NotEqual(t, f.backend.ConfigurationDigest(), backend.ConfigurationDigest())
		record, err := backend.Resume(t.Context(), f.request, receipt)
		require.ErrorIs(t, err, contract.ErrConflict)
		require.Nil(t, record)
	}
	require.Len(t, f.jobs, 1)
}

func TestPreparationRejectsManifestAsRuntimeIDAndLateRuntimeDrift(t *testing.T) {
	for _, stage := range []string{"before report", "after report"} {
		t.Run(stage, func(t *testing.T) {
			f := newFixture(t, true)
			var receipt contract.ExecutionReceipt
			_, err := f.backend.Start(t.Context(), f.request, func(r contract.ExecutionReceipt) error {
				receipt = r
				return errors.New("pause after persisting receipt")
			})
			require.Error(t, err)
			input, _, err := f.backend.requestInput(f.request)
			require.NoError(t, err)
			input.TaskUID = receipt.TaskUID
			require.NoError(t, f.backend.createPreparation(t.Context(), input, receipt))
			changeRuntimeID := func(ctx context.Context, ns, name string) {
				pod := &corev1.Pod{}
				require.NoError(t, f.backend.client.Get(ctx, client.ObjectKey{Namespace: ns, Name: name}, pod))
				pod.Status.ContainerStatuses[0].ImageID = f.backend.config.GeneralWorkerImage
				require.NoError(t, f.backend.client.Status().Update(ctx, pod))
			}
			if stage == "before report" {
				changeRuntimeID(t.Context(), receipt.Namespace, receipt.TaskName+"-pod")
				f.backend.readReport = func(context.Context, string, string) (pv.PreparationReport, error) {
					t.Fatal("mismatched image identity reached evidence ingestion")
					return pv.PreparationReport{}, nil
				}
			} else {
				read := f.backend.readReport
				f.backend.readReport = func(ctx context.Context, ns, name string) (pv.PreparationReport, error) {
					report, err := read(ctx, ns, name)
					changeRuntimeID(ctx, ns, name)
					return report, err
				}
			}
			record, err := f.backend.Resume(t.Context(), f.request, receipt)
			require.Error(t, err)
			require.Nil(t, record)
			submissions, err := f.store.ListActiveKubernetesValidationSubmissions(t.Context(), receipt.Namespace)
			require.NoError(t, err)
			require.Empty(t, submissions)
			require.Len(t, f.jobs, 1)
		})
	}
}
