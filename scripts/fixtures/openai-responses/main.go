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
	"regexp"
	"slices"
	"sort"
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
// across cancellation or controller restart). Keys are marker DIGESTS
// (markerKey), never the raw prompt-derived marker: the counts endpoint is
// unauthenticated, and a customized prompt could embed sensitive material in
// its marker.
var markerCounts sync.Map

// markerHistory records whether any request resolving to each marker carried
// prior conversation turns (an assistant item in the structured input), so
// continuation scenarios can prove the runtime replayed the session history
// rather than silently starting a fresh session.
var markerHistory sync.Map

// markerDisconnects counts held requests for each marker whose client
// disconnected before the hold elapsed - the observable proof that a
// cancellation actually closed the in-flight provider stream.
var markerDisconnects sync.Map

// markerHistoryMarkers accumulates, per resolved marker key, the digest keys
// of the OTHER markers found anywhere in that marker's request bodies - the
// evidence that a continuation replayed the EXPECTED earlier turns, not just
// any transcript with an assistant item.
var markerHistoryMarkers sync.Map

// markerKey reduces a prompt-derived marker to a fixed-length digest key so
// the unauthenticated counts endpoint never discloses raw prompt material.
func markerKey(marker string) string {
	digest := sha256.Sum256([]byte(marker))
	return hex.EncodeToString(digest[:8])
}

// responseHoldMarker requests a bounded server-side hold before the response
// completes ("ORKA_HOLD_120S"), keeping the prompt observably Running for
// cancellation and restart scenarios. The hold is capped defensively.
var responseHoldMarker = regexp.MustCompile(`ORKA_HOLD_([0-9]{1,3})S`)

const maxHoldSeconds = 240

func requestHold(body []byte) time.Duration {
	// Resolve the hold from the newest user message only: a continued Session
	// replays earlier turns in its history, and a stale ORKA_HOLD_* marker
	// from a previous prompt must never delay later responses.
	scope := body
	if newest, ok := newestUserMessage(body); ok {
		scope = newest
	}
	matches := responseHoldMarker.FindAllSubmatch(scope, -1)
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
	value, _ := markerCounts.LoadOrStore(markerKey(marker), &atomic.Uint64{})
	counter, ok := value.(*atomic.Uint64)
	if !ok {
		return 0
	}
	return counter.Add(1)
}

func recordMarkerHistory(marker string, sawHistory bool) {
	if sawHistory {
		markerHistory.Store(markerKey(marker), true)
		return
	}
	markerHistory.LoadOrStore(markerKey(marker), false)
}

// recordMarkerHistoryMarkers stores the digest keys of every marker present
// in the request body other than the resolved one, accumulating across
// requests exactly like the sawHistory latch.
func recordMarkerHistoryMarkers(marker string, body []byte) {
	current := markerKey(marker)
	value, _ := markerHistoryMarkers.LoadOrStore(current, &sync.Map{})
	set, ok := value.(*sync.Map)
	if !ok {
		return
	}
	for _, match := range responseTextMarker.FindAll(body, -1) {
		if key := markerKey(string(match)); key != current {
			set.Store(key, true)
		}
	}
}

