/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/harness/v2/conformance/conformancetest"
)

func TestBrokerObserverRecordsOnlyAcceptedPromptAndNoSensitiveInput(t *testing.T) {
	const bearer = "fixture-observer-unit-bearer"
	const inputCanary = "private-input-not-exported"
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+bearer {
			t.Error("forwarded fixture auth changed")
		}
		w.Header().Set("Content-Type", harnessv2.NDJSONMediaType)
		_ = json.NewEncoder(w).Encode(harnessv2.Event{Type: harnessv2.EventAccepted,
			Identity: harnessv2.EventIdentity{TaskUID: "task-unit", PromptID: "prompt-unit"},
		})
	}))
	defer backend.Close()
	recorder, err := newBrokerPromptRecorder(backend.URL, bearer)
	if err != nil {
		t.Fatal("observer construction failed")
	}
	prompt := harnessv2.StartPromptRequest{
		Metadata: harnessv2.MutationMetadata{TaskUID: "task-unit", PromptID: "prompt-unit"},
		Input:    harnessv2.PromptInput{Metadata: map[string]string{"private": inputCanary}},
	}
	body, _ := json.Marshal(prompt)
	request := httptest.NewRequest(http.MethodPut,
		"/v2/runtime-sessions/session/prompts/prompt-unit",
		bytes.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+bearer)
	request.Header.Set(harnessv2.OperationCapabilityHeader, "capability-never-exported")
	response := httptest.NewRecorder()
	recorder.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "accepted") {
		t.Fatal("accepted stream not preserved")
	}
	read := httptest.NewRequest(http.MethodGet, brokerPromptPath+"?taskUID=task-unit", nil)
	read.Header.Set("Authorization", "Bearer "+bearer)
	response = httptest.NewRecorder()
	recorder.ServeHTTP(response, read)
	if response.Code != http.StatusOK {
		t.Fatal("accepted metadata was not recorded")
	}
	for _, canary := range []string{bearer, inputCanary, "capability-never-exported"} {
		if strings.Contains(response.Body.String(), canary) {
			t.Fatal("observer exported sensitive data")
		}
	}
	var captured acceptedBrokerPrompt
	if json.Unmarshal(response.Body.Bytes(), &captured) != nil || captured.Metadata.TaskUID != "task-unit" {
		t.Fatal("accepted prompt identity differs")
	}
}

func TestBrokerObserverNeverRecordsRejectedPrompt(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rejected by fixture", http.StatusUnauthorized)
	}))
	defer backend.Close()
	recorder, err := newBrokerPromptRecorder(backend.URL, "unit-bearer")
	if err != nil {
		t.Fatal("observer construction failed")
	}
	body, _ := json.Marshal(harnessv2.StartPromptRequest{Metadata: harnessv2.MutationMetadata{TaskUID: "rejected"}})
	recorder.ServeHTTP(httptest.NewRecorder(),
		httptest.NewRequest(http.MethodPut,
			"/v2/runtime-sessions/s/prompts/p",
			bytes.NewReader(body)))
	read := httptest.NewRequest(http.MethodGet, brokerPromptPath+"?taskUID=rejected", nil)
	read.Header.Set("Authorization", "Bearer unit-bearer")
	response := httptest.NewRecorder()
	recorder.ServeHTTP(response, read)
	if response.Code != http.StatusNotFound {
		t.Fatal("rejected prompt was exported")
	}
	unauthorized := httptest.NewRecorder()
	recorder.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, brokerPromptPath, nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatal("observer allowed unauthenticated metadata access")
	}
}

func TestBrokerProbeProfileDeclaresFrozenBrokerTools(t *testing.T) {
	baseline, err := conformancetest.DeterministicProfile("unit-fixture")
	if err != nil {
		t.Fatal(err)
	}
	profile, err := brokerProbeProfile(baseline)
	if err != nil || profile.Validate() != nil {
		t.Fatal("probe profile invalid")
	}
	toolDigest, _ := harnessv2.CanonicalRuntimeToolPolicyDigest(brokerProbeTools(), []string{}, false)
	mcpDigest, _ := harnessv2.CanonicalMCPConfigurationDigest(brokerProbeTools())
	if profile.ToolPolicyDigest != toolDigest || profile.MCPConfigurationDigest != mcpDigest ||
		profile.AgentConfigurationDigest != baseline.AgentConfigurationDigest {
		t.Fatal("probe profile drifted")
	}
}
