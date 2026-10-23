package storageclient

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// scriptedStep is what a scripted provider call does.
type scriptedStep int

const (
	// stepPass reaches the store and returns its outcome.
	stepPass scriptedStep = iota
	// stepLose commits in the store, then reports ErrOutcomeUnknown, as a
	// lost response would.
	stepLose
	// stepLoseThenDown commits, reports ErrOutcomeUnknown, and makes the
	// store unreachable for every later call, including ResolveOperation.
	stepLoseThenDown
	// stepUnknown reports ErrOutcomeUnknown without reaching the store.
	stepUnknown
	// stepUnavailable reports ErrUnavailable without reaching the store.
	stepUnavailable
	// stepRefuse reports ErrConflict without reaching the store, as a
	// refused precondition would.
	stepRefuse
)

// lossyStore wraps the SQLite store with scripted provider failures for the
// writes these tests use, and records the envelope of every call.
type lossyStore struct {
	*sqlite.Store

	mu        sync.Mutex
	script    map[string][]scriptedStep
	errs      map[string][]error
	down      bool
	envelopes map[string][]storagev1.WriteEnvelope
}

func (s *lossyStore) intercept(ctx context.Context, method string, call func(context.Context) error) error {
	s.mu.Lock()
	envelope, _ := storagev1.WriteEnvelopeFrom(ctx)
	s.envelopes[method] = append(s.envelopes[method], envelope)
	if s.down {
		s.mu.Unlock()
		return fmt.Errorf("%w: store unreachable", storagev1.ErrUnavailable)
	}
	if errs := s.errs[method]; len(errs) > 0 {
		s.errs[method] = errs[1:]
		s.mu.Unlock()
		return errs[0]
	}
	step := stepPass
	if steps := s.script[method]; len(steps) > 0 {
		step, s.script[method] = steps[0], steps[1:]
	}
	s.mu.Unlock()
	switch step {
	case stepUnknown:
		return fmt.Errorf("%w: connection reset before the response", storagev1.ErrOutcomeUnknown)
	case stepUnavailable:
		return fmt.Errorf("%w: connection refused", storagev1.ErrUnavailable)
	case stepRefuse:
		return store.ConflictErrorf("a precondition no longer holds")
	}
	if err := call(ctx); err != nil {
		return err
	}
	switch step {
	case stepLose:
		return fmt.Errorf("%w: response lost", storagev1.ErrOutcomeUnknown)
	case stepLoseThenDown:
		s.setDown(true)
		return fmt.Errorf("%w: response lost", storagev1.ErrOutcomeUnknown)
	}
	return nil
}

func (s *lossyStore) setDown(down bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.down = down
}

func (s *lossyStore) plan(method string, steps ...scriptedStep) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.script[method] = append(s.script[method], steps...)
}

func (s *lossyStore) fail(method string, errs ...error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.errs[method] = append(s.errs[method], errs...)
}

func (s *lossyStore) seen(method string) []storagev1.WriteEnvelope {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]storagev1.WriteEnvelope(nil), s.envelopes[method]...)
}

func (s *lossyStore) SendMessage(ctx context.Context, msg *store.Message) error {
	return s.intercept(ctx, "SendMessage", func(ctx context.Context) error { return s.Store.SendMessage(ctx, msg) })
}

func (s *lossyStore) GetMessages(ctx context.Context, namespace, taskName, parentTask string, markRead bool) ([]store.Message, error) {
	var messages []store.Message
	err := s.intercept(ctx, "GetMessages", func(ctx context.Context) error {
		var err error
		messages, err = s.Store.GetMessages(ctx, namespace, taskName, parentTask, markRead)
		return err
	})
	if err != nil {
		return nil, err
	}
	return messages, nil
}

func (s *lossyStore) UpdateTokenCounts(ctx context.Context, namespace, name string, input, output int) error {
	return s.intercept(ctx, "UpdateTokenCounts", func(ctx context.Context) error {
		return s.Store.UpdateTokenCounts(ctx, namespace, name, input, output)
	})
}

