package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const acpPromptStreamTestCancellationTimeout = 10 * time.Millisecond

func TestStreamACPPromptWithCancellationAlreadyCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newACPPromptStreamTestFixture(t)
		transport := newACPPromptStreamTestTransport(t, fixture, nil, nil)
		runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
		cancelRuntime()
		emitted := false
		summary, cancellation, err := streamACPPromptWithCancellation(
			runtimeCtx, transport.client, "runtime-session-1", fixture.request, fixture.task, fixture.fence,
			acpPromptStreamTestCancellationTimeout, func(harnessv2.Event) error { emitted = true; return nil },
		)
		if err == nil || summary.Accepted || summary.Terminal != nil || cancellation != nil || emitted {
			t.Fatalf("already cancelled execution reached prompt processing: accepted=%t terminal=%t cancellation=%t emitted=%t error=%v",
				summary.Accepted, summary.Terminal != nil, cancellation != nil, emitted, err)
		}
		if !summary.WriteEvidence.SafeToResendSameIdentity() || summary.WriteEvidence.State != harnessv2.RequestWriteZeroBytes {
			t.Fatal("already cancelled execution did not retain zero-write evidence")
		}
		transport.assertCalls(t, 0, 0)
	})
}

func TestStreamACPPromptWithCancellationBeforeAccepted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		fixture := newACPPromptStreamTestFixture(t)
		transport := newACPPromptStreamTestTransport(t, fixture, nil, nil)
		runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
		defer cancelRuntime()
		result := runACPPromptStreamTest(runtimeCtx, transport, fixture, func(harnessv2.Event) error {
			t.Error("pre-acceptance cancellation emitted an event")
			return nil
		})
		promptCtx := <-transport.opened
		started := time.Now()
		cancelRuntime()
		got := <-result
		if !errors.Is(got.err, context.Canceled) || got.summary.Accepted || got.summary.Terminal != nil || got.cancellation != nil {
			t.Fatalf("pre-acceptance cancellation did not abort transport without cancellation mutation: accepted=%t terminal=%t cancellation=%t error=%v",
				got.summary.Accepted, got.summary.Terminal != nil, got.cancellation != nil, got.err)
		}
		if promptCtx.Err() != context.Canceled || time.Since(started) != 0 {
			t.Fatalf("pre-acceptance transport was retained: context error=%v elapsed=%s", promptCtx.Err(), time.Since(started))
		}
		transport.assertCalls(t, 1, 0)
	})
}

func TestStreamACPPromptWithCancellationBoundsCompletedDrain(t *testing.T) {
	for _, terminal := range []bool{false, true} {
		name := "missing terminal"
		if terminal {
			name = "missing clean EOF"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newACPPromptStreamTestFixture(t)
				transport := newACPPromptStreamTestTransport(t, fixture, nil, func(*http.Request) (*http.Response, error) {
					return acpPromptStreamTestCancellationResponse(t, harnessv2.EventCompleted), nil
				})
				runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
				defer cancelRuntime()
				accepted := make(chan struct{})
				result := runACPPromptStreamTest(runtimeCtx, transport, fixture, func(event harnessv2.Event) error {
					if event.Type == harnessv2.EventAccepted {
						close(accepted)
					}
					return nil
				})
				promptCtx := <-transport.opened
				transport.writeEvent(t, fixture.accepted)
				<-accepted
				started := time.Now()
				cancelRuntime()
				<-transport.cancels
				if terminal {
					transport.writeEvent(t, fixture.completed)
				}
				got := <-result
				if !errors.Is(got.err, context.Canceled) || !got.summary.Accepted || (got.summary.Terminal != nil) != terminal {
					t.Fatalf("incomplete stream lost its original summary or became successful: accepted=%t terminal=%t error=%v",
						got.summary.Accepted, got.summary.Terminal != nil, got.err)
				}
				assertACPPromptStreamTestCompletedCancellation(t, got.cancellation)
				if time.Since(started) != acpPromptStreamTestCancellationTimeout || promptCtx.Err() != context.Canceled {
					t.Fatalf("completed stream drain exceeded its private bound: elapsed=%s context error=%v", time.Since(started), promptCtx.Err())
				}
				transport.assertCalls(t, 1, 1)
			})
		})
	}
}

