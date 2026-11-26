package controller

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/labels"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/storetest"
)

type permissionHoldFixture struct {
	permissionResolutionRetryFixture
	dispatcher *ACPDispatcher
	events     *storetest.FakeExecutionEventStore
	live       *liveevents.Subscription

	mu      sync.Mutex
	answers []harnessv2.PermissionDecision
}

func newPermissionHoldFixture(t *testing.T) *permissionHoldFixture {
	t.Helper()
	base := newPermissionResolutionRetryFixture()
	base.task.Annotations = map[string]string{labels.AnnotationRequestedBySource: labels.RequestedBySourceAPI}
	base.task.Spec.RequestedBy = &corev1alpha1.RequestedBy{Issuer: "https://issuer.example", Subject: "alice"}
	base.task.Spec.AgentRuntime = &corev1alpha1.AgentRuntimeSpec{PermissionDecisions: corev1alpha1.PermissionDecisionsRequester}
	base.event.PermissionRequested.Title = "Run native-read"
	base.event.PermissionRequested.ToolCallID = "tool-call-v1-sha256-" + "ab"
	base.event.PermissionRequested.ExpiresAt = time.Now().UTC().Add(5 * time.Minute)
	broker := liveevents.NewBroker(32)
	fixture := &permissionHoldFixture{
		permissionResolutionRetryFixture: base,
		events:                           storetest.NewFakeExecutionEventStore(),
		live:                             broker.Subscribe(base.task.Namespace, base.task.Name),
	}
	fixture.dispatcher = &ACPDispatcher{EventStore: fixture.events, LiveEvents: broker, permissionHoldPollInterval: 100 * time.Millisecond}
	return fixture
}

func (f *permissionHoldFixture) client(t *testing.T) *harnessv2.Client {
	t.Helper()
	return permissionResolutionRetryClient(t, func(context.Context, string) error { return nil }, func(request *http.Request) (*http.Response, error) {
		got := readPermissionResolutionRetryRequest(t, request)
		f.mu.Lock()
		f.answers = append(f.answers, got.Decision)
		f.mu.Unlock()
		return permissionResolutionRetryResponse(t, got), nil
	})
}

func (f *permissionHoldFixture) resolve(t *testing.T, ctx, holdCtx context.Context) {
	t.Helper()
	if err := f.dispatcher.resolvePromptPermission(ctx, holdCtx, f.client(t), "permission-runtime-session",
		f.task, f.fence, f.policy, "claude", f.event); err != nil {
		t.Fatalf("resolve permission: %v", err)
	}
}

func (f *permissionHoldFixture) answered() []harnessv2.PermissionDecision {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]harnessv2.PermissionDecision(nil), f.answers...)
}

func (f *permissionHoldFixture) approval(t *testing.T) approvals.Approval {
	t.Helper()
	listed, err := approvals.ListEvents(context.Background(), f.events, f.task.Namespace, f.task.Name)
	if err != nil {
		t.Fatal(err)
	}
	derived := approvals.Derive(listed, time.Time{})
	if len(derived) != 1 {
		t.Fatalf("approvals = %d, want 1", len(derived))
	}
	return derived[0]
}

func (f *permissionHoldFixture) decide(t *testing.T, id, eventType string) error {
	t.Helper()
	content, _ := json.Marshal(map[string]string{"approvalID": id, "taskUID": string(f.task.UID)})
	_, err := f.events.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace: f.task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: f.task.Name,
		TaskName: f.task.Name, Type: eventType, ToolCallID: id, Content: content,
	})
	return err
}

func (f *permissionHoldFixture) liveStates(t *testing.T) []liveevents.ApprovalState {
	t.Helper()
	var states []liveevents.ApprovalState
	for {
		select {
		case event := <-f.live.C():
			if event.Approval == nil {
				t.Fatalf("unexpected live event %+v", event)
			}
			states = append(states, event.Approval.State)
		default:
			return states
		}
	}
}

func optionDecision(id string) harnessv2.PermissionDecision {
	return harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionSelected, OptionID: id}
}

