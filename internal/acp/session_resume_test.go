package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func runHelperPrompt(t *testing.T, session *RuntimeSession, promptID string) (PromptResult, int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	run, err := session.StartPrompt(ctx, promptID, "sha256:"+promptID, []ContentBlock{Text("hello")})
	if err != nil {
		t.Fatalf("start %s: %v", promptID, err)
	}
	updates := 0
	for event := range run.Events {
		run.Release(event)
		if event.Type == PromptEventUpdate {
			updates++
		}
	}
	return <-run.Result, updates
}

func awaitAdapterExit(t *testing.T, session *RuntimeSession) {
	t.Helper()
	select {
	case <-session.Process().Done():
	case <-time.After(10 * time.Second):
		t.Fatal("adapter did not exit after the prompt")
	}
}

func TestRuntimeSessionResumesAdapterInPlaceAfterIdleExit(t *testing.T) {
	session, stateDir := newTestRuntimeSessionWithOptions(t, helperModeResume, nil, map[string]string{helperExitAfterPromptEnv: "1"})
	if !session.AgentCapabilities().SessionCapability(SessionCapabilityResume) {
		t.Fatal("helper did not advertise session/resume")
	}
	providerSession := session.ProviderSessionID()
	first := session.Process()

	result, _ := runHelperPrompt(t, session, "prompt-1")
	if result.Outcome != PromptOutcomeCompleted {
		t.Fatalf("first prompt = %#v", result)
	}
	awaitAdapterExit(t, session)

	result, updates := runHelperPrompt(t, session, "prompt-2")
	if result.Outcome != PromptOutcomeCompleted || !result.Accepted {
		t.Fatalf("prompt after adapter exit = %#v", result)
	}
	if updates != 1 {
		t.Fatalf("prompt after resume saw %d updates, want exactly the prompt's own chunk (resume-time updates must be dropped)", updates)
	}
	if session.ProviderSessionID() != providerSession {
		t.Fatalf("provider session changed across resume: %q -> %q", providerSession, session.ProviderSessionID())
	}
	if session.AdapterRestarts() != 1 {
		t.Fatalf("adapter restarts = %d, want 1", session.AdapterRestarts())
	}
	if session.Process() == first {
		t.Fatal("resume did not replace the exited adapter process")
	}
	if _, err := os.Stat(filepath.Join(stateDir, helperResumedStateFile)); err != nil {
		t.Fatalf("helper did not record the resume: %v", err)
	}
}

func TestRuntimeSessionIdleAdapterExitWithoutResumeCapabilityIsLost(t *testing.T) {
	session, _ := newTestRuntimeSessionWithOptions(t, "immediate", nil, map[string]string{helperExitAfterPromptEnv: "1"})
	result, _ := runHelperPrompt(t, session, "prompt-1")
	if result.Outcome != PromptOutcomeCompleted {
		t.Fatalf("first prompt = %#v", result)
	}
	awaitAdapterExit(t, session)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := session.StartPrompt(ctx, "prompt-2", "sha256:prompt-2", []ContentBlock{Text("again")})
	var lost *AdapterLostError
	if !errors.As(err, &lost) {
		t.Fatalf("prompt after unresumable adapter exit = %v, want AdapterLostError", err)
	}
	if lost.Attempted {
		t.Fatalf("resume was attempted without the capability: %v", lost)
	}
	if session.AdapterRestarts() != 0 {
		t.Fatalf("adapter restarts = %d, want 0", session.AdapterRestarts())
	}
}

func TestRuntimeSessionRejectedResumeIsLostAndStopsReplacement(t *testing.T) {
	session, _ := newTestRuntimeSessionWithOptions(t, helperModeResumeReject, nil, map[string]string{helperExitAfterPromptEnv: "1"})
	result, _ := runHelperPrompt(t, session, "prompt-1")
	if result.Outcome != PromptOutcomeCompleted {
		t.Fatalf("first prompt = %#v", result)
	}
	awaitAdapterExit(t, session)
	exited := session.Process()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, err := session.StartPrompt(ctx, "prompt-2", "sha256:prompt-2", []ContentBlock{Text("again")})
	var lost *AdapterLostError
	if !errors.As(err, &lost) || !lost.Attempted {
		t.Fatalf("prompt after rejected resume = %v, want attempted AdapterLostError", err)
	}
	if session.Process() != exited {
		t.Fatal("rejected resume bound the replacement adapter")
	}
	if session.AdapterRestarts() != 0 {
		t.Fatalf("adapter restarts = %d, want 0", session.AdapterRestarts())
	}
	// A later prompt must fail the same way rather than wedge on the
	// in-flight marker.
	if _, err := session.StartPrompt(ctx, "prompt-3", "sha256:prompt-3", []ContentBlock{Text("again")}); !errors.As(err, &lost) {
		t.Fatalf("second prompt after rejected resume = %v, want AdapterLostError", err)
	}
}

