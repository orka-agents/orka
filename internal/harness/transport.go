package harness

import (
	"encoding/json"
	"fmt"
	"net/http"
)

const sseDone = "[DONE]"

func WriteJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func WriteError(w http.ResponseWriter, status int, message string) {
	WriteJSON(w, status, map[string]string{"error": message})
}

func WriteSSEFrame(w http.ResponseWriter, frame HarnessEventFrame) error {
	payload, err := json.Marshal(frame)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", payload); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}

func WriteSSEDone(w http.ResponseWriter) error {
	if _, err := fmt.Fprintf(w, "data: %s\n\n", sseDone); err != nil {
		return err
	}
	if flusher, ok := w.(http.Flusher); ok {
		flusher.Flush()
	}
	return nil
}
