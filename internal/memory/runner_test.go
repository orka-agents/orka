package memory

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/store"
)

type runnerBindingStore struct {
	store.GovernedMemoryStore
	bindings []store.MemoryBackendBinding
}

func (s *runnerBindingStore) ListMemoryBackendBindings(
	context.Context,
	store.MemoryBackendBindingFilter,
) ([]store.MemoryBackendBinding, error) {
	return append([]store.MemoryBackendBinding(nil), s.bindings...), nil
}

type runnerResolver struct {
	resolveLocal func(context.Context, string) (*ResolvedAuthority, error)
}

func (r runnerResolver) Resolve(context.Context, string) (*ResolvedAuthority, error) {
	return nil, errors.New("unexpected remote authority resolution")
}

func (r runnerResolver) ResolveLocal(ctx context.Context, namespace string) (*ResolvedAuthority, error) {
	return r.resolveLocal(ctx, namespace)
}

func TestRunnerPassDispatchesNamespacesConcurrently(t *testing.T) {
	governed := &runnerBindingStore{bindings: runnerTestBindings("slow", "fast")}
	slowRelease := make(chan struct{})
	started := make(chan string, len(governed.bindings))
	resolver := runnerResolver{resolveLocal: func(ctx context.Context, namespace string) (*ResolvedAuthority, error) {
		started <- namespace
		if namespace == "slow" {
			select {
			case <-slowRelease:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		return nil, store.ErrNotReady
	}}
	runner := &Runner{
		Dispatcher: &Dispatcher{
			Store: governed, Resolver: resolver, GlobalConcurrency: 2, NamespaceConcurrency: 1,
		},
		Store: governed,
	}

	done := make(chan error, 1)
	go func() {
		done <- runner.runPass(context.Background())
	}()

	seen := make(map[string]bool)
	timer := time.NewTimer(time.Second)
	defer timer.Stop()
	for len(seen) < 2 {
		select {
		case namespace := <-started:
			seen[namespace] = true
		case <-timer.C:
			close(slowRelease)
			<-done
			t.Fatalf("started namespaces = %v, want fast dispatch to start while slow dispatch is blocked", seen)
		}
	}
	close(slowRelease)
	if err := <-done; err != nil {
		t.Fatalf("runPass() error = %v", err)
	}
}

func TestRunnerPassBoundsWorkersAndDispatchesEachNamespaceOnce(t *testing.T) {
	governed := &runnerBindingStore{bindings: runnerTestBindings(
		"team-a", "team-b", "team-c", "team-d", "team-a",
	)}
	release := make(chan struct{})
	started := make(chan string, len(governed.bindings))
	var mu sync.Mutex
	active := 0
	maxActive := 0
	calls := make(map[string]int)
	resolver := runnerResolver{resolveLocal: func(ctx context.Context, namespace string) (*ResolvedAuthority, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		calls[namespace]++
		mu.Unlock()

		started <- namespace
		select {
		case <-release:
		case <-ctx.Done():
			return nil, ctx.Err()
		}

		mu.Lock()
		active--
		mu.Unlock()
		return nil, store.ErrNotReady
	}}
	runner := &Runner{
		Dispatcher: &Dispatcher{
			Store: governed, Resolver: resolver, GlobalConcurrency: 2, NamespaceConcurrency: 1,
		},
		Store: governed,
	}

	done := make(chan error, 1)
	go func() {
		done <- runner.runPass(context.Background())
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			close(release)
			<-done
			t.Fatal("runPass() did not use the configured global concurrency")
		}
	}
	select {
	case namespace := <-started:
		close(release)
		<-done
		t.Fatalf("namespace %q started before a configured worker was available", namespace)
	case <-time.After(100 * time.Millisecond):
	}

	close(release)
	if err := <-done; err != nil {
		t.Fatalf("runPass() error = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if maxActive != 2 {
		t.Fatalf("maximum concurrent dispatches = %d, want 2", maxActive)
	}
	for _, namespace := range []string{"team-a", "team-b", "team-c", "team-d"} {
		if calls[namespace] != 1 {
			t.Errorf("dispatch calls for %q = %d, want 1", namespace, calls[namespace])
		}
	}
}

func TestRunnerPassCancellationStopsWorkers(t *testing.T) {
	governed := &runnerBindingStore{bindings: runnerTestBindings(
		"team-a", "team-b", "team-c", "team-d",
	)}
	started := make(chan struct{}, 2)
	var mu sync.Mutex
	active := 0
	resolver := runnerResolver{resolveLocal: func(ctx context.Context, _ string) (*ResolvedAuthority, error) {
		mu.Lock()
		active++
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()

		started <- struct{}{}
		<-ctx.Done()
		return nil, ctx.Err()
	}}
	runner := &Runner{
		Dispatcher: &Dispatcher{
			Store: governed, Resolver: resolver, GlobalConcurrency: 2, NamespaceConcurrency: 1,
		},
		Store: governed,
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runner.runPass(ctx)
	}()

	for range 2 {
		select {
		case <-started:
		case <-time.After(time.Second):
			cancel()
			<-done
			t.Fatal("runPass() did not start the configured workers")
		}
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runPass() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("runPass() did not stop workers after cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if active != 0 {
		t.Fatalf("active dispatch workers = %d, want 0 after runPass() returns", active)
	}
}

func runnerTestBindings(namespaces ...string) []store.MemoryBackendBinding {
	bindings := make([]store.MemoryBackendBinding, 0, len(namespaces))
	for index, namespace := range namespaces {
		bindings = append(bindings, store.MemoryBackendBinding{
			Namespace: namespace, NamespaceUID: fmt.Sprintf("namespace-%d", index),
			Mode: store.MemoryBackendModeRemote, State: store.MemoryBackendBindingAccepting,
		})
	}
	return bindings
}