func TestRuntimeSessionInterruptedResumeIsLostAndStopsReplacement(t *testing.T) {
	session, _ := newTestRuntimeSessionWithOptions(t, helperModeResume, nil, map[string]string{
		helperExitAfterPromptEnv: "1",
		helperHangFirstResumeEnv: "1",
	})
	result, _ := runHelperPrompt(t, session, "prompt-1")
	if result.Outcome != PromptOutcomeCompleted {
		t.Fatalf("first prompt = %#v", result)
	}
	awaitAdapterExit(t, session)
	exited := session.Process()

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := session.StartPrompt(ctx, "prompt-2", "sha256:prompt-2", []ContentBlock{Text("again")})
	if lost, ok := errors.AsType[*AdapterLostError](err); !ok || !lost.Attempted {
		t.Fatalf("prompt whose caller left mid-resume = %v, want attempted AdapterLostError", err)
	}
	if session.Process() != exited {
		t.Fatal("interrupted resume bound the replacement adapter")
	}
	if session.AdapterRestarts() != 0 {
		t.Fatalf("adapter restarts = %d, want 0", session.AdapterRestarts())
	}
}

// The resume marker is written before the helper waits on a release file, so
// cancellation always races a known, blocked pre-admission restart, not a sleep.
func TestRuntimeSessionCancelPromptDuringBlockedResume(t *testing.T) {
	for _, expiredCancel := range []bool{false, true} {
		name := "live cancellation context"
		if expiredCancel {
			name = "expired cancellation context"
		}
		t.Run(name, func(t *testing.T) {
			session, stateDir := newTestRuntimeSessionWithOptions(t, helperModeResume, nil, map[string]string{
				helperExitAfterPromptEnv:  "1",
				helperBlockFirstResumeEnv: "1",
			})
			session.config.CancelGrace = 50 * time.Millisecond
			result, _ := runHelperPrompt(t, session, "prompt-1")
			if result.Outcome != PromptOutcomeCompleted {
				t.Fatalf("first prompt = %#v", result)
			}
			awaitAdapterExit(t, session)
			exited := session.Process()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			releaseResume := func() {
				if err := os.WriteFile(filepath.Join(stateDir, helperResumeReleaseFile), []byte("1"), 0o644); err != nil {
					t.Errorf("release resume: %v", err)
				}
			}
			t.Cleanup(releaseResume)
			started := make(chan error, 1)
			go func() {
				run, err := session.StartPrompt(ctx, "prompt-2", "sha256:prompt-2", []ContentBlock{Text("must not run")})
				if err == nil {
					for event := range run.Events {
						run.Release(event)
					}
					<-run.Result
				}
				started <- err
			}()
			pidText := awaitResumeStateFile(t, ctx, filepath.Join(stateDir, helperResumeBlockedFile))
			replacementPID, err := strconv.Atoi(pidText)
			if err != nil || replacementPID <= 0 {
				t.Fatalf("replacement PID = %q, error = %v", pidText, err)
			}
			assertRestartPromptIdentityReserved(t, session, ctx)

			cancelCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			if expiredCancel {
				stop()
			}
			cancelled, cancelErr := session.CancelPrompt(cancelCtx, "prompt-2")
			releaseResume()
			var startErr error
			select {
			case startErr = <-started:
			case <-ctx.Done():
				t.Fatal("prompt restart did not settle after cancellation")
			}
			if cancelErr != nil {
				if !expiredCancel || !errors.Is(cancelErr, context.Canceled) {
					t.Fatalf("cancel blocked restart = %v; original StartPrompt = %v", cancelErr, startErr)
				}
			}
			if cancelErr == nil && (cancelled.Accepted || cancelled.Outcome != PromptOutcomeCancelled) {
				t.Fatalf("pre-admission cancellation = %#v", cancelled)
			}
			lost, ok := errors.AsType[*AdapterLostError](startErr)
			if !ok || !lost.Attempted {
				t.Fatalf("cancelled restart = %v, want attempted AdapterLostError", startErr)
			}
			assertRestartCancelledBeforeAdmission(t, session, exited, replacementPID, stateDir)
			assertCancelledPromptIdentityNotReplayed(t, session, ctx)
			cleanup, err := session.Delete(ctx)
			if runtime.GOOS == "linux" {
				if err != nil || !cleanup.Proven {
					t.Fatalf("delete after cancelled restart = %#v, %v", cleanup, err)
				}
			} else if err == nil || cleanup.Proven {
				t.Fatal("unsupported descendant inspection produced a cleanup proof")
			}
			if _, err := session.StartPrompt(ctx, "after-delete", "sha256:after-delete", []ContentBlock{Text("must not run")}); err == nil {
				t.Fatal("cleanup reopened the runtime session")
			}
		})
	}
}

