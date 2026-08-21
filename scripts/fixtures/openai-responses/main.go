package main

import (
	"bytes"
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

const maxRequestBytes = 4 << 20

var responseSequence atomic.Uint64

type responsesRequest struct {
	Model  string `json:"model"`
	Stream bool   `json:"stream"`
}

func main() {
	addr := strings.TrimSpace(os.Getenv("ORKA_RESPONSES_FIXTURE_LISTEN_ADDR"))
	if addr == "" {
		addr = ":1337"
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", handleHealth)
	mux.HandleFunc("/models", handleModels)
	mux.HandleFunc("/v1/models", handleModels)
	mux.HandleFunc("/responses", handleResponses)
	mux.HandleFunc("/v1/responses", handleResponses)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	log.Printf("OpenAI Responses fixture listening on %s", addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func handleModels(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object": "list",
		"data": []map[string]any{{
			"id":       "gpt-5.5",
			"object":   "model",
			"created":  0,
			"owned_by": "orka-e2e-fixture",
		}},
	})
}

func handleResponses(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBytes))
	if err != nil {
		http.Error(w, "invalid request body", http.StatusBadRequest)
		return
	}
	var request responsesRequest
	if err := json.Unmarshal(body, &request); err != nil || strings.TrimSpace(request.Model) == "" {
		http.Error(w, "model is required", http.StatusBadRequest)
		return
	}

	text := responseText(body)
	responseID := fmt.Sprintf("resp_orka_fixture_%d", responseSequence.Add(1))
	item := map[string]any{
		"type":    "message",
		"role":    "assistant",
		"id":      "msg_" + responseID,
		"status":  "completed",
		"content": []map[string]any{{"type": "output_text", "text": text, "annotations": []any{}}},
	}
	completed := map[string]any{
		"id":       responseID,
		"status":   "completed",
		"model":    request.Model,
		"output":   []any{item},
		"end_turn": true,
		"usage": map[string]any{
			"input_tokens":          1,
			"input_tokens_details":  nil,
			"output_tokens":         1,
			"output_tokens_details": nil,
			"total_tokens":          2,
		},
	}

	if !request.Stream {
		writeJSON(w, http.StatusOK, completed)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	writeSSE(w, "response.created", map[string]any{
		"type":            "response.created",
		"sequence_number": 0,
		"response":        map[string]any{"id": responseID},
	})
	writeSSE(w, "response.output_item.done", map[string]any{
		"type":            "response.output_item.done",
		"sequence_number": 1,
		"output_index":    0,
		"item":            item,
	})
	writeSSE(w, "response.completed", map[string]any{
		"type":            "response.completed",
		"sequence_number": 2,
		"response":        completed,
	})
}

func responseText(body []byte) string {
	for _, marker := range []string{"ORKA_WS_SANDBOX_OK", "ORKA_WS_SUBSTRATE_OK"} {
		if bytes.Contains(body, []byte(marker)) {
			return marker
		}
	}
	return "ORKA_RESPONSES_FIXTURE_OK"
}

func writeSSE(w http.ResponseWriter, event string, value any) {
	payload, err := json.Marshal(value)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, payload)
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
