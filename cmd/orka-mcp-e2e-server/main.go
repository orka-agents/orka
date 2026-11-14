/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

const (
	jsonRPCField = "jsonrpc"
)

const (
	jsonRPCVersion = "2.0"
)

const (
	defaultListenAddr   = ":8080"
	mcpProtocolVersion  = "2025-06-18"
	mcpSessionIDHeader  = "Mcp-Session-Id"
	mcpE2ESessionID     = "orka-mcp-e2e-session"
	mcpInitializeMethod = "initialize"
	mcpInitialized      = "notifications/initialized"
	mcpToolsCallMethod  = "tools/call"
)

const (
	// upstreamModeEnv turns the binary into the plain HTTP upstream that the
	// egress probe calls, instead of an MCP server.
	upstreamModeEnv = "ORKA_MCP_E2E_UPSTREAM_MODE"
	// upstreamModeCredentialDigest answers with the SHA-256 of the received
	// Authorization header, so a test compares the injected credential without
	// anyone printing it.
	upstreamModeCredentialDigest = "credential-digest"
	// upstreamURLEnv is the origin the MCP server's egress probe calls.
	upstreamURLEnv = "ORKA_MCP_E2E_UPSTREAM_URL"
	// egressPlaceholder is the value an Actor sends for a gateway-injected
	// header. The gateway replaces it only when an egress rule says so.
	egressPlaceholder = "placeholder"
	// egressBlocked is reported for any failed or non-200 upstream request,
	// including the gateway's default-deny refusal.
	egressBlocked = "blocked"
	// upstreamMissing is the upstream's answer when no Authorization arrived.
	upstreamMissing = "missing"
	maxUpstreamBody = 256
)

var (
	processStartedAt = time.Now().UnixNano()
	toolCallCount    atomic.Uint64
)

type jsonRPCRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type toolsCallParams struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func main() {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	if strings.TrimSpace(os.Getenv(upstreamModeEnv)) == upstreamModeCredentialDigest {
		mux.HandleFunc("/", handleCredentialDigest)
	} else {
		mux.HandleFunc("/mcp", handleMCP)
	}

	addr := strings.TrimSpace(os.Getenv("ORKA_WORKSPACE_AGENT_LISTEN_ADDR"))
	if addr == "" {
		addr = defaultListenAddr
	}
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func handleMCP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodHead {
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method == http.MethodDelete {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var req jsonRPCRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSONRPCError(w, json.RawMessage(`null`), -32700, "parse error")
		return
	}

	switch req.Method {
	case mcpInitializeMethod:
		w.Header().Set(mcpSessionIDHeader, mcpE2ESessionID)
		writeJSON(w, map[string]any{
			jsonRPCField: jsonRPCVersion,
			"id":         rawID(req.ID),
			"result": map[string]any{
				"protocolVersion": mcpProtocolVersion,
				"capabilities":    map[string]any{},
				"serverInfo": map[string]string{
					"name":    "orka-mcp-e2e",
					"version": "1",
				},
			},
		})
	case mcpInitialized:
		w.WriteHeader(http.StatusAccepted)
	case mcpToolsCallMethod:
		var params toolsCallParams
		if err := json.Unmarshal(req.Params, &params); err != nil {
			writeJSONRPCError(w, req.ID, -32602, "invalid params")
			return
		}
		message, _ := params.Arguments["message"].(string)
		if strings.TrimSpace(message) == "" {
			message = "empty"
		}
		result := fmt.Sprintf("mcp-e2e-ok:%s:%s", params.Name, message)
		callCount := toolCallCount.Add(1)
		switch message {
		case "boot-state":
			result = fmt.Sprintf("mcp-e2e-state:%s:%d:%d", params.Name, processStartedAt, callCount)
		case "egress-probe":
			result = "mcp-e2e-egress:" + egressProbe(r.Context(), true)
		case "egress-probe-unauthenticated":
			result = "mcp-e2e-egress:" + egressProbe(r.Context(), false)
		case "trace-context":
			result = "mcp-e2e-trace:" + traceID(r.Header.Get("traceparent"))
		}
		writeJSON(w, map[string]any{
			jsonRPCField: jsonRPCVersion,
			"id":         rawID(req.ID),
			"result": map[string]any{
				"content": []map[string]string{
					{
						"type": "text",
						"text": result,
					},
				},
			},
		})
	default:
		writeJSONRPCError(w, req.ID, -32601, "method not found")
	}
}

// egressProbe calls the configured upstream from inside the Actor. With
// authenticated set, it sends only a placeholder Authorization header, which
// the egress gateway must replace with the injected credential.
func egressProbe(ctx context.Context, authenticated bool) string {
	target := strings.TrimSpace(os.Getenv(upstreamURLEnv))
	if target == "" {
		return "unconfigured"
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return egressBlocked
	}
	if authenticated {
		req.Header.Set("Authorization", egressPlaceholder)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("egress probe failed: %v", err)
		return egressBlocked
	}
	defer resp.Body.Close() //nolint:errcheck
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody))
	if err != nil || resp.StatusCode != http.StatusOK {
		log.Printf("egress probe returned HTTP %d", resp.StatusCode)
		return egressBlocked
	}
	return strings.TrimSpace(string(body))
}

// handleCredentialDigest reports only a digest of the received credential.
func handleCredentialDigest(w http.ResponseWriter, r *http.Request) {
	value := r.Header.Get("Authorization")
	answer := upstreamMissing
	if value != "" {
		sum := sha256.Sum256([]byte(value))
		answer = "sha256:" + hex.EncodeToString(sum[:])
	}
	w.Header().Set("Content-Type", "text/plain")
	_, _ = io.WriteString(w, answer)
}

// traceID returns the W3C trace ID the Actor received, or "none".
func traceID(traceparent string) string {
	parts := strings.Split(strings.TrimSpace(traceparent), "-")
	if len(parts) != 4 || len(parts[1]) != 32 {
		return "none"
	}
	return strings.ToLower(parts[1])
}

func rawID(id json.RawMessage) any {
	if len(id) == 0 {
		return nil
	}
	var value any
	if err := json.Unmarshal(id, &value); err != nil {
		return nil
	}
	return value
}

func writeJSONRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(w, map[string]any{
		jsonRPCField: jsonRPCVersion,
		"id":         rawID(id),
		"error": map[string]any{
			"code":    code,
			"message": message,
		},
	})
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		log.Printf("encode response: %v", err)
	}
}
