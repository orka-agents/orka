package acp

import (
	"context"
	"errors"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestRuntimeSessionDeleteRetriesExpiredCleanup(t *testing.T) {
	session := newDeleteRetryRuntimeSession(t, 50*time.Millisecond)
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	status, err := session.Delete(ctx)
	if status.Proven || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expired deletion = %#v, %v; want unproven cleanup and deadline", status, err)
	}
	select {
	case <-session.Process().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("expired deletion did not stop the exact child")
	}
	if _, err := session.StartPrompt(t.Context(), "after-delete", "sha256:after-delete", []ContentBlock{Text("must not run")}); err == nil {
		t.Fatal("failed cleanup reopened the deleted session")
	}
	retryCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
	defer stop()
	status, err = session.Delete(retryCtx)
	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("later deletion replayed the expired observation instead of checking the stopped child")
	}
	if runtime.GOOS == "linux" {
		if err != nil || !status.Proven || len(status.RemainingPIDs) != 0 {
			t.Fatalf("retry after child exit = %#v, %v; want real cleanup proof", status, err)
		}
		cancelledCtx, cancelWait := context.WithCancel(t.Context())
		cancelWait()
		replayed, replayErr := session.Delete(cancelledCtx)
		if replayErr != nil || !replayed.Proven {
			t.Fatalf("conclusive cleanup replay = %#v, %v", replayed, replayErr)
		}
	} else if err == nil || status.Proven {
		t.Fatal("unsupported descendant inspection produced a cleanup proof")
	}
}

func TestRuntimeSessionDeleteWaitersKeepOriginalFailure(t *testing.T) {
	session := newDeleteRetryRuntimeSession(t, 5*time.Second)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	firstContext := &deleteObservationContext{Context: ctx, entered: make(chan struct{})}
	firstDone := startObservedDeletion(session, firstContext)
	awaitDeleteObservation(t, firstContext.entered)

	cancelledWaiter, cancelWaiter := context.WithCancel(t.Context())
	cancelWaiter()
	if status, err := session.Delete(cancelledWaiter); status.Proven || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled cleanup waiter = %#v, %v", status, err)
	}
	select {
	case <-firstDone:
		t.Fatal("a cancelled waiter stopped the original cleanup")
	default:
	}

	// Park an already-joined waiter before it can observe completion. The
	// original result must survive even if a new retry finishes first.
	release := make(chan struct{})
	unblock := sync.OnceFunc(func() { close(release) })
	defer unblock()
	waiterContext := &deleteObservationContext{Context: t.Context(), entered: make(chan struct{}), release: release}
	waiterDone := startObservedDeletion(session, waiterContext)
	awaitDeleteObservation(t, waiterContext.entered)
	cancel()
	first := awaitObservedDeletion(t, firstDone)
	if first.status.Proven || !errors.Is(first.err, context.Canceled) {
		t.Fatalf("original cleanup = %#v, %v", first.status, first.err)
	}
	select {
	case <-session.Process().Done():
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled cleanup did not stop the child")
	}
	retryContext, stop := context.WithTimeout(t.Context(), 10*time.Second)
	defer stop()
	status, err := session.Delete(retryContext)
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("fresh cleanup did not re-observe the child: %#v, %v", status, err)
	}
	unblock()
	waiter := awaitObservedDeletion(t, waiterDone)
	if !errors.Is(waiter.err, context.Canceled) || !reflect.DeepEqual(waiter.status, first.status) {
		t.Fatalf("joined waiter lost its original result: %#v, %v", waiter.status, waiter.err)
	}
}

type deleteObservationContext struct {
	context.Context
	entered chan struct{}
	release <-chan struct{}
	once    sync.Once
}

func (c *deleteObservationContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.entered) })
	if c.release != nil {
		<-c.release
	}
	return c.Context.Done()
}

type observedDeletion struct {
	status CleanupStatus
	err    error
}

func startObservedDeletion(session *RuntimeSession, ctx context.Context) <-chan observedDeletion {
	done := make(chan observedDeletion, 1)
	go func() {
		status, err := session.Delete(ctx)
		done <- observedDeletion{status: status, err: err}
	}()
	return done
}

func awaitDeleteObservation(t *testing.T, entered <-chan struct{}) {
	t.Helper()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not reach its bounded wait")
	}
}

func awaitObservedDeletion(t *testing.T, done <-chan observedDeletion) observedDeletion {
	t.Helper()
	select {
	case result := <-done:
		return result
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup observation did not finish")
		return observedDeletion{}
	}
}

// Ignore TERM so an already-expired observation cannot accidentally succeed
// before Stop reaches its context check. Stop must still issue KILL.
func TestACPDeleteRetryHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_ACP_DELETE_RETRY_HELPER") != "1" {
		return
	}
	signal.Ignore(terminateSignal())
	TestACPHelperProcess(t)
}

func newDeleteRetryRuntimeSession(t *testing.T, grace time.Duration) *RuntimeSession {
	t.Helper()
	uid, gid := os.Getuid(), os.Getgid()
	if uid == 0 {
		uid, gid = 65534, 65534
	}
	root, err := os.MkdirTemp("", "orka-acp-delete-retry-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	if err := os.Chmod(root, 0o755); err != nil {
		t.Fatal(err)
	}
	paths, err := PrepareSessionPaths(filepath.Join(root, "sessions"), "delete-retry")
	if err != nil {
		t.Fatal(err)
	}
	environment, err := BuildChildEnvironment(paths, EnvironmentConfig{Values: map[string]string{
		"GO_WANT_ACP_HELPER":              "1",
		"GO_WANT_ACP_DELETE_RETRY_HELPER": "1",
		"ACP_HELPER_MODE":                 "wait",
	}})
	if err != nil {
		t.Fatal(err)
	}
	session, err := NewRuntimeSession(t.Context(), RuntimeSessionConfig{
		ID: "delete-retry", Generation: 1, ProfileDigest: "sha256:delete-retry",
		Process: ProcessConfig{
			Command: testAdapterCommand(t), Args: []string{"-test.run=^TestACPDeleteRetryHelperProcess$"},
			Environment: environment, Paths: paths, UID: uid, GID: gid, ExecHelperCommand: testExecHelperCommand(t),
		},
		CancelGrace: grace,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_, _ = session.Process().Stop(ctx, 50*time.Millisecond)
	})
	return session
}
