package wire_test

import (
	"context"
	"reflect"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
)

type recordingProvider struct {
	storagev1.StateProvider

	mu      sync.Mutex
	calls   []recordedCall
	results map[string][]any
	written map[string][]any
	errs    map[string]error
	panics  map[string]any
	block   map[string]chan struct{}
	entered chan string
}

type recordedCall struct {
	method      string
	args        []any
	envelope    storagev1.WriteEnvelope
	hasEnvelope bool
	fence       storagev1.TaskDataFence
	hasFence    bool
}

const testDataset = "dataset-test"

var boundConfig = wire.ClientConfig{Dataset: testDataset}

func serveBoth(provider *recordingProvider) wire.ServerConfig {
	return wire.ServerConfig{State: provider, Artifacts: provider}
}

func describeDefault() storagev1.ProviderInfo {
	return storagev1.ProviderInfo{
		ContractVersion: storagev1.ContractVersion,
		Contracts:       []string{storagev1.StateContract, storagev1.ArtifactsContract},
		DatasetID:       testDataset,
		DatasetOrigin:   storagev1.DatasetOriginNew,
		Limits:          storagev1.ProviderLimits{MaxArtifactBytes: storagev1.MaxArtifactBytes},
		Durability:      storagev1.DurabilityLocalFsync,
	}
}

func newRecordingProvider() *recordingProvider {
	return &recordingProvider{
		results: map[string][]any{"Describe": {describeDefault()}},
		written: map[string][]any{},
		errs:    map[string]error{},
		panics:  map[string]any{},
		block:   map[string]chan struct{}{},
		entered: make(chan string, 16),
	}
}

func (p *recordingProvider) called(ctx context.Context, method string, args ...any) ([]any, error) {
	envelope, hasEnvelope := storagev1.WriteEnvelopeFrom(ctx)
	fence, hasFence := storagev1.TaskDataFenceFrom(ctx)
	recorded := make([]any, len(args))
	for index, arg := range args {
		recorded[index] = snapshot(arg)
	}
	p.mu.Lock()
	p.calls = append(p.calls, recordedCall{
		method: method, args: recorded, envelope: envelope, hasEnvelope: hasEnvelope, fence: fence, hasFence: hasFence,
	})
	results, err, panicValue, block := p.results[method], p.errs[method], p.panics[method], p.block[method]
	p.mu.Unlock()
	select {
	case p.entered <- method:
	default:
	}
	if block != nil {
		<-block
	}
	if panicValue != nil {
		panic(panicValue)
	}
	return results, err
}

func (p *recordingProvider) waitEntered(t *testing.T, method string) {
	t.Helper()
	timeout := time.After(10 * time.Second)
	for {
		select {
		case entered := <-p.entered:
			if entered == method {
				return
			}
		case <-timeout:
			t.Fatalf("no call to %s reached the provider", method)
		}
	}
}

func (p *recordingProvider) set(method string, results []any, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.results[method], p.errs[method] = results, err
}

func (p *recordingProvider) setWritten(method string, written []any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.written[method] = written
}

func (p *recordingProvider) callsTo(method string) []recordedCall {
	p.mu.Lock()
	defer p.mu.Unlock()
	var calls []recordedCall
	for _, call := range p.calls {
		if call.method == method {
			calls = append(calls, call)
		}
	}
	return calls
}

func (p *recordingProvider) writtenFor(method string) []any {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.written[method]
}

func writeBackInto[T any](written []any, index int, target *T) {
	if target == nil || index >= len(written) {
		return
	}
	if value, ok := written[index].(T); ok {
		*target = value
	}
}

func snapshot(arg any) any {
	value := reflect.ValueOf(arg)
	if !value.IsValid() || value.Kind() != reflect.Pointer || value.IsNil() {
		return arg
	}
	copied := reflect.New(value.Type().Elem())
	copied.Elem().Set(value.Elem())
	return copied.Interface()
}

func resultAs[T any](results []any, index int) T {
	var zero T
	if index >= len(results) {
		return zero
	}
	value, _ := results[index].(T)
	return value
}