func TestStreamACPPromptWithCancellationAbortsWithoutCompletedProof(t *testing.T) {
	for _, test := range []struct {
		name        string
		terminal    harnessv2.EventType
		preflight   bool
		transport   bool
		wantFailure bool
	}{
		{name: "cancellation preflight failure", preflight: true, wantFailure: true},
		{name: "uncertain cancellation write", transport: true, wantFailure: true},
		{name: "unproven settlement", terminal: harnessv2.EventOutcomeUnknown},
		{name: "proven cancellation", terminal: harnessv2.EventCancelled},
		{name: "proven failure", terminal: harnessv2.EventFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newACPPromptStreamTestFixture(t)
				transport := newACPPromptStreamTestTransport(t, fixture, func(_ context.Context, operation string) error {
					if operation == "cancel_prompt" && test.preflight {
						return errors.New("synthetic cancellation preflight rejection")
					}
					return nil
				}, func(*http.Request) (*http.Response, error) {
					if test.transport {
						return nil, io.ErrUnexpectedEOF
					}
					return acpPromptStreamTestCancellationResponse(t, test.terminal), nil
				})
				runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
				defer cancelRuntime()
				accepted := make(chan struct{})
				result := runACPPromptStreamTest(runtimeCtx, transport, fixture, func(event harnessv2.Event) error {
					if event.Type == harnessv2.EventAccepted {
						close(accepted)
					}
					return nil
				})
				promptCtx := <-transport.opened
				transport.writeEvent(t, fixture.accepted)
				<-accepted
				started := time.Now()
				cancelRuntime()
				got := <-result
				if !errors.Is(got.err, context.Canceled) || !got.summary.Accepted || got.summary.Terminal != nil || got.cancellation == nil {
					t.Fatalf("silent accepted stream was not aborted: accepted=%t terminal=%t cancellation=%t error=%v",
						got.summary.Accepted, got.summary.Terminal != nil, got.cancellation != nil, got.err)
				}
				if (got.cancellation.err != nil) != test.wantFailure {
					t.Fatalf("cancellation error=%v; want failure=%t", got.cancellation.err, test.wantFailure)
				}
				if !test.wantFailure && (got.cancellation.response == nil || got.cancellation.response.Settlement.TerminalEvent != test.terminal) {
					t.Fatal("helper discarded the noncompleted cancellation settlement")
				}
				if time.Since(started) != 0 || promptCtx.Err() != context.Canceled {
					t.Fatalf("helper retained transport without completed proof: elapsed=%s context error=%v", time.Since(started), promptCtx.Err())
				}
				wantCancelCalls := int32(1)
				if test.preflight {
					wantCancelCalls = 0
				}
				transport.assertCalls(t, 1, wantCancelCalls)
			})
		})
	}
}