func TestPermissionHoldAppliesTheRequestersDecision(t *testing.T) {
	for _, test := range []struct {
		name      string
		eventType string
		want      harnessv2.PermissionDecision
	}{
		{name: "approve", eventType: events.ExecutionEventTypeApprovalApproved, want: optionDecision("allow-once")},
		{name: "decline", eventType: events.ExecutionEventTypeApprovalDeclined, want: optionDecision("reject-once")},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newPermissionHoldFixture(t)
				f.resolve(t, context.Background(), context.Background())
				synctest.Wait()
				if got := f.answered(); len(got) != 0 {
					t.Fatalf("answered %v before a decision", got)
				}
				pending := f.approval(t)
				if pending.Status != approvals.StatusPending || pending.Kind != approvals.KindPermission ||
					pending.Binding == nil || pending.TaskUID != string(f.task.UID) || len(pending.Options) != 2 ||
					pending.ToolCallID != f.event.PermissionRequested.ToolCallID || pending.ExpiresAt == nil ||
					!pending.ExpiresAt.Equal(f.event.PermissionRequested.ExpiresAt.Add(-acpPermissionHoldMargin)) {
					t.Fatalf("pending approval = %+v", pending)
				}
				if pending.Binding.PromptID != "permission-prompt" || pending.Binding.TaskAttempt != 2 ||
					pending.Binding.RuntimeSessionUID != "permission-session" || pending.Binding.ControllerEpoch != 7 {
					t.Fatalf("binding = %+v, want the running prompt's identity", pending.Binding)
				}
				if err := f.decide(t, pending.ID, test.eventType); err != nil {
					t.Fatal(err)
				}
				f.dispatcher.permissionHoldWG.Wait()
				if got := f.answered(); len(got) != 1 || got[0] != test.want {
					t.Fatalf("answers = %v, want %v", got, test.want)
				}
				if states := f.liveStates(t); len(states) != 2 || states[0] != liveevents.ApprovalPending ||
					states[1] != liveevents.ApprovalResolved {
					t.Fatalf("live states = %v, want Pending then Resolved", states)
				}
			})
		})
	}
}

// A held request shows the person the call's input live, and never stores it.
func TestPermissionHoldShowsTheCallInputLiveOnly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPermissionHoldFixture(t)
		f.event.PermissionRequested.RawInput = json.RawMessage(`{"command":"go test ./internal/secret-plan"}`)
		f.resolve(t, context.Background(), context.Background())
		synctest.Wait()
		var pending *liveevents.Approval
		select {
		case event := <-f.live.C():
			pending = event.Approval
		default:
		}
		if pending == nil || pending.State != liveevents.ApprovalPending ||
			string(pending.Input) != `{"command":"go test ./internal/secret-plan"}` {
			t.Fatalf("live approval = %+v, want the pending request with the call's input", pending)
		}
		listed, err := f.events.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
			Namespace: f.task.Namespace, StreamType: events.ExecutionEventStreamTypeTask, StreamID: f.task.Name,
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, event := range listed {
			if strings.Contains(string(event.Content), "secret-plan") {
				t.Fatalf("stored %s event carries the call's input: %s", event.Type, event.Content)
			}
		}
		if err := f.decide(t, f.approval(t).ID, events.ExecutionEventTypeApprovalApproved); err != nil {
			t.Fatal(err)
		}
		f.dispatcher.permissionHoldWG.Wait()
	})
}

func TestPermissionHoldRefusesAnUnansweredRequestBeforeTheRuntimeTimesOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPermissionHoldFixture(t)
		started := time.Now()
		f.resolve(t, context.Background(), context.Background())
		f.dispatcher.permissionHoldWG.Wait()
		if elapsed := time.Since(started); elapsed < 5*time.Minute-acpPermissionHoldMargin || elapsed >= 5*time.Minute {
			t.Fatalf("hold ended after %s, want just before the runtime's own timeout", elapsed)
		}
		if got := f.answered(); len(got) != 1 || got[0] != optionDecision("reject-once") {
			t.Fatalf("answers = %v, want one reject", got)
		}
		if status := f.approval(t).Status; status != approvals.StatusExpired {
			t.Fatalf("approval status = %s, want expired", status)
		}
		if states := f.liveStates(t); len(states) != 2 || states[1] != liveevents.ApprovalExpired {
			t.Fatalf("live states = %v, want Pending then Expired", states)
		}
	})
}

func TestPermissionHoldEndsAtTheTaskDeadline(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPermissionHoldFixture(t)
		holdCtx, cancel := context.WithTimeout(context.Background(), time.Minute)
		defer cancel()
		f.resolve(t, context.Background(), holdCtx)
		synctest.Wait()
		if expires := f.approval(t).ExpiresAt; expires == nil || !expires.Equal(time.Now().UTC().Add(time.Minute)) {
			t.Fatalf("expiresAt = %v, want the Task deadline", expires)
		}
		cancel()
		f.dispatcher.permissionHoldWG.Wait()
	})
}