func (s *lossyStore) UpdateTokenCountsWithLock(ctx context.Context, namespace, name, owner, ownerUID string, input, output int) error {
	return s.intercept(ctx, "UpdateTokenCountsWithLock", func(ctx context.Context) error {
		return s.Store.UpdateTokenCountsWithLock(ctx, namespace, name, owner, ownerUID, input, output)
	})
}

func (s *lossyStore) ReleaseLockWithTokenUsage(ctx context.Context, namespace, name, owner, ownerUID string, input, output int) error {
	return s.intercept(ctx, "ReleaseLockWithTokenUsage", func(ctx context.Context) error {
		return s.Store.ReleaseLockWithTokenUsage(ctx, namespace, name, owner, ownerUID, input, output)
	})
}

func (s *lossyStore) SaveResult(ctx context.Context, namespace, taskName string, data []byte) error {
	return s.intercept(ctx, "SaveResult", func(ctx context.Context) error {
		return s.Store.SaveResult(ctx, namespace, taskName, data)
	})
}

func (s *lossyStore) ResolveOperation(ctx context.Context, request storagev1.ResolveOperationRequest) (storagev1.OperationOutcome, error) {
	var outcome storagev1.OperationOutcome
	err := s.intercept(ctx, "ResolveOperation", func(ctx context.Context) error {
		var err error
		outcome, err = s.Store.ResolveOperation(ctx, request)
		return err
	})
	return outcome, err
}

// staticFences serves one dataset and fence for every contract.
type staticFences struct {
	datasetID string
	fence     storagev1.ControllerEpochFence
}

func (f staticFences) WriteFence(context.Context, string) (string, storagev1.ControllerEpochFence, error) {
	return f.datasetID, f.fence, nil
}

var testFence = storagev1.ControllerEpochFence{Name: storagev1.DefaultControllerEpochName, Epoch: 1, HolderID: "controller-a"}

