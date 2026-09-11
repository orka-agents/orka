package controller

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestPromptCancellationRetriesUnsentPreflight(t *testing.T) {
	for _, persistenceFailure := range []bool{false, true} {
		name := "task timeout"
		if persistenceFailure {
			name = "plan persistence failure"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newPromptStreamLifecycleFixture(t, harnessv2.PromptSettlement{})
			preflights, writes := 0, 0
			runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error {
				preflights++
				if preflights < 3 {
					return harnessv2.MarkPreMutationRetryable(errors.New("synthetic authority lookup unavailable"))
				}
				return nil
			}, func(request *http.Request) (*http.Response, error) {
				writes++
				cancelRequest := readCancelRetryRequest(t, request, "prompt-stream-lifecycle-1")
				wantReason := harnessv2.CancelReasonTaskTimeout
				if persistenceFailure {
					wantReason = harnessv2.CancelReasonStreamDisconnected
				}
				if cancelRequest.Metadata.Fence != fixture.runtimeFence || cancelRequest.Metadata.TaskUID != harnessv2.TaskUID(fixture.task.UID) ||
					cancelRequest.Metadata.TaskAttempt != 1 || cancelRequest.Reason != wantReason {
					t.Fatal("cancellation changed the accepted prompt owner or reason")
				}
				return cancelRetryResponse(t), nil
			})
			runtimeContextErr, streamErr := context.DeadlineExceeded, context.DeadlineExceeded
			if persistenceFailure {
				runtimeContextErr = nil
				streamErr = acpUpdatePersistenceError(nil, errors.New("synthetic plan persistence failure"))
			}
			if err := fixture.dispatcher.handlePromptStreamError(
				fixture.ctx, nil, runtimeClient, "runtime-session-1", fixture.task.DeepCopy(), fixture.attemptID,
				fixture.fence, fixture.runtimeFence, fixture.journalState, true, harnessv2.RequestWriteEvidence{}, runtimeContextErr, streamErr,
			); err != nil {
				t.Fatal(err)
			}
			if preflights != 3 || writes != 1 {
				t.Fatalf("preflights = %d, cancellation writes = %d; want 3 and 1", preflights, writes)
			}
			completed := &corev1alpha1.Task{}
			if err := fixture.dispatcher.Client.Get(fixture.ctx, client.ObjectKeyFromObject(fixture.task), completed); err != nil {
				t.Fatal(err)
			}
			wantState, wantReason := corev1alpha1.TaskExecutionStateCancelled, corev1alpha1.TaskExecutionReason(acpTaskTimeoutReason)
			if persistenceFailure {
				wantState, wantReason = corev1alpha1.TaskExecutionStateFailed, acpExecutionEventPersistenceFailureReason
			}
			if completed.Status.Execution == nil || completed.Status.Execution.State != wantState || completed.Status.Execution.Reason != wantReason {
				t.Fatalf("settled original prompt status = %#v", completed.Status.Execution)
			}
			fixture.assertTerminalLifecycle(t, harnessv2.EventCancelled, true)
		})
	}
}

func TestPromptCancellationRetryKeepsSealedIdentity(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		request := cancelRetryRequest(t)
		preflights, writes := 0, 0
		runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error {
			preflights++
			if preflights < 3 {
				return harnessv2.MarkPreMutationRetryable(errors.New("synthetic preflight unavailable"))
			}
			return nil
		}, func(sent *http.Request) (*http.Response, error) {
			writes++
			if got := readCancelRetryRequest(t, sent, string(request.Metadata.PromptID)); !reflect.DeepEqual(got, request) {
				t.Fatal("retry changed original cancellation identity, fence, reason, digest, or deadlines")
			}
			return cancelRetryResponse(t), nil
		})
		response, err := cancelPromptWithUnsentRetry(t.Context(), runtimeClient, "runtime-session-1", request)
		if err != nil || response == nil || !response.SettlementProven || preflights != 3 || writes != 1 {
			t.Fatalf("response=%#v error=%v preflights=%d writes=%d", response, err, preflights, writes)
		}
	})
}

func TestPromptCancellationRetryDoesNotReplayUncertainWrite(t *testing.T) {
	for _, consumed := range []bool{false, true} {
		name := "unknown transport"
		if consumed {
			name = "request consumed before response loss"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				request := cancelRetryRequest(t)
				preflights, writes := 0, 0
				runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error {
					preflights++
					return nil
				}, func(sent *http.Request) (*http.Response, error) {
					writes++
					if consumed {
						_ = readCancelRetryRequest(t, sent, string(request.Metadata.PromptID))
					}
					return nil, io.ErrUnexpectedEOF
				})
				response, err := cancelPromptWithUnsentRetry(t.Context(), runtimeClient, "runtime-session-1", request)
				var clientErr *harnessv2.ClientError
				if response != nil || !errors.As(err, &clientErr) || clientErr.WriteEvidence.SafeToResendSameIdentity() {
					t.Fatalf("uncertain cancellation lost write evidence: response=%#v error=%v", response, err)
				}
				if preflights != 1 || writes != 1 {
					t.Fatalf("uncertain cancellation replayed: preflights=%d writes=%d", preflights, writes)
				}
			})
		})
	}
}

