package wire_test

import (
	"context"
	"sync"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// recordingProvider records every call to a method with a wire RPC and
// answers with preset results, errors or panics. zz_generated.recorder_test.go
// implements the methods; families without RPCs stay unimplemented.
type recordingProvider struct {
	storagev1.StateProvider

	mu      sync.Mutex
	calls   []recordedCall
	results map[string][]any
	errs    map[string]error
	panics  map[string]any
	// block holds a method's call until the channel closes; entered then
	// receives the method's name when a call arrives.
	block   map[string]chan struct{}
	entered chan string
}

type recordedCall struct {
	method      string
	args        []any
	envelope    storagev1.WriteEnvelope
	hasEnvelope bool
}

func newRecordingProvider() *recordingProvider {
	return &recordingProvider{
		results: map[string][]any{},
		errs:    map[string]error{},
		panics:  map[string]any{},
		block:   map[string]chan struct{}{},
		entered: make(chan string, 16),
	}
}

func (p *recordingProvider) called(ctx context.Context, method string, args ...any) ([]any, error) {
	envelope, hasEnvelope := storagev1.WriteEnvelopeFrom(ctx)
	p.mu.Lock()
	p.calls = append(p.calls, recordedCall{method: method, args: args, envelope: envelope, hasEnvelope: hasEnvelope})
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

func (p *recordingProvider) set(method string, results []any, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.results[method], p.errs[method] = results, err
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

func resultAs[T any](results []any, index int) T {
	var zero T
	if index >= len(results) {
		return zero
	}
	value, _ := results[index].(T)
	return value
}
