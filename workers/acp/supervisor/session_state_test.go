package supervisor

import (
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// TestMain checks every RuntimeSession state write made by the supervisor
// test suite against the published harness v2 lifecycle table.
func TestMain(m *testing.M) {
	var (
		mu         sync.Mutex
		violations = map[string]struct{}{}
	)
	sessionStateTransitionHook = func(from, to harnessv2.RuntimeSessionState) {
		if harnessv2.RuntimeSessionTransitionAllowed(from, to) {
			return
		}
		location := "unknown"
		// Skip the hook and setSessionState to report the writing call site.
		if _, file, line, ok := runtime.Caller(2); ok {
			location = fmt.Sprintf("%s:%d", file, line)
		}
		mu.Lock()
		violations[fmt.Sprintf("%s -> %s at %s", from, to, location)] = struct{}{}
		mu.Unlock()
	}
	code := m.Run()
	mu.Lock()
	defer mu.Unlock()
	if len(violations) > 0 {
		fmt.Fprintln(os.Stderr, "RuntimeSession transitions not allowed by the harness v2 lifecycle table:")
		for violation := range violations {
			fmt.Fprintln(os.Stderr, "  "+violation)
		}
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

// The supervisor suite exercises many lifecycle writes only as root. These
// cases drive the settlement and deletion writers directly so the transitions
// they make are checked against the lifecycle table in every environment.
func TestSupervisorLifecycleWritersFollowHarnessTable(t *testing.T) {
	now := time.Now().UTC()
	newPrompt := func() *promptState {
		return &promptState{request: harnessv2.StartPromptRequest{Metadata: harnessv2.MutationMetadata{
			OperationID: "prompt-operation", ExpiresAt: now.Add(time.Minute),
		}}}
	}
	for _, test := range []struct {
		name  string
		from  harnessv2.RuntimeSessionState
		event harnessv2.EventType
		want  harnessv2.RuntimeSessionState
	}{
		{name: "completed prompt validates", from: harnessv2.RuntimeSessionStatePromptRunning, event: harnessv2.EventCompleted, want: harnessv2.RuntimeSessionStateValidating},
		{name: "failed prompt poisons", from: harnessv2.RuntimeSessionStatePromptRunning, event: harnessv2.EventFailed, want: harnessv2.RuntimeSessionStatePoisoned},
		{name: "prompt completed before cancellation validates", from: harnessv2.RuntimeSessionStateCancelling, event: harnessv2.EventCompleted, want: harnessv2.RuntimeSessionStateValidating},
		{name: "cancelled prompt poisons", from: harnessv2.RuntimeSessionStateCancelling, event: harnessv2.EventCancelled, want: harnessv2.RuntimeSessionStatePoisoned},
	} {
		t.Run("settlement/"+test.name, func(t *testing.T) {
			prompt := newPrompt()
			state := &sessionState{prompt: prompt, descriptor: harnessv2.RuntimeSessionDescriptor{State: test.from}}
			recordPromptSettlementLocked(state, prompt, harnessv2.PromptSettlement{TerminalEvent: test.event, SettledAt: now})
			if state.descriptor.State != test.want {
				t.Fatalf("settled state = %s, want %s", state.descriptor.State, test.want)
			}
			if !harnessv2.RuntimeSessionTransitionAllowed(test.from, test.want) {
				t.Fatalf("lifecycle table rejects supervisor settlement %s -> %s", test.from, test.want)
			}
		})
	}

	for _, test := range []struct {
		name                 string
		from                 harnessv2.RuntimeSessionState
		activePrompt         bool
		publicationFinalized bool
		want                 harnessv2.RuntimeSessionState
	}{
		{name: "idle deletes", from: harnessv2.RuntimeSessionStateIdle, want: harnessv2.RuntimeSessionStateDeleting},
		{name: "poisoned deletes", from: harnessv2.RuntimeSessionStatePoisoned, want: harnessv2.RuntimeSessionStateDeleting},
		{name: "running prompt cancels first", from: harnessv2.RuntimeSessionStatePromptRunning, activePrompt: true, want: harnessv2.RuntimeSessionStateCancelling},
		{name: "finalized publication deletes", from: harnessv2.RuntimeSessionStateFinalizing, publicationFinalized: true, want: harnessv2.RuntimeSessionStateDeleting},
	} {
		t.Run("deletion/"+test.name, func(t *testing.T) {
			state := &sessionState{runtime: &acp.RuntimeSession{}, descriptor: harnessv2.RuntimeSessionDescriptor{State: test.from}}
			if test.activePrompt {
				state.prompt = newPrompt()
			}
			if _, err := prepareSessionDeletionLocked(state, test.publicationFinalized, now); err != nil {
				t.Fatal(err)
			}
			if state.descriptor.State != test.want {
				t.Fatalf("deletion state = %s, want %s", state.descriptor.State, test.want)
			}
			if !harnessv2.RuntimeSessionTransitionAllowed(test.from, test.want) {
				t.Fatalf("lifecycle table rejects supervisor deletion %s -> %s", test.from, test.want)
			}
		})
	}
}
