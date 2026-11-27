package storagefault

import (
	"context"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storageclienttest"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

type stateProvider struct {
	inner    storagev1.StateProvider
	injector *Injector
	contract string
}

type artifactProvider struct {
	inner    storagev1.ArtifactProvider
	injector *Injector
	contract string
}

type embeddedStore struct {
	*stateProvider
	store    storageclient.EmbeddedStore
	injector *Injector
}

var (
	_ storagev1.StateProvider     = (*stateProvider)(nil)
	_ storagev1.ArtifactProvider  = (*artifactProvider)(nil)
	_ storageclient.EmbeddedStore = (*embeddedStore)(nil)
)

func (i *Injector) WrapState(provider storagev1.StateProvider) storagev1.StateProvider {
	return &stateProvider{inner: provider, injector: i, contract: storagev1.StateContract}
}

func (i *Injector) WrapArtifacts(provider storagev1.ArtifactProvider) storagev1.ArtifactProvider {
	return &artifactProvider{inner: provider, injector: i, contract: storagev1.ArtifactsContract}
}

func (i *Injector) WrapEmbedded(store storageclient.EmbeddedStore) storageclient.EmbeddedStore {
	return &embeddedStore{
		stateProvider: &stateProvider{inner: store, injector: i, contract: storagev1.StateContract},
		store:         store,
		injector:      i,
	}
}

func NewEmbeddedClient(store storageclient.EmbeddedStore) (*storageclient.Client, *Injector, error) {
	if _, err := storageclient.NewEmbedded(store); err != nil {
		return nil, nil, err
	}
	injector := New()
	wrapped := injector.WrapEmbedded(store)
	client, err := storageclient.New(storageclient.Options{
		State: wrapped, Artifacts: wrapped, Embedded: wrapped,
		Fences: &storageclienttest.FollowDataset{Dataset: store},
	})
	if err != nil {
		return nil, nil, err
	}
	return client, injector, nil
}

func (p *stateProvider) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	return p.injector.describe(ctx, p.contract, p.inner.Describe)
}

func (p *artifactProvider) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	return p.injector.describe(ctx, p.contract, p.inner.Describe)
}
