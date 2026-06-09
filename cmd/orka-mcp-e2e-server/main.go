/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"
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
	mux.HandleFunc("/mcp", handleMCP)

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
			"jsonrpc": "2.0",
			"id":      rawID(req.ID),
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
		if message == "boot-state" {
			result = fmt.Sprintf("mcp-e2e-state:%s:%d:%d", params.Name, processStartedAt, callCount)
		}
		writeJSON(w, map[string]any{
			"jsonrpc": "2.0",
			"id":      rawID(req.ID),
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
		"jsonrpc": "2.0",
		"id":      rawID(id),
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