func TestStreamACPPromptWithCancellationValidatesCompletedStream(t *testing.T) {
	for _, test := range []struct {
		name       string
		trailing   bool
		malformed  bool
		disconnect bool
		wantError  error
	}{
		{name: "clean EOF"},
		{name: "trailing event", trailing: true, wantError: harnessv2.ErrEventAfterTerminal},
		{name: "malformed trailing frame", malformed: true, wantError: harnessv2.ErrMalformedEvent},
		{name: "disconnect after terminal", disconnect: true, wantError: harnessv2.ErrPromptStreamDisconnected},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fixture := newACPPromptStreamTestFixture(t)
				transport := newACPPromptStreamTestTransport(t, fixture, nil, func(request *http.Request) (*http.Response, error) {
					if request.Context().Err() != nil {
						t.Error("execution cancellation propagated into cancellation mutation context")
					}
					return acpPromptStreamTestCancellationResponse(t, harnessv2.EventCompleted), nil
				})
				runtimeCtx, cancelRuntime := context.WithCancel(t.Context())
				defer cancelRuntime()
				accepted := make(chan struct{})
				emitted := 0
				result := runACPPromptStreamTest(runtimeCtx, transport, fixture, func(event harnessv2.Event) error {
					emitted++
					if event.Type == harnessv2.EventAccepted {
						// Status updates must not change the frozen cancellation owner.
						fixture.task.UID = "next-task-uid"
						fixture.task.Status.Execution.Attempt++
						fixture.task.Status.Execution.PromptID = "next-prompt"
						close(accepted)
					}
					return nil
				})
				<-transport.opened
				transport.writeEvent(t, fixture.accepted)
				<-accepted
				started := time.Now().UTC()
				cancelRuntime()
				cancelRequest := <-transport.cancels
				if cancelRequest.Metadata.Fence != fixture.request.Metadata.Fence ||
					cancelRequest.Metadata.TaskUID != fixture.request.Metadata.TaskUID ||
					cancelRequest.Metadata.TaskAttempt != fixture.request.Metadata.TaskAttempt ||
					cancelRequest.Metadata.PromptID != fixture.request.Metadata.PromptID ||
					cancelRequest.Metadata.OperationID != harnessv2.OperationID("cancel-prompt-"+string(fixture.request.Metadata.PromptID)) ||
					cancelRequest.Reason != harnessv2.CancelReasonControllerShutdown {
					t.Fatal("cancellation changed the original operation identity, fences, or reason")
				}
				if !cancelRequest.Metadata.ExpiresAt.Equal(started.Add(30*time.Second)) ||
					!cancelRequest.SettlementDeadline.Equal(started.Add(20*time.Second)) {
					t.Fatal("cancellation changed its sealed request or settlement deadlines")
				}
				if err := cancelRequest.ValidateAt(started); err != nil {
					t.Fatalf("cancellation identity or digest is invalid: %v", err)
				}
				transport.writeEvent(t, fixture.completed)
				if test.trailing {
					trailing := fixture.completed
					trailing.Identity.Sequence++
					transport.writeEvent(t, trailing)
				}
				if test.malformed {
					if _, err := io.WriteString(transport.writer, "{not-json}\n"); err != nil {
						t.Fatalf("write trailing frame: %v", err)
					}
				}
				if test.disconnect {
					if err := transport.writer.CloseWithError(io.ErrUnexpectedEOF); err != nil {
						t.Fatal(err)
					}
				} else if err := transport.writer.Close(); err != nil {
					t.Fatal(err)
				}
				got := <-result
				if test.wantError == nil && got.err != nil || test.wantError != nil && !errors.Is(got.err, test.wantError) {
					t.Fatalf("stream error=%v; want %v", got.err, test.wantError)
				}
				if !got.summary.Accepted || got.summary.Terminal == nil || got.summary.EventCount != 2 || emitted != 2 ||
					got.summary.Terminal.Completed == nil || got.summary.Terminal.Completed.Result.Content[0].Text != "original completed result" {
					t.Fatal("helper did not preserve the original validated terminal content and event count")
				}
				assertACPPromptStreamTestCompletedCancellation(t, got.cancellation)
				transport.assertCalls(t, 1, 1)
			})
		})
	}
}

type acpPromptStreamTestFixture struct {
	task      *corev1alpha1.Task
	fence     harnessv2.Fence
	request   harnessv2.StartPromptRequest
	accepted  harnessv2.Event
	completed harnessv2.Event
}

