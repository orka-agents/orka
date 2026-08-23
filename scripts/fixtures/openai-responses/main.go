package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
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

// markerCounts records how many /responses requests resolved to each marker so
// lifecycle E2E scenarios can prove a prompt was sent exactly once (no replay
// across cancellation or controller restart).
var markerCounts sync.Map

// responseHoldMarker requests a bounded server-side hold before the response
// completes ("ORKA_HOLD_120S"), keeping the prompt observably Running for
// cancellation and restart scenarios. The hold is capped defensively.
var responseHoldMarker = regexp.MustCompile(`ORKA_HOLD_([0-9]{1,3})S`)

const maxHoldSeconds = 240

func requestHold(body []byte) time.Duration {
	// Match the last hold marker for the same history reason as responseText.
	matches := responseHoldMarker.FindAllSubmatch(body, -1)
	if len(matches) == 0 {
		return 0
	}
	seconds, err := strconv.Atoi(string(matches[len(matches)-1][1]))
	if err != nil || seconds <= 0 {
		return 0
	}
	if seconds > maxHoldSeconds {
		seconds = maxHoldSeconds
	}
	return time.Duration(seconds) * time.Second
}

func recordMarker(marker string) uint64 {
	value, _ := markerCounts.LoadOrStore(marker, &atomic.Uint64{})
	counter, ok := value.(*atomic.Uint64)
	if !ok {
		return 0
	}
	return counter.Add(1)
}

func handleMarkerCounts(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	counts := map[string]uint64{}
	markerCounts.Range(func(key, value any) bool {
		marker, markerOK := key.(string)
		counter, counterOK := value.(*atomic.Uint64)
		if markerOK && counterOK {
			counts[marker] = counter.Load()
		}
		return true
	})
	writeJSON(w, http.StatusOK, counts)
}

// holdBeforeCompletion keeps the connection demonstrably alive for the
// requested hold using SSE comments, so intermediaries do not sever the
// stream while the prompt stays Running.
func holdBeforeCompletion(w http.ResponseWriter, hold time.Duration, streaming bool) {
	if hold <= 0 {
		return
	}
	deadline := time.Now().Add(hold)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return
		}
		step := min(remaining, 5*time.Second)
		time.Sleep(step)
		if streaming {
			_, _ = fmt.Fprint(w, ": keepalive\n\n")
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}
}

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
	mux.HandleFunc("/fixture/marker-counts", handleMarkerCounts)
	mux.HandleFunc("/responses", handleResponses)
	mux.HandleFunc("/v1/responses", handleResponses)

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		// Held responses (ORKA_HOLD_<n>S) stream keepalives while the prompt
		// intentionally stays Running; the write deadline must outlast the
		// maximum hold.
		WriteTimeout: 10 * time.Minute,
		IdleTimeout:  30 * time.Second,
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
	hold := requestHold(body)
	recordMarker(text)
	// The resolved marker is user-controlled prompt material: log only a
	// digest and length so fixture diagnostics can correlate requests
	// without disclosing prompt content.
	markerDigest := sha256.Sum256([]byte(text))
	log.Printf("responses request resolved marker_sha=%x marker_len=%d hold=%s roles=%s",
		markerDigest[:8], len(text), hold, inputRoles(body))
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
		holdBeforeCompletion(w, hold, false)
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
	holdBeforeCompletion(w, hold, true)
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
	// Continuation requests carry the full session history. Prefer the marker
	// from the newest user message in a structured input array - a resumed
	// session may order replayed context after the active prompt, so a raw
	// last-match can resolve to an earlier turn.
	if marker, ok := structuredUserMarker(body); ok {
		return marker
	}
	matches := responseTextMarker.FindAll(body, -1)
	if len(matches) == 0 {
		return "ORKA_RESPONSES_FIXTURE_OK"
	}
	return string(matches[len(matches)-1])
}

// structuredUserMarker walks a structured Responses input array from the end
// and returns the marker of the newest user message.
func structuredUserMarker(body []byte) (string, bool) {
	var request struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil || len(request.Input) == 0 {
		return "", false
	}
	var items []map[string]any
	if err := json.Unmarshal(request.Input, &items); err != nil {
		return "", false
	}
	for _, item := range slices.Backward(items) {
		role, _ := item["role"].(string)
		if role != "user" {
			continue
		}
		encoded, err := json.Marshal(item)
		if err != nil {
			continue
		}
		// The runtime may concatenate the bootstrap transcript and the active
		// prompt into one user message with the active prompt last; the last
		// match inside the newest user message is the turn being answered.
		if matches := responseTextMarker.FindAll(encoded, -1); len(matches) > 0 {
			return string(matches[len(matches)-1]), true
		}
	}
	return "", false
}

// inputRoles renders the structured input's role sequence (never content) so
// fixture logs explain marker resolution for replayed sessions.
func inputRoles(body []byte) string {
	var request struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil || len(request.Input) == 0 {
		return "unstructured"
	}
	var items []map[string]any
	if err := json.Unmarshal(request.Input, &items); err != nil {
		return "unstructured"
	}
	roles := make([]string, 0, len(items))
	for _, item := range items {
		role, _ := item["role"].(string)
		if role == "" {
			role, _ = item["type"].(string)
		}
		if role == "" {
			role = "?"
		}
		roles = append(roles, role)
	}
	return strings.Join(roles, ",")
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
