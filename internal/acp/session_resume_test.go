package acp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
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

func TestRuntimeSessionInterruptedResumeStaysRetryable(t *testing.T) {
	session, _ := newTestRuntimeSessionWithOptions(t, helperModeResume, nil, map[string]string{
		helperExitAfterPromptEnv: "1",
		helperHangFirstResumeEnv: "1",
	})
	result, _ := runHelperPrompt(t, session, "prompt-1")
	if result.Outcome != PromptOutcomeCompleted {
		t.Fatalf("first prompt = %#v", result)
	}
	awaitAdapterExit(t, session)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_, err := session.StartPrompt(ctx, "prompt-2", "sha256:prompt-2", []ContentBlock{Text("again")})
	if err == nil {
		t.Fatal("prompt whose caller left mid-resume started")
	}
	if lost, ok := errors.AsType[*AdapterLostError](err); ok {
		t.Fatalf("caller cancellation retired the provider session: %v", lost)
	}
	if session.AdapterRestarts() != 0 {
		t.Fatalf("adapter restarts = %d, want 0", session.AdapterRestarts())
	}

	result, _ = runHelperPrompt(t, session, "prompt-3")
	if result.Outcome != PromptOutcomeCompleted || !result.Accepted {
		t.Fatalf("prompt retrying the resume = %#v", result)
	}
	if session.AdapterRestarts() != 1 {
		t.Fatalf("adapter restarts = %d, want 1", session.AdapterRestarts())
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
