/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func unitLoopPrompt(t *testing.T) acceptedBrokerPrompt {
	t.Helper()
	now := time.Now().UTC()
	names := []string{brokerReadTool}
	policy := harnessv2.MCPToolPolicy{AllowedToolNames: names, DisallowedToolNames: []string{}}
	for _, name := range names {
		policy.Tools = append(policy.Tools, harnessv2.MCPToolDescriptor{Name: name, Description: "Unit read",
			Source: harnessv2.MCPToolSourceBrokeredBuiltin, Effect: harnessv2.MCPToolEffectReadOnly,
			InputSchema: json.RawMessage(`{"type":"object"}`)})
	}
	policy.DescriptorDigest, _ = harnessv2.CanonicalMCPToolDescriptorDigest(policy.Tools)
	toolDigest, _ := harnessv2.CanonicalRuntimeToolPolicyDigest(names, []string{}, false)
	approvalDigest, _ := harnessv2.CanonicalMCPApprovalPolicyDigest(harnessv2.MCPApprovalPolicy{})
	mcpDigest, _ := harnessv2.CanonicalMCPConfigurationDigest(names)
	return acceptedBrokerPrompt{
		Metadata: harnessv2.MutationMetadata{Fence: harnessv2.Fence{RuntimeInstanceID: "fixture", SupervisorBootID: "boot",
			ControllerEpoch: 1, RuntimePoolUID: "pool", RuntimePoolGeneration: 1, RuntimeSessionUID: "session",
			RuntimeSessionGeneration: 1, RuntimeProfileDigest: harnessv2.ProfileDigest("sha256:" + strings.Repeat("0", 64)),
			ProfileDigestSchemaVersion: 1}, TaskUID: "unit-task", TaskAttempt: 1, PromptID: "prompt", OperationID: "admitted",
			RequestDigestSchemaVersion: 1, ExpiresAt: now.Add(time.Minute)},
		Lease: harnessv2.PromptLease{Generation: 1, IssuedAt: now, ExpiresAt: now.Add(time.Minute)},
		Authorization: harnessv2.PromptMCPAuthorization{RuntimeSessionUID: "session", SessionGeneration: 1,
			TaskUID: "unit-task", TaskAttempt: 1, PromptID: "prompt", LeaseGeneration: 1, ToolPolicyDigest: toolDigest,
			ApprovalPolicyDigest: approvalDigest, MCPConfigurationDigest: mcpDigest,
			ToolPolicy: policy, ExpiresAt: now.Add(time.Minute)},
	}
}

func TestModelLoopUsesFrozenFencesAndReturnsBrokerDenialToInference(t *testing.T) {
	const bearer = "unit-loop-bearer"
	const signing = "unit-loop-signing-key-with-32-bytes-minimum"
	const deniedName = "unlisted_tool"
	prompt := unitLoopPrompt(t)
	brokerCalls, modelRequests := 0, 0
	broker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request harnessv2.MCPBrokerCallRequest
		if json.NewDecoder(r.Body).Decode(&request) != nil {
			t.Error("broker request schema changed")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		brokerCalls++
		if request.Metadata.Fence != prompt.Metadata.Fence ||
			request.Authorization.ToolPolicyDigest != prompt.Authorization.ToolPolicyDigest ||
			request.Metadata.ValidateDigest(request) != nil ||
			request.Authorization.ValidateForAt(request.Metadata, request.Lease, time.Now()) != nil ||
			harnessv2.VerifyOperationCapability([]byte(signing), r.Header.Get(harnessv2.OperationCapabilityHeader),
				request.Metadata, true, time.Now()) != nil {
			t.Error("model call changed admitted fences, policy, digest, or capability")
		}
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			t.Error("public broker bearer not supplied")
		}
		if _, err := request.ValidateAt(time.Now()); err != nil {
			if request.Call.ToolName != deniedName || !strings.Contains(err.Error(), "not an allowed brokered tool") {
				t.Error("negative was rejected for a reason other than the frozen allowlist")
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":"invalid MCP broker request"}`)
			return
		}
		result := json.RawMessage(`{"success":true}`)
		if request.Call.ToolName == "e2eauthorizationnote" {
			result = json.RawMessage(`{"note":"lower-trust-note"}`)
		}
		_ = json.NewEncoder(w).Encode(harnessv2.MCPBrokerCallResponse{Protocol: harnessv2.ProtocolVersion,
			CallID: request.Call.CallID, Result: result})
	}))
	defer broker.Close()
	model := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), bearer) || strings.Contains(string(body), signing) ||
			strings.Contains(string(body), "requestDigest") {
			t.Error("transport authority entered model content")
		}
		modelRequests++
		message := loopMessage{Role: "assistant"}
		switch modelRequests {
		case 1:
			message.ToolCalls = []loopToolCall{{ID: "read", Type: "function"}}
			message.ToolCalls[0].Function.Name = "e2eauthorizationnote"
			message.ToolCalls[0].Function.Arguments = `{}`
		case 2:
			if !strings.Contains(string(body), "lower-trust-note") {
				t.Error("model did not receive the actual read result")
			}
			message.ToolCalls = []loopToolCall{{ID: "denied", Type: "function"}}
			message.ToolCalls[0].Function.Name = deniedName
			message.ToolCalls[0].Function.Arguments = `{}`
		case 3:
			if !strings.Contains(string(body), "invalid MCP broker request") {
				t.Error("broker denial did not reach a later inference")
			}
			message.Content = "AUTHORIZATION_RESULT_OBSERVED"
		default:
			t.Error("unexpected extra inference")
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": message}}})
	}))
	defer model.Close()
	recorder, err := newBrokerPromptRecorder(broker.URL, bearer)
	if err != nil {
		t.Fatal("recorder construction failed")
	}
	recorder.loopConfig = &brokerLoopConfig{modelURL: model.URL, brokerURL: broker.URL,
		namespace: "eval", capability: []byte(signing)}
	recorder.loops[prompt.Metadata.TaskUID] = brokerLoopState{Phase: "running"}
	recorder.runModelLoop(prompt.Metadata.TaskUID, prompt,
		harnessv2.PromptInput{Content: []harnessv2.ContentBlock{
			{Type: harnessv2.ContentBlockText, Text: "Read and summarize the note."}}})
	state := recorder.loops[prompt.Metadata.TaskUID]
	if state.Phase != "complete" || !state.TerminalSeen || modelRequests != 3 || brokerCalls != 4 {
		t.Fatalf("bounded loop incomplete: phase=%s stage=%s requests=%d brokerCalls=%d",
			state.Phase, state.Failure, modelRequests, brokerCalls)
	}
	if state.Calls[2].Tool != deniedName || state.Calls[2].Status != http.StatusBadRequest {
		t.Fatal("unknown discovery name did not reach public broker transport")
	}
}

func TestModelLoopDoesNotExportInputOrRunWithoutAuthentication(t *testing.T) {
	recorder, _ := newBrokerPromptRecorder("http://127.0.0.1:1", "unit-only-bearer")
	recorder.loopConfig = &brokerLoopConfig{}
	request := httptest.NewRequest(http.MethodPost, brokerLoopPath+"?taskUID=absent", nil)
	response := httptest.NewRecorder()
	recorder.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || len(recorder.loops) != 0 {
		t.Fatal("unauthenticated caller could start the fixture model loop")
	}
}
