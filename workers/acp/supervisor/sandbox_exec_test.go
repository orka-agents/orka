package supervisor

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/hyperlight"
)

type fakeSandboxRunner struct {
	calls   chan fakeSandboxCall
	result  acp.SandboxExecResult
	err     error
	blockOn bool
}

type fakeSandboxCall struct {
	request   acp.SandboxExecRequest
	workspace sandboxWorkspace
	ctx       context.Context
}

func (f *fakeSandboxRunner) run(ctx context.Context, request acp.SandboxExecRequest, workspace sandboxWorkspace) (acp.SandboxExecResult, time.Duration, error) {
	f.calls <- fakeSandboxCall{request: request, workspace: workspace, ctx: ctx}
	if f.blockOn {
		<-ctx.Done()
		return acp.SandboxExecResult{ExitCode: -1}, 0, ctx.Err()
	}
	return f.result, 0, f.err
}

func sandboxTestAuthorization(t *testing.T, fence harnessv2.Fence, now time.Time) (harnessv2.PromptMCPAuthorization, harnessv2.PromptLease) {
	t.Helper()
	authorization, lease := buildTestMCPAuthorization(t, fence, now, acp.SandboxExecToolName, harnessv2.MCPToolEffectConsequential, false)
	tool := &authorization.ToolPolicy.Tools[0]
	tool.Source = harnessv2.MCPToolSourceRuntimeLocal
	tool.Description = acp.SandboxExecDescription
	tool.InputSchema = json.RawMessage(acp.SandboxExecInputSchema)
	digest, err := harnessv2.CanonicalMCPToolDescriptorDigest(authorization.ToolPolicy.Tools)
	if err != nil {
		t.Fatal(err)
	}
	authorization.ToolPolicy.DescriptorDigest = digest
	return authorization, lease
}

// newSandboxTestSession is a session whose prompt runs and may call
// sandbox_exec; its broker fails the test if anything reaches it.
func newSandboxTestSession(t *testing.T, runner sandboxRunner) (*mcpProxySession, string) {
	t.Helper()
	broker := MCPBrokerFunc(func(context.Context, harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
		t.Error("a runtime-local call reached the controller broker")
		return harnessv2.MCPBrokerCallResponse{}, fmt.Errorf("unexpected broker call")
	})
	proxy, err := newMCPProxy(broker)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = proxy.close(ctx)
	})
	if runner != nil {
		proxy.sandbox = runner
	}
	fence := harnessv2.Fence{
		RuntimeInstanceID: "runtime-instance", SupervisorBootID: "boot", ControllerEpoch: 2,
		RuntimePoolUID: "pool-uid", RuntimePoolGeneration: 4,
		RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3,
		RuntimeProfileDigest:       harnessv2.ProfileDigest(testDigest("profile")),
		ProfileDigestSchemaVersion: harnessv2.ProfileDigestSchemaVersion,
	}
	now := time.Now().UTC()
	authorization, lease := sandboxTestAuthorization(t, fence, now)
	session, binding, err := proxy.newSession(fence, authorization.Configuration())
	if err != nil {
		t.Fatal(err)
	}
	session.mu.Lock()
	session.credential = []byte("credential")
	session.mu.Unlock()
	session.bindSandboxWorkspace("/sessions/session-a/workspace", 20001, 20002, harnessv2.WorkspaceIntentWrite)

	idle := decodeMCPResponse(t, doMCPRequest(t, binding.URL, "credential", sandboxCall("idle", `{"code":"true"}`)))
	if idle.Error == nil {
		t.Fatalf("a sandbox_exec call ran with no prompt running: %#v", idle)
	}
	if err := session.activate(t.Context(), authorization, lease, now); err != nil {
		t.Fatal(err)
	}
	if err := session.markRunning(authorization.PromptID, now.Add(time.Millisecond)); err != nil {
		t.Fatal(err)
	}
	return session, binding.URL
}

