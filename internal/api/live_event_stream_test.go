package api

import (
	"bufio"
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/orka-agents/orka/internal/liveevents"
	"github.com/orka-agents/orka/internal/store/storetest"
)

func TestStreamTaskEventsSendsLiveFramesOnlyWhenAsked(t *testing.T) {
	for _, live := range []bool{false, true} {
		eventStore := storetest.NewFakeExecutionEventStore()
		h, app := setupTaskEventHandlers(t, eventStore, testTask("default", "task-live"))
		configureShortTaskEventStream(h)
		h.liveEvents = liveevents.NewBroker(0)
		useCancelingContext(app, 80*time.Millisecond)
		app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)
		go func() {
			time.Sleep(20 * time.Millisecond)
			h.liveEvents.Publish(liveevents.Event{
				Namespace: "default", Task: "task-live", TaskUID: "task-uid",
				Runtime: &harnessv2.Event{Protocol: harnessv2.ProtocolVersion, Type: harnessv2.EventUpdate, Update: &harnessv2.UpdateEvent{
					Kind: harnessv2.UpdateAssistantMessageChunk, AssistantMessage: &harnessv2.AssistantMessageChunk{Text: "streamed delta"},
				}},
			})
		}()
		target := "/api/v1/tasks/task-live/stream?namespace=default"
		if live {
			target += "&live=true"
		}
		body := doStreamRequest(t, app, target)
		if got := strings.Contains(body, "event: live\n") && strings.Contains(body, "streamed delta"); got != live {
			t.Fatalf("live=%v: live frame present=%v in %q", live, got, body)
		}
	}
}

func TestStreamTaskEventsRejectsAnInvalidLiveParameter(t *testing.T) {
	h, app := setupTaskEventHandlers(t, storetest.NewFakeExecutionEventStore(), testTask("default", "task-live"))
	app.Get("/api/v1/tasks/:id/stream", h.StreamTaskEvents)
	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/api/v1/tasks/task-live/stream?namespace=default&live=maybe", nil))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", resp.StatusCode)
	}
}

func TestWriteLiveExecutionEventSSEFrameReportsALaggedSubscription(t *testing.T) {
	var buf bytes.Buffer
	if !writeLiveExecutionEventSSEFrame(bufio.NewWriter(&buf), liveevents.Event{}, false) {
		t.Fatal("writing the lag frame failed")
	}
	if buf.String() != "event: live_lagged\ndata: {}\n\n" {
		t.Fatalf("lag frame = %q", buf.String())
	}
}
