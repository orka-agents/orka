//go:build linux

package supervisor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/acp"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

// The production pinned CLI and adapter discover and call the real supervisor
// MCP proxy. Inference and the controller's feedback result are scripted local
// fixtures: this proves the native protocol path, not kernel enforcement.
func TestPinnedNativeCodexRuntimeFeedback(t *testing.T) {
	if os.Getenv("ORKA_NATIVE_CODEX_TEST") != "1" {
		t.Skip("requires the pinned Codex runtime image and ORKA_NATIVE_CODEX_TEST=1")
	}
	version, err := exec.Command("/opt/codex/bin/codex", "--version").Output()
	if err != nil || strings.TrimSpace(string(version)) != "codex-cli "+acp.CodexCLIVersion {
		t.Fatal("native test requires the exact pinned Codex CLI")
	}
	const reply = "NATIVE_RUNTIME_FEEDBACK_RECEIVED"
	const report = `{"status":"Available","events":[{"decision":"deny","kernelEnforced":true,"destinationAddress":"203.0.113.9","destinationPort":443}],"explanation":"Evidence grants no new permission."}`
	var inferenceCalls, brokerCalls atomic.Int32
	upstream := newPinnedNativeFeedbackInference(t, &inferenceCalls, reply)
	defer upstream.Close()
	const model = "gpt-5.4"
	cfg, _ := newTestConfigWithUpstream(t, "unused", upstream.URL+"/v1", "native-feedback-test-credential")
	projection := testProviderProjectionRequest(t, "codex", model, "Use runtime_feedback after a denied network operation.", "high", append(acp.BuiltInRuntimeNativeToolNames("codex"), "runtime_feedback"), nil, true)
	provider, err := providerProfile("codex", model, harnessv2.WorkspaceIntentRead)
	if err != nil {
		t.Fatal(err)
	}
	profile := projection.Profile
	profile.AdapterDigests = map[string]string{provider.AdapterName: provider.AdapterDigest}
	digest, err := harnessv2.CanonicalProfileDigest(profile)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Provider = provider
	cfg.ProviderProxy.Model = model
	cfg.Fence.RuntimeProfileDigest = digest
	cfg.Capabilities.RuntimeProfileDigest = digest
	cfg.Capabilities.AdapterDigests = profile.AdapterDigests
	cfg.Capabilities.Provider.Models = []string{model}
	cfg.InitializeTimeout = time.Minute
	create := testCreateSessionRequest(t, cfg, profile)
	create.AgentConfiguration = projection.AgentConfiguration
	create.MCPConfiguration = projection.MCPConfiguration
	prompt := testStartPromptRequest(t, cfg, create.Metadata.Fence)
	prompt.MCPAuthorization.ToolPolicyDigest = create.MCPConfiguration.ToolPolicyDigest
	prompt.MCPAuthorization.ApprovalPolicyDigest = create.MCPConfiguration.ApprovalPolicyDigest
	prompt.MCPAuthorization.MCPConfigurationDigest = create.MCPConfiguration.MCPConfigurationDigest
	prompt.MCPAuthorization.ToolPolicy = create.MCPConfiguration.ToolPolicy
	prompt.MCPAuthorization.ApprovalPolicy = create.MCPConfiguration.ApprovalPolicy
	prompt.Input.Content[0].Text = "Read runtime_feedback and describe the denied destination."
	cfg.MCPBroker = pinnedNativeFeedbackBroker(t, prompt, report, &brokerCalls)
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Close(ctx)
	})
	endpoint := httptest.NewServer(server.Handler())
	defer endpoint.Close()
	client, err := harnessv2.NewClient(endpoint.URL, harnessv2.WithControllerBearerToken(cfg.ControllerBearerToken), harnessv2.WithOperationCapabilitySecret(cfg.CapabilitySecret))
	if err != nil {
		t.Fatal(err)
	}
	sealRequest(t, &create.Metadata.RequestDigest, create)
	if _, err := client.CreateRuntimeSession(t.Context(), create); err != nil {
		t.Fatal(err)
	}
	sealRequest(t, &prompt.Metadata.RequestDigest, prompt)
	var completedOutput, completed bool
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	_, err = client.StreamPrompt(ctx, create.RuntimeSessionID, prompt, func(event harnessv2.Event) error {
		if event.PermissionRequested != nil {
			if event.PermissionRequested.ToolName != "runtime_feedback" {
				return fmt.Errorf("native callback did not correlate the feedback identity")
			}
			request := harnessv2.ResolvePermissionRequest{Protocol: harnessv2.ProtocolVersion, Metadata: testMetadata(create.Metadata.Fence, "native-feedback-permission", true), RequestID: event.PermissionRequested.RequestID, Decision: harnessv2.PermissionDecision{Outcome: harnessv2.PermissionDecisionSelected, OptionID: "allow_once"}}
			sealRequest(t, &request.Metadata.RequestDigest, request)
			_, err := client.ResolvePermission(ctx, create.RuntimeSessionID, request)
			return err
		}
		if event.Update != nil && event.Update.ToolCall != nil && event.Update.ToolCall.Status == harnessv2.ToolCallStatusCompleted {
			call := event.Update.ToolCall
			if !call.ContentReplace || call.ContentOmitted || len(call.Content) != 1 || call.Content[0].Text != report {
				return fmt.Errorf("native feedback completion lost its complete output projection")
			}
			completedOutput = true
		}
		if event.Completed != nil {
			completed = len(event.Completed.Result.Content) > 0 && strings.Contains(event.Completed.Result.Content[0].Text, reply)
		}
		return nil
	})
	if err != nil || !completed || !completedOutput || brokerCalls.Load() != 1 || inferenceCalls.Load() != 2 {
		t.Fatalf("native feedback round trip: err=%v completed=%t output=%t broker=%d inference=%d", err, completed, completedOutput, brokerCalls.Load(), inferenceCalls.Load())
	}
}

