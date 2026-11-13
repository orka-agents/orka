package storageclient

import (
	"context"
	"errors"
	"testing"

	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// digestCheckingStore recomputes the request digest of each attempt of a
// write-back method from the arguments that attempt carries, as a provider
// behind a transport that rebuilds the request would, and records whether it
// matches the digest in the attempt's envelope.
type digestCheckingStore struct {
	*lossyStore
	matched map[string][]bool
}

func (s *digestCheckingStore) check(ctx context.Context, method string, args ...any) {
	envelope, _ := storagev1.WriteEnvelopeFrom(ctx)
	digest, err := storagev1.RequestDigest(method, args...)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.matched[method] = append(s.matched[method], err == nil && digest == envelope.RequestDigest)
}

func (s *digestCheckingStore) SendMessage(ctx context.Context, msg *store.Message) error {
	s.check(ctx, "SendMessage", msg)
	return s.lossyStore.SendMessage(ctx, msg)
}

func (s *digestCheckingStore) CreateMonitorRun(ctx context.Context, run *store.MonitorRun) error {
	s.check(ctx, "CreateMonitorRun", run)
	return s.intercept(ctx, "CreateMonitorRun", func(ctx context.Context) error {
		return s.Store.CreateMonitorRun(ctx, run)
	})
}

func newDigestFixture(t *testing.T) (*lossyFixture, *digestCheckingStore) {
	t.Helper()
	f := newLossyFixture(t)
	checking := &digestCheckingStore{lossyStore: f.lossy, matched: map[string][]bool{}}
	client, err := New(Options{
		State: checking, Artifacts: checking, Embedded: checking,
		Fences: staticFences{datasetID: f.datasetID, fence: testFence},
	})
	if err != nil {
		t.Fatal(err)
	}
	client.ops.retryHorizon = f.client.ops.retryHorizon
	f.client = client
	return f, checking
}

func requireEveryAttemptMatched(t *testing.T, checking *digestCheckingStore, method string, attempts int) {
	t.Helper()
	checking.mu.Lock()
	matched := append([]bool(nil), checking.matched[method]...)
	checking.mu.Unlock()
	if len(matched) != attempts {
		t.Fatalf("%s attempts = %d, want %d", method, len(matched), attempts)
	}
	for i, ok := range matched {
		if !ok {
			t.Fatalf("%s attempt %d carried a request its envelope's digest does not cover (matched = %v)", method, i+1, matched)
		}
	}
}

// A write that fills in the record it is given, as SendMessage assigns an ID
// and CreateMonitorRun a phase and start time, retries a lost response with
// the request its digest covers: each attempt gets a fresh copy of the
// caller's record, and the caller's record receives the stored values only
// once the write has succeeded.
func TestLostResponseRetriesWriteBackWithTheOriginalRequest(t *testing.T) {
	t.Run("SendMessage", func(t *testing.T) {
		f, checking := newDigestFixture(t)
		f.lossy.plan("SendMessage", stepLose)
		sent := message("hello")
		if err := f.client.SendMessage(t.Context(), sent); err != nil {
			t.Fatalf("SendMessage: %v", err)
		}
		requireRetriedWithOneEnvelope(t, f, "SendMessage", 2)
		requireEveryAttemptMatched(t, checking, "SendMessage", 2)
		inbox := f.inbox(t)
		if len(inbox) != 1 || sent.ID == 0 || inbox[0].ID != sent.ID || !inbox[0].CreatedAt.Equal(sent.CreatedAt) {
			t.Fatalf("sent = %#v, inbox = %#v; want the stored message's identity written back", sent, inbox)
		}
	})

	t.Run("CreateMonitorRun", func(t *testing.T) {
		f, checking := newDigestFixture(t)
		f.lossy.plan("CreateMonitorRun", stepLose)
		run := &store.MonitorRun{ID: "run-1", MonitorNamespace: "tenant", MonitorName: "monitor", Trigger: "manual"}
		if err := f.client.CreateMonitorRun(t.Context(), run); err != nil {
			t.Fatalf("CreateMonitorRun: %v", err)
		}
		requireRetriedWithOneEnvelope(t, f, "CreateMonitorRun", 2)
		requireEveryAttemptMatched(t, checking, "CreateMonitorRun", 2)
		stored, err := f.client.GetMonitorRun(t.Context(), "tenant", "run-1")
		if err != nil || run.Phase != "queued" || run.StartedAt.IsZero() || !stored.StartedAt.Equal(run.StartedAt) {
			t.Fatalf("run = %#v, stored = %#v, %v; want the stored phase and start time written back", run, stored, err)
		}
	})

	// A write whose outcome stays unknown leaves the caller's record as it
	// was, even though an attempt committed and filled in its own copy.
	t.Run("unknown outcome", func(t *testing.T) {
		f, _ := newDigestFixture(t)
		f.lossy.plan("SendMessage", stepLoseThenDown)
		sent := message("hello")
		err := f.client.SendMessage(t.Context(), sent)
		if !errors.Is(err, storagev1.ErrOutcomeUnknown) {
			t.Fatalf("SendMessage = %v, want ErrOutcomeUnknown", err)
		}
		if *sent != *message("hello") {
			t.Fatalf("sent = %#v after an unknown outcome, want the caller's record unchanged", sent)
		}
	})
}
