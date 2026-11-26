package storageclient

import (
	"context"
	"errors"
	"maps"
	"reflect"
	"sync/atomic"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

type Embedded any

type Options struct {
	State     storagev1.StateProvider
	Artifacts storagev1.ArtifactProvider
	Embedded  Embedded
	Fences    FenceSource
}

type Client struct {
	storagev1.StateProvider
	storagev1.ArtifactStore
	Embedded

	custody               atomic.Pointer[keyCustody]
	nativeSessionMaxBytes atomic.Int64

	state     storagev1.StateProvider
	artifacts storagev1.ArtifactProvider
	datasets  map[string]Dataset
	ops       *operations
}

var (
	_ store.MemoryProposalStore  = (*Client)(nil)
	_ store.TaskDataAccessStore  = (*Client)(nil)
	_ store.SecurityStore        = (*Client)(nil)
	_ store.ConnectorGrantReader = (*Client)(nil)
	_ storagev1.StateProvider    = (*Client)(nil)
)

var ErrSplitEmbeddedStore = errors.New("embedded storage families require state and artifacts on the same embedded store")

func New(options Options) (*Client, error) {
	if isNilProvider(options.State) || isNilProvider(options.Artifacts) || isNilProvider(options.Embedded) {
		return nil, store.ValidationErrorf("storage client requires state, artifact and embedded stores")
	}
	if !sameStore(options.State, options.Embedded) || !sameStore(options.Artifacts, options.Embedded) {
		return nil, ErrSplitEmbeddedStore
	}
	ops := newOperations(map[string]storagev1.OperationReceiptStore{
		storagev1.StateContract:     options.State,
		storagev1.ArtifactsContract: options.Artifacts,
	})
	if options.Fences != nil {
		if err := ops.bindFences(options.Fences); err != nil {
			return nil, err
		}
	}
	state := envelopedState{StateProvider: options.State, ops: ops}
	artifacts := envelopedArtifacts{ArtifactProvider: options.Artifacts, ops: ops}
	return &Client{
		StateProvider: state,
		ArtifactStore: artifacts,
		Embedded:      options.Embedded,
		state:         options.State,
		artifacts:     options.Artifacts,
		datasets: map[string]Dataset{
			storagev1.StateContract:     state,
			storagev1.ArtifactsContract: artifacts,
		},
		ops: ops,
	}, nil
}

func (c *Client) BindFences(source FenceSource) error {
	return c.ops.bindFences(source)
}

type Dataset interface {
	storagev1.ProviderDescriber
	storagev1.ControllerEpochAdvancer
	storagev1.WriteWatermarkStore
}

func (c *Client) Datasets() map[string]Dataset {
	return maps.Clone(c.datasets)
}

func (c *Client) UnresolvedOperations() int {
	return c.ops.unresolvedCount()
}

type EmbeddedStore interface {
	storagev1.StateProvider
	storagev1.ArtifactProvider
	Embedded
}

func NewEmbedded(backend EmbeddedStore) (*Client, error) {
	if isNilProvider(backend) {
		return nil, store.ValidationErrorf("embedded storage backend is required")
	}
	return New(Options{State: backend, Artifacts: backend, Embedded: backend})
}

func (c *Client) HealthCheck(ctx context.Context) error {
	return c.state.HealthCheck(ctx)
}

func isNilProvider(provider any) bool {
	if provider == nil {
		return true
	}
	value := reflect.ValueOf(provider)
	switch value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.Interface:
		return value.IsNil()
	default:
		return false
	}
}

func sameStore(a, b any) bool {
	valueA, valueB := reflect.ValueOf(a), reflect.ValueOf(b)
	if !valueA.IsValid() || !valueB.IsValid() || valueA.Type() != valueB.Type() {
		return false
	}
	if !valueA.Comparable() || !valueB.Comparable() {
		return false
	}
	return a == b
}