func TestPromptCancellationRetryStopsAtOriginalBound(t *testing.T) {
	for _, callerBound := range []time.Duration{time.Millisecond, 0} {
		t.Run(callerBound.String(), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				request := cancelRetryRequest(t)
				ctx := t.Context()
				wantElapsed := time.Millisecond
				if callerBound == 0 {
					request.SettlementDeadline = time.Now().UTC().Add(wantElapsed)
					if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
						t.Fatal(err)
					}
				} else {
					var cancel context.CancelFunc
					ctx, cancel = context.WithTimeout(ctx, callerBound)
					defer cancel()
				}
				preflights, writes := 0, 0
				runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error {
					preflights++
					return harnessv2.MarkPreMutationRetryable(errors.New("synthetic preflight unavailable"))
				}, func(*http.Request) (*http.Response, error) {
					writes++
					return nil, errors.New("unvalidated cancellation must not reach transport")
				})
				started := time.Now()
				response, err := cancelPromptWithUnsentRetry(ctx, runtimeClient, "runtime-session-1", request)
				if response != nil || !errors.Is(err, context.DeadlineExceeded) || time.Since(started) != wantElapsed {
					t.Fatalf("response=%#v error=%v elapsed=%s", response, err, time.Since(started))
				}
				if preflights != 1 || writes != 0 {
					t.Fatalf("expired cancellation retried: preflights=%d writes=%d", preflights, writes)
				}
			})
		})
	}
}

func TestPromptCancellationRetryPreservesAcknowledgmentGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		request := cancelRetryRequest(t)
		request.SettlementDeadline = time.Now().UTC().Add(time.Second)
		if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		defer cancel()
		writes := 0
		runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error { return nil }, func(sent *http.Request) (*http.Response, error) {
			writes++
			_ = readCancelRetryRequest(t, sent, string(request.Metadata.PromptID))
			response := cancelRetryResponse(t)
			// The runtime proves settlement at its deadline; response delivery
			// may finish later within the original control request grace.
			time.Sleep(2 * time.Second)
			if err := sent.Context().Err(); err != nil {
				return nil, err
			}
			return response, nil
		})
		response, err := cancelPromptWithUnsentRetry(ctx, runtimeClient, "runtime-session-1", request)
		if err != nil || response == nil || !response.SettlementProven || writes != 1 {
			t.Fatalf("acknowledgment grace lost: response=%#v error=%v writes=%d", response, err, writes)
		}
	})
}

func TestPromptCancellationRetryRejectsDefinitiveOrExhaustedPreflight(t *testing.T) {
	for _, retryable := range []bool{false, true} {
		name := "definitive rejection"
		if retryable {
			name = "retry budget exhausted"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				preflights, writes := 0, 0
				runtimeClient := permissionResolutionRetryClient(t, func(context.Context, string) error {
					preflights++
					err := errors.New("synthetic preflight rejected")
					if retryable {
						return harnessv2.MarkPreMutationRetryable(err)
					}
					return err
				}, func(*http.Request) (*http.Response, error) {
					writes++
					return nil, errors.New("rejected cancellation must not reach transport")
				})
				response, err := cancelPromptWithUnsentRetry(t.Context(), runtimeClient, "runtime-session-1", cancelRetryRequest(t))
				var clientErr *harnessv2.ClientError
				if response != nil || !errors.As(err, &clientErr) || clientErr.Retryable != retryable || !clientErr.WriteEvidence.SafeToResendSameIdentity() {
					t.Fatalf("original preflight classification lost: response=%#v error=%v", response, err)
				}
				wantAttempts := 1
				if retryable {
					wantAttempts = retry.DefaultBackoff.Steps
				}
				if preflights != wantAttempts || writes != 0 {
					t.Fatalf("preflights=%d writes=%d; want %d and 0", preflights, writes, wantAttempts)
				}
			})
		})
	}
}

func cancelRetryRequest(t *testing.T) harnessv2.CancelPromptRequest {
	t.Helper()
	fixture := newPermissionResolutionRetryFixture()
	now := time.Now().UTC()
	request := harnessv2.CancelPromptRequest{
		Protocol: harnessv2.ProtocolVersion,
		Metadata: mutationMetadata(fixture.fence, fixture.task, "cancel-prompt", true, now.Add(30*time.Second)),
		Reason:   harnessv2.CancelReasonTaskTimeout, SettlementDeadline: now.Add(20 * time.Second),
	}
	if err := sealMutation(&request.Metadata.RequestDigest, request); err != nil {
		t.Fatal(err)
	}
	return request
}

func readCancelRetryRequest(t *testing.T, request *http.Request, promptID string) harnessv2.CancelPromptRequest {
	t.Helper()
	if request.Method != http.MethodPut || request.URL.Path != "/v2/runtime-sessions/runtime-session-1/prompts/"+promptID+"/cancel" {
		t.Fatal("cancellation changed its original method or target")
	}
	var decoded harnessv2.CancelPromptRequest
	if err := json.NewDecoder(request.Body).Decode(&decoded); err != nil {
		t.Fatal(err)
	}
	if err := decoded.ValidateAt(time.Now().UTC()); err != nil {
		t.Fatalf("cancellation was sent outside its original validity window: %v", err)
	}
	return decoded
}

func cancelRetryResponse(t *testing.T) *http.Response {
	t.Helper()
	body, err := json.Marshal(harnessv2.CancelPromptResponse{
		Protocol: harnessv2.ProtocolVersion, Classification: harnessv2.Classification{Class: harnessv2.RequestClassificationFresh},
		BarrierState: harnessv2.CancellationBarrierSettled, SettlementProven: true,
		Settlement: harnessv2.PromptSettlement{TerminalEvent: harnessv2.EventCancelled, Outcome: harnessv2.PromptOutcomeCancelled,
			StopReason: harnessv2.ACPStopReasonCancelled, SettledAt: time.Now().UTC()},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(string(body)))}
}