func recordMarkerDisconnect(marker string) {
	value, _ := markerDisconnects.LoadOrStore(markerKey(marker), &atomic.Uint64{})
	if counter, ok := value.(*atomic.Uint64); ok {
		counter.Add(1)
	}
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

// handleMarkerObservations reports per-marker request evidence: whether the
// newest request carried prior conversation history, and how many held
// requests observed a client disconnect before their hold elapsed.
func handleMarkerObservations(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	type observation struct {
		SawHistory     bool     `json:"sawHistory"`
		Disconnects    uint64   `json:"disconnects"`
		HistoryMarkers []string `json:"historyMarkers"`
	}
	observations := map[string]*observation{}
	entry := func(marker string) *observation {
		if existing, ok := observations[marker]; ok {
			return existing
		}
		fresh := &observation{}
		observations[marker] = fresh
		return fresh
	}
	markerHistory.Range(func(key, value any) bool {
		marker, markerOK := key.(string)
		sawHistory, historyOK := value.(bool)
		if markerOK && historyOK {
			entry(marker).SawHistory = sawHistory
		}
		return true
	})
	markerHistoryMarkers.Range(func(key, value any) bool {
		marker, markerOK := key.(string)
		set, setOK := value.(*sync.Map)
		if markerOK && setOK {
			seen := []string{}
			set.Range(func(inner, _ any) bool {
				if digest, digestOK := inner.(string); digestOK {
					seen = append(seen, digest)
				}
				return true
			})
			sort.Strings(seen)
			entry(marker).HistoryMarkers = seen
		}
		return true
	})
	markerDisconnects.Range(func(key, value any) bool {
		marker, markerOK := key.(string)
		counter, counterOK := value.(*atomic.Uint64)
		if markerOK && counterOK {
			entry(marker).Disconnects = counter.Load()
		}
		return true
	})
	writeJSON(w, http.StatusOK, observations)
}

// holdBeforeCompletion keeps the connection demonstrably alive for the
// requested hold using SSE comments, so intermediaries do not sever the
// stream while the prompt stays Running. It observes the request context: a
// client disconnect (the adapter cancelling its in-flight provider request)
// ends the hold immediately and is recorded per marker, so cancellation
// scenarios can assert the provider stream was actually closed.
func holdBeforeCompletion(
	ctx context.Context, w http.ResponseWriter, marker string, hold time.Duration, streaming bool,
) {
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
		select {
		case <-ctx.Done():
			recordMarkerDisconnect(marker)
			// The marker is user-controlled prompt material; diagnostics log
			// only its digest, matching handleResponses.
			log.Printf("held request for marker_sha=%s observed a client disconnect with %s remaining", markerKey(marker), remaining)
			return
		case <-time.After(step):
		}
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
	mux.HandleFunc("/fixture/marker-observations", handleMarkerObservations)
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
	recordMarkerHistory(text, requestCarriesHistory(body))
	recordMarkerHistoryMarkers(text, body)
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
		holdBeforeCompletion(r.Context(), w, text, hold, false)
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
	holdBeforeCompletion(r.Context(), w, text, hold, true)
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
	newest, ok := newestUserMessage(body)
	if !ok {
		return "", false
	}
	// The runtime may concatenate the bootstrap transcript and the active
	// prompt into one user message with the active prompt last; the last
	// match inside the newest user message is the turn being answered.
	if matches := responseTextMarker.FindAll(newest, -1); len(matches) > 0 {
		return string(matches[len(matches)-1]), true
	}
	return "", false
}

// newestUserMessage returns the encoded newest user item of a structured
// Responses input array.
func newestUserMessage(body []byte) ([]byte, bool) {
	items, ok := structuredInputItems(body)
	if !ok {
		return nil, false
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
		return encoded, true
	}
	return nil, false
}

// requestCarriesHistory reports whether the structured input replays prior
// conversation turns: an assistant item proves a live-session replay, and a
// recreated session bootstraps by concatenating the prior transcript and the
// active prompt into one user message, where multiple resolved markers are
// equally proof the transcript was restored instead of silently dropped.
func requestCarriesHistory(body []byte) bool {
	items, ok := structuredInputItems(body)
	if !ok {
		return false
	}
	for _, item := range items {
		if role, _ := item["role"].(string); role == "assistant" {
			return true
		}
	}
	if newest, ok := newestUserMessage(body); ok {
		return len(responseTextMarker.FindAll(newest, -1)) > 1
	}
	return false
}

func structuredInputItems(body []byte) ([]map[string]any, bool) {
	var request struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil || len(request.Input) == 0 {
		return nil, false
	}
	var items []map[string]any
	if err := json.Unmarshal(request.Input, &items); err != nil {
		return nil, false
	}
	return items, true
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
	// Roles are client-controlled: only whitelisted values are logged
	// verbatim so a crafted role/type field can never smuggle request
	// material into fixture diagnostics.
	known := map[string]bool{
		"user": true, "assistant": true, "system": true, "developer": true,
		"tool": true, "message": true, "function_call": true,
		"function_call_output": true, "reasoning": true,
	}
	roles := make([]string, 0, len(items))
	for _, item := range items {
		role, _ := item["role"].(string)
		if role == "" {
			role, _ = item["type"].(string)
		}
		switch {
		case role == "":
			role = "?"
		case !known[role]:
			role = "other"
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