// advanceTestDataset gives backend's dataset testFence's epoch and returns
// its dataset ID.
func advanceTestDataset(t *testing.T, backend *sqlite.Store, fence storagev1.ControllerEpochFence) string {
	t.Helper()
	info, err := backend.Describe(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	acquired := time.Date(2026, time.March, 1, 0, 0, 0, 0, time.UTC)
	_, err = backend.AdvanceControllerEpoch(t.Context(), storagev1.AdvanceControllerEpochRequest{
		DatasetID: info.DatasetID,
		Epoch: storagev1.ControllerEpoch{
			Name: fence.Name, Epoch: fence.Epoch, Version: fence.Epoch, HolderID: fence.HolderID,
			RequestDigest: storagev1.CanonicalBytesDigest([]byte(fmt.Sprintf("%s/%d", fence.HolderID, fence.Epoch))),
			AcquiredAt:    acquired, UpdatedAt: acquired,
		},
		InstallationID: "installation-test",
	})
	if err != nil {
		t.Fatalf("advance the test dataset: %v", err)
	}
	return info.DatasetID
}

type lossyFixture struct {
	backend   *sqlite.Store
	lossy     *lossyStore
	client    *Client
	datasetID string
}

func newLossyFixture(t *testing.T) *lossyFixture {
	t.Helper()
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	datasetID := advanceTestDataset(t, backend, testFence)
	lossy := &lossyStore{
		Store:     backend,
		script:    map[string][]scriptedStep{},
		errs:      map[string][]error{},
		envelopes: map[string][]storagev1.WriteEnvelope{},
	}
	client, err := New(Options{
		State: lossy, Artifacts: lossy, Embedded: lossy,
		Fences: staticFences{datasetID: datasetID, fence: testFence},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.ops.fenceWait = 200 * time.Millisecond
	client.ops.retryHorizon = 300 * time.Millisecond
	client.ops.resolveTimeout = time.Second
	client.ops.resolveBackoff = 10 * time.Millisecond
	client.ops.resolveMaxBackoff = 50 * time.Millisecond
	return &lossyFixture{backend: backend, lossy: lossy, client: client, datasetID: datasetID}
}

func (f *lossyFixture) inbox(t *testing.T) []store.Message {
	t.Helper()
	messages, err := f.client.GetMessages(t.Context(), "tenant", "reader", "parent", false)
	if err != nil {
		t.Fatalf("read the inbox: %v", err)
	}
	return messages
}

func (f *lossyFixture) session(t *testing.T) *store.SessionRecord {
	t.Helper()
	session, err := f.client.GetSession(t.Context(), "tenant", "session")
	if err != nil {
		t.Fatalf("read the session: %v", err)
	}
	return session
}

func message(content string) *store.Message {
	return &store.Message{Namespace: "tenant", FromTask: "writer", ToTask: "reader", ParentTask: "parent", Content: content}
}

// requireRetriedWithOneEnvelope checks that every attempt of method carried
// the same envelope.
func requireRetriedWithOneEnvelope(t *testing.T, f *lossyFixture, method string, attempts int) {
	t.Helper()
	seen := f.lossy.seen(method)
	if len(seen) != attempts {
		t.Fatalf("%s attempts = %d, want %d", method, len(seen), attempts)
	}
	for _, envelope := range seen[1:] {
		if envelope != seen[0] {
			t.Fatalf("%s retried with envelope %#v, first attempt %#v", method, envelope, seen[0])
		}
	}
}

// A lost response after a committed write is retried with the same envelope
// and returns the original outcome; the effect happens once.
func TestLostResponseReturnsTheOriginalOutcome(t *testing.T) {
	t.Run("SendMessage", func(t *testing.T) {
		f := newLossyFixture(t)
		f.lossy.plan("SendMessage", stepLose)
		if err := f.client.SendMessage(t.Context(), message("hello")); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		requireRetriedWithOneEnvelope(t, f, "SendMessage", 2)
		if inbox := f.inbox(t); len(inbox) != 1 || inbox[0].Content != "hello" {
			t.Fatalf("inbox = %#v, want the one message", inbox)
		}
	})
	t.Run("GetMessages consumes once", func(t *testing.T) {
		f := newLossyFixture(t)
		for _, content := range []string{"one", "two", "three"} {
			if err := f.client.SendMessage(t.Context(), message(content)); err != nil {
				t.Fatal(err)
			}
		}
		f.lossy.plan("GetMessages", stepLose)
		consumed, err := f.client.GetMessages(t.Context(), "tenant", "reader", "parent", true)
		if err != nil || len(consumed) != 3 {
			t.Fatalf("consuming read = %d messages, %v; want the batch of 3", len(consumed), err)
		}
		if again, err := f.client.GetMessages(t.Context(), "tenant", "reader", "parent", true); err != nil || len(again) != 0 {
			t.Fatalf("second consuming read = %d messages, %v; want none", len(again), err)
		}
	})
	t.Run("UpdateTokenCounts", func(t *testing.T) {
		f := newLossyFixture(t)
		if err := f.client.CreateSession(t.Context(), &store.SessionRecord{Namespace: "tenant", Name: "session", SessionType: "task"}); err != nil {
			t.Fatal(err)
		}
		f.lossy.plan("UpdateTokenCounts", stepLose)
		if err := f.client.UpdateTokenCounts(t.Context(), "tenant", "session", 5, 7); err != nil {
			t.Fatalf("UpdateTokenCounts: %v", err)
		}
		if session := f.session(t); session.InputTokens != 5 || session.OutputTokens != 7 {
			t.Fatalf("tokens = %d/%d, want 5/7 counted once", session.InputTokens, session.OutputTokens)
		}
	})
	t.Run("UpdateTokenCountsWithLock and ReleaseLockWithTokenUsage", func(t *testing.T) {
		f := newLossyFixture(t)
		ctx := t.Context()
		if err := f.client.CreateSession(ctx, &store.SessionRecord{Namespace: "tenant", Name: "session", SessionType: "task"}); err != nil {
			t.Fatal(err)
		}
		if err := f.client.AcquireLock(ctx, "tenant", "session", "task", "uid-1"); err != nil {
			t.Fatal(err)
		}
		f.lossy.plan("UpdateTokenCountsWithLock", stepLose)
		if err := f.client.UpdateTokenCountsWithLock(ctx, "tenant", "session", "task", "uid-1", 2, 3); err != nil {
			t.Fatalf("UpdateTokenCountsWithLock: %v", err)
		}
		f.lossy.plan("ReleaseLockWithTokenUsage", stepLose)
		if err := f.client.ReleaseLockWithTokenUsage(ctx, "tenant", "session", "task", "uid-1", 10, 20); err != nil {
			t.Fatalf("ReleaseLockWithTokenUsage: %v", err)
		}
		session := f.session(t)
		if session.InputTokens != 12 || session.OutputTokens != 23 || session.ActiveTask != "" {
			t.Fatalf("session = %d/%d tokens, active %q; want 12/23 counted once and the lock released",
				session.InputTokens, session.OutputTokens, session.ActiveTask)
		}
	})
}

// ErrUnavailable means nothing committed, so the same envelope is retried.
func TestUnavailableWriteIsRetriedWithTheSameEnvelope(t *testing.T) {
	f := newLossyFixture(t)
	f.lossy.plan("SaveResult", stepUnavailable, stepUnavailable)
	if err := f.client.SaveResult(t.Context(), "tenant", "task", []byte("result")); err != nil {
		t.Fatalf("SaveResult: %v", err)
	}
	requireRetriedWithOneEnvelope(t, f, "SaveResult", 3)
	if got, err := f.client.GetResult(t.Context(), "tenant", "task"); err != nil || string(got) != "result" {
		t.Fatalf("result = %q, %v", got, err)
	}
}

// Every write carries the dataset and fence the fence source gives, a new
// UUIDv7 operation ID, the digest of its arguments, the issue time and the
// next sequence.
func TestWriteEnvelopeFields(t *testing.T) {
	f := newLossyFixture(t)
	before := time.Now().UTC()
	for _, data := range []string{"first", "second"} {
		if err := f.client.SaveResult(t.Context(), "tenant", "task", []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	seen := f.lossy.seen("SaveResult")
	if len(seen) != 2 {
		t.Fatalf("SaveResult calls = %d, want 2", len(seen))
	}
	for i, envelope := range seen {
		if err := envelope.Validate(); err != nil {
			t.Fatalf("envelope %d: %v", i, err)
		}
		digest, err := storagev1.RequestDigest("SaveResult", "tenant", "task", []byte([]string{"first", "second"}[i]))
		if err != nil {
			t.Fatal(err)
		}
		id, err := uuid.Parse(envelope.OperationID)
		if err != nil || id.Version() != 7 {
			t.Fatalf("operation ID %q is not a UUIDv7 (%v)", envelope.OperationID, err)
		}
		if envelope.DatasetID != f.datasetID || envelope.Fence != testFence || envelope.RequestDigest != digest ||
			envelope.IssuedAt.Before(before) || envelope.IssuedAt.After(time.Now().UTC()) {
			t.Fatalf("envelope %d = %#v", i, envelope)
		}
	}
	if seen[0].OperationID == seen[1].OperationID || seen[1].Sequence != seen[0].Sequence+1 {
		t.Fatalf("envelopes share an operation or are out of order: %#v", seen)
	}
}

// When the store becomes unreachable after a write committed, the client
// cannot settle it: the write returns ErrOutcomeUnknown, never success or
// failure, and new writes to the dataset are refused until background
// resolution finds the operation committed. Nothing is repeated.
func TestUnsettledWriteBlocksTheDatasetUntilItIsResolved(t *testing.T) {
	f := newLossyFixture(t)
	ctx := t.Context()
	f.lossy.plan("SendMessage", stepLoseThenDown)
	err := f.client.SendMessage(ctx, message("hello"))
	if !errors.Is(err, storagev1.ErrOutcomeUnknown) {
		t.Fatalf("SendMessage error = %v, want ErrOutcomeUnknown", err)
	}
	if f.client.UnresolvedOperations() != 1 {
		t.Fatalf("unresolved operations = %d, want 1", f.client.UnresolvedOperations())
	}
	if err := f.client.SaveResult(ctx, "tenant", "task", []byte("later")); !errors.Is(err, storagev1.ErrNotReady) {
		t.Fatalf("write while an outcome is unknown = %v, want ErrNotReady", err)
	}
	if calls := len(f.lossy.seen("SaveResult")); calls != 0 {
		t.Fatalf("a refused write reached the store %d times", calls)
	}
	if _, err := f.client.GetResult(ctx, "tenant", "task"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("read while an outcome is unknown = %v, want reads to keep working", err)
	}

	f.lossy.setDown(false)
	deadline := time.Now().Add(10 * time.Second)
	for f.client.UnresolvedOperations() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the unknown outcome was never resolved")
		}
		time.Sleep(10 * time.Millisecond)
	}
	resolutions := f.lossy.seen("ResolveOperation")
	sent := f.lossy.seen("SendMessage")[0]
	outcome, err := f.backend.ResolveOperation(ctx, storagev1.ResolveOperationRequest{
		DatasetID: f.datasetID, OperationID: sent.OperationID, Method: "SendMessage",
		RequestDigest: sent.RequestDigest, IssuedAt: sent.IssuedAt,
	})
	if err != nil || outcome.State != storagev1.OperationCommitted || len(resolutions) == 0 {
		t.Fatalf("operation outcome = %#v, %v after %d resolutions; want committed", outcome, err, len(resolutions))
	}
	if inbox := f.inbox(t); len(inbox) != 1 {
		t.Fatalf("inbox = %d messages, want the one committed message", len(inbox))
	}
	if err := f.client.SaveResult(ctx, "tenant", "task", []byte("later")); err != nil {
		t.Fatalf("write after resolution: %v", err)
	}
}

// An unknown outcome that did not commit is resolved as aborted, so a late
// arrival of the same operation can never commit.
func TestUnknownOutcomeThatDidNotCommitIsAborted(t *testing.T) {
	f := newLossyFixture(t)
	ctx := t.Context()
	f.lossy.plan("SaveResult", stepUnknown)
	for range 100 {
		f.lossy.plan("SaveResult", stepUnavailable)
	}
	err := f.client.SaveResult(ctx, "tenant", "task", []byte("result"))
	if !errors.Is(err, storagev1.ErrOperationAborted) || store.IsTemporaryStorageError(err) {
		t.Fatalf("SaveResult error = %v, want only ErrOperationAborted: the outcome is settled", err)
	}
	if f.client.UnresolvedOperations() != 0 {
		t.Fatal("an aborted operation still blocks the dataset")
	}
	late := storagev1.WithWriteEnvelope(ctx, f.lossy.seen("SaveResult")[0])
	if err := f.backend.SaveResult(late, "tenant", "task", []byte("result")); !errors.Is(err, storagev1.ErrOperationAborted) {
		t.Fatalf("late arrival = %v, want ErrOperationAborted", err)
	}
	if _, err := f.backend.GetResult(ctx, "tenant", "task"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("result after abort = %v, want none", err)
	}
}

// A refusal after an attempt whose outcome is unknown settles only that
// attempt, so the client resolves the operation before returning it.
func TestRefusalAfterAnUnknownOutcomeIsResolved(t *testing.T) {
	f := newLossyFixture(t)
	f.lossy.plan("SaveResult", stepUnknown, stepRefuse)
	err := f.client.SaveResult(t.Context(), "tenant", "task", []byte("result"))
	if !errors.Is(err, store.ErrConflict) || !errors.Is(err, storagev1.ErrOperationAborted) {
		t.Fatalf("SaveResult error = %v, want the refusal, resolved as aborted", err)
	}
	if resolutions := len(f.lossy.seen("ResolveOperation")); resolutions != 1 {
		t.Fatalf("resolutions = %d, want 1", resolutions)
	}
}

// Clock skew means the store is not ready to accept writes from this
// controller.
func TestClockSkewRefusalIsNotReady(t *testing.T) {
	f := newLossyFixture(t)
	f.lossy.fail("SaveResult", fmt.Errorf("%w: issued ahead of the store clock", storagev1.ErrClockSkew))
	err := f.client.SaveResult(t.Context(), "tenant", "task", []byte("result"))
	if !errors.Is(err, storagev1.ErrNotReady) || !errors.Is(err, storagev1.ErrClockSkew) {
		t.Fatalf("SaveResult error = %v, want ErrNotReady wrapping ErrClockSkew", err)
	}
}

// A write whose context already carries an envelope belongs to its caller
// and runs once with that envelope.
func TestCallerEnvelopeRunsUnchanged(t *testing.T) {
	f := newLossyFixture(t)
	digest, err := storagev1.RequestDigest("SaveResult", "tenant", "task", []byte("result"))
	if err != nil {
		t.Fatal(err)
	}
	envelope := storagev1.WriteEnvelope{
		DatasetID: f.datasetID, Fence: testFence, OperationID: "caller-operation", RequestDigest: digest,
		IssuedAt: time.Now().UTC(), Sequence: 42,
	}
	f.lossy.plan("SaveResult", stepUnavailable)
	err = f.client.SaveResult(storagev1.WithWriteEnvelope(t.Context(), envelope), "tenant", "task", []byte("result"))
	if !errors.Is(err, storagev1.ErrUnavailable) {
		t.Fatalf("caller-owned write = %v, want the provider's error without a retry", err)
	}
	if seen := f.lossy.seen("SaveResult"); len(seen) != 1 || seen[0] != envelope {
		t.Fatalf("caller-owned write ran with %#v", seen)
	}
}

// gatedFences opens every contract's fence once open is closed.
type gatedFences struct {
	open      chan struct{}
	datasetID string
	fence     storagev1.ControllerEpochFence
}

func (g gatedFences) WriteFence(ctx context.Context, _ string) (string, storagev1.ControllerEpochFence, error) {
	select {
	case <-g.open:
		return g.datasetID, g.fence, nil
	case <-ctx.Done():
		return "", storagev1.ControllerEpochFence{}, ctx.Err()
	}
}

// A write, and a task-data transaction, waits for its dataset's fence, and
// is refused with ErrNotReady once the wait passes.
func TestWritesWaitForTheirFence(t *testing.T) {
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backend.RequireWriteEnvelopes()
	datasetID := advanceTestDataset(t, backend, testFence)
	client, err := NewEmbedded(backend)
	if err != nil {
		t.Fatal(err)
	}
	client.ops.fenceWait = 100 * time.Millisecond
	ctx := t.Context()
	if err := client.SaveResult(ctx, "tenant", "task", []byte("early")); !errors.Is(err, storagev1.ErrNotReady) {
		t.Fatalf("write without a fence source = %v, want ErrNotReady", err)
	}
	ran := false
	err = client.WithTaskDataTransaction(ctx, func(context.Context) error { ran = true; return nil })
	if !errors.Is(err, storagev1.ErrNotReady) || ran {
		t.Fatalf("task-data transaction without a fence = %v (ran %v), want ErrNotReady before it opens", err, ran)
	}

	gate := gatedFences{open: make(chan struct{}), datasetID: datasetID, fence: testFence}
	if err := client.BindFences(gate); err != nil {
		t.Fatal(err)
	}
	if err := client.BindFences(gate); !errors.Is(err, storagev1.ErrValidation) {
		t.Fatalf("second BindFences = %v, want ErrValidation", err)
	}
	if err := client.SaveResult(ctx, "tenant", "task", []byte("early")); !errors.Is(err, storagev1.ErrNotReady) {
		t.Fatalf("write before the fence opens = %v, want ErrNotReady", err)
	}
	client.ops.fenceWait = 10 * time.Second
	done := make(chan error, 1)
	go func() { done <- client.SaveResult(ctx, "tenant", "task", []byte("result")) }()
	select {
	case err := <-done:
		t.Fatalf("write finished before its fence opened: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(gate.open)
	if err := <-done; err != nil {
		t.Fatalf("write after the fence opened: %v", err)
	}
	if got, err := backend.GetResult(ctx, "tenant", "task"); err != nil || string(got) != "result" {
		t.Fatalf("result = %q, %v", got, err)
	}
}

// A method that takes its own fence carries that fence in its envelope, so a
// caller holding a replaced fence is refused with ErrFenced, as before.
func TestExplicitFenceIsTheEnvelopeFence(t *testing.T) {
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backend.RequireWriteEnvelopes()
	advanceTestDataset(t, backend, testFence)
	current := storagev1.ControllerEpochFence{Name: testFence.Name, Epoch: 2, HolderID: "controller-b"}
	datasetID := advanceTestDataset(t, backend, current)
	client, err := New(Options{
		State: backend, Artifacts: backend, Embedded: backend,
		Fences: staticFences{datasetID: datasetID, fence: current},
	})
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"n":1}`)
	projection := func() *store.OutboxProjection {
		return &store.OutboxProjection{
			ID: "projection", AggregateKind: "Task", AggregateID: "task", ProjectionKind: "Delivery",
			Payload: payload, PayloadDigest: storagev1.CanonicalBytesDigest(payload),
			CreatedAt: time.Now().UTC(), AvailableAt: time.Now().UTC(),
		}
	}
	_, err = client.EnqueueOutboxProjectionRecord(t.Context(), projection(), testFence)
	if !errors.Is(err, store.ErrFenced) {
		t.Fatalf("enqueue under the replaced fence = %v, want ErrFenced", err)
	}
	if _, err := client.EnqueueOutboxProjectionRecord(t.Context(), projection(), current); err != nil {
		t.Fatalf("enqueue under the current fence: %v", err)
	}
}

// countingFences counts the calls of a static fence source.
type countingFences struct {
	staticFences
	mu    sync.Mutex
	calls int
}

func (f *countingFences) WriteFence(ctx context.Context, contract string) (string, storagev1.ControllerEpochFence, error) {
	f.mu.Lock()
	f.calls++
	f.mu.Unlock()
	return f.staticFences.WriteFence(ctx, contract)
}

// A task-data transaction captures the write fences before it opens, so the
// writes inside it never call the fence source while the transaction holds
// the store.
func TestTaskDataTransactionWritesUseTheFencesItCaptured(t *testing.T) {
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backend.RequireWriteEnvelopes()
	fences := &countingFences{staticFences: staticFences{datasetID: advanceTestDataset(t, backend, testFence), fence: testFence}}
	client, err := New(Options{State: backend, Artifacts: backend, Embedded: backend, Fences: fences})
	if err != nil {
		t.Fatal(err)
	}
	err = client.WithTaskDataTransaction(t.Context(), func(ctx context.Context) error {
		if err := client.SaveResult(ctx, "tenant", "task", []byte("result")); err != nil {
			return err
		}
		return client.SaveArtifact(ctx, store.SaveArtifactRequest{
			Owner: store.ArtifactOwner{Namespace: "tenant", TaskName: "task"}, Filename: "a.txt", ContentType: "text/plain", Data: []byte("artifact"),
		})
	})
	if err != nil {
		t.Fatalf("transaction: %v", err)
	}
	if fences.calls != 2 {
		t.Fatalf("fence source calls = %d, want one per contract before the transaction opened", fences.calls)
	}
	if got, err := backend.GetResult(t.Context(), "tenant", "task"); err != nil || string(got) != "result" {
		t.Fatalf("result = %q, %v", got, err)
	}
}

// When a retry after an unknown outcome is refused and the outcome cannot be
// resolved, the write's error matches only ErrOutcomeUnknown: the refusal
// settled only that attempt, so it must not read as a definite failure.
func TestUnsettledOutcomeHidesTheRefusalBehindIt(t *testing.T) {
	f := newLossyFixture(t)
	f.lossy.plan("SaveResult", stepUnknown, stepRefuse)
	f.lossy.fail("ResolveOperation", fmt.Errorf("%w: connection refused", storagev1.ErrUnavailable))
	err := f.client.SaveResult(t.Context(), "tenant", "task", []byte("result"))
	if !errors.Is(err, storagev1.ErrOutcomeUnknown) || errors.Is(err, store.ErrConflict) || errors.Is(err, storagev1.ErrUnavailable) {
		t.Fatalf("SaveResult error = %v, want only ErrOutcomeUnknown", err)
	}
	if f.client.UnresolvedOperations() != 1 {
		t.Fatalf("unresolved operations = %d, want 1", f.client.UnresolvedOperations())
	}
	deadline := time.Now().Add(10 * time.Second)
	for f.client.UnresolvedOperations() != 0 {
		if time.Now().After(deadline) {
			t.Fatal("the operation was never resolved")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Embedded families run through the client unchanged against a store that,
// as in production, refuses contract writes without an envelope: scan
// ingestion writes through its transaction-scoped store, a task-data
// transaction writes through the client, and memory proposal application
// runs inside SQLite.
func TestEmbeddedFamiliesRunAgainstAStrictStore(t *testing.T) {
	backend, err := sqlite.OpenLockedStore(filepath.Join(t.TempDir(), "orka.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Close() })
	backend.RequireWriteEnvelopes()
	client, err := New(Options{
		State: backend, Artifacts: backend, Embedded: backend,
		Fences: staticFences{datasetID: advanceTestDataset(t, backend, testFence), fence: testFence},
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()

	identity := store.ScanTaskIdentity{Namespace: "ns", RepositoryScan: "repo", ScanRunID: "run", TaskName: "review", TaskUID: "task-uid", Stage: "review", SliceID: "slice"}
	if err := client.CreateScanRun(ctx, &store.ScanRun{ID: "run", Namespace: "ns", RepositoryScan: "repo", Phase: "running"}); err != nil {
		t.Fatal(err)
	}
	applied, err := client.ApplyScanTaskIngestion(ctx, &store.ScanTaskIngestion{ScanTaskIdentity: identity}, nil,
		func(tx store.SecurityStore, current *store.ScanRun) error {
			if err := tx.UpsertObservedFinding(ctx, &store.Finding{ID: "finding", Namespace: "ns", RepositoryScan: "repo", ScanRunID: "run", Fingerprint: "finding"}); err != nil {
				return err
			}
			current.AcceptedFindings++
			return nil
		})
	if err != nil || !applied {
		t.Fatalf("scan ingestion = %v, %v", applied, err)
	}
	if _, err := client.GetFinding(ctx, "ns", "finding"); err != nil {
		t.Fatalf("finding written by the ingestion: %v", err)
	}

	err = client.WithTaskDataTransaction(ctx, func(ctx context.Context) error {
		return client.SaveResult(ctx, "ns", "task", []byte("result"))
	})
	if err != nil {
		t.Fatalf("task-data transaction: %v", err)
	}

	proposal := &store.MemoryProposal{Namespace: "ns", Type: "memory", Title: "title", Content: "remember this"}
	if err := client.CreateMemoryProposal(ctx, proposal); err != nil {
		t.Fatal(err)
	}
	if err := client.ReviewMemoryProposal(ctx, store.MemoryProposalReview{Namespace: "ns", ID: proposal.ID, Status: "accepted", Reviewer: "reviewer"}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApplyMemoryProposal(ctx, store.MemoryProposalApply{Namespace: "ns", ID: proposal.ID, AppliedBy: "applier"}); err != nil {
		t.Fatalf("memory proposal application: %v", err)
	}
}
