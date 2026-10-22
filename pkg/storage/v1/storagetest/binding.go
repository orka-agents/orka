package storagetest

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/stretchr/testify/require"
)

// bindingProvider is what the dataset binding checks need from a provider.
type bindingProvider interface {
	storagev1.ProviderDescriber
	storagev1.ControllerEpochAdvancer
	storagev1.OperationReceiptStore
	storagev1.DatasetBindingStore
}

// Installations, records and key fingerprints the binding checks use.
const (
	bindingInstallation = "installation-a"
	otherInstallation   = "installation-b"
	bindingRecord       = "record-uid-1"
	otherRecord         = "record-uid-2"
	bindingKey          = "hmac-sha256:key-1"
	rotatedKey          = "hmac-sha256:key-2"
)

func bindRequest(dataset string, mode storagev1.BindMode, generation int64) storagev1.BindDatasetRequest {
	return storagev1.BindDatasetRequest{
		DatasetID: dataset, InstallationID: bindingInstallation, RecordUID: bindingRecord,
		SwitchGeneration: generation, KeyFingerprint: bindingKey, Mode: mode,
	}
}

func detachRequest(dataset string, generation int64) storagev1.DetachDatasetRequest {
	return storagev1.DetachDatasetRequest{
		DatasetID: dataset, InstallationID: bindingInstallation, RecordUID: bindingRecord, SwitchGeneration: generation,
	}
}

// requireBindingConflict checks a binding refusal: it matches ErrConflict and
// carries the dataset's stored binding, which the refusal left unchanged.
func requireBindingConflict(t *testing.T, provider bindingProvider, dataset string, err error) {
	t.Helper()
	require.ErrorIs(t, err, storagev1.ErrConflict)
	var conflict storagev1.BindingConflictError
	require.ErrorAs(t, err, &conflict)
	require.Equal(t, dataset, conflict.DatasetID)
	stored, getErr := provider.GetDatasetBinding(t.Context(), dataset)
	if errors.Is(getErr, storagev1.ErrNotFound) {
		require.Nil(t, conflict.Binding, "an unbound dataset's refusal carries no binding")
		return
	}
	require.NoError(t, getErr)
	require.NotNil(t, conflict.Binding)
	require.Equal(t, stored, *conflict.Binding, "the refusal carries the stored binding")
}

// requireBindRefused checks that the dataset's binding does not admit request:
// it returns a BindingConflictError and leaves the binding unchanged. A
// provider that does not serve the embedded mode refuses that mode with
// ErrValidation instead.
func requireBindRefused(t *testing.T, provider bindingProvider, request storagev1.BindDatasetRequest) {
	t.Helper()
	_, err := provider.BindDataset(t.Context(), request)
	require.Errorf(t, err, "%s in mode %s at generation %d", request.InstallationID, request.Mode, request.SwitchGeneration)
	if request.Mode == storagev1.BindEmbedded && errors.Is(err, storagev1.ErrValidation) {
		return
	}
	requireBindingConflict(t, provider, request.DatasetID, err)
}

// allBindModes lists every bind mode.
var allBindModes = []storagev1.BindMode{
	storagev1.BindFresh, storagev1.BindEmbedded, storagev1.BindMigrated, storagev1.BindAdopt, storagev1.BindRebind,
}

// requireUnbound checks that the dataset belongs to no installation.
func requireUnbound(t *testing.T, provider bindingProvider, dataset string) {
	t.Helper()
	_, err := provider.GetDatasetBinding(t.Context(), dataset)
	require.ErrorIs(t, err, storagev1.ErrNotFound)
	info, err := provider.Describe(t.Context())
	require.NoError(t, err)
	require.Nil(t, info.Binding)
}

