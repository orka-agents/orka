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

// envelope_gen.go holds the generated write methods of envelopedState and
// envelopedArtifacts; `make generate` writes it with the storage client's
// decorators, from the same provider interfaces and method catalog.

// Option configures RunState and RunArtifacts.
type Option func(*suiteOptions)

type suiteOptions struct {
	providerAttachesEnvelopes bool
}

// ProviderAttachesEnvelopes runs the suite against a provider that attaches
// its own write envelopes, such as a client that wraps a provider. Without
// it, the suite attaches an envelope to every write it sends without one, as
// the contract requires of callers, so a provider that refuses writes without
// an envelope passes the suite.
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

// writeCall describes one call of a contract write method the suite sends.
type writeCall struct {
	method   string
	contract string
	// args are the arguments after the context, which the request digest
	// covers.
	args []any
	// fence is the controller epoch fence the method takes as a parameter,
	// or nil; the envelope carries it.
	fence *storagev1.ControllerEpochFence
}

// envelopedState attaches a suite envelope to every state.v1 write sent
// without one. Its write methods are generated; reads and administrative
// methods are the provider's own.
type envelopedState struct {
	storagev1.StateProvider
	ops *suiteEnvelopes
}

// envelopedArtifacts attaches a suite envelope to every artifacts.v1 write
// sent without one, as envelopedState does.
type envelopedArtifacts struct {
	storagev1.ArtifactProvider
	ops *suiteEnvelopes
}

// suiteEnvelopes builds the envelopes the suite attaches: the provider's
// dataset, the controller epoch the dataset holds when the write is sent, a
// new operation ID, the request digest, the current time and the next
// sequence. A dataset that holds no epoch is given suiteFence's epoch first,
// so a case that writes without managing epochs runs as the current
// controller would.
type suiteEnvelopes struct {
	describer storagev1.ProviderDescriber
	epochs    storagev1.ControllerEpochAdvancer
	// establish gives a dataset that holds no epoch suiteFence's epoch.
	establish func(ctx context.Context, datasetID string) error
	// advanced records that establish advanced the dataset, which records
	// suiteInstance on it at generation 0 without a binding record.
	advanced atomic.Bool

	mu        sync.Mutex
	datasetID string
}

func wrapState(provider storagev1.StateProvider) storagev1.StateProvider {
	ops := &suiteEnvelopes{describer: provider, epochs: provider}
	// SyncControllerEpochMirror binds no installation, so a case that
	// manages epochs itself can still synchronize or advance the dataset.
	ops.establish = func(ctx context.Context, _ string) error {
		return provider.SyncControllerEpochMirror(ctx, mirroredEpoch(suiteFence.Epoch, suiteFence.HolderID))
	}
	return envelopedState{StateProvider: provider, ops: ops}
}

func wrapArtifacts(provider storagev1.ArtifactProvider) storagev1.ArtifactProvider {
	ops := &suiteEnvelopes{describer: provider, epochs: provider}
	if mirror, ok := provider.(storagev1.ControllerEpochMirror); ok {
		// A provider that also serves the state mirror gets the epoch as
		// wrapState gives it, binding no installation, so the dataset
		// binding cases still find the dataset unbound.
		ops.establish = func(ctx context.Context, _ string) error {
			return mirror.SyncControllerEpochMirror(ctx, mirroredEpoch(suiteFence.Epoch, suiteFence.HolderID))
		}
		return envelopedArtifacts{ArtifactProvider: provider, ops: ops}
	}
	// Otherwise the advance repeats exactly what the write catalog's own
	// advance stores, so the two never conflict. It records the suite's
	// installation on an unbound dataset, as every advance does, and the
	// dataset binding cases account for it (see suiteAdvanced).
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

// suiteAdvanced reports whether the suite gave provider's dataset its first
// controller epoch through AdvanceControllerEpoch, which records
// suiteInstance on the dataset at generation 0 without a binding record. It
// does so only for an artifacts-only provider, which serves no state mirror.
func suiteAdvanced(provider any) bool {
	wrapped, ok := provider.(envelopedArtifacts)
	return ok && wrapped.ops.advanced.Load()
}

// stateFactory and artifactFactory wrap a factory's providers unless the
// provider attaches its own envelopes.
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

// run sends one write. A context that already carries an envelope belongs to
// a case that tests envelopes itself, and the write runs with it unchanged.
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

// current returns the dataset ID and the fence of the epoch the dataset
// holds, giving it suiteFence's epoch first when it holds none. Only that
// first step is serialized, so concurrent cases still write concurrently.
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
