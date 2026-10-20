/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const brokerPromptPath = "/__e2e/accepted-prompt"
const brokerReadTool = "e2eauthorizationnote"

// acceptedBrokerPrompt contains only controller-supplied, non-secret fences and
// policy. It never exports prompt content, HTTP auth headers, or capabilities.
type acceptedBrokerPrompt struct {
	Metadata      harnessv2.MutationMetadata       `json:"metadata"`
	Lease         harnessv2.PromptLease            `json:"lease"`
	Authorization harnessv2.PromptMCPAuthorization `json:"authorization"`
}

type brokerPromptContextKey struct{}

type brokerPromptRecorder struct {
	bearer     string
	proxy      *httputil.ReverseProxy
	mu         sync.Mutex
	prompts    map[harnessv2.TaskUID]acceptedBrokerPrompt
	inputs     map[harnessv2.TaskUID]harnessv2.PromptInput
	loops      map[harnessv2.TaskUID]brokerLoopState
	loopConfig *brokerLoopConfig
}

func newBrokerPromptRecorder(backend, bearer string) (*brokerPromptRecorder, error) {
	endpoint, err := url.Parse(backend)
	if err != nil {
		return nil, err
	}
	recorder := &brokerPromptRecorder{
		bearer: bearer, proxy: httputil.NewSingleHostReverseProxy(endpoint),
		prompts: map[harnessv2.TaskUID]acceptedBrokerPrompt{},
		inputs:  map[harnessv2.TaskUID]harnessv2.PromptInput{},
		loops:   map[harnessv2.TaskUID]brokerLoopState{},
	}
	recorder.proxy.ErrorLog = log.New(io.Discard, "", 0)
	recorder.proxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, _ error) {
		http.Error(w, "fixture forwarding failed", http.StatusBadGateway)
	}
	recorder.proxy.ModifyResponse = recorder.recordAccepted
	return recorder, nil
}

func (p *brokerPromptRecorder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == brokerLoopPath {
		p.modelLoop(w, r)
		return
	}
	if r.URL.Path == brokerPromptPath {
		p.readPrompt(w, r)
		return
	}
	if r.Method == http.MethodPut && strings.Contains(r.URL.Path, "/prompts/") &&
		!strings.HasSuffix(r.URL.Path, "/cancel") {
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
		if err != nil {
			http.Error(w, "invalid fixture prompt", http.StatusBadRequest)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		var prompt harnessv2.StartPromptRequest
		if json.Unmarshal(body, &prompt) == nil && prompt.Input.Metadata["orka.conformance"] == "" {
			// The inner conformant fixture still verifies bearer, capability, request
			// digest, session, and fences. Recording alone never grants acceptance.
			captured := acceptedBrokerPrompt{Metadata: prompt.Metadata,
				Lease:         prompt.Lease,
				Authorization: prompt.MCPAuthorization}
			r = r.WithContext(context.WithValue(r.Context(), brokerPromptContextKey{}, struct {
				acceptedBrokerPrompt
				Input harnessv2.PromptInput
			}{captured, prompt.Input}))
		}
	}
	p.proxy.ServeHTTP(w, r)
}

func (p *brokerPromptRecorder) recordAccepted(response *http.Response) error {
	captured, ok := response.Request.Context().Value(brokerPromptContextKey{}).(struct {
		acceptedBrokerPrompt
		Input harnessv2.PromptInput
	})
	if !ok || response.StatusCode != http.StatusOK ||
		!strings.Contains(response.Header.Get("Content-Type"), harnessv2.NDJSONMediaType) {
		return nil
	}
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadBytes('\n')
	if err != nil {
		return err
	}
	// Preserve exactly the accepted event for the real controller stream.
	response.Body = &readCloser{Reader: io.MultiReader(bytes.NewReader(line), reader), Closer: response.Body}
	var event harnessv2.Event
	if json.Unmarshal(line, &event) != nil || event.Type != harnessv2.EventAccepted ||
		event.Identity.TaskUID != captured.Metadata.TaskUID || event.Identity.PromptID != captured.Metadata.PromptID {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.prompts) < 32 {
		p.prompts[captured.Metadata.TaskUID] = captured.acceptedBrokerPrompt
		p.inputs[captured.Metadata.TaskUID] = captured.Input
	}
	return nil
}

type readCloser struct {
	io.Reader
	io.Closer
}

func (p *brokerPromptRecorder) readPrompt(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet || !p.authorized(r) {
		http.Error(w, "fixture prompt observer is unauthorized", http.StatusUnauthorized)
		return
	}
	taskUID := harnessv2.TaskUID(r.URL.Query().Get("taskUID"))
	p.mu.Lock()
	prompt, ok := p.prompts[taskUID]
	p.mu.Unlock()
	if !ok {
		http.Error(w, "fixture prompt not accepted", http.StatusNotFound)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(prompt)
}

func brokerProbeProfile(profile harnessv2.RuntimeProfile) (harnessv2.RuntimeProfile, error) {
	allowed := brokerProbeTools()
	var err error
	profile.ToolPolicyDigest, err = harnessv2.CanonicalRuntimeToolPolicyDigest(allowed, []string{}, false)
	if err != nil {
		return profile, err
	}
	profile.MCPConfigurationDigest, err = harnessv2.CanonicalMCPConfigurationDigest(allowed)
	return profile, err
}

func (p *brokerPromptRecorder) authorized(r *http.Request) bool {
	return subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+p.bearer)) == 1
}

func brokerProbeTools() []string {
	return []string{"delegate_task", "e2eauthorizationevidence", brokerReadTool}
}