func TestPermissionHoldIsCancelledWhenThePromptEnds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPermissionHoldFixture(t)
		holdCtx, endPrompt := context.WithCancel(context.Background())
		f.resolve(t, context.Background(), holdCtx)
		synctest.Wait()
		endPrompt()
		f.dispatcher.permissionHoldWG.Wait()
		if got := f.answered(); len(got) != 0 {
			t.Fatalf("answers = %v, want none after the prompt ended", got)
		}
		approval := f.approval(t)
		if approval.Status != approvals.StatusCancelled {
			t.Fatalf("approval status = %s, want cancelled", approval.Status)
		}
		// A late decision cannot reopen a cancelled request.
		if err := f.decide(t, approval.ID, events.ExecutionEventTypeApprovalApproved); err == nil {
			t.Fatal("a decision after cancellation was recorded")
		}
		if states := f.liveStates(t); len(states) != 2 || states[1] != liveevents.ApprovalCancelled {
			t.Fatalf("live states = %v, want Pending then Cancelled", states)
		}
	})
}

// refusingCancelStore fails the first cancellations it is asked to record.
type refusingCancelStore struct {
	*storetest.FakeExecutionEventStore
	mu      sync.Mutex
	refuse  int
	refused int
}

func (s *refusingCancelStore) AppendExecutionEventIfAbsent(
	ctx context.Context, event *store.ExecutionEvent, dedupeKey string,
) (*store.ExecutionEvent, bool, error) {
	s.mu.Lock()
	if event.Type == events.ExecutionEventTypeApprovalCancelled && s.refused < s.refuse {
		s.refused++
		s.mu.Unlock()
		return nil, false, errors.New("the store refused the write")
	}
	s.mu.Unlock()
	return s.FakeExecutionEventStore.AppendExecutionEventIfAbsent(ctx, event, dedupeKey)
}

// A cancellation the store refuses at first is recorded once it can be, so
// the durable request does not stay pending with no hold behind it.
func TestPermissionHoldRecordsACancellationTheStoreRefusedAtFirst(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPermissionHoldFixture(t)
		refusing := &refusingCancelStore{FakeExecutionEventStore: f.events, refuse: 2}
		f.dispatcher.EventStore = refusing
		holdCtx, endPrompt := context.WithCancel(context.Background())
		f.resolve(t, context.Background(), holdCtx)
		synctest.Wait()
		endPrompt()
		f.dispatcher.permissionHoldWG.Wait()
		refusing.mu.Lock()
		refused := refusing.refused
		refusing.mu.Unlock()
		if refused != 2 {
			t.Fatalf("refused %d cancellations, want 2", refused)
		}
		if approval := f.approval(t); approval.Status != approvals.StatusCancelled {
			t.Fatalf("approval status = %s, want cancelled", approval.Status)
		}
	})
}

// refusingExpiryStore fails the first expiries it is asked to record.
type refusingExpiryStore struct {
	*storetest.FakeExecutionEventStore
	mu      sync.Mutex
	refuse  int
	refused int
}

func (s *refusingExpiryStore) AppendExecutionEventIfAbsent(
	ctx context.Context, event *store.ExecutionEvent, dedupeKey string,
) (*store.ExecutionEvent, bool, error) {
	s.mu.Lock()
	if event.Type == events.ExecutionEventTypeApprovalExpired && s.refused < s.refuse {
		s.refused++
		s.mu.Unlock()
		return nil, false, errors.New("the store refused the write")
	}
	s.mu.Unlock()
	return s.FakeExecutionEventStore.AppendExecutionEventIfAbsent(ctx, event, dedupeKey)
}