func newACPPromptStreamTestFixture(t *testing.T) acpPromptStreamTestFixture {
	t.Helper()
	owner := newPermissionResolutionRetryFixture()
	now := time.Now().UTC()
	metadata := mutationMetadata(owner.fence, owner.task, "start-prompt", true, now.Add(30*time.Second))
	lease := harnessv2.PromptLease{Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute)}
	toolPolicy := owner.policy.ToolPolicy
	var err error
	toolPolicy.DescriptorDigest, err = harnessv2.CanonicalMCPToolDescriptorDigest(toolPolicy.Tools)
	if err != nil {
		t.Fatal(err)
	}
	authorization := harnessv2.PromptMCPAuthorization{
		RuntimeSessionUID: owner.fence.RuntimeSessionUID, SessionGeneration: owner.fence.RuntimeSessionGeneration,
		TaskUID: metadata.TaskUID, TaskAttempt: metadata.TaskAttempt, PromptID: metadata.PromptID,
		LeaseGeneration: lease.Generation, ToolPolicy: toolPolicy, ExpiresAt: metadata.ExpiresAt,
	}
	authorization.ToolPolicyDigest, err = harnessv2.CanonicalRuntimeToolPolicyDigest(toolPolicy.AllowedToolNames, toolPolicy.DisallowedToolNames, toolPolicy.AllowBash)
	if err != nil {
		t.Fatal(err)
	}
	authorization.ApprovalPolicyDigest, err = harnessv2.CanonicalMCPApprovalPolicyDigest(authorization.ApprovalPolicy)
	if err != nil {
		t.Fatal(err)
	}
	authorization.MCPConfigurationDigest, err = harnessv2.CanonicalMCPConfigurationDigest(toolPolicy.AllowedToolNames)
	if err != nil {
		t.Fatal(err)
	}
	request := harnessv2.StartPromptRequest{
		Protocol: harnessv2.ProtocolVersion, Metadata: metadata, Lease: lease, MCPAuthorization: authorization,
		Input: harnessv2.PromptInput{Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "test prompt"}}},
	}
	if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
		t.Fatal(err)
	}
	identity := harnessv2.EventIdentity{
		RuntimeInstanceID: owner.fence.RuntimeInstanceID, SupervisorBootID: owner.fence.SupervisorBootID,
		RuntimeSessionUID: owner.fence.RuntimeSessionUID, RuntimeSessionGeneration: owner.fence.RuntimeSessionGeneration,
		TaskUID: metadata.TaskUID, TaskAttempt: metadata.TaskAttempt, PromptID: metadata.PromptID,
		Sequence: 1, RequestDigest: request.Metadata.RequestDigest, Timestamp: now,
	}
	accepted := harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventAccepted, Identity: identity,
		Accepted: &harnessv2.AcceptedEvent{AcceptedAt: now, Lease: lease, ACPVersion: harnessv2.ACPProfileV1},
	}
	identity.Sequence++
	completed := harnessv2.Event{
		Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventCompleted, Identity: identity,
		Completed: &harnessv2.CompletedEvent{StopReason: harnessv2.ACPStopReasonEndTurn,
			Result: harnessv2.PromptResult{Content: []harnessv2.ContentBlock{{Type: harnessv2.ContentBlockText, Text: "original completed result"}}}},
	}
	return acpPromptStreamTestFixture{task: owner.task, fence: owner.fence, request: request, accepted: accepted, completed: completed}
}

type acpPromptStreamTestTransport struct {
	client      *harnessv2.Client
	writer      *io.PipeWriter
	opened      chan context.Context
	cancels     chan harnessv2.CancelPromptRequest
	promptCalls atomic.Int32
	cancelCalls atomic.Int32
}

func newACPPromptStreamTestTransport(
	t *testing.T,
	fixture acpPromptStreamTestFixture,
	beforeMutation func(context.Context, string) error,
	cancel permissionResolutionRetryTransport,
) *acpPromptStreamTestTransport {
	t.Helper()
	reader, writer := io.Pipe()
	t.Cleanup(func() { _ = writer.Close(); _ = reader.Close() })
	transport := &acpPromptStreamTestTransport{
		writer: writer, opened: make(chan context.Context, 1), cancels: make(chan harnessv2.CancelPromptRequest, 1),
	}
	if beforeMutation == nil {
		beforeMutation = func(context.Context, string) error { return nil }
	}
	transport.client = permissionResolutionRetryClient(t, beforeMutation, func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodPut {
			return nil, errors.New("unexpected prompt mutation method")
		}
		path := "/v2/runtime-sessions/runtime-session-1/prompts/" + string(fixture.request.Metadata.PromptID)
		switch request.URL.Path {
		case path:
			transport.promptCalls.Add(1)
			context.AfterFunc(request.Context(), func() { _ = reader.CloseWithError(request.Context().Err()) })
			transport.opened <- request.Context()
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {harnessv2.NDJSONMediaType}}, Body: reader}, nil
		case path + "/cancel":
			transport.cancelCalls.Add(1)
			var decoded harnessv2.CancelPromptRequest
			if err := json.NewDecoder(request.Body).Decode(&decoded); err != nil {
				return nil, err
			}
			if err := decoded.ValidateAt(time.Now().UTC()); err != nil {
				return nil, err
			}
			transport.cancels <- decoded
			if cancel == nil {
				return nil, errors.New("unexpected cancellation mutation")
			}
			return cancel(request)
		default:
			return nil, errors.New("unexpected prompt mutation target")
		}
	})
	return transport
}

