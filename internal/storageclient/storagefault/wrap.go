package storagefault

import (
	"context"

	"github.com/orka-agents/orka/internal/storageclient"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// stateProvider wraps a state.v1 provider. Its other methods are generated.
type stateProvider struct {
	inner    storagev1.StateProvider
	injector *Injector
	contract string
}

// artifactProvider wraps an artifacts.v1 provider. Its other methods are
// generated.
type artifactProvider struct {
	inner    storagev1.ArtifactProvider
	injector *Injector
	contract string
}

// embeddedStore wraps an embedded store that serves both contracts and the
// embedded families. Its state.v1 methods, Describe and HealthCheck come from
// stateProvider; its artifact methods and embedded families are generated.
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

// WrapState returns provider with this injector's faults applied to every
// call, each made through storagev1.StateContract.
func (i *Injector) WrapState(provider storagev1.StateProvider) storagev1.StateProvider {
	return &stateProvider{inner: provider, injector: i, contract: storagev1.StateContract}
}

// WrapArtifacts returns provider with this injector's faults applied to every
// call, each made through storagev1.ArtifactsContract.
func (i *Injector) WrapArtifacts(provider storagev1.ArtifactProvider) storagev1.ArtifactProvider {
	return &artifactProvider{inner: provider, injector: i, contract: storagev1.ArtifactsContract}
}

// WrapEmbedded returns store with this injector's faults applied to every
// call. Artifact methods are made through storagev1.ArtifactsContract and all
// others, including Describe, HealthCheck and the embedded families, through
// storagev1.StateContract, because the storage client probes the store
// through its state binding. The storage client accepts the result as one
// embedded store, so the families it keeps on one store stay together.
//
// To probe the artifacts binding of the same store, as a binding check does,
// use WrapArtifacts on the same store: it shares this injector, so
// Down(storagev1.ArtifactsContract) and Describe changes for artifacts apply
// to its Describe and HealthCheck.
func (i *Injector) WrapEmbedded(store storageclient.EmbeddedStore) storageclient.EmbeddedStore {
	return &embeddedStore{
		stateProvider: &stateProvider{inner: store, injector: i, contract: storagev1.StateContract},
		store:         store,
		injector:      i,
	}
}

// NewEmbeddedClient wraps store with a new injector and returns a storage
// client over the wrapper, as cmd/main.go builds one over the embedded store,
// together with the injector that controls it.
func NewEmbeddedClient(store storageclient.EmbeddedStore) (*storageclient.Client, *Injector, error) {
	injector := New()
	client, err := storageclient.NewEmbedded(injector.WrapEmbedded(store))
	if err != nil {
		return nil, nil, err
	}
	return client, injector, nil
}

// Describe reports what the provider reports, with this injector's faults and
// Describe changes applied.
func (p *stateProvider) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	return p.injector.describe(ctx, p.contract, p.inner.Describe)
}

// Describe reports what the provider reports, with this injector's faults and
// Describe changes applied.
func (p *artifactProvider) Describe(ctx context.Context) (storagev1.ProviderInfo, error) {
	return p.injector.describe(ctx, p.contract, p.inner.Describe)
}
