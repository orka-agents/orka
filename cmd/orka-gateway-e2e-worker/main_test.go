package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestWorkerAuthenticatedReplayAndReleaseFence(t *testing.T) {
	for _, capable := range []bool{true, false} {
		t.Run(map[bool]string{true: "capable", false: "unsupported"}[capable], func(t *testing.T) {
			calls, results := 0, 0
			released := false
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("worker auth missing")
				}
				body, _ := io.ReadAll(r.Body)
				switch r.URL.Path {
				case "/internal/v1/tasks/ns/task/gateway-messages":
					calls++
					if string(body) != `{"content":"Gateway interim E2E message","requestID":"gateway-e2e-message"}` {
						t.Errorf("wrong message body: %s", body)
					}
					if calls == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					if !capable {
						w.WriteHeader(http.StatusConflict)
						_, _ = io.WriteString(w, `{"error":{"code":"interim_delivery_unsupported"}}`)
						return
					}
					created := calls == 2
					if created {
						w.WriteHeader(http.StatusAccepted)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"deliveryID": "receipt-1", "status": "Pending", "created": created})
				case "/internal/v1/results/ns/task":
					results++
					if !released || string(body) != "Gateway interim E2E final" {
						t.Error("result crossed release fence or changed")
					}
					w.WriteHeader(http.StatusNoContent)
				default:
					t.Error("unexpected worker route")
					w.WriteHeader(http.StatusNotFound)
				}
			}))
			defer server.Close()
			cfg := workerConfig{
				controllerURL: server.URL, namespace: "ns", task: "task", token: "fixture-token",
				capable: capable, client: server.Client(), retryInterval: time.Millisecond,
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			ready := false
			err := runWorker(ctx, cfg, func(report workerReport) error {
				ready = true
				if results != 0 {
					t.Error("final sent before evidence published")
				}
				if capable && (report.First.DeliveryID != "receipt-1" || !report.First.Created ||
					report.Replay.Created || report.Replay.DeliveryID != report.First.DeliveryID) {
					t.Error("bad replay evidence")
				}
				if !capable && report.ErrorCode != "interim_delivery_unsupported" {
					t.Error("missing rejection evidence")
				}
				return nil
			}, func(context.Context) error {
				if !ready {
					t.Error("released before ready")
				}
				released = true
				return nil
			})
			if err != nil || results != 1 || calls != map[bool]int{true: 3, false: 2}[capable] {
				t.Fatalf("worker result: %v, calls=%d results=%d", err, calls, results)
			}
		})
	}
}

func TestWorkerFailsClosedOnAdmissionAndReplayErrors(t *testing.T) {
	for _, mode := range []string{
		"unauthorized", "generic conflict", "replay ID", "replay created", "malformed", "oversized",
	} {
		t.Run(mode, func(t *testing.T) {
			calls := 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if !strings.HasSuffix(r.URL.Path, "/gateway-messages") {
					t.Error("unexpected final after admission failure")
				}
				switch mode {
				case "unauthorized":
					w.WriteHeader(http.StatusForbidden)
				case "generic conflict":
					w.WriteHeader(http.StatusConflict)
					_, _ = io.WriteString(w, `{"error":{"code":"conflict"}}`)
				case "malformed":
					_, _ = io.WriteString(w, `{`)
				case "oversized":
					_, _ = io.WriteString(w, strings.Repeat("x", 65537))
				default:
					id := "receipt-1"
					created := calls == 1
					if calls == 1 {
						w.WriteHeader(http.StatusAccepted)
					}
					if calls == 2 && mode == "replay ID" {
						id = "different"
					}
					if calls == 2 && mode == "replay created" {
						created = true
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"deliveryID": id, "status": "Pending", "created": created})
				}
			}))
			defer server.Close()
			cfg := workerConfig{
				controllerURL: server.URL, namespace: "ns", task: "task", token: "fixture-token",
				capable: mode != "generic conflict", client: server.Client(),
			}
			err := runWorker(context.Background(), cfg, func(workerReport) error {
				t.Error("published false evidence")
				return nil
			}, func(context.Context) error {
				t.Error("waited for release after failure")
				return nil
			})
			if err == nil {
				t.Fatal("expected closed failure")
			}
		})
	}
}
