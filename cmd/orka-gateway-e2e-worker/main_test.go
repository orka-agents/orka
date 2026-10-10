package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func fixtureConfig(t *testing.T, server *httptest.Server, capable bool) workerConfig {
	t.Helper()
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("fixture-token"), 0600); err != nil {
		t.Fatal(err)
	}
	return workerConfig{
		controllerURL: server.URL, namespace: "ns", task: "task", taskUID: "task-uid", tokenFile: tokenFile,
		capable: capable, client: server.Client(), retryInterval: time.Millisecond,
	}
}

func TestWorkerAuthenticatedReplayAndReleaseFence(t *testing.T) {
	for _, capable := range []bool{true, false} {
		t.Run(map[bool]string{true: "capable", false: "unsupported"}[capable], func(t *testing.T) {
			origins, budgets, calls, results := 0, 0, 0, 0
			released := false
			requestID := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer fixture-token" {
					t.Error("worker auth missing")
				}
				body, _ := io.ReadAll(r.Body)
				switch r.URL.Path {
				case "/internal/v1/tasks/ns/task/gateway-messages/origin":
					origins++
					if r.Method != http.MethodGet || len(body) != 0 || budgets != 0 || calls != 0 {
						t.Error("origin must precede admission and carry no content")
					}
					if origins == 1 {
						w.WriteHeader(http.StatusServiceUnavailable)
						return
					}
					_, _ = io.WriteString(w, `{"taskUID":"task-uid"}`)
				case "/internal/v1/tasks/ns/task/gateway-messages/budget":
					budgets++
					id := r.URL.Query().Get("requestID")
					if r.Method != http.MethodGet || len(body) != 0 || origins != 2 ||
						!strings.HasPrefix(id, "gr-") || len(id) != 67 {
						t.Error("budget must use authenticated origin and tool-derived identity")
					}
					if requestID == "" {
						requestID = id
					} else if requestID != id {
						t.Error("replay changed logical operation identity")
					}
					if !capable {
						w.WriteHeader(http.StatusConflict)
						_, _ = io.WriteString(w, `{"error":{"code":"interim_delivery_unsupported"}}`)
						return
					}
					// Replay remains possible at the configured lifetime cap.
					_ = json.NewEncoder(w).Encode(map[string]any{"accepted": calls, "limit": 1, "requestExists": calls > 0})
				case "/internal/v1/tasks/ns/task/gateway-messages":
					calls++
					var message map[string]string
					if r.Method != http.MethodPost || json.Unmarshal(body, &message) != nil || len(message) != 2 ||
						message["content"] != "Gateway interim E2E message" || message["requestID"] != requestID ||
						requestID == "" || budgets != calls || !capable {
						t.Errorf("message bypassed tool preflight or changed content/identity: %s", body)
					}
					created := calls == 1
					if created {
						w.WriteHeader(http.StatusAccepted)
					}
					_ = json.NewEncoder(w).Encode(map[string]any{
						"deliveryID": "gdm-receipt-1", "status": "Pending", "created": created,
					})
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
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			ready := false
			err := runWorker(ctx, fixtureConfig(t, server, capable), func(report workerReport) error {
				ready = true
				if results != 0 {
					t.Error("final sent before evidence published")
				}
				if capable && (report.First.DeliveryID != "gdm-receipt-1" || !report.First.Created ||
					report.Replay.Created || report.Replay.DeliveryID != report.First.DeliveryID) {
					t.Error("bad replay evidence")
				}
				if !capable && report.ErrorCode != "interim_delivery_unsupported" {
					t.Error("missing typed tool rejection evidence")
				}
				return nil
			}, func(context.Context) error {
				if !ready {
					t.Error("released before ready")
				}
				released = true
				return nil
			})
			if err != nil || !ready || !released || origins != 2 || results != 1 ||
				budgets != map[bool]int{true: 2, false: 1}[capable] || calls != map[bool]int{true: 2, false: 0}[capable] {
				t.Fatalf("worker: %v; origins=%d budgets=%d calls=%d results=%d", err, origins, budgets, calls, results)
			}
		})
	}
}

func TestWorkerFailsClosedOnAdmissionAndReplayErrors(t *testing.T) {
	for _, mode := range []string{
		"origin unauthorized", "origin UID", "budget exhausted", "unauthorized", "generic conflict",
		"untyped unsupported", "replay ID", "replay created", "malformed", "oversized",
	} {
		t.Run(mode, func(t *testing.T) {
			calls, origins, budgets := 0, 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/internal/v1/tasks/ns/task/gateway-messages/origin":
					origins++
					if mode == "origin unauthorized" {
						w.WriteHeader(http.StatusForbidden)
						return
					}
					uid := "task-uid"
					if mode == "origin UID" {
						uid = "replaced-task-uid"
					}
					_ = json.NewEncoder(w).Encode(map[string]any{"taskUID": uid})
				case "/internal/v1/tasks/ns/task/gateway-messages/budget":
					budgets++
					switch mode {
					case "budget exhausted":
						_, _ = io.WriteString(w, `{"accepted":1,"limit":1,"requestExists":false}`)
					case "generic conflict":
						w.WriteHeader(http.StatusConflict)
						_, _ = io.WriteString(w, `{"error":{"code":"conflict"}}`)
					case "untyped unsupported":
						// Matching backend prose is not a typed, definitive tool rejection.
						w.WriteHeader(http.StatusConflict)
						_, _ = io.WriteString(w,
							`{"error":{"code":"unknown","message":"gateway adapter does not support interim delivery"}}`)

					default:
						_, _ = io.WriteString(w, `{"accepted":0,"limit":10,"requestExists":false}`)
					}
				case "/internal/v1/tasks/ns/task/gateway-messages":
					calls++
					switch mode {
					case "unauthorized":
						w.WriteHeader(http.StatusForbidden)
					case "malformed":
						_, _ = io.WriteString(w, `{`)
					case "oversized":
						_, _ = io.WriteString(w, strings.Repeat("x", 4097))
					default:
						id := "gdm-receipt-1"
						created := calls == 1
						if calls == 1 {
							w.WriteHeader(http.StatusAccepted)
						}
						if calls == 2 && mode == "replay ID" {
							id = "gdm-different"
						}
						if calls == 2 && mode == "replay created" {
							created = true
						}
						_ = json.NewEncoder(w).Encode(map[string]any{"deliveryID": id, "status": "Pending", "created": created})
					}
				default:
					t.Error("unexpected final after admission failure")
				}
			}))
			defer server.Close()
			cfg := fixtureConfig(t, server, mode != "generic conflict" && mode != "untyped unsupported")
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
			if origins != 1 {
				t.Error("must authenticate origin exactly once before sending")
			}
			if strings.HasPrefix(mode, "origin") && (budgets != 0 || calls != 0) {
				t.Error("attempted admission with unauthenticated origin")
			}
			if (mode == "budget exhausted" || mode == "generic conflict" || mode == "untyped unsupported") && calls != 0 {
				t.Error("bypassed tool budget rejection")
			}
		})
	}
}
