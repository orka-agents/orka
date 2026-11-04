package storagefault

import (
	"context"
	"fmt"
	"sync"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Action is what a call that matches a fault does. The actions are [Fail],
// [Delay], [LoseResponse] and [Hang].
type Action interface {
	// apply runs one matching call of method. next reaches the wrapped
	// provider and is called at most once. apply reports whether its outcome
	// replaced the provider's, in which case err is the injected outcome.
	apply(ctx context.Context, method string, next func(context.Context) error) (injected bool, err error)
}

// Fail returns err without reaching the provider, so nothing is committed.
// Use storagev1.ErrUnavailable for an unreachable store,
// storagev1.ErrUnauthenticated for rejected credentials and
// storagev1.ErrIncompatible for an incompatible provider. A dataset that is
// migrating refuses only writes with storagev1.ErrNotReady, so match writes
// (Match.Kinds) for that.
func Fail(err error) Action {
	if err == nil {
		panic("storagefault: Fail needs an error")
	}
	return failAction{err: err}
}

type failAction struct{ err error }

func (a failAction) apply(context.Context, string, func(context.Context) error) (bool, error) {
	return true, a.err
}

// Delay waits for d before the call reaches the provider. A call whose
// context has ended when the delay is over returns storagev1.ErrUnavailable,
// wrapping the context's error, without reaching the provider.
func Delay(d time.Duration) Action {
	return delayAction{d: d}
}

type delayAction struct{ d time.Duration }

func (a delayAction) apply(ctx context.Context, _ string, next func(context.Context) error) (bool, error) {
	timer := time.NewTimer(a.d)
	defer timer.Stop()
	select {
	case <-timer.C:
		// When the delay and the context end together, select may pick
		// either; the context's end still wins.
		if ctx.Err() != nil {
			return true, abandoned(ctx)
		}
		return false, next(ctx)
	case <-ctx.Done():
		return true, abandoned(ctx)
	}
}

// LoseResponse lets the call reach the provider and then loses its response,
// as when a connection fails after the request was sent. A read returns
// storagev1.ErrUnavailable, because it changed nothing. Any other call,
// including a call outside the method catalog, returns
// storagev1.ErrOutcomeUnknown whatever the provider did: a write it
// committed stays committed.
func LoseResponse() Action {
	return loseResponseAction{}
}

type loseResponseAction struct{}

func (loseResponseAction) apply(ctx context.Context, method string, next func(context.Context) error) (bool, error) {
	_ = next(ctx)
	if info, ok := storagev1.LookupMethod(method); ok && info.Kind == storagev1.MethodRead {
		return true, fmt.Errorf("%w: the response to a read was lost", storagev1.ErrUnavailable)
	}
	return true, fmt.Errorf("%w: the response was lost after the request was sent", storagev1.ErrOutcomeUnknown)
}

// abandoned is the outcome of a call whose context ended before it reached the
// provider: nothing was committed.
func abandoned(ctx context.Context) error {
	return fmt.Errorf("%w: %w", storagev1.ErrUnavailable, ctx.Err())
}

// Hang holds matching calls before they reach the provider until Release, as
// a provider that stopped answering does. A held call whose context ends
// returns storagev1.ErrUnavailable, wrapping the context's error, without
// reaching the provider. After Release, held calls whose context has not
// ended proceed, and later calls pass as if there were no fault. Create one
// with NewHang; it is safe for concurrent use.
type Hang struct {
	mu       sync.Mutex
	released bool
	release  chan struct{}
	held     int
	// changed is closed and replaced whenever held changes.
	changed chan struct{}
}

// NewHang returns a hang that holds calls until Release.
func NewHang() *Hang {
	return &Hang{release: make(chan struct{}), changed: make(chan struct{})}
}

func (h *Hang) apply(ctx context.Context, _ string, next func(context.Context) error) (bool, error) {
	h.mu.Lock()
	if h.released {
		h.mu.Unlock()
		return false, next(ctx)
	}
	h.setHeldLocked(h.held + 1)
	h.mu.Unlock()
	select {
	case <-h.release:
		h.leave()
		// When the release and the context's end arrive together, select may
		// pick either; a held call whose context ended is still abandoned.
		if ctx.Err() != nil {
			return true, abandoned(ctx)
		}
		return false, next(ctx)
	case <-ctx.Done():
		h.leave()
		return true, abandoned(ctx)
	}
}

// leave records that a held call stopped waiting.
func (h *Hang) leave() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.setHeldLocked(h.held - 1)
}

// Release lets every held call whose context has not ended, and every later
// matching call, reach the provider. Calling it again has no effect.
func (h *Hang) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.released {
		h.released = true
		close(h.release)
	}
}

// Held returns how many calls the hang is holding.
func (h *Hang) Held() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held
}

// WaitHeld waits until the hang holds at least n calls, or ctx ends.
func (h *Hang) WaitHeld(ctx context.Context, n int) error {
	for {
		h.mu.Lock()
		held, changed := h.held, h.changed
		h.mu.Unlock()
		if held >= n {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return fmt.Errorf("waiting for %d held calls, %d held: %w", n, held, ctx.Err())
		}
	}
}

// setHeldLocked records the number of held calls and wakes waiters. The
// caller holds h.mu.
func (h *Hang) setHeldLocked(held int) {
	h.held = held
	close(h.changed)
	h.changed = make(chan struct{})
}
