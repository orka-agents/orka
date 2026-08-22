package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync/atomic"
	"time"
)

const (
	maxRequestBytes             = 4 << 20
	responseStatusField         = "status"
	responseTypeField           = "type"
	responseAnnotationsField    = "annotations"
	responseSequenceNumberField = "sequence_number"
	responseOutputIndexField    = "output_index"
	responseItemIDField         = "item_id"
	responseContentIndexField   = "content_index"
	responseOutputTextType      = "output_text"
	responseTextField           = "text"
)

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
	writeJSON(w, http.StatusOK, map[string]string{responseStatusField: "ok"})
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
	itemID := "msg_" + responseID
	item := map[string]any{
		responseTypeField:   "message",
		"role":              "assistant",
		"id":                itemID,
		responseStatusField: "completed",
		"content": []map[string]any{{
			responseTypeField: responseOutputTextType, responseTextField: text, responseAnnotationsField: []any{},
		}},
	}
	completed := map[string]any{
		"id":                responseID,
		responseStatusField: "completed",
		"model":             request.Model,
		"output":            []any{item},
		"end_turn":          true,
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
		responseTypeField:           "response.created",
		responseSequenceNumberField: 0,
		"response":                  map[string]any{"id": responseID},
	})
	writeSSE(w, "response.output_item.added", map[string]any{
		responseTypeField:           "response.output_item.added",
		responseSequenceNumberField: 1,
		responseOutputIndexField:    0,
		"item": map[string]any{
			responseTypeField: "message", "role": "assistant", "id": itemID,
			responseStatusField: "in_progress", "content": []any{},
		},
	})
	writeSSE(w, "response.content_part.added", map[string]any{
		responseTypeField:           "response.content_part.added",
		responseSequenceNumberField: 2,
		responseItemIDField:         itemID,
		responseOutputIndexField:    0,
		responseContentIndexField:   0,
		"part": map[string]any{
			responseTypeField: responseOutputTextType, responseTextField: "", responseAnnotationsField: []any{},
		},
	})
	writeSSE(w, "response.output_text.delta", map[string]any{
		responseTypeField:           "response.output_text.delta",
		responseSequenceNumberField: 3,
		responseItemIDField:         itemID,
		responseOutputIndexField:    0,
		responseContentIndexField:   0,
		"delta":                     text,
	})
	writeSSE(w, "response.output_text.done", map[string]any{
		responseTypeField:           "response.output_text.done",
		responseSequenceNumberField: 4,
		responseItemIDField:         itemID,
		responseOutputIndexField:    0,
		responseContentIndexField:   0,
		responseTextField:           text,
	})
	writeSSE(w, "response.content_part.done", map[string]any{
		responseTypeField:           "response.content_part.done",
		responseSequenceNumberField: 5,
		responseItemIDField:         itemID,
		responseOutputIndexField:    0,
		responseContentIndexField:   0,
		"part": map[string]any{
			responseTypeField: responseOutputTextType, responseTextField: text, responseAnnotationsField: []any{},
		},
	})
	writeSSE(w, "response.output_item.done", map[string]any{
		responseTypeField:           "response.output_item.done",
		responseSequenceNumberField: 6,
		responseOutputIndexField:    0,
		"item":                      item,
	})
	writeSSE(w, "response.completed", map[string]any{
		responseTypeField:           "response.completed",
		responseSequenceNumberField: 7,
		"response":                  completed,
	})
}

// responseTextMarker matches deterministic scenario markers embedded in the
// request ("Reply exactly: ORKA_..._OK") so each Task in a multi-Task scenario
// gets an independently verifiable result.
var responseTextMarker = regexp.MustCompile(`ORKA_[A-Z0-9_]+_OK`)

func responseText(body []byte) string {
	if marker := responseTextMarker.Find(body); marker != nil {
		return string(marker)
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