func TestRuntimeSessionDeleteDuringBlockedResume(t *testing.T) {
	for _, expiredDelete := range []bool{false, true} {
		name := "live deletion context"
		if expiredDelete {
			name = "expired deletion context"
		}
		t.Run(name, func(t *testing.T) {
			session, stateDir := newTestRuntimeSessionWithOptions(t, helperModeResume, nil, map[string]string{
				helperExitAfterPromptEnv:  "1",
				helperBlockFirstResumeEnv: "1",
			})
			session.config.CancelGrace = 50 * time.Millisecond
			result, _ := runHelperPrompt(t, session, "prompt-1")
			if result.Outcome != PromptOutcomeCompleted {
				t.Fatalf("first prompt = %#v", result)
			}
			awaitAdapterExit(t, session)
			exited := session.Process()
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			releaseResume := func() {
				if err := os.WriteFile(filepath.Join(stateDir, helperResumeReleaseFile), []byte("1"), 0o644); err != nil {
					t.Errorf("release resume: %v", err)
				}
			}
			t.Cleanup(releaseResume)
			started := make(chan error, 1)
			go func() {
				run, err := session.StartPrompt(ctx, "prompt-2", "sha256:prompt-2", []ContentBlock{Text("must not run")})
				if err == nil {
					for event := range run.Events {
						run.Release(event)
					}
					<-run.Result
				}
				started <- err
			}()
			pidText := awaitResumeStateFile(t, ctx, filepath.Join(stateDir, helperResumeBlockedFile))
			replacementPID, err := strconv.Atoi(pidText)
			if err != nil || replacementPID <= 0 {
				t.Fatalf("replacement PID = %q, error = %v", pidText, err)
			}
			deleteCtx, stop := context.WithTimeout(t.Context(), 5*time.Second)
			defer stop()
			if expiredDelete {
				stop()
			}
			observation := &deleteObservationContext{Context: deleteCtx, entered: make(chan struct{})}
			deleted := startObservedDeletion(session, observation)
			awaitDeleteObservation(t, observation.entered)
			releaseResume()
			select {
			case err := <-started:
				if lost, ok := errors.AsType[*AdapterLostError](err); !ok || !lost.Attempted {
					t.Fatalf("deletion left the original prompt free to start: %v", err)
				}
			case <-ctx.Done():
				t.Fatal("prompt restart did not settle after deletion")
			}
			deletion := awaitObservedDeletion(t, deleted)
			if expiredDelete {
				if deletion.status.Proven || !errors.Is(deletion.err, context.Canceled) {
					t.Fatalf("expired deletion = %#v, %v", deletion.status, deletion.err)
				}
			} else if runtime.GOOS == "linux" {
				if deletion.err != nil || !deletion.status.Proven {
					t.Fatalf("deletion = %#v, %v", deletion.status, deletion.err)
				}
			} else if deletion.err == nil || deletion.status.Proven {
				t.Fatal("unsupported descendant inspection produced a cleanup proof")
			}
			assertRestartCancelledBeforeAdmission(t, session, exited, replacementPID, stateDir)
			if _, err := session.StartPrompt(ctx, "after-delete", "sha256:after-delete", []ContentBlock{Text("must not run")}); err == nil {
				t.Fatal("deletion reopened admission")
			}
		})
	}
}

