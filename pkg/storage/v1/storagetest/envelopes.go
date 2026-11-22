package storagetest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

type Option func(*suiteOptions)

type suiteOptions struct {
	providerAttachesEnvelopes bool
}

func ProviderAttachesEnvelopes() Option {
	return func(o *suiteOptions) { o.providerAttachesEnvelopes = true }
}

func collectOptions(options []Option) suiteOptions {
	var collected suiteOptions
	for _, option := range options {
		option(&collected)
	}
	return collected
}

type writeCall struct {
	method   string
	contract string
	args     []any
	fence    *storagev1.ControllerEpochFence
}

type envelopedState struct {
	storagev1.StateProvider
	ops *suiteEnvelopes
}

type envelopedArtifacts struct {
	storagev1.ArtifactProvider
	ops *suiteEnvelopes
}

type stateWithArtifacts struct {
	storagev1.StateProvider
	storagev1.ArtifactStore
}

type suiteEnvelopes struct {
	describer storagev1.ProviderDescriber
	epochs    storagev1.ControllerEpochAdvancer
	establish func(ctx context.Context, datasetID string) error
	advanced  atomic.Bool

	mu        sync.Mutex
	datasetID string
}

func wrapState(provider storagev1.StateProvider) storagev1.StateProvider {
	ops := &suiteEnvelopes{describer: provider, epochs: provider}
	ops.establish = func(ctx context.Context, _ string) error {
		return provider.SyncControllerEpochMirror(ctx, mirroredEpoch(suiteFence.Epoch, suiteFence.HolderID))
	}
	state := envelopedState{StateProvider: provider, ops: ops}
	if artifacts, ok := provider.(storagev1.ArtifactStore); ok {
		combined := stateWithArtifacts{StateProvider: provider, ArtifactStore: artifacts}
		return stateWithArtifacts{
			StateProvider: state,
			ArtifactStore: envelopedArtifacts{ArtifactProvider: combined, ops: ops},
		}
	}
	return state
}

func wrapArtifacts(provider storagev1.ArtifactProvider) storagev1.ArtifactProvider {
	ops := &suiteEnvelopes{describer: provider, epochs: provider}
	if mirror, ok := provider.(storagev1.ControllerEpochMirror); ok {
		ops.establish = func(ctx context.Context, _ string) error {
			return mirror.SyncControllerEpochMirror(ctx, mirroredEpoch(suiteFence.Epoch, suiteFence.HolderID))
		}
		return envelopedArtifacts{ArtifactProvider: provider, ops: ops}
	}
	ops.establish = func(ctx context.Context, datasetID string) error {
		_, err := provider.AdvanceControllerEpoch(ctx, storagev1.AdvanceControllerEpochRequest{
			DatasetID: datasetID, Epoch: mirroredEpoch(suiteFence.Epoch, suiteFence.HolderID), InstallationID: suiteInstance,
		})
		if err == nil {
			ops.advanced.Store(true)
		}
		return err
	}
	return envelopedArtifacts{ArtifactProvider: provider, ops: ops}
}

func suiteAdvanced(provider any) bool {
	wrapped, ok := provider.(envelopedArtifacts)
	return ok && wrapped.ops.advanced.Load()
}

func stateFactory(newProvider StateFactory, options suiteOptions) StateFactory {
	if options.providerAttachesEnvelopes {
		return newProvider
	}
	return func(t *testing.T) storagev1.StateProvider { return wrapState(newProvider(t)) }
}

func artifactFactory(newProvider ArtifactFactory, options suiteOptions) ArtifactFactory {
	if options.providerAttachesEnvelopes {
		return newProvider
	}
	return func(t *testing.T) storagev1.ArtifactProvider { return wrapArtifacts(newProvider(t)) }
}

func (o *suiteEnvelopes) run(ctx context.Context, call writeCall, attempt func(context.Context) error) error {
	if _, ok := storagev1.WriteEnvelopeFrom(ctx); ok {
		return attempt(ctx)
	}
	datasetID, fence, err := o.current(ctx)
	if err != nil {
		return fmt.Errorf("storagetest: envelope for %s: %w", call.method, err)
	}
	if call.fence != nil {
		if fence, err = storagev1.NormalizeEpochFence(*call.fence); err != nil {
			return err
		}
	}
	digest, err := storagev1.RequestDigest(call.method, call.args...)
	if err != nil {
		return err
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		return err
	}
	return attempt(storagev1.WithWriteEnvelope(ctx, storagev1.WriteEnvelope{
		DatasetID: datasetID, Fence: fence, OperationID: "suite-" + hex.EncodeToString(id[:]),
		RequestDigest: digest, IssuedAt: time.Now().UTC(), Sequence: envelopeSequence.Add(1),
	}))
}

func (o *suiteEnvelopes) current(ctx context.Context) (string, storagev1.ControllerEpochFence, error) {
	o.mu.Lock()
	datasetID := o.datasetID
	o.mu.Unlock()
	if datasetID == "" {
		info, err := o.describer.Describe(ctx)
		if err != nil {
			return "", storagev1.ControllerEpochFence{}, err
		}
		datasetID = info.DatasetID
		o.mu.Lock()
		o.datasetID = datasetID
		o.mu.Unlock()
	}
	state, err := o.epochs.GetControllerEpochMirror(ctx, controlEpochName)
	if errors.Is(err, storagev1.ErrNotFound) {
		state, err = o.establishOnce(ctx, datasetID)
	}
	if err != nil {
		return "", storagev1.ControllerEpochFence{}, err
	}
	return datasetID, storagev1.ControllerEpochFence{
		Name: state.Epoch.Name, Epoch: state.Epoch.Epoch, HolderID: state.Epoch.HolderID,
	}, nil
}

func (o *suiteEnvelopes) establishOnce(ctx context.Context, datasetID string) (storagev1.ControllerEpochMirrorState, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	state, err := o.epochs.GetControllerEpochMirror(ctx, controlEpochName)
	if !errors.Is(err, storagev1.ErrNotFound) {
		return state, err
	}
	if err := o.establish(ctx, datasetID); err != nil {
		return storagev1.ControllerEpochMirrorState{}, err
	}
	return o.epochs.GetControllerEpochMirror(ctx, controlEpochName)
}