// requireStoredBinding checks that GetDatasetBinding and Describe both report
// want.
func requireStoredBinding(t *testing.T, provider bindingProvider, dataset string, want storagev1.DatasetBinding) {
	t.Helper()
	stored, err := provider.GetDatasetBinding(t.Context(), dataset)
	require.NoError(t, err)
	require.Equal(t, want, stored)
	info, err := provider.Describe(t.Context())
	require.NoError(t, err)
	require.NotNil(t, info.Binding)
	require.Equal(t, want, *info.Binding)
}

// runDatasetBinding checks that a dataset is bound to one installation, once,
// under the mode rules, and that a detached dataset refuses writes until its
// installation binds it again. populate stores one application record.
func runDatasetBinding[P bindingProvider](t *testing.T, newProvider func(*testing.T) P, populate func(*testing.T, P)) {
	t.Run("binds once and replays the binding", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		requireUnbound(t, provider, dataset)

		before := time.Now().UTC()
		bound, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindFresh, 1))
		require.NoError(t, err)
		require.Equal(t, bindingInstallation, bound.InstallationID)
		require.Equal(t, bindingRecord, bound.RecordUID)
		require.EqualValues(t, 1, bound.SwitchGeneration)
		require.Equal(t, bindingKey, bound.KeyFingerprint)
		require.Equal(t, storagev1.DatasetBound, bound.State)
		require.Equal(t, time.UTC, bound.BoundAt.Location())
		require.False(t, bound.BoundAt.Before(before.Add(-storagev1.MaxClockSkew)), "BoundAt is the provider clock")
		requireStoredBinding(t, provider, dataset, bound)

		for _, mode := range []storagev1.BindMode{storagev1.BindFresh, storagev1.BindAdopt, storagev1.BindRebind, storagev1.BindMigrated} {
			repeat, err := provider.BindDataset(ctx, bindRequest(dataset, mode, 1))
			require.NoErrorf(t, err, "a repeat in mode %s replays the binding", mode)
			require.Equal(t, bound, repeat)
		}
		requireStoredBinding(t, provider, dataset, bound)
	})

	t.Run("another installation never claims a bound dataset", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		bound, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindFresh, 1))
		require.NoError(t, err)
		for _, mode := range []storagev1.BindMode{storagev1.BindFresh, storagev1.BindAdopt, storagev1.BindRebind, storagev1.BindMigrated, storagev1.BindEmbedded} {
			request := bindRequest(dataset, mode, 2)
			request.InstallationID = otherInstallation
			_, err := provider.BindDataset(ctx, request)
			if mode == storagev1.BindEmbedded && errors.Is(err, storagev1.ErrValidation) {
				continue
			}
			requireBindingConflict(t, provider, dataset, err)
		}
		requireStoredBinding(t, provider, dataset, bound)
		_, err = provider.DetachDatasetBinding(ctx, storagev1.DetachDatasetRequest{
			DatasetID: dataset, InstallationID: otherInstallation, RecordUID: bindingRecord, SwitchGeneration: 2,
		})
		requireBindingConflict(t, provider, dataset, err)
		_, err = provider.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
			DatasetID: dataset, Epoch: mirroredEpoch(1, "owner-b"), InstallationID: otherInstallation,
		})
		requireFenced(t, err)
		_, err = provider.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
			DatasetID: dataset, Epoch: mirroredEpoch(1, "owner-a"), InstallationID: bindingInstallation,
		})
		require.NoError(t, err, "the bound installation advances the dataset")
		requireStoredBinding(t, provider, dataset, bound)
	})

	t.Run("one installation moves a binding only to a higher generation", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		bound, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindFresh, 1))
		require.NoError(t, err)
		for name, mutate := range map[string]func(*storagev1.BindDatasetRequest){
			"another record":          func(r *storagev1.BindDatasetRequest) { r.RecordUID = otherRecord },
			"another key fingerprint": func(r *storagev1.BindDatasetRequest) { r.KeyFingerprint = rotatedKey },
			"no key fingerprint":      func(r *storagev1.BindDatasetRequest) { r.KeyFingerprint = "" },
			"a higher generation":     func(r *storagev1.BindDatasetRequest) { r.SwitchGeneration = 2 },
		} {
			t.Run(name, func(t *testing.T) {
				for _, mode := range []storagev1.BindMode{storagev1.BindFresh, storagev1.BindEmbedded, storagev1.BindAdopt, storagev1.BindMigrated} {
					request := bindRequest(dataset, mode, 1)
					mutate(&request)
					requireBindRefused(t, provider, request)
				}
			})
		}
		rebindSame := bindRequest(dataset, storagev1.BindRebind, 1)
		rebindSame.RecordUID = otherRecord
		_, err = provider.BindDataset(ctx, rebindSame)
		requireBindingConflict(t, provider, dataset, err)
		requireStoredBinding(t, provider, dataset, bound)

		rebind := bindRequest(dataset, storagev1.BindRebind, 3)
		rebind.RecordUID = otherRecord
		rebind.KeyFingerprint = rotatedKey
		rebound, err := provider.BindDataset(ctx, rebind)
		require.NoError(t, err, "a recovered installation replaces its record at a higher generation")
		require.Equal(t, storagev1.DatasetBinding{
			InstallationID: bindingInstallation, RecordUID: otherRecord, SwitchGeneration: 3,
			KeyFingerprint: rotatedKey, State: storagev1.DatasetBound, BoundAt: rebound.BoundAt,
		}, rebound)
		requireStoredBinding(t, provider, dataset, rebound)
		_, err = provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindRebind, 2))
		requireBindingConflict(t, provider, dataset, err)
		requireStoredBinding(t, provider, dataset, rebound)
	})

	t.Run("modes check the dataset", func(t *testing.T) {
		ctx := t.Context()
		t.Run("fresh refuses a dataset holding a record", func(t *testing.T) {
			provider := newProvider(t)
			dataset := datasetID(t, provider)
			populate(t, provider)
			_, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindFresh, 1))
			requireBindingConflict(t, provider, dataset, err)
			requireUnbound(t, provider, dataset)
			adopted, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindAdopt, 1))
			require.NoError(t, err, "a dataset holding data is adopted")
			requireStoredBinding(t, provider, dataset, adopted)
		})
		t.Run("fresh refuses a dataset that completed its bootstrap", func(t *testing.T) {
			provider := newProvider(t)
			dataset := datasetID(t, provider)
			require.ErrorIs(t, provider.CompleteDatasetBootstrap(ctx, "00000000-0000-4000-8000-000000000000"), storagev1.ErrDatasetMismatch)
			require.ErrorIs(t, provider.CompleteDatasetBootstrap(ctx, ""), storagev1.ErrValidation)
			fresh, err := provider.BindDataset(ctx, storagev1.BindDatasetRequest{
				DatasetID: dataset, InstallationID: otherInstallation, RecordUID: bindingRecord,
				SwitchGeneration: 1, Mode: storagev1.BindFresh,
			})
			require.NoError(t, err, "the dataset is still new: a mismatched completion recorded nothing")
			require.Equal(t, otherInstallation, fresh.InstallationID)

			provider = newProvider(t)
			dataset = datasetID(t, provider)
			require.NoError(t, provider.CompleteDatasetBootstrap(ctx, dataset))
			require.NoError(t, provider.CompleteDatasetBootstrap(ctx, dataset), "completing again succeeds")
			_, err = provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindFresh, 1))
			requireBindingConflict(t, provider, dataset, err)
			requireUnbound(t, provider, dataset)
			_, err = provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindAdopt, 1))
			require.NoError(t, err)
			require.NoError(t, provider.CompleteDatasetBootstrap(ctx, dataset), "completion does not depend on the binding")
		})
		t.Run("migrated needs an imported dataset", func(t *testing.T) {
			provider := newProvider(t)
			info, err := provider.Describe(ctx)
			require.NoError(t, err)
			if info.DatasetOrigin == storagev1.DatasetOriginMigrated {
				t.Skip("the factory's dataset was imported")
			}
			_, err = provider.BindDataset(ctx, bindRequest(info.DatasetID, storagev1.BindMigrated, 1))
			requireBindingConflict(t, provider, info.DatasetID, err)
			requireUnbound(t, provider, info.DatasetID)
		})
		t.Run("rebind needs a dataset of the installation", func(t *testing.T) {
			provider := newProvider(t)
			dataset := datasetID(t, provider)
			_, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindRebind, 1))
			requireBindingConflict(t, provider, dataset, err)
			requireUnbound(t, provider, dataset)
		})
		t.Run("embedded binds an unclaimed dataset or is not served", func(t *testing.T) {
			provider := newProvider(t)
			dataset := datasetID(t, provider)
			populate(t, provider)
			bound, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindEmbedded, 1))
			if errors.Is(err, storagev1.ErrValidation) {
				requireUnbound(t, provider, dataset)
				return
			}
			require.NoError(t, err, "an embedded provider binds a dataset in any bootstrap state")
			requireStoredBinding(t, provider, dataset, bound)
		})
	})

	t.Run("a detached dataset refuses advances until its installation binds it again", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		advance := func(installation string, epoch int64) error {
			_, err := provider.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
				DatasetID: dataset, Epoch: mirroredEpoch(epoch, "owner"), InstallationID: installation,
			})
			return err
		}
		bound, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindFresh, 1))
		require.NoError(t, err)
		require.NoError(t, advance(bindingInstallation, 1))

		_, err = provider.DetachDatasetBinding(ctx, detachRequest(dataset, 1))
		requireBindingConflict(t, provider, dataset, err)
		before := time.Now().UTC()
		detached, err := provider.DetachDatasetBinding(ctx, detachRequest(dataset, 2))
		require.NoError(t, err)
		require.Equal(t, storagev1.DatasetBinding{
			InstallationID: bindingInstallation, RecordUID: bindingRecord, SwitchGeneration: 2,
			KeyFingerprint: bindingKey, State: storagev1.DatasetDetached, BoundAt: detached.BoundAt,
		}, detached, "detaching keeps the installation and key fingerprint")
		require.False(t, detached.BoundAt.Before(before.Add(-storagev1.MaxClockSkew)))
		require.Equal(t, time.UTC, detached.BoundAt.Location())
		requireStoredBinding(t, provider, dataset, detached)
		repeat, err := provider.DetachDatasetBinding(ctx, detachRequest(dataset, 2))
		require.NoError(t, err, "a repeated detach replays")
		require.Equal(t, detached, repeat)
		for _, request := range []storagev1.DetachDatasetRequest{
			detachRequest(dataset, 3),
			{DatasetID: dataset, InstallationID: bindingInstallation, RecordUID: otherRecord, SwitchGeneration: 2},
		} {
			_, err := provider.DetachDatasetBinding(ctx, request)
			requireBindingConflict(t, provider, dataset, err)
		}
		_, err = provider.UpdateBindingKeyFingerprint(ctx, storagev1.UpdateBindingKeyRequest{
			DatasetID: dataset, InstallationID: bindingInstallation, RecordUID: bindingRecord, SwitchGeneration: 2,
			PreviousKeyFingerprint: bindingKey, KeyFingerprint: rotatedKey,
		})
		requireBindingConflict(t, provider, dataset, err)

		for _, epoch := range []int64{1, 2} {
			err := advance(bindingInstallation, epoch)
			require.ErrorIs(t, err, storagev1.ErrDatasetDetached)
			require.ErrorIs(t, err, storagev1.ErrNotReady)
		}
		requireFenced(t, advance(otherInstallation, 2))
		stored, err := provider.GetControllerEpochMirror(ctx, controlEpochName)
		require.NoError(t, err)
		require.EqualValues(t, 1, stored.Epoch.Epoch, "a refused advance changed nothing")
		resolved, err := provider.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
			DatasetID: dataset, OperationID: "op-on-detached", Method: "SaveResult",
			RequestDigest: controlDigest("op-on-detached"), IssuedAt: time.Now().UTC(),
		})
		require.NoError(t, err, "resolution can only prevent a change, so it works on a detached dataset")
		require.Equal(t, storagev1.OperationAborted, resolved.State)

		for _, request := range []storagev1.BindDatasetRequest{
			bindRequest(dataset, storagev1.BindFresh, 3),
			bindRequest(dataset, storagev1.BindEmbedded, 2),
			bindRequest(dataset, storagev1.BindEmbedded, 3),
			bindRequest(dataset, storagev1.BindMigrated, 3),
			bindRequest(dataset, storagev1.BindAdopt, 2),
			bindRequest(dataset, storagev1.BindRebind, 2),
		} {
			requireBindRefused(t, provider, request)
		}
		for _, mode := range allBindModes {
			request := bindRequest(dataset, mode, 3)
			request.InstallationID = otherInstallation
			requireBindRefused(t, provider, request)
		}
		_, err = provider.DetachDatasetBinding(ctx, storagev1.DetachDatasetRequest{
			DatasetID: dataset, InstallationID: otherInstallation, RecordUID: bindingRecord, SwitchGeneration: 3,
		})
		requireBindingConflict(t, provider, dataset, err)
		requireStoredBinding(t, provider, dataset, detached)

		adopted, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindAdopt, 3))
		require.NoError(t, err, "the installation adopts its detached dataset at a higher generation")
		require.Equal(t, storagev1.DatasetBound, adopted.State)
		require.NoError(t, advance(bindingInstallation, 2))

		_, err = provider.DetachDatasetBinding(ctx, detachRequest(dataset, 4))
		require.NoError(t, err)
		rebound, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindRebind, 5))
		require.NoError(t, err, "a switch back rebinds the detached dataset")
		require.Equal(t, storagev1.DatasetBound, rebound.State)
		require.EqualValues(t, 5, rebound.SwitchGeneration)
		require.NoError(t, advance(bindingInstallation, 3))
		require.NotEqual(t, bound.SwitchGeneration, rebound.SwitchGeneration)
	})

	t.Run("the key fingerprint changes by compare-and-swap", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		unkeyed := bindRequest(dataset, storagev1.BindFresh, 1)
		unkeyed.KeyFingerprint = ""
		bound, err := provider.BindDataset(ctx, unkeyed)
		require.NoError(t, err)
		require.Empty(t, bound.KeyFingerprint)
		update := storagev1.UpdateBindingKeyRequest{
			DatasetID: dataset, InstallationID: bindingInstallation, RecordUID: bindingRecord, SwitchGeneration: 1,
			KeyFingerprint: bindingKey,
		}
		keyed, err := provider.UpdateBindingKeyFingerprint(ctx, update)
		require.NoError(t, err)
		require.Equal(t, bindingKey, keyed.KeyFingerprint)
		require.Equal(t, bound.BoundAt, keyed.BoundAt, "a key update leaves BoundAt")

		update.PreviousKeyFingerprint, update.KeyFingerprint = bindingKey, rotatedKey
		rotated, err := provider.UpdateBindingKeyFingerprint(ctx, update)
		require.NoError(t, err)
		require.Equal(t, rotatedKey, rotated.KeyFingerprint)
		repeat, err := provider.UpdateBindingKeyFingerprint(ctx, update)
		require.NoError(t, err, "a repeated update replays")
		require.Equal(t, rotated, repeat)
		requireStoredBinding(t, provider, dataset, rotated)

		for name, mutate := range map[string]func(*storagev1.UpdateBindingKeyRequest){
			"stale previous fingerprint": func(r *storagev1.UpdateBindingKeyRequest) { r.KeyFingerprint = "hmac-sha256:key-3" },
			"another record":             func(r *storagev1.UpdateBindingKeyRequest) { r.RecordUID = otherRecord },
			"another generation":         func(r *storagev1.UpdateBindingKeyRequest) { r.SwitchGeneration = 2 },
			"another installation":       func(r *storagev1.UpdateBindingKeyRequest) { r.InstallationID = otherInstallation },
		} {
			request := update
			mutate(&request)
			_, err := provider.UpdateBindingKeyFingerprint(ctx, request)
			require.Errorf(t, err, name)
			requireBindingConflict(t, provider, dataset, err)
		}
		requireStoredBinding(t, provider, dataset, rotated)
		fresh := newProvider(t)
		freshDataset := datasetID(t, fresh)
		update.DatasetID = freshDataset
		_, err = fresh.UpdateBindingKeyFingerprint(ctx, update)
		requireBindingConflict(t, fresh, freshDataset, err)
	})

	t.Run("the controller epoch advance records its installation", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		before := time.Now().UTC()
		_, err := provider.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
			DatasetID: dataset, Epoch: mirroredEpoch(1, "owner-a"), InstallationID: bindingInstallation,
		})
		require.NoError(t, err)
		recorded, err := provider.GetDatasetBinding(ctx, dataset)
		require.NoError(t, err)
		require.Equal(t, storagev1.DatasetBinding{
			InstallationID: bindingInstallation, State: storagev1.DatasetBound, BoundAt: recorded.BoundAt,
		}, recorded, "the advance records the installation without a record, at generation 0")
		if !recorded.BoundAt.IsZero() {
			require.False(t, recorded.BoundAt.Before(before.Add(-storagev1.MaxClockSkew)))
		}
		requireStoredBinding(t, provider, dataset, recorded)

		for _, mode := range []storagev1.BindMode{storagev1.BindAdopt, storagev1.BindRebind, storagev1.BindFresh} {
			request := bindRequest(dataset, mode, 1)
			request.InstallationID = otherInstallation
			_, err := provider.BindDataset(ctx, request)
			requireBindingConflict(t, provider, dataset, err)
		}
		_, err = provider.DetachDatasetBinding(ctx, storagev1.DetachDatasetRequest{
			DatasetID: dataset, InstallationID: otherInstallation, RecordUID: bindingRecord, SwitchGeneration: 1,
		})
		requireBindingConflict(t, provider, dataset, err)
		requireStoredBinding(t, provider, dataset, recorded)

		requireBindRefused(t, provider, bindRequest(dataset, storagev1.BindFresh, 1))
		requireStoredBinding(t, provider, dataset, recorded)
		bound, err := provider.BindDataset(ctx, bindRequest(dataset, storagev1.BindAdopt, 1))
		require.NoError(t, err, "the installation the advance recorded binds the dataset itself")
		require.Equal(t, bindingRecord, bound.RecordUID)
		require.EqualValues(t, 1, bound.SwitchGeneration)
		_, err = provider.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
			DatasetID: dataset, Epoch: mirroredEpoch(2, "owner-b"), InstallationID: bindingInstallation,
		})
		require.NoError(t, err)
		requireStoredBinding(t, provider, dataset, bound)

		advanced := newProvider(t)
		advancedDataset := datasetID(t, advanced)
		_, err = advanced.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
			DatasetID: advancedDataset, Epoch: mirroredEpoch(1, "owner-a"), InstallationID: bindingInstallation,
		})
		require.NoError(t, err)
		rebound, err := advanced.BindDataset(ctx, bindRequest(advancedDataset, storagev1.BindRebind, 1))
		require.NoError(t, err, "rebind moves the generation 0 binding the advance recorded")
		require.EqualValues(t, 1, rebound.SwitchGeneration)
		requireStoredBinding(t, advanced, advancedDataset, rebound)
	})

	t.Run("concurrent binders have one winner", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		const binders = 8
		results := make([]storagev1.DatasetBinding, binders)
		errs := make([]error, binders)
		var wg sync.WaitGroup
		for i := range binders {
			wg.Go(func() {
				request := bindRequest(dataset, storagev1.BindFresh, 1)
				request.InstallationID = fmt.Sprintf("installation-%d", i)
				results[i], errs[i] = provider.BindDataset(ctx, request)
			})
		}
		wg.Wait()
		winner := -1
		for i, err := range errs {
			if err == nil {
				require.Equal(t, -1, winner, "only one installation binds the dataset")
				winner = i
			}
		}
		require.NotEqual(t, -1, winner, "one installation binds the dataset")
		requireStoredBinding(t, provider, dataset, results[winner])
		for i, err := range errs {
			if i != winner {
				var conflict storagev1.BindingConflictError
				require.ErrorAs(t, err, &conflict)
				require.NotNil(t, conflict.Binding)
				require.Equal(t, results[winner], *conflict.Binding, "each loser sees the winner's binding")
			}
		}

		same := newProvider(t)
		sameDataset := datasetID(t, same)
		for i := range binders {
			wg.Go(func() { results[i], errs[i] = same.BindDataset(ctx, bindRequest(sameDataset, storagev1.BindFresh, 1)) })
		}
		wg.Wait()
		for i := range binders {
			require.NoError(t, errs[i], "a concurrent repeat replays the binding")
			require.Equal(t, results[0], results[i])
		}
	})

	t.Run("requests are validated and name the dataset", func(t *testing.T) {
		provider := newProvider(t)
		ctx := t.Context()
		dataset := datasetID(t, provider)
		const otherDataset = "00000000-0000-4000-8000-000000000000"
		for name, mutate := range map[string]func(*storagev1.BindDatasetRequest){
			"missing dataset":        func(r *storagev1.BindDatasetRequest) { r.DatasetID = "" },
			"missing installation":   func(r *storagev1.BindDatasetRequest) { r.InstallationID = "" },
			"missing record":         func(r *storagev1.BindDatasetRequest) { r.RecordUID = "" },
			"generation 0":           func(r *storagev1.BindDatasetRequest) { r.SwitchGeneration = 0 },
			"undefined mode":         func(r *storagev1.BindDatasetRequest) { r.Mode = "switch" },
			"padded key fingerprint": func(r *storagev1.BindDatasetRequest) { r.KeyFingerprint = " key" },
		} {
			request := bindRequest(dataset, storagev1.BindFresh, 1)
			mutate(&request)
			_, err := provider.BindDataset(ctx, request)
			require.ErrorIsf(t, err, storagev1.ErrValidation, name)
		}
		_, err := provider.BindDataset(ctx, bindRequest(otherDataset, storagev1.BindFresh, 1))
		require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
		_, err = provider.DetachDatasetBinding(ctx, detachRequest(dataset, 0))
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = provider.DetachDatasetBinding(ctx, detachRequest(otherDataset, 1))
		require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
		_, err = provider.UpdateBindingKeyFingerprint(ctx, storagev1.UpdateBindingKeyRequest{
			DatasetID: dataset, InstallationID: bindingInstallation, RecordUID: bindingRecord, SwitchGeneration: 1,
		})
		require.ErrorIs(t, err, storagev1.ErrValidation, "a new key fingerprint is required")
		_, err = provider.UpdateBindingKeyFingerprint(ctx, storagev1.UpdateBindingKeyRequest{
			DatasetID: otherDataset, InstallationID: bindingInstallation, RecordUID: bindingRecord, SwitchGeneration: 1,
			KeyFingerprint: bindingKey,
		})
		require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
		_, err = provider.GetDatasetBinding(ctx, "")
		require.ErrorIs(t, err, storagev1.ErrValidation)
		_, err = provider.GetDatasetBinding(ctx, otherDataset)
		require.ErrorIs(t, err, storagev1.ErrDatasetMismatch)
		requireUnbound(t, provider, dataset)
	})
}
