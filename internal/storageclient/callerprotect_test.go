package storageclient

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/events"
	"github.com/orka-agents/orka/internal/store"
)

// The digest-checking store also records the protected writes the tests below
// send, before the provider handles them.

func (s *digestCheckingStore) UpdateMemory(ctx context.Context, memory *store.Memory) error {
	s.check(ctx, "UpdateMemory", memory)
	return s.Store.UpdateMemory(ctx, memory)
}

func (s *digestCheckingStore) RetryGatewayEvent(ctx context.Context, namespace, id, owner, reason string, nextAttemptAt time.Time) error {
	s.check(ctx, "RetryGatewayEvent", namespace, id, owner, reason, nextAttemptAt)
	return s.Store.RetryGatewayEvent(ctx, namespace, id, owner, reason, nextAttemptAt)
}

func (s *digestCheckingStore) AdmitGatewayEvent(ctx context.Context, admission store.GatewayEventAdmission) (*store.GatewayEvent, bool, error) {
	s.check(ctx, "AdmitGatewayEvent", admission)
	return s.Store.AdmitGatewayEvent(ctx, admission)
}

func (s *digestCheckingStore) CreateGatewayDelivery(ctx context.Context, delivery *store.GatewayDelivery) (*store.GatewayDelivery, bool, error) {
	s.check(ctx, "CreateGatewayDelivery", delivery)
	return s.Store.CreateGatewayDelivery(ctx, delivery)
}

func (s *digestCheckingStore) EnqueueGatewayMessage(ctx context.Context, request store.GatewayMessageEnqueue) (*store.GatewayDelivery, bool, error) {
	s.check(ctx, "EnqueueGatewayMessage", request)
	return s.Store.EnqueueGatewayMessage(ctx, request)
}

func (s *digestCheckingStore) SaveThreatModel(ctx context.Context, model *store.ThreatModel) error {
	s.check(ctx, "SaveThreatModel", model)
	return s.Store.SaveThreatModel(ctx, model)
}

func (s *digestCheckingStore) RecordUsage(ctx context.Context, observation store.UsageObservation) error {
	s.check(ctx, "RecordUsage", observation)
	return s.Store.RecordUsage(ctx, observation)
}

func (s *digestCheckingStore) AppendExecutionEvent(ctx context.Context, event *store.ExecutionEvent) (*store.ExecutionEvent, error) {
	s.check(ctx, "AppendExecutionEvent", event)
	return s.Store.AppendExecutionEvent(ctx, event)
}

// secretText is redacted by Orka's credential redaction.
const secretText = "dial failed: password=correct-horse-battery-staple rejected"

func protectedEvent() *store.ExecutionEvent {
	return &store.ExecutionEvent{
		Namespace: "tenant", StreamType: store.ExecutionEventStreamTypeTask, StreamID: "task", TaskName: "task",
		Type: events.ExecutionEventTypePlanUpdated, Summary: "planned", CreatedAt: callerStamp,
	}
}

