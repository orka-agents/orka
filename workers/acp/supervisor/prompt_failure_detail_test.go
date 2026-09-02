package supervisor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestPromptFailureErrorDetailRendersRPCErrors(t *testing.T) {
	t.Parallel()
	rpcErr := &acp.RPCError{
		Code:    -32603,
		Message: "provider-secret-must-not-leak http://127.0.0.1:1/_orka/provider/secret-route/v1/chat",
		Data:    json.RawMessage(`{"service":"session","errorName":"UnknownError"}`),
	}
	got := promptFailureErrorDetail(fmt.Errorf("prompt: %w", rpcErr))
	if got != "json-rpc error -32603 session/UnknownError" {
		t.Fatalf("promptFailureErrorDetail() = %q", got)
	}
	if strings.Contains(got, "provider-secret") || strings.Contains(got, "_orka/provider") {
		t.Fatalf("promptFailureErrorDetail() leaked free-text error content: %q", got)
	}
	if got := promptFailureErrorDetail(nil); got != "" {
		t.Fatalf("promptFailureErrorDetail(nil) = %q, want empty", got)
	}
	if got := promptFailureErrorDetail(errors.New("transport closed provider-secret-must-not-leak")); got != "client-error" {
		t.Fatalf("promptFailureErrorDetail(plain) = %q, want stage only", got)
	}
	if got := promptFailureErrorDetail(&acp.RPCError{Code: 1, Message: strings.Repeat("x", 2000)}); got != "json-rpc error 1" {
		t.Fatalf("promptFailureErrorDetail(no data) = %q", got)
	}
}

func TestFailedEventStopReasonNeverEmitsNonFailureReasons(t *testing.T) {
	t.Parallel()
	cases := map[acp.StopReason]harnessv2.ACPStopReason{
		acp.StopReasonCancelled:       harnessv2.ACPStopReasonRefusal,
		acp.StopReasonEndTurn:         harnessv2.ACPStopReasonRefusal,
		acp.StopReasonRefusal:         harnessv2.ACPStopReasonRefusal,
		acp.StopReasonMaxTurnRequests: harnessv2.ACPStopReasonMaxTurnRequests,
		acp.StopReason(""):            harnessv2.ACPStopReason(""),
	}
	for in, want := range cases {
		got := failedEventStopReason(in)
		if got != want {
			t.Fatalf("failedEventStopReason(%q) = %q, want %q", in, got, want)
		}
		if err := (harnessv2.FailedEvent{StopReason: got, Code: "acp_prompt_failed", Message: "x"}).Validate(); err != nil {
			t.Fatalf("Validate(%q) = %v", got, err)
		}
	}
}

func TestProviderProxySessionWaitReturnsOnceInFlightRequestsRelease(t *testing.T) {
	t.Parallel()
	session := &providerProxySession{drained: make(chan struct{})}
	close(session.drained)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := session.wait(ctx); err != nil {
		t.Fatalf("wait with no in-flight requests = %v, want nil", err)
	}
	session.mu.Lock()
	session.drained = make(chan struct{})
	session.inflight = 1
	session.mu.Unlock()
	shortCtx, shortCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer shortCancel()
	if err := session.wait(shortCtx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait with an in-flight request = %v, want deadline exceeded", err)
	}
	session.releaseRequest()
	if err := session.wait(ctx); err != nil {
		t.Fatalf("wait after release = %v, want nil", err)
	}
}
