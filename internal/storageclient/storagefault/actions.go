package storagefault

import (
	"context"
	"fmt"
	"sync"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

type Action interface {
	apply(ctx context.Context, method string, next func(context.Context) error) (injected bool, err error)
}

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

func Delay(d time.Duration) Action {
	return delayAction{d: d}
}

type delayAction struct{ d time.Duration }

func (a delayAction) apply(ctx context.Context, _ string, next func(context.Context) error) (bool, error) {
	timer := time.NewTimer(a.d)
	defer timer.Stop()
	select {
	case <-timer.C:
		if ctx.Err() != nil {
			return true, abandoned(ctx)
		}
		return false, next(ctx)
	case <-ctx.Done():
		return true, abandoned(ctx)
	}
}

func LoseResponse() Action {
	return loseResponseAction{}
}

type loseResponseAction struct{}

func (loseResponseAction) apply(ctx context.Context, method string, next func(context.Context) error) (bool, error) {
	_ = next(context.WithValue(ctx, lostResponseContextKey{}, true))
	if info, ok := storagev1.LookupMethod(method); ok && info.Kind == storagev1.MethodRead {
		return true, fmt.Errorf("%w: the response to a read was lost", storagev1.ErrUnavailable)
	}
	return true, fmt.Errorf("%w: the response was lost after the request was sent", storagev1.ErrOutcomeUnknown)
}

type lostResponseContextKey struct{}

func responseWillBeLost(ctx context.Context) bool {
	lost, _ := ctx.Value(lostResponseContextKey{}).(bool)
	return lost
}

func abandoned(ctx context.Context) error {
	return fmt.Errorf("%w: %w", storagev1.ErrUnavailable, ctx.Err())
}

type Hang struct {
	mu       sync.Mutex
	released bool
	release  chan struct{}
	held     int
	changed  chan struct{}
}

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
		if ctx.Err() != nil {
			return true, abandoned(ctx)
		}
		return false, next(ctx)
	case <-ctx.Done():
		h.leave()
		return true, abandoned(ctx)
	}
}

func (h *Hang) leave() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.setHeldLocked(h.held - 1)
}

func (h *Hang) Release() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.released {
		h.released = true
		close(h.release)
	}
}

func (h *Hang) Held() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.held
}

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

func (h *Hang) setHeldLocked(held int) {
	h.held = held
	close(h.changed)
	h.changed = make(chan struct{})
}