// The runtime hears an expiry only once the store records it, so its answer
// matches what the approvals API reports, also when a decision lands while
// the store refuses the expiry.
func TestPermissionHoldAnswersOnlyARecordedOutcome(t *testing.T) {
	t.Run("expiry recorded late", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newPermissionHoldFixture(t)
			refusing := &refusingExpiryStore{FakeExecutionEventStore: f.events, refuse: 2}
			f.dispatcher.EventStore = refusing
			f.resolve(t, context.Background(), context.Background())
			f.dispatcher.permissionHoldWG.Wait()
			refusing.mu.Lock()
			refused := refusing.refused
			refusing.mu.Unlock()
			if refused != 2 {
				t.Fatalf("refused %d expiries, want 2", refused)
			}
			if got := f.answered(); len(got) != 1 || got[0] != optionDecision("reject-once") {
				t.Fatalf("answers = %v, want one reject", got)
			}
			if status := f.approval(t).Status; status != approvals.StatusExpired {
				t.Fatalf("approval status = %s, want expired", status)
			}
		})
	})
	t.Run("decision while the expiry is refused", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			f := newPermissionHoldFixture(t)
			f.dispatcher.EventStore = &refusingExpiryStore{FakeExecutionEventStore: f.events, refuse: 1 << 20}
			f.resolve(t, context.Background(), context.Background())
			time.Sleep(5 * time.Minute)
			synctest.Wait()
			if got := f.answered(); len(got) != 0 {
				t.Fatalf("answers = %v before any outcome was recorded", got)
			}
			if err := f.decide(t, f.approval(t).ID, events.ExecutionEventTypeApprovalApproved); err != nil {
				t.Fatal(err)
			}
			f.dispatcher.permissionHoldWG.Wait()
			if got := f.answered(); len(got) != 1 || got[0] != optionDecision("allow-once") {
				t.Fatalf("answers = %v, want the approval", got)
			}
		})
	})
}

func TestPermissionHoldDecisionThatWinsTheExpiryRaceIsHonoured(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPermissionHoldFixture(t)
		f.dispatcher.permissionHoldPollInterval = time.Hour
		f.resolve(t, context.Background(), context.Background())
		synctest.Wait()
		// The decision lands after the last poll but before the expiry write.
		if err := f.decide(t, f.approval(t).ID, events.ExecutionEventTypeApprovalApproved); err != nil {
			t.Fatal(err)
		}
		f.dispatcher.permissionHoldWG.Wait()
		if got := f.answered(); len(got) != 1 || got[0] != optionDecision("allow-once") {
			t.Fatalf("answers = %v, want the approval that won", got)
		}
		if status := f.approval(t).Status; status != approvals.StatusApproved {
			t.Fatalf("approval status = %s, want approved", status)
		}
	})
}

func TestPermissionHoldIsSkippedWhenPolicyDecidesOrNobodyCanAnswer(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*permissionHoldFixture)
		want   harnessv2.PermissionDecision
	}{
		{name: "policy mode", mutate: func(f *permissionHoldFixture) {
			f.task.Spec.AgentRuntime.PermissionDecisions = corev1alpha1.PermissionDecisionsPolicy
		}, want: optionDecision("allow-once")},
		{name: "tool outside the policy", mutate: func(f *permissionHoldFixture) {
			f.event.PermissionRequested.ToolName = "native-write"
		}, want: optionDecision("reject-once")},
		{name: "no verified requester", mutate: func(f *permissionHoldFixture) {
			f.task.Annotations = nil
		}, want: optionDecision("reject-once")},
		{name: "requester without subject", mutate: func(f *permissionHoldFixture) {
			f.task.Spec.RequestedBy.Subject = ""
		}, want: optionDecision("reject-once")},
		{name: "no time left to ask", mutate: func(f *permissionHoldFixture) {
			f.event.PermissionRequested.ExpiresAt = time.Now().UTC().Add(acpPermissionHoldMargin)
		}, want: optionDecision("reject-once")},
		{name: "brokered tool approved at the broker", mutate: func(f *permissionHoldFixture) {
			f.policy.ToolPolicy.Tools[0].Source = harnessv2.MCPToolSourceBrokeredBuiltin
			f.policy.ApprovalPolicy.RequiredTools = []string{"native-read"}
		}, want: optionDecision("allow-once")},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newPermissionHoldFixture(t)
				test.mutate(f)
				f.resolve(t, context.Background(), context.Background())
				f.dispatcher.permissionHoldWG.Wait()
				if got := f.answered(); len(got) != 1 || got[0] != test.want {
					t.Fatalf("answers = %v, want %v immediately", got, test.want)
				}
				listed, err := approvals.ListEvents(context.Background(), f.events, f.task.Namespace, f.task.Name)
				if err != nil || len(listed) != 0 {
					t.Fatalf("approval events = %d (%v), want none", len(listed), err)
				}
			})
		})
	}
}

func TestPermissionHoldIgnoresARepeatedRequest(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newPermissionHoldFixture(t)
		holdCtx, endPrompt := context.WithCancel(context.Background())
		f.resolve(t, context.Background(), holdCtx)
		f.resolve(t, context.Background(), holdCtx)
		synctest.Wait()
		if states := f.liveStates(t); len(states) != 1 {
			t.Fatalf("live states = %v, want one hold", states)
		}
		endPrompt()
		f.dispatcher.permissionHoldWG.Wait()
	})
}
