package common

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/orka/internal/events"
	"github.com/sozercan/orka/internal/store"
)

func TestHTTPExecutionEventStoreRejectsApprovalDecisionEvents(t *testing.T) {
	eventStore := &HTTPExecutionEventStore{controllerURL: "http://127.0.0.1", client: &http.Client{}}
	_, err := eventStore.AppendExecutionEvent(context.Background(), &store.ExecutionEvent{
		Namespace:  "default",
		StreamType: events.ExecutionEventStreamTypeTask,
		StreamID:   "task-a",
		Type:       events.ExecutionEventTypeApprovalApproved,
	})
	if err == nil || !strings.Contains(err.Error(), "approval decision events") {
		t.Fatalf("AppendExecutionEvent() error = %v, want approval decision rejection", err)
	}
}

func TestHTTPExecutionEventStoreReadsLargeListResponses(t *testing.T) {
	largeContent := strings.Repeat("x", 2<<20)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		if err := json.NewEncoder(w).Encode(map[string]any{
			"events": []store.ExecutionEvent{{
				Namespace:   "default",
				StreamType:  store.ExecutionEventStreamTypeTask,
				StreamID:    "task-large",
				TaskName:    "task-large",
				Type:        events.ExecutionEventTypeApprovalRequested,
				ContentText: largeContent,
			}},
		}); err != nil {
			t.Fatalf("encode response: %v", err)
		}
	}))
	defer server.Close()

	eventStore := &HTTPExecutionEventStore{controllerURL: server.URL, client: server.Client()}
	listed, err := eventStore.ListExecutionEvents(context.Background(), store.ExecutionEventFilter{
		Namespace: "default",
		StreamID:  "task-large",
		Limit:     1,
	})
	if err != nil {
		t.Fatalf("ListExecutionEvents: %v", err)
	}
	if len(listed) != 1 || len(listed[0].ContentText) != len(largeContent) {
		t.Fatalf(
			"listed event content length=%d events=%d, want full large response",
			len(listed[0].ContentText),
			len(listed),
		)
	}
}