func assertRestartPromptIdentityReserved(t *testing.T, session *RuntimeSession, ctx context.Context) {
	t.Helper()
	_, err := session.StartPrompt(ctx, "prompt-2", "sha256:prompt-2", []ContentBlock{Text("duplicate")})
	if duplicate, ok := errors.AsType[*DuplicatePromptError](err); !ok || !duplicate.Active {
		t.Errorf("pending identity duplicate = %v, want active duplicate", err)
	}
	_, err = session.StartPrompt(ctx, "prompt-2", "sha256:changed", []ContentBlock{Text("changed")})
	if _, ok := errors.AsType[*DigestConflictError](err); !ok {
		t.Errorf("pending identity digest conflict = %v", err)
	}
	if _, err := session.StartPrompt(ctx, "prompt-other", "sha256:other", []ContentBlock{Text("other")}); err == nil {
		t.Error("another prompt was admitted during restart")
	}
	if _, err := session.CancelPrompt(ctx, "prompt-other"); err == nil {
		t.Error("unrelated identity cancelled the pending restart")
	} else if _, ok := errors.AsType[*StalePromptError](err); !ok {
		t.Errorf("unrelated identity cancellation = %v", err)
	}
}

func assertRestartCancelledBeforeAdmission(t *testing.T, session *RuntimeSession, exited *Process, replacementPID int, stateDir string) {
	t.Helper()
	if session.Process() != exited || session.AdapterRestarts() != 0 {
		t.Fatal("cancelled restart bound the replacement adapter")
	}
	if runtime.GOOS != "windows" {
		if err := signalProcessGroup(replacementPID, syscall.Signal(0)); !errors.Is(err, syscall.ESRCH) {
			t.Fatalf("cancelled replacement process group still exists: %v", err)
		}
	}
	if _, err := os.Stat(filepath.Join(stateDir, helperPromptAfterResumeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("original prompt reached the resumed adapter: stat = %v", err)
	}
	tombstone, ok := session.Tombstone("prompt-2")
	if !ok || tombstone.RequestDigest != "sha256:prompt-2" || tombstone.Result.Accepted || tombstone.Result.Outcome != PromptOutcomeCancelled {
		t.Fatalf("cancelled restart tombstone = %#v, found = %v", tombstone, ok)
	}
}

func assertCancelledPromptIdentityNotReplayed(t *testing.T, session *RuntimeSession, ctx context.Context) {
	t.Helper()
	_, err := session.StartPrompt(ctx, "prompt-2", "sha256:prompt-2", []ContentBlock{Text("must not replay")})
	duplicate, ok := errors.AsType[*DuplicatePromptError](err)
	if !ok || duplicate.Active || duplicate.Result == nil || duplicate.Result.Accepted || duplicate.Result.Outcome != PromptOutcomeCancelled {
		t.Fatalf("cancelled identity replay = %v, want settled not-accepted duplicate", err)
	}
	if _, err := session.StartPrompt(ctx, "prompt-2", "sha256:changed", []ContentBlock{Text("changed")}); err == nil {
		t.Fatal("cancelled prompt identity accepted a conflicting digest")
	} else if _, ok := errors.AsType[*DigestConflictError](err); !ok {
		t.Fatalf("cancelled identity digest conflict = %v", err)
	}
	if replayed, err := session.CancelPrompt(ctx, "prompt-2"); err != nil || replayed.Accepted || replayed.Outcome != PromptOutcomeCancelled {
		t.Fatalf("settled cancellation replay = %#v, %v", replayed, err)
	}
}

func awaitResumeStateFile(t *testing.T, ctx context.Context, path string) string {
	t.Helper()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		if data, err := os.ReadFile(path); err == nil {
			if len(data) > 0 {
				return string(data)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("observe resume marker: %v", err)
		}
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal("adapter did not reach the blocked resume")
		}
	}
}

