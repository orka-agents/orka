package executor

import (
	"context"
	"errors"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/orka-agents/orka/internal/remediation/contract"
	"github.com/stretchr/testify/require"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func TestEvidenceScopeRejectedBeforeBackendEffects(t *testing.T) {
	for _, tc := range []struct {
		name  string
		scope contract.EvidenceScope
		err   error
	}{
		{"strict runtime", contract.ApplicationRuntime, contract.ErrEvidenceScopeUnsupported},
		{"unknown", "future-scope", contract.ErrInvalid},
		{"malformed", "source-harness ", contract.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t, false)
			f.request.EvidenceScope = tc.scope
			apiCalls, accepted := 0, false
			blocked := interceptor.NewClient(f.backend.client.(client.WithWatch), interceptor.Funcs{
				Get: func(context.Context, client.WithWatch, client.ObjectKey, client.Object, ...client.GetOption) error {
					apiCalls++
					return errors.New("unexpected read")
				},
				Create: func(context.Context, client.WithWatch, client.Object, ...client.CreateOption) error {
					apiCalls++
					return errors.New("unexpected creation")
				},
			})
			f.backend.client, f.backend.reader = blocked, blocked
			_, err := f.backend.RequestDigest(f.request)
			require.ErrorIs(t, err, tc.err)
			record, err := f.backend.Start(t.Context(), f.request, func(contract.ExecutionReceipt) error {
				accepted = true
				return nil
			})
			require.Nil(t, record)
			require.ErrorIs(t, err, tc.err)
			var noEffects interface{ NoEffects() bool }
			require.ErrorAs(t, err, &noEffects)
			require.True(t, noEffects.NoEffects())
			_, err = f.backend.Resume(t.Context(), f.request, contract.ExecutionReceipt{})
			require.ErrorIs(t, err, tc.err)
			require.False(t, errors.As(err, &noEffects), "Resume cannot erase prior accepted work")
			require.Zero(t, apiCalls)
			require.False(t, accepted)
			require.Empty(t, f.jobs)
		})
	}
}

func TestEvidenceScopeBoundIntoExecutionDigest(t *testing.T) {
	f := newFixture(t, false)
	require.Equal(t, contract.SourceHarness, f.backend.EvidenceScope())
	configuration := f.backend.ConfigurationDigest()
	legacy, legacyDigest, err := f.backend.requestInput(f.request)
	require.NoError(t, err)
	require.NotContains(t, legacy.Manifest.Environment.Dependencies, contract.EvidenceScopeKey)
	f.request.EvidenceScope = contract.SourceHarness
	scoped, scopedDigest, err := f.backend.requestInput(f.request)
	require.NoError(t, err)
	require.Equal(t, "source-harness", scoped.Manifest.Environment.Dependencies[contract.EvidenceScopeKey])
	require.NotEqual(t, legacyDigest, scopedDigest)
	require.Equal(t, configuration, f.backend.ConfigurationDigest(), "unchanged execution policy must not invalidate legacy cleanup")
	delete(scoped.Manifest.Environment.Dependencies, contract.EvidenceScopeKey)
	require.Equal(t, legacy, scoped)
	recomputed, err := pv.PreparationInputDigest(scoped)
	require.NoError(t, err)
	require.Equal(t, legacyDigest, recomputed, "legacy input bytes must retain their exact digest")
}

func TestScopedEvidenceResumesAndCancelsExactExecution(t *testing.T) {
	f := newFixture(t, true)
	f.request.EvidenceScope = contract.SourceHarness
	digest, err := f.backend.RequestDigest(f.request)
	require.NoError(t, err)
	var receipt contract.ExecutionReceipt
	record, err := f.backend.Start(t.Context(), f.request, func(value contract.ExecutionReceipt) error {
		require.Empty(t, f.jobs)
		receipt = value
		return nil
	})
	require.NoError(t, err)
	require.Equal(t, digest, receipt.InputDigest)
	require.Equal(t, "source-harness", record.Manifest.Environment.Dependencies[contract.EvidenceScopeKey])
	require.NoError(t, pv.ValidateRunBinding(record.Manifest, record.Binding))
	require.Equal(t, pv.Reproduced, record.Assessment.Conclusion)
	again, err := f.backend.Resume(t.Context(), f.request, receipt)
	require.NoError(t, err)
	require.Equal(t, record.Seal.Digest, again.Seal.Digest)
	require.Len(t, f.jobs, 1)
	altered := f.request
	altered.EvidenceScope = ""
	_, err = f.backend.Resume(t.Context(), altered, receipt)
	require.ErrorIs(t, err, contract.ErrConflict, "dropping scope must not reuse a scoped receipt")
	altered.EvidenceScope = contract.ApplicationRuntime
	_, err = f.backend.Resume(t.Context(), altered, receipt)
	require.ErrorIs(t, err, contract.ErrEvidenceScopeUnsupported)
	require.Len(t, f.jobs, 1, "scope changes cannot authorize replacement execution")
	require.NoError(t, f.backend.Cancel(t.Context(), receipt))
	require.Len(t, f.jobs, 1)
}