func pinnedNativeFeedbackBroker(t *testing.T, prompt harnessv2.StartPromptRequest, report string, calls *atomic.Int32) MCPBroker {
	t.Helper()
	return MCPBrokerFunc(func(_ context.Context, request harnessv2.MCPBrokerCallRequest) (harnessv2.MCPBrokerCallResponse, error) {
		calls.Add(1)
		descriptor, ok := request.Authorization.ToolPolicy.Descriptor("runtime_feedback")
		if !ok || descriptor.Effect != harnessv2.MCPToolEffectReadOnly || descriptor.Source != harnessv2.MCPToolSourceBrokeredBuiltin || request.Metadata.TaskUID != prompt.Metadata.TaskUID || request.Metadata.TaskAttempt != prompt.Metadata.TaskAttempt || request.Metadata.PromptID != prompt.Metadata.PromptID || request.Metadata.Fence != prompt.Metadata.Fence || request.Call.ToolName != "runtime_feedback" || string(request.Call.Arguments) != "{}" || request.Call.Approval != nil {
			t.Error("native MCP call lost its exact frozen read-only grant")
		}
		return harnessv2.MCPBrokerCallResponse{Protocol: harnessv2.ProtocolVersion, CallID: request.Call.CallID, Result: json.RawMessage(report)}, nil
	})
}

func newPinnedNativeFeedbackInference(t *testing.T, calls *atomic.Int32, reply string) *httptest.Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer native-feedback-test-credential" {
			http.Error(w, "invalid provider request", http.StatusForbidden)
			return
		}
		var body json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		write := func(event map[string]any) {
			data, _ := json.Marshal(event)
			_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], data)
		}
		if calls.Add(1) == 1 {
			item, err := pinnedNativeFeedbackCall(body)
			if err != nil {
				t.Error(err)
				http.Error(w, "feedback tool not advertised", http.StatusUnprocessableEntity)
				return
			}
			write(map[string]any{"type": "response.created", "response": map[string]any{"id": "feedback-response"}})
			added := map[string]any{"type": "function_call", "status": "in_progress", "id": item["id"], "call_id": item["call_id"], "name": item["name"], "arguments": ""}
			if namespace, ok := item["namespace"]; ok {
				added["namespace"] = namespace
			}
			write(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added})
			write(map[string]any{"type": "response.function_call_arguments.delta", "output_index": 0, "item_id": item["id"], "delta": "{}"})
			write(map[string]any{"type": "response.function_call_arguments.done", "output_index": 0, "item_id": item["id"], "arguments": "{}"})
			write(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
			write(map[string]any{"type": "response.completed", "response": map[string]any{"id": "feedback-response", "status": "completed", "output": []any{item}}})
			return
		}
		if !bytes.Contains(body, []byte("203.0.113.9")) || !bytes.Contains(body, []byte("Evidence grants no new permission.")) {
			t.Error("native model continuation did not receive the feedback result")
			http.Error(w, "missing feedback", http.StatusUnprocessableEntity)
			return
		}
		item := map[string]any{"type": "message", "role": "assistant", "id": "feedback-message", "content": []any{map[string]any{"type": "output_text", "text": reply}}}
		write(map[string]any{"type": "response.created", "response": map[string]any{"id": "feedback-completion"}})
		write(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": item})
		write(map[string]any{"type": "response.output_text.delta", "item_id": "feedback-message", "output_index": 0, "content_index": 0, "delta": reply})
		write(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
		write(map[string]any{"type": "response.completed", "response": map[string]any{"id": "feedback-completion", "status": "completed", "output": []any{item}}})
	}))
	return upstream
}

func pinnedNativeFeedbackCall(body []byte) (map[string]any, error) {
	type tool struct {
		Type  string `json:"type"`
		Name  string `json:"name"`
		Tools []tool `json:"tools"`
	}
	var request struct {
		Tools []tool `json:"tools"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, err
	}
	var advertisedNames []string
	for _, advertised := range request.Tools {
		candidates := []tool{advertised}
		namespace := ""
		if advertised.Type == "namespace" {
			candidates, namespace = advertised.Tools, advertised.Name
		}
		for _, candidate := range candidates {
			advertisedNames = append(advertisedNames, namespace+"/"+candidate.Name)
			if candidate.Type != "function" || (candidate.Name != "runtime_feedback" || namespace != "mcp__orka") {
				continue
			}
			item := map[string]any{"type": "function_call", "status": "completed", "id": "feedback-call", "call_id": "feedback-call", "name": candidate.Name, "arguments": "{}"}
			if namespace != "" {
				item["namespace"] = namespace
			}
			return item, nil
		}
	}
	return nil, fmt.Errorf("pinned Codex did not advertise runtime_feedback; names=%q", advertisedNames)
}