// A caller that supplies its own write envelope sends a request the client
// would redact, bound or otherwise prepare: the client does not change it,
// and refuses it with ErrValidation before the provider is called. A record
// the caller passed by pointer is left as it was.
func TestCallerEnvelopeRefusesARequestTheClientWouldProtect(t *testing.T) {
	long := strings.Repeat("r", gatewayReasonBytes+1)
	cases := map[string]struct {
		method string
		write  func(*testing.T, *lossyFixture) (func(*testing.T), error)
	}{
		"memory content with a credential": {"CreateMemory", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			memory := &store.Memory{ID: "mem-caller", Namespace: "tenant", Content: secretText, CreatedAt: callerStamp, UpdatedAt: callerStamp}
			err := f.client.CreateMemory(callerEnvelope(t, t.Context(), f, "CreateMemory", memory), memory)
			return func(t *testing.T) {
				if memory.Content != secretText {
					t.Fatalf("the caller's memory changed: %q", memory.Content)
				}
			}, err
		}},
		"memory update with a credential": {"UpdateMemory", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			memory := &store.Memory{ID: "mem-caller", Namespace: "tenant", Content: secretText, UpdatedAt: callerStamp}
			return nil, f.client.UpdateMemory(callerEnvelope(t, t.Context(), f, "UpdateMemory", memory), memory)
		}},
		"gateway reason over its limit": {"RetryGatewayEvent", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			ctx := callerEnvelope(t, t.Context(), f, "RetryGatewayEvent", "tenant", "event", "owner", long, callerStamp)
			return nil, f.client.RetryGatewayEvent(ctx, "tenant", "event", "owner", long, callerStamp)
		}},
		"gateway reason with a credential": {"RetryGatewayEvent", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			ctx := callerEnvelope(t, t.Context(), f, "RetryGatewayEvent", "tenant", "event", "owner", secretText, callerStamp)
			return nil, f.client.RetryGatewayEvent(ctx, "tenant", "event", "owner", secretText, callerStamp)
		}},
		"gateway admission with a credential in its state message": {"AdmitGatewayEvent", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			admission := store.GatewayEventAdmission{Event: store.GatewayEvent{Namespace: "tenant", ID: "event", StateMessage: secretText}}
			_, _, err := f.client.AdmitGatewayEvent(callerEnvelope(t, t.Context(), f, "AdmitGatewayEvent", admission), admission)
			return nil, err
		}},
		"gateway delivery error with a credential": {"CreateGatewayDelivery", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			delivery := &store.GatewayDelivery{Namespace: "tenant", ID: "delivery", LastError: secretText}
			_, _, err := f.client.CreateGatewayDelivery(callerEnvelope(t, t.Context(), f, "CreateGatewayDelivery", delivery), delivery)
			return func(t *testing.T) {
				if delivery.LastError != secretText {
					t.Fatalf("the caller's delivery changed: %q", delivery.LastError)
				}
			}, err
		}},
		"gateway message with a credential": {"EnqueueGatewayMessage", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			request := store.GatewayMessageEnqueue{Text: secretText}
			_, _, err := f.client.EnqueueGatewayMessage(callerEnvelope(t, t.Context(), f, "EnqueueGatewayMessage", request), request)
			return nil, err
		}},
		"threat model with a credential": {"SaveThreatModel", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			model := &store.ThreatModel{Namespace: "tenant", Content: secretText}
			err := f.client.SaveThreatModel(callerEnvelope(t, t.Context(), f, "SaveThreatModel", model), model)
			return func(t *testing.T) {
				if model.Content != secretText {
					t.Fatalf("the caller's threat model changed: %q", model.Content)
				}
			}, err
		}},
		"usage model name over its limit": {"RecordUsage", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			observation := store.UsageObservation{Provider: "provider", Model: strings.Repeat("m", maxUsageNameChars+1)}
			return nil, f.client.RecordUsage(callerEnvelope(t, t.Context(), f, "RecordUsage", observation), observation)
		}},
		"execution event with an untrimmed stream": {"AppendExecutionEvent", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			event := protectedEvent()
			event.StreamID = " task "
			_, err := f.client.AppendExecutionEvent(callerEnvelope(t, t.Context(), f, "AppendExecutionEvent", event), event)
			return nil, err
		}},
		"execution event without a time": {"AppendExecutionEvent", func(t *testing.T, f *lossyFixture) (func(*testing.T), error) {
			event := protectedEvent()
			event.CreatedAt = time.Time{}
			_, err := f.client.AppendExecutionEvent(callerEnvelope(t, t.Context(), f, "AppendExecutionEvent", event), event)
			return nil, err
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f, checking := newDigestFixture(t)
			unchanged, err := c.write(t, f)
			if !errors.Is(err, store.ErrValidation) || !strings.Contains(err.Error(), callerEnvelopeProtectRule) {
				t.Fatalf("%s = %v, want ErrValidation naming the protection rule", c.method, err)
			}
			if attempts := checking.attempts(c.method); len(attempts) != 0 {
				t.Fatalf("%s reached the provider %d times; want it refused before", c.method, len(attempts))
			}
			if unchanged != nil {
				unchanged(t)
			}
		})
	}
}

