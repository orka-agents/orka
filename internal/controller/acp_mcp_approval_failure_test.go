package controller

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-logr/logr/funcr"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/orka-agents/orka/internal/approvals"
	"github.com/orka-agents/orka/internal/events"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/tools"
	workerexecutor "github.com/orka-agents/orka/internal/worker"
)

const approvalFailureTestToken = "ghp_abcdefghijklmnopqrstuvwxyz0123456789"

type approvalFailureLog struct {
	mu    sync.Mutex
	lines []string
}

func (l *approvalFailureLog) context(t *testing.T) context.Context {
	return logf.IntoContext(t.Context(), funcr.New(func(prefix, args string) {
		l.mu.Lock()
		defer l.mu.Unlock()
		l.lines = append(l.lines, prefix+" "+args)
	}, funcr.Options{}))
}

func (l *approvalFailureLog) failure(t *testing.T, outcome string) string {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, line := range l.lines {
		if strings.Contains(line, "Approved MCP tool call failed") && strings.Contains(line, `"outcome"="`+outcome+`"`) {
			return line
		}
	}
	t.Fatalf("no %s failure was logged: %q", outcome, l.lines)
	return ""
}

func (f *mcpApprovalFixture) approvalEffect() *store.ExternalEffect {
	f.t.Helper()
	id, err := store.ExternalEffectIdentity{
		Kind: acpMCPToolEffectKind, Namespace: f.request.Namespace,
		AggregateID: string(f.request.Authorization.RuntimeSessionUID), OperationID: string(f.request.Metadata.OperationID),
	}.CanonicalID()
	if err != nil {
		f.t.Fatal(err)
	}
	effect, err := f.broker.Effects.GetExternalEffect(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return effect
}

func (f *mcpApprovalFixture) approval() approvals.Approval {
	f.t.Helper()
	listed, err := approvals.ListEvents(f.t.Context(), f.events, f.request.Namespace, "approval-task")
	if err != nil {
		f.t.Fatal(err)
	}
	values := approvals.Derive(listed, time.Time{})
	if len(values) != 1 {
		f.t.Fatalf("approvals = %#v, want one", values)
	}
	return values[0]
}

func TestMCPApprovalDefiniteToolFailureSettlesFailedWithSummary(t *testing.T) {
	f := newMCPApprovalFixture(t)
	const summary = "GitHub refused the request (HTTP 403): Resource not accessible by integration"
	f.broker.Executor = ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
		f.count.Add(1)
		return nil, &tools.NoEffectError{Summary: summary, Err: fmt.Errorf(
			`failed to post comment: GitHub API returned 403: {"message":"Resource not accessible by integration","echo":"Authorization: Bearer %s"}`, approvalFailureTestToken)}
	})
	var logs approvalFailureLog
	done := f.startContext(logs.context(t), f.request)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)

	want := acpApprovalResult(pending.ID, acpApprovalCodeNoEffect, summary)
	if !result.IsError || result.Replayed || string(result.Result) != string(want) || f.count.Load() != 1 {
		t.Fatalf("approved definite failure result=%s replayed=%t executions=%d", result.Result, result.Replayed, f.count.Load())
	}
	if effect := f.approvalEffect(); effect.State != store.ExternalEffectFailed {
		t.Fatalf("effect state = %s, want %s", effect.State, store.ExternalEffectFailed)
	}
	if approval := f.approval(); approval.Status != approvals.StatusApproved ||
		approval.ExecutionOutcome != acpApprovalOutcomeFailed || approval.ExecutionReason != summary {
		t.Fatalf("approval = %#v, want an approved, failed execution with the safe summary", approval)
	}
	line := logs.failure(t, acpApprovalOutcomeFailed)
	if !strings.Contains(line, "Resource not accessible by integration") || strings.Contains(line, approvalFailureTestToken) ||
		!strings.Contains(line, pending.ID) {
		t.Fatalf("failure log is not a redacted cause for the approval: %s", line)
	}

	// Redelivery after a restart replays the receipt and recovery keeps it.
	f.reopen()
	replay := awaitMCPApprovalResult(t, f.start(f.request))
	if !replay.Replayed || string(replay.Result) != string(want) || f.count.Load() != 1 {
		t.Fatalf("redelivery result=%s replayed=%t executions=%d", replay.Result, replay.Replayed, f.count.Load())
	}
	approval := f.approval()
	outcome, reason, _ := mcpApprovalRecoveredOutcome(f.approvalEffect(), pending.ID)
	if outcome != acpApprovalOutcomeFailed || reason != summary || !mcpApprovalProjectionCurrent(approval, outcome, reason) {
		t.Fatalf("recovered outcome=%q reason=%q is not the projected %q/%q", outcome, reason, approval.ExecutionOutcome, approval.ExecutionReason)
	}

	// A waiter that settles after the failure reports it, never "not started".
	secrets, err := f.broker.ApprovalSecrets.CoreV1().Secrets(f.request.Namespace).List(t.Context(), metav1.ListOptions{})
	if err != nil || len(secrets.Items) != 1 {
		t.Fatalf("approval call secrets = %v, %v", secrets, err)
	}
	var call acpMCPApprovalCall
	if err := json.Unmarshal(secrets.Items[0].Data[acpApprovalSecretKey], &call); err != nil {
		t.Fatal(err)
	}
	credentials, err := f.broker.Credentials.ResolveACPMCPBrokerCredentials(t.Context(), f.request)
	if err != nil {
		t.Fatal(err)
	}
	blocked, _, err := f.broker.blockApproval(t.Context(), &call, credentials, acpApprovalCodeStale)
	if approval := f.approval(); err != nil || string(blocked) != string(want) ||
		approval.ExecutionOutcome != acpApprovalOutcomeFailed || approval.ExecutionReason != summary {
		t.Fatalf("blocked result=%s err=%v approval=%#v, want the recorded failure", blocked, err, approval)
	}
}