func TestRuntimeSessionDeleteClosesProviderSessionWhenAdvertised(t *testing.T) {
	session, stateDir := newTestRuntimeSessionWithOptions(t, helperModeClose, nil, nil)
	result, _ := runHelperPrompt(t, session, "prompt-1")
	if result.Outcome != PromptOutcomeCompleted {
		t.Fatalf("first prompt = %#v", result)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = session.Delete(ctx)
	if _, err := os.Stat(filepath.Join(stateDir, helperClosedStateFile)); err != nil {
		t.Fatalf("helper did not receive session/close before the stop: %v", err)
	}
}

func TestRuntimeSessionDeleteSkipsCloseAfterFreeze(t *testing.T) {
	session, stateDir := newTestRuntimeSessionWithOptions(t, helperModeClose, nil, nil)
	freezeCtx, cancelFreeze := context.WithTimeout(context.Background(), time.Second)
	// Freeze proof is Linux-only; the attempt alone may leave the tree
	// stopped, so it must suppress the close either way.
	_ = session.Freeze(freezeCtx)
	cancelFreeze()
	// Continue the tree behind the session's back so a close, if sent, is
	// answered and recorded: only the frozen marker may suppress it.
	if err := signalProcessGroup(session.Process().PID(), continueSignal()); err != nil {
		t.Fatalf("continue adapter: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = session.Delete(ctx)
	if _, err := os.Stat(filepath.Join(stateDir, helperClosedStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session/close was sent to a frozen adapter (stat err=%v)", err)
	}
}

func TestRuntimeSessionDeleteSkipsCloseWithoutCapability(t *testing.T) {
	session, stateDir := newTestRuntimeSessionWithOptions(t, "immediate", nil, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_, _ = session.Delete(ctx)
	if _, err := os.Stat(filepath.Join(stateDir, helperClosedStateFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("session/close was sent to an agent that does not advertise it (stat err=%v)", err)
	}
}

func TestAgentCapabilitiesSessionCapability(t *testing.T) {
	capabilities := AgentCapabilities{SessionCapabilities: map[string]any{
		"resume": map[string]any{}, "close": true, "fork": nil, "list": false, "delete": "yes",
	}}
	for name, want := range map[string]bool{"resume": true, "close": true, "fork": false, "list": false, "delete": false, "missing": false} {
		if got := capabilities.SessionCapability(name); got != want {
			t.Errorf("SessionCapability(%q) = %v, want %v", name, got, want)
		}
	}
	if (AgentCapabilities{}).SessionCapability("resume") {
		t.Error("nil capabilities reported resume support")
	}
}

func TestRuntimeSessionRejectsLeaseExpiredWhileResumeWaitsForAdmissionLock(t *testing.T) {
	session, stateDir := newTestRuntimeSessionWithOptions(t, helperModeResume, nil, map[string]string{
		helperExitAfterPromptEnv: "1", helperBlockFirstResumeEnv: "1", helperSkipResumeUpdateEnv: "1",
	})
	if result, _ := runHelperPrompt(t, session, "prompt-1"); result.Outcome != PromptOutcomeCompleted {
		t.Fatalf("first prompt = %#v", result)
	}
	awaitAdapterExit(t, session)
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	deadline := time.Now().Add(2 * time.Second)
	started := make(chan error, 1)
	go func() {
		run, err := session.StartPromptWithLeaseDeadline(ctx, "prompt-2", "sha256:prompt-2", []ContentBlock{Text("must not run after lease expiry")}, deadline)
		if err == nil {
			for event := range run.Events {
				run.Release(event)
			}
			<-run.Result
		}
		started <- err
	}()
	awaitResumeStateFile(t, ctx, filepath.Join(stateDir, helperResumeBlockedFile))
	// Let the adapter answer on time, but hold the admission mutex until
	// after the lease. The reply reader does not need this session mutex.
	func() {
		session.mu.Lock()
		defer session.mu.Unlock()
		if err := os.WriteFile(filepath.Join(stateDir, helperResumeReleaseFile), []byte("1"), 0o644); err != nil {
			t.Fatal(err)
		}
		awaitResumeStateFile(t, ctx, filepath.Join(stateDir, helperResumedStateFile))
		time.Sleep(time.Until(deadline) + 20*time.Millisecond)
	}()
	select {
	case err := <-started:
		lost, ok := errors.AsType[*AdapterLostError](err)
		if !ok || !lost.Attempted || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("expired resumed prompt = %v, want attempted adapter loss with deadline expiry", err)
		}
	case <-ctx.Done():
		t.Fatal("expired resumed prompt did not settle")
	}
	if session.AdapterRestarts() != 1 {
		t.Fatal("test did not complete the resume RPC before lease expiry")
	}
	if _, err := session.StartPrompt(ctx, "prompt-3", "sha256:prompt-3", []ContentBlock{Text("must not reuse retired replacement")}); err == nil {
		t.Fatal("lease-expired replacement remained open to prompt admission")
	}
	if _, err := os.Stat(filepath.Join(stateDir, helperPromptAfterResumeFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expired prompt reached the replacement adapter: %v", err)
	}
	select {
	case <-session.Process().Done():
	case <-ctx.Done():
		t.Fatal("expired replacement adapter was not stopped")
	}
}