func sandboxCall(id, arguments string) string {
	return `{"jsonrpc":"2.0","id":"` + id + `","method":"tools/call","params":{"name":"sandbox_exec","arguments":` + arguments + `}}`
}

func sandboxToolResult(t *testing.T, response mcpJSONRPCResponse) (string, bool) {
	t.Helper()
	if response.Error != nil {
		t.Fatalf("tool call failed: %#v", response.Error)
	}
	result, ok := response.Result.(map[string]any)
	if !ok {
		t.Fatalf("result = %#v", response.Result)
	}
	content := result["content"].([]any)[0].(map[string]any)
	return content["text"].(string), result["isError"].(bool)
}

func TestSandboxExecRunsInThePodForTheSession(t *testing.T) {
	runner := &fakeSandboxRunner{calls: make(chan fakeSandboxCall, 1), result: acp.SandboxExecResult{Stdout: "hi\n", ExitCode: 0}}
	_, server := newSandboxTestSession(t, runner)

	list := decodeMCPResponse(t, doMCPRequest(t, server, "credential", `{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if list.Error != nil || responseToolCount(list.Result) != 1 || !strings.Contains(fmt.Sprint(list.Result), acp.SandboxExecToolName) {
		t.Fatalf("tools/list = %#v, want sandbox_exec", list)
	}

	text, isError := sandboxToolResult(t, decodeMCPResponse(t, doMCPRequest(t, server, "credential",
		sandboxCall("run", `{"language":"python","code":"print('hi')","timeout":3}`))))
	call := <-runner.calls
	if call.request.Runtime != "python" || call.request.Timeout != 3*time.Second || !strings.Contains(call.request.Script, "print('hi')") {
		t.Fatalf("runner got %+v", call.request)
	}
	if call.workspace != (sandboxWorkspace{dir: "/sessions/session-a/workspace", uid: 20001, gid: 20002}) {
		t.Fatalf("runner ran for %+v, want the session's workspace and user", call.workspace)
	}
	var result acp.SandboxExecResult
	if err := json.Unmarshal([]byte(text), &result); err != nil || result.Stdout != "hi\n" || isError {
		t.Fatalf("result = %q (isError %t), err = %v", text, isError, err)
	}

	runner.result = acp.SandboxExecResult{Stderr: "boom", ExitCode: 2}
	if _, isError := sandboxToolResult(t, decodeMCPResponse(t, doMCPRequest(t, server, "credential", sandboxCall("fail", `{"code":"false"}`)))); !isError {
		t.Fatal("a non-zero exit is not reported as a tool error")
	}
	<-runner.calls

	text, isError = sandboxToolResult(t, decodeMCPResponse(t, doMCPRequest(t, server, "credential", sandboxCall("bad", `{"code":"x","language":"ruby"}`))))
	if !isError || !strings.Contains(text, "ruby") {
		t.Fatalf("invalid arguments = %q (isError %t)", text, isError)
	}

	oversized := `{"code":"` + strings.Repeat("x", harnessv2.MaxMCPArgumentsBytes) + `"}`
	text, isError = sandboxToolResult(t, decodeMCPResponse(t, doMCPRequest(t, server, "credential", sandboxCall("big", oversized))))
	if !isError || !strings.Contains(text, "no larger than") {
		t.Fatalf("oversized arguments = %q (isError %t)", text, isError)
	}
	select {
	case got := <-runner.calls:
		t.Fatalf("invalid arguments reached the runner: %+v", got)
	default:
	}
}

func TestSandboxExecFailsClosedWhenTheRuntimeCannotRunIt(t *testing.T) {
	_, server := newSandboxTestSession(t, nil)
	text, isError := sandboxToolResult(t, decodeMCPResponse(t, doMCPRequest(t, server, "credential", sandboxCall("off", `{"code":"true"}`))))
	if !isError || !strings.Contains(text, "not enabled") {
		t.Fatalf("disabled sandbox_exec = %q (isError %t)", text, isError)
	}

	runner := &fakeSandboxRunner{calls: make(chan fakeSandboxCall, 1), err: fmt.Errorf("%w: no hypervisor device", hyperlight.ErrUnavailable)}
	_, server = newSandboxTestSession(t, runner)
	text, isError = sandboxToolResult(t, decodeMCPResponse(t, doMCPRequest(t, server, "credential", sandboxCall("nodevice", `{"code":"true"}`))))
	<-runner.calls
	if !isError || !strings.Contains(text, "unavailable") {
		t.Fatalf("unavailable sandbox_exec = %q (isError %t)", text, isError)
	}
}

func TestSandboxExecSettlementCancelsTheRun(t *testing.T) {
	runner := &fakeSandboxRunner{calls: make(chan fakeSandboxCall, 1), blockOn: true}
	session, server := newSandboxTestSession(t, runner)
	responseDone := make(chan mcpJSONRPCResponse, 1)
	go func() {
		responseDone <- decodeMCPResponse(t, doMCPRequest(t, server, "credential", sandboxCall("inflight", `{"code":"sleep 60"}`)))
	}()
	var call fakeSandboxCall
	select {
	case call = <-runner.calls:
	case <-time.After(2 * time.Second):
		t.Fatal("sandbox_exec did not start")
	}
	session.deactivate(testPromptOneID, harnessv2.RuntimeSessionStateValidating)
	select {
	case <-call.ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("settlement did not cancel the running micro-VM")
	}
	select {
	case response := <-responseDone:
		if response.Error == nil {
			t.Fatalf("a cancelled sandbox_exec returned a result: %#v", response)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the cancelled call did not return")
	}
}

// The executor, not per-session MCP slots, bounds actual guests for the Pod.
type blockingHyperlightRunner struct {
	calls   chan hyperlight.Request
	release chan struct{}
}

func (r *blockingHyperlightRunner) Run(ctx context.Context, request hyperlight.Request) (hyperlight.Result, error) {
	r.calls <- request
	select {
	case <-r.release:
		return hyperlight.Result{ExitCode: 0}, nil
	case <-ctx.Done():
		return hyperlight.Result{ExitCode: -1}, ctx.Err()
	}
}

func TestSandboxExecutorSerializesGuestsAcrossSessions(t *testing.T) {
	runner := &blockingHyperlightRunner{calls: make(chan hyperlight.Request, 2), release: make(chan struct{}, 2)}
	executor := newSandboxExecutor(&SandboxExecConfig{})
	executor.runner = runner
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	request := acp.SandboxExecRequest{Runtime: "node", Script: "1", Timeout: time.Second}
	done := make(chan error, 2)
	go func() {
		_, _, err := executor.run(ctx, request, sandboxWorkspace{dir: "/session-a", uid: 20001, gid: 20001})
		done <- err
	}()
	select {
	case got := <-runner.calls:
		if got.Mounts[0].Host != "/session-a" {
			t.Fatalf("first workspace = %s", got.Mounts[0].Host)
		}
	case <-ctx.Done():
		t.Fatal("first guest did not start")
	}
	go func() {
		_, _, err := executor.run(ctx, request, sandboxWorkspace{dir: "/session-b", uid: 20002, gid: 20002})
		done <- err
	}()
	select {
	case <-runner.calls:
		t.Fatal("two sessions started simultaneous guests")
	case <-time.After(50 * time.Millisecond):
	}
	runner.release <- struct{}{}
	select {
	case got := <-runner.calls:
		if got.Mounts[0].Host != "/session-b" {
			t.Fatalf("second workspace = %s", got.Mounts[0].Host)
		}
	case <-ctx.Done():
		t.Fatal("queued guest did not start after slot release")
	}
	runner.release <- struct{}{}
	for range 2 {
		if err := <-done; err != nil {
			t.Fatalf("guest execution: %v", err)
		}
	}
}

func TestSandboxExecutorQueuedCallRespectsCancellation(t *testing.T) {
	for _, timedOut := range []bool{false, true} {
		name := "cancelled"
		if timedOut {
			name = "timeout"
		}
		t.Run(name, func(t *testing.T) {
			runner := &blockingHyperlightRunner{calls: make(chan hyperlight.Request, 1), release: make(chan struct{}, 1)}
			executor := newSandboxExecutor(&SandboxExecConfig{})
			executor.runner = runner
			executor.slot <- struct{}{} // another session is already running
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			request := acp.SandboxExecRequest{Runtime: "node", Script: "1", Timeout: 30 * time.Millisecond}
			if !timedOut {
				cancel()
			}
			start := time.Now()
			result, _, err := executor.run(ctx, request, sandboxWorkspace{dir: "/queued"})
			if time.Since(start) > time.Second || result.ExitCode != -1 || result.TimedOut != timedOut {
				t.Fatalf("queued call = %+v error=%v elapsed=%s", result, err, time.Since(start))
			}
			// A call that never got the guest must not read as code that ran too long.
			if timedOut && (!strings.Contains(result.Stderr, "did not start") || !strings.Contains(result.Stderr, "nothing ran")) {
				t.Fatalf("queued timeout stderr = %q, want it to say nothing ran", result.Stderr)
			}
			if timedOut && err != nil || !timedOut && !errors.Is(err, context.Canceled) {
				t.Fatalf("queued cancellation error = %v", err)
			}
			select {
			case <-runner.calls:
				t.Fatal("cancelled queued call started a guest")
			default:
			}
			<-executor.slot
			runner.release <- struct{}{}
			request.Timeout = time.Second
			result, _, err = executor.run(t.Context(), request, sandboxWorkspace{dir: "/later"})
			if err != nil || result.ExitCode != 0 {
				t.Fatalf("slot reuse = %+v error=%v", result, err)
			}
		})
	}
}

func TestSandboxExecutorExpiredCallDoesNotAcquireGuest(t *testing.T) {
	runner := &blockingHyperlightRunner{calls: make(chan hyperlight.Request, 1), release: make(chan struct{}, 1)}
	executor := newSandboxExecutor(&SandboxExecConfig{})
	executor.runner = runner
	ctx, cancel := context.WithDeadline(t.Context(), time.Now().Add(-time.Second))
	defer cancel()
	// Both the free slot and expired context can win the select; every
	// outcome must retain timeout semantics and release any acquired slot.
	for range 20 {
		result, _, err := executor.run(ctx, acp.SandboxExecRequest{Runtime: "node", Script: "1", Timeout: time.Second}, sandboxWorkspace{dir: "/expired"})
		if err != nil || !result.TimedOut || result.ExitCode != -1 || len(executor.slot) != 0 {
			t.Fatalf("expired call = %+v error=%v slots=%d", result, err, len(executor.slot))
		}
	}
	select {
	case <-runner.calls:
		t.Fatal("expired call started a guest")
	default:
	}
}

// The timeout bounds the guest's own run: a call that waited for another
// session's guest still gets its whole timeout once it has the slot.
type deadlineHyperlightRunner struct {
	remaining chan time.Duration
	mounts    chan []hyperlight.Mount
}

func (r *deadlineHyperlightRunner) Run(ctx context.Context, request hyperlight.Request) (hyperlight.Result, error) {
	deadline, _ := ctx.Deadline()
	r.remaining <- time.Until(deadline)
	r.mounts <- request.Mounts
	return hyperlight.Result{ExitCode: 0}, nil
}

func TestSandboxExecutorTimeoutStartsWhenTheGuestDoes(t *testing.T) {
	runner := &deadlineHyperlightRunner{remaining: make(chan time.Duration, 1), mounts: make(chan []hyperlight.Mount, 1)}
	executor := newSandboxExecutor(&SandboxExecConfig{})
	executor.runner = runner
	executor.slot <- struct{}{} // another session's guest is running
	const wait = 300 * time.Millisecond
	time.AfterFunc(wait, func() { <-executor.slot })
	result, queued, err := executor.run(t.Context(), acp.SandboxExecRequest{Runtime: "python", Script: "1", Timeout: time.Second},
		sandboxWorkspace{dir: "/queued"})
	if err != nil || result.ExitCode != 0 || result.TimedOut {
		t.Fatalf("queued call = %+v error=%v", result, err)
	}
	if queued < wait {
		t.Fatalf("queued = %s, want at least the %s the slot was held", queued, wait)
	}
	if remaining := <-runner.remaining; remaining < 900*time.Millisecond {
		t.Fatalf("the guest started with %s of its 1s timeout left; the wait was charged to the run", remaining)
	}
	<-runner.mounts
}

// Only a write-intent session's guests may change the workspace; any other
// intent, including an unset one, mounts it read-only.
func TestSandboxExecMountsTheWorkspaceReadOnlyUnlessTheSessionWrites(t *testing.T) {
	for _, tc := range []struct {
		intent   harnessv2.WorkspaceIntent
		readOnly bool
	}{
		{harnessv2.WorkspaceIntentWrite, false},
		{harnessv2.WorkspaceIntentRead, true},
		{"", true},
	} {
		name := string(tc.intent)
		if name == "" {
			name = "unset"
		}
		t.Run(name, func(t *testing.T) {
			runner := &fakeSandboxRunner{calls: make(chan fakeSandboxCall, 1)}
			session, server := newSandboxTestSession(t, runner)
			session.bindSandboxWorkspace("/sessions/session-a/workspace", 20001, 20002, tc.intent)
			sandboxToolResult(t, decodeMCPResponse(t, doMCPRequest(t, server, "credential", sandboxCall("run", `{"code":"print(1)"}`))))
			if call := <-runner.calls; call.workspace.readOnly != tc.readOnly {
				t.Fatalf("intent %q bound workspace %+v, want readOnly %t", tc.intent, call.workspace, tc.readOnly)
			}

			guest := &deadlineHyperlightRunner{remaining: make(chan time.Duration, 1), mounts: make(chan []hyperlight.Mount, 1)}
			executor := newSandboxExecutor(&SandboxExecConfig{})
			executor.runner = guest
			if _, _, err := executor.run(t.Context(), acp.SandboxExecRequest{Runtime: "python", Script: "1", Timeout: time.Second},
				sandboxWorkspace{dir: "/workspace-host", readOnly: tc.readOnly}); err != nil {
				t.Fatal(err)
			}
			<-guest.remaining
			if mounts := <-guest.mounts; len(mounts) != 1 || mounts[0].ReadOnly != tc.readOnly || mounts[0].Host != "/workspace-host" {
				t.Fatalf("guest mounts = %+v, want only the workspace with readOnly %t", mounts, tc.readOnly)
			}
		})
	}
}

// Admission must reject an unusable frozen policy before creating a workspace
// or consuming a never-reused session identity, including workspace pools.
func TestCreateSessionRejectsDisabledSandboxExec(t *testing.T) {
	for _, workspaceBacked := range []bool{false, true} {
		name := "disabled standard runtime"
		if workspaceBacked {
			name = "workspace-backed runtime"
		}
		t.Run(name, func(t *testing.T) {
			cfg, profile := newTestConfigWithUpstream(t, "immediate", "http://127.0.0.1:1", strings.Repeat("p", 32))
			if workspaceBacked {
				cfg.DurableWorkspaceDir = t.TempDir()
				cfg.DurableWorkspaceKey = "workspace"
				cfg.Capabilities.Limits.MaxResidentSessions = 1
				cfg.Capabilities.Limits.MaxConcurrentPrompts = 1
			}
			authorization, _ := sandboxTestAuthorization(t, cfg.Fence, time.Now().UTC())
			policy := authorization.Configuration()
			profile.ToolPolicyDigest = policy.ToolPolicyDigest
			profile.ApprovalPolicyDigest = policy.ApprovalPolicyDigest
			profile.MCPConfigurationDigest = policy.MCPConfigurationDigest
			digest, err := harnessv2.CanonicalProfileDigest(profile)
			if err != nil {
				t.Fatal(err)
			}
			cfg.Fence.RuntimeProfileDigest = digest
			cfg.Capabilities.RuntimeProfileDigest = digest
			materialized := false
			cfg.WorkspaceMaterializer = WorkspaceMaterializerFunc(func(context.Context, harnessv2.CreateRuntimeSessionRequest, string) error {
				materialized = true
				return errors.New("unexpected workspace setup")
			})
			server, err := New(cfg)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				if err := server.Close(ctx); err != nil {
					t.Error(err)
				}
			})
			request := testCreateSessionRequest(t, cfg, profile)
			request.MCPConfiguration = policy
			request.Metadata.RequestDigest = ""
			sealRequest(t, &request.Metadata.RequestDigest, request)
			if err := request.ValidateAt(time.Now().UTC()); err != nil {
				t.Fatalf("invalid admission fixture: %v", err)
			}
			remaining := cfg.UIDAllocator.Remaining()
			for range 2 {
				response := performMutation(t, server.Handler(), http.MethodPut, "/v2/runtime-sessions/session-1", request, cfg)
				if response.Code != http.StatusBadRequest {
					t.Fatalf("unavailable sandbox admission status=%d body=%s", response.Code, response.Body)
				}
				var rejected harnessv2.ErrorResponse
				if err := json.Unmarshal(response.Body.Bytes(), &rejected); err != nil {
					t.Fatal(err)
				}
				if rejected.Code != harnessv2.ErrorCodeInvalidRequest || rejected.Retryable || !strings.Contains(rejected.Message, "sandbox_exec") {
					t.Fatalf("unavailable sandbox admission error=%#v", rejected)
				}
			}
			if materialized || cfg.UIDAllocator.Remaining() != remaining || len(server.sessions) != 0 || len(server.mcpProxy.sessions) != 0 {
				t.Fatal("rejected sandbox policy created resources or consumed a session identity")
			}
		})
	}
}

type gatedSandboxRunner struct {
	release chan struct{}
	result  acp.SandboxExecResult
}

func (g *gatedSandboxRunner) run(ctx context.Context, _ acp.SandboxExecRequest, _ sandboxWorkspace) (acp.SandboxExecResult, time.Duration, error) {
	select {
	case <-g.release:
		return g.result, 0, nil
	case <-ctx.Done():
		return acp.SandboxExecResult{ExitCode: -1}, 0, ctx.Err()
	}
}

func TestSandboxExecKeepsALongCallAliveForEventStreamClients(t *testing.T) {
	keepalive := sandboxExecKeepalive
	sandboxExecKeepalive = time.Millisecond
	t.Cleanup(func() { sandboxExecKeepalive = keepalive })
	runner := &gatedSandboxRunner{release: make(chan struct{}), result: acp.SandboxExecResult{Stdout: "done\n"}}
	_, server := newSandboxTestSession(t, runner)

	request, err := http.NewRequest(http.MethodPost, server, strings.NewReader(sandboxCall("long", `{"code":"print('done')"}`)))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer credential")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want an event stream", got)
	}
	// Headers and keepalives arrive while the guest still runs.
	reader := bufio.NewReader(response.Body)
	if line, err := reader.ReadString('\n'); err != nil || line != ": keepalive\n" {
		t.Fatalf("first line = %q, %v; want a keepalive before the result", line, err)
	}
	close(runner.release)
	rest, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	events := strings.Split(strings.TrimSpace(string(rest)), "\n\n")
	last := events[len(events)-1]
	data, found := strings.CutPrefix(last, "event: message\ndata: ")
	if !found || strings.Contains(data, "\n") {
		t.Fatalf("last event = %q, want one message event", last)
	}
	var decoded mcpJSONRPCResponse
	if err := json.Unmarshal([]byte(data), &decoded); err != nil {
		t.Fatal(err)
	}
	text, isError := sandboxToolResult(t, decoded)
	if isError || !strings.Contains(text, `"stdout":"done\n"`) {
		t.Fatalf("result = %q (isError %t)", text, isError)
	}
}