func (transport *acpPromptStreamTestTransport) writeEvent(t *testing.T, event harnessv2.Event) {
	t.Helper()
	if err := json.NewEncoder(transport.writer).Encode(event); err != nil {
		t.Fatalf("write prompt event: %v", err)
	}
}

func (transport *acpPromptStreamTestTransport) assertCalls(t *testing.T, prompts, cancellations int32) {
	t.Helper()
	if transport.promptCalls.Load() != prompts || transport.cancelCalls.Load() != cancellations {
		t.Fatalf("prompt submissions=%d cancellation submissions=%d; want %d and %d",
			transport.promptCalls.Load(), transport.cancelCalls.Load(), prompts, cancellations)
	}
}

type acpPromptStreamTestResult struct {
	summary      harnessv2.PromptStreamSummary
	cancellation *acpPromptCancellationResult
	err          error
}

func runACPPromptStreamTest(
	runtimeCtx context.Context,
	transport *acpPromptStreamTestTransport,
	fixture acpPromptStreamTestFixture,
	emit func(harnessv2.Event) error,
) <-chan acpPromptStreamTestResult {
	result := make(chan acpPromptStreamTestResult, 1)
	go func() {
		summary, cancellation, err := streamACPPromptWithCancellation(
			runtimeCtx, transport.client, "runtime-session-1", fixture.request, fixture.task, fixture.fence,
			acpPromptStreamTestCancellationTimeout, emit,
		)
		result <- acpPromptStreamTestResult{summary: summary, cancellation: cancellation, err: err}
	}()
	return result
}

func acpPromptStreamTestCancellationResponse(t *testing.T, terminal harnessv2.EventType) *http.Response {
	t.Helper()
	response := harnessv2.CancelPromptResponse{
		Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
		BarrierState: harnessv2.CancellationBarrierSettled, SettlementProven: true,
		Settlement: harnessv2.PromptSettlement{TerminalEvent: terminal, SettledAt: time.Now().UTC()},
	}
	switch terminal {
	case harnessv2.EventCompleted:
		response.Settlement.Outcome, response.Settlement.StopReason = harnessv2.PromptOutcomeSucceeded, harnessv2.ACPStopReasonEndTurn
	case harnessv2.EventCancelled:
		response.Settlement.Outcome, response.Settlement.StopReason = harnessv2.PromptOutcomeCancelled, harnessv2.ACPStopReasonCancelled
	case harnessv2.EventFailed:
		response.Settlement.Outcome, response.Settlement.StopReason = harnessv2.PromptOutcomeFailed, harnessv2.ACPStopReasonMaxTokens
	case harnessv2.EventOutcomeUnknown:
		response.BarrierState, response.SettlementProven = harnessv2.CancellationBarrierOutcomeUnknown, false
		response.Settlement.Outcome = harnessv2.PromptOutcomeUnknown
	}
	body, err := json.Marshal(response)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}
}

func assertACPPromptStreamTestCompletedCancellation(t *testing.T, cancellation *acpPromptCancellationResult) {
	t.Helper()
	if cancellation == nil || cancellation.err != nil || cancellation.response == nil || !cancellation.response.SettlementProven ||
		cancellation.response.Settlement.TerminalEvent != harnessv2.EventCompleted {
		t.Fatal("helper did not retain its proven completed cancellation response")
	}
}