// A caller-owned request the client would not change reaches the provider as
// the caller sent it: the digest recomputed from what the provider receives
// matches the caller's envelope. The provider may refuse it for its own
// reasons; the client does not.
func TestCallerEnvelopeSendsAProtectedRequestUnchanged(t *testing.T) {
	cases := map[string]func(*testing.T, *lossyFixture) error{
		"UpdateMemory": func(t *testing.T, f *lossyFixture) error {
			memory := &store.Memory{ID: "mem-caller", Namespace: "tenant", Content: "nothing secret", UpdatedAt: callerStamp}
			return f.client.UpdateMemory(callerEnvelope(t, t.Context(), f, "UpdateMemory", memory), memory)
		},
		"RetryGatewayEvent": func(t *testing.T, f *lossyFixture) error {
			ctx := callerEnvelope(t, t.Context(), f, "RetryGatewayEvent", "tenant", "event", "owner", "transient", callerStamp)
			return f.client.RetryGatewayEvent(ctx, "tenant", "event", "owner", "transient", callerStamp)
		},
		"AdmitGatewayEvent": func(t *testing.T, f *lossyFixture) error {
			admission := store.GatewayEventAdmission{Event: store.GatewayEvent{
				Namespace: "tenant", ID: "event", StateMessage: "queued", SenderDisplayName: "a sender", Metadata: map[string]string{"channel": "c1"},
			}}
			_, _, err := f.client.AdmitGatewayEvent(callerEnvelope(t, t.Context(), f, "AdmitGatewayEvent", admission), admission)
			return err
		},
		"CreateGatewayDelivery": func(t *testing.T, f *lossyFixture) error {
			delivery := &store.GatewayDelivery{Namespace: "tenant", ID: "delivery", LastError: "timed out", Metadata: map[string]string{}}
			_, _, err := f.client.CreateGatewayDelivery(callerEnvelope(t, t.Context(), f, "CreateGatewayDelivery", delivery), delivery)
			return err
		},
		"EnqueueGatewayMessage": func(t *testing.T, f *lossyFixture) error {
			request := store.GatewayMessageEnqueue{Text: "an update"}
			_, _, err := f.client.EnqueueGatewayMessage(callerEnvelope(t, t.Context(), f, "EnqueueGatewayMessage", request), request)
			return err
		},
		"SaveThreatModel": func(t *testing.T, f *lossyFixture) error {
			model := &store.ThreatModel{Namespace: "tenant", Content: "trust boundaries"}
			return f.client.SaveThreatModel(callerEnvelope(t, t.Context(), f, "SaveThreatModel", model), model)
		},
		"RecordUsage": func(t *testing.T, f *lossyFixture) error {
			observation := store.UsageObservation{Provider: "provider", Model: "model"}
			return f.client.RecordUsage(callerEnvelope(t, t.Context(), f, "RecordUsage", observation), observation)
		},
		"AppendExecutionEvent": func(t *testing.T, f *lossyFixture) error {
			event := protectedEvent()
			_, err := f.client.AppendExecutionEvent(callerEnvelope(t, t.Context(), f, "AppendExecutionEvent", event), event)
			return err
		},
	}
	for method, write := range cases {
		t.Run(method, func(t *testing.T) {
			f, checking := newDigestFixture(t)
			if err := write(t, f); err != nil && strings.Contains(err.Error(), callerEnvelopeProtectRule) {
				t.Fatalf("%s refused a request the client would not change: %v", method, err)
			}
			requireEveryAttemptMatched(t, checking, method, 1)
		})
	}
}
