package supervisor

import (
	"context"
	"encoding/json"
	"fmt"
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

func (f *fakeSandboxRunner) run(ctx context.Context, request acp.SandboxExecRequest, workspace sandboxWorkspace) (acp.SandboxExecResult, error) {
	f.calls <- fakeSandboxCall{request: request, workspace: workspace, ctx: ctx}
	if f.blockOn {
		<-ctx.Done()
		return acp.SandboxExecResult{ExitCode: -1}, ctx.Err()
	}
	return f.result, f.err
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
	session.bindSandboxWorkspace("/sessions/session-a/workspace", 20001, 20002)

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