func TestMCPApprovalUncertainToolFailureStaysUnknownAndIsLogged(t *testing.T) {
	f := newMCPApprovalFixture(t)
	f.broker.Executor = ACPMCPToolExecutorFunc(func(context.Context, harnessv2.MCPBrokerCallRequest, harnessv2.MCPToolDescriptor) (json.RawMessage, error) {
		f.count.Add(1)
		return nil, fmt.Errorf("HTTP request failed after sending with token %s: connection reset", approvalFailureTestToken)
	})
	var logs approvalFailureLog
	done := f.startContext(logs.context(t), f.request)
	pending := f.pending()
	f.decide(pending.ID, events.ExecutionEventTypeApprovalApproved)
	result := awaitMCPApprovalResult(t, done)

	if !result.IsError || !strings.Contains(string(result.Result), acpApprovalCodeUnknown) || f.count.Load() != 1 {
		t.Fatalf("uncertain failure result=%s executions=%d", result.Result, f.count.Load())
	}
	if effect := f.approvalEffect(); effect.State != store.ExternalEffectOutcomeUnknown {
		t.Fatalf("effect state = %s, want %s", effect.State, store.ExternalEffectOutcomeUnknown)
	}
	if approval := f.approval(); approval.ExecutionOutcome != acpApprovalOutcomeUnknown || approval.ExecutionReason != acpMCPApprovalUnknownReason {
		t.Fatalf("approval = %#v, want an unknown outcome", approval)
	}
	line := logs.failure(t, acpApprovalOutcomeUnknown)
	if !strings.Contains(line, "connection reset") || strings.Contains(line, approvalFailureTestToken) {
		t.Fatalf("failure log is not a redacted cause: %s", line)
	}
}

func TestCustomToolNoEffectError(t *testing.T) {
	attempted := func(err error) error { return workerexecutor.ToolRequestAttemptedError{Err: err} }
	status := func(code int) error {
		return workerexecutor.ToolHTTPStatusError{StatusCode: code, Err: fmt.Errorf("tool returned HTTP %d: upstream body", code)}
	}
	for _, test := range []struct {
		name        string
		err         error
		wantSummary string
	}{
		{name: "stopped before sending", err: errors.New("resolve outbound access policy: connection revoked"), wantSummary: "The tool stopped before sending its request."},
		{name: "refused authorization", err: attempted(status(403)), wantSummary: "The tool endpoint refused the request (HTTP 403)."},
		{name: "refused authentication", err: attempted(status(401)), wantSummary: "The tool endpoint refused the request (HTTP 401)."},
		{name: "refused after a redirect", err: attempted(workerexecutor.ToolHTTPStatusError{StatusCode: 403, Redirected: true, Err: errors.New("tool returned HTTP 403")})},
		{name: "request timeout", err: attempted(status(408))},
		{name: "server error", err: attempted(status(502))},
		{name: "transport after sending", err: attempted(errors.New("connection reset"))},
		{name: "delivered as a tool result", err: attempted(workerexecutor.ToolExecutionError{Err: status(404)})},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := customToolNoEffectError(test.err)
			noEffect, ok := errors.AsType[*tools.NoEffectError](got)
			if test.wantSummary == "" {
				if got != test.err {
					t.Fatalf("customToolNoEffectError() = %v, want the error unchanged", got)
				}
				return
			}
			if !ok || noEffect.Summary != test.wantSummary || !errors.Is(got, test.err) || strings.Contains(noEffect.Summary, "upstream body") {
				t.Fatalf("customToolNoEffectError() = %#v, want summary %q wrapping the cause", got, test.wantSummary)
			}
		})
	}
	if customToolNoEffectError(nil) != nil {
		t.Fatal("customToolNoEffectError(nil) is not nil")
	}
}

func TestAcpApprovalFailureSummaryRedactsBeforeBounding(t *testing.T) {
	// Cut at the bound first, the URL would lose its "@" and its credential
	// would no longer be recognized; redacting the token first also moves the
	// URL back inside the bound.
	const exposed = "https://user:pa"
	padding := strings.Repeat("x", acpPromptFailureMessageLimit-len("token=")-len(approvalFailureTestToken)-2-len(exposed))
	summary := "token=" + approvalFailureTestToken + " " + padding + " https://user:password-value@example.com/hook"
	if cut := summary[:acpPromptFailureMessageLimit]; !strings.HasSuffix(cut, exposed) {
		t.Fatalf("fixture cut = %q, want it to end inside the URL credential", cut[len(cut)-20:])
	}
	got := acpApprovalFailureSummary(summary)
	if strings.Contains(got, "user:") || strings.Contains(got, approvalFailureTestToken) || len(got) > acpPromptFailureMessageLimit ||
		!strings.Contains(got, "[REDACTED]@example.com") {
		t.Fatalf("acpApprovalFailureSummary() = %q, want credentials redacted before the bound", got)
	}
	if acpApprovalFailureSummary(" \x00 ") != "The tool failed without any effect." {
		t.Fatal("an empty summary has no fallback")
	}
}
