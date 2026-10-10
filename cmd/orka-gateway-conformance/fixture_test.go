/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/protocol"
)

func TestLoadDeliveryFixture(t *testing.T) {
	const valid = `{"accountId":"fixture-account","contextId":"fixture-context",` +
		`"replyTarget":"fixture-reply","originatingEventId":"fixture-event"}`
	cases := []struct {
		name, body string
		valid      bool
	}{
		{"valid", valid, true},
		{"single case-insensitive key", strings.Replace(valid, `"accountId"`, `"ACCOUNTID"`, 1), true},
		{"escaped key", strings.Replace(valid, `"accountId"`, `"account\u0049d"`, 1), true},
		{"optional thread", strings.TrimSuffix(valid, "}") + `,"threadId":"fixture-thread"}`, true},
		{"exact body limit", valid + strings.Repeat(" ", protocol.MaxHTTPBodyBytes-len(valid)), true},
		{"over body limit", valid + strings.Repeat(" ", protocol.MaxHTTPBodyBytes+1-len(valid)), false},
		{"unknown field", strings.TrimSuffix(valid, "}") + `,"private-field":"private-content"}`, false},
		{"caller text", strings.TrimSuffix(valid, "}") + `,"text":"private-content"}`, false},
		{"caller delivery ID", strings.TrimSuffix(valid, "}") + `,"deliveryId":"private-content"}`, false},
		{"caller idempotency ID", strings.TrimSuffix(valid, "}") + `,"idempotencyId":"private-content"}`, false},
		{"caller metadata", strings.TrimSuffix(valid, "}") + `,"metadata":{"private-field":"private-content"}}`, false},
		{"caller credentials", strings.TrimSuffix(valid, "}") + `,"authorizationValue":"private-content"}`, false},
		{"trailing JSON", valid + ` {"private-field":"private-content"}`, false},
		{"trailing malformed", valid + ` private-content`, false},
		{"malformed", `{"private-field":"private-content"`, false},
		{"invalid UTF8", strings.Replace(valid, "fixture-account", "fixture-\xff-account", 1), false},
		{"lone surrogate", strings.Replace(valid, "fixture-context", `room-\ud800`, 1), false},
		{"empty", "", false},
		{"null", "null", false},
		{"array", "[]", false},
		{"wrong type", `{"accountId":123}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private-path.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal("could not create test fixture")
			}
			fixture, err := loadDeliveryFixture(path)
			if tc.valid {
				if err != nil || fixture == nil {
					t.Fatal("valid fixture rejected")
				}
				if fixture.AccountID != "fixture-account" || fixture.ContextID != "fixture-context" ||
					fixture.ReplyTarget != "fixture-reply" || fixture.OriginatingEventID != "fixture-event" {
					t.Error("loaded routing fields differ")
				}
				if tc.name == "optional thread" && fixture.ThreadID != "fixture-thread" {
					t.Error("thread missing")
				}
			} else if err == nil || fixture != nil {
				t.Error("invalid fixture accepted")
			} else if err.Error() != "invalid delivery fixture" {
				t.Error("fixture diagnostic is not the fixed sanitized message")
			}
		})
	}
}

func TestDeliveryFixtureDuplicateRoutingKeysRejectedBeforeNetwork(t *testing.T) {
	const valid = `{"accountId":"fixture-account","contextId":"fixture-context",` +
		`"threadId":"fixture-thread","replyTarget":"fixture-reply","originatingEventId":"fixture-event"}`
	type testCase struct{ name, body string }
	var cases []testCase
	for _, field := range []string{"accountId", "contextId", "threadId", "replyTarget", "originatingEventId"} {
		key := `"` + field + `":`
		for _, alias := range []string{field, strings.ToUpper(field)} {
			cases = append(cases, testCase{
				name: field + " after " + alias,
				body: strings.Replace(valid, key, `"`+alias+`":"private\nroute",`+key, 1),
			})
		}
	}
	cases = append(cases,
		testCase{
			name: "case-insensitive overwrite",
			body: strings.Replace(valid, `"contextId":`, `"contextId":"private\nroute","ContextID":`, 1),
		},
		testCase{
			name: "escaped key overwrite",
			body: strings.Replace(valid, `"contextId":`, `"context\u0049d":"private\nroute","contextId":`, 1),
		},
		testCase{
			name: "identical duplicate values",
			body: strings.Replace(valid, `"contextId":`, `"contextId":"fixture-context","contextId":`, 1),
		},
	)
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "private-fixture.json")
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal("could not create test fixture")
			}
			before := requests.Load()
			command := exec.Command(os.Args[0], "-test.run=^TestDeliveryFixtureCLIHelper$")
			command.Env = append(os.Environ(),
				"ORKA_GATEWAY_FIXTURE_TEST_HELPER=1",
				"ORKA_GATEWAY_FIXTURE_TEST_ENDPOINT="+server.URL,
				"ORKA_GATEWAY_FIXTURE_TEST_PATH="+path,
				"ORKA_GATEWAY_BEARER_TOKEN=fixture-test-token",
			)
			output, err := command.CombinedOutput()
			if exit, ok := err.(*exec.ExitError); !ok || exit.ExitCode() != 2 {
				t.Error("duplicate routing keys did not fail CLI preflight with exit code 2")
			}
			if string(output) != "invalid delivery fixture\n" {
				t.Error("duplicate routing key diagnostic is not fixed and sanitized")
			}
			if requests.Load() != before {
				t.Error("duplicate routing keys reached the network")
			}
		})
	}
}

func TestDeliveryFixtureCLIHelper(_ *testing.T) {
	if os.Getenv("ORKA_GATEWAY_FIXTURE_TEST_HELPER") != "1" {
		return
	}
	flag.CommandLine = flag.NewFlagSet("orka-gateway-conformance", flag.ExitOnError)
	os.Args = []string{"orka-gateway-conformance",
		"--endpoint", os.Getenv("ORKA_GATEWAY_FIXTURE_TEST_ENDPOINT"),
		"--delivery-fixture", os.Getenv("ORKA_GATEWAY_FIXTURE_TEST_PATH"),
	}
	main()
	os.Exit(0)
}

func TestDeliveryFixtureDoesNotLogUnsolicitedResponse(t *testing.T) {
	const route = "fixture-private-unsolicited-route"
	fixture := `{"accountId":"fixture-account","contextId":"` + route +
		`","replyTarget":"fixture-reply","originatingEventId":"fixture-event"}`
	path := filepath.Join(t.TempDir(), "private-fixture.json")
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal("could not create test fixture")
	}
	var injected atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/deliveries" {
			body, err := io.ReadAll(r.Body)
			var delivery protocol.DeliveryRequest
			if err != nil || json.Unmarshal(body, &delivery) != nil || delivery.ContextID != route {
				t.Error("delivery lost fixture routing")
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			if r.Header.Get("Authorization") == "" {
				connection, buffer, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error("could not hijack test connection")
					return
				}
				defer connection.Close() //nolint:errcheck
				// Echo routing outside the declared response body, bypassing result masking.
				_, err = fmt.Fprintf(buffer,
					"HTTP/1.1 401 Unauthorized\r\nContent-Length: 0\r\n\r\nprovider diagnostic %s\r\n", delivery.ContextID)
				if err != nil || buffer.Flush() != nil {
					t.Error("could not inject test diagnostic")
					return
				}
				injected.Add(1)
				return
			}
			if r.Header.Get("Authorization") != "Bearer fixture-test-token" {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			if len(body) > protocol.MaxHTTPBodyBytes || len(delivery.Text) > protocol.MaxTextBytes {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			_ = json.NewEncoder(w).Encode(protocol.DeliveryResponse{
				Status: protocol.DeliveryStatusDelivered, ProviderMessageID: "fixture-result",
			})
			return
		}
		if r.Header.Get("Authorization") != "Bearer fixture-test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case "/v1/health":
			_ = json.NewEncoder(w).Encode(protocol.HealthResponse{Status: "ok"})
		case "/v1/capabilities":
			_ = json.NewEncoder(w).Encode(protocol.CapabilitiesResponse{
				ProtocolVersion: protocol.Version, AdapterName: "fixture-adapter",
				Capabilities: protocol.Capabilities{IdempotentDelivery: true},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()
	command := exec.Command(os.Args[0], "-test.run=^TestDeliveryFixtureCLIHelper$")
	command.Env = append(os.Environ(),
		"ORKA_GATEWAY_FIXTURE_TEST_HELPER=1",
		"ORKA_GATEWAY_FIXTURE_TEST_ENDPOINT="+server.URL,
		"ORKA_GATEWAY_FIXTURE_TEST_PATH="+path,
		"ORKA_GATEWAY_BEARER_TOKEN=fixture-test-token",
	)
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatal("fixture CLI did not pass")
	}
	if injected.Load() != 1 {
		t.Error("expected one injected provider diagnostic")
	}
	if strings.Contains(string(output), route) || strings.Contains(string(output), "Unsolicited response") {
		t.Error("fixture CLI leaked unsolicited transport diagnostics")
	}
	var result struct{ Passed bool }
	if err := json.Unmarshal(output, &result); err != nil || !result.Passed {
		t.Error("fixture CLI output is not a single passing JSON result")
	}
}

func TestLoadDeliveryFixtureReadFailurePrivacy(t *testing.T) {
	for _, path := range []string{filepath.Join(t.TempDir(), "private-missing-path"), t.TempDir()} {
		fixture, err := loadDeliveryFixture(path)
		if fixture != nil || err == nil {
			t.Error("unreadable fixture accepted")
		} else if err.Error() != "could not read delivery fixture" {
			t.Error("read diagnostic is not fixed and sanitized")
		}
	}
}

func TestLoadDeliveryFixtureUnicode(t *testing.T) {
	cases := []struct {
		name, encoded, want string
		valid               bool
	}{
		{"raw UTF8", "fixture-\U0001f600", "fixture-\U0001f600", true},
		{"BMP escape", `\u0066ixture-context`, "fixture-context", true},
		{"surrogate pair", `fixture-\ud83d\ude00`, "fixture-\U0001f600", true},
		{"boundary pairs", `fixture-\ud800\udc00\uDBFF\uDFFF`, "fixture-\U00010000\U0010ffff", true},
		{"replacement character", `fixture-\ufffd`, "fixture-\ufffd", true},
		{"escaped surrogate text", `fixture-\\ud800`, `fixture-\ud800`, true},
		{"lone low surrogate", `fixture-\udc00`, "", false},
		{"surrogate before text", `fixture-\ud800-text`, "", false},
		{"surrogate before BMP", `fixture-\ud800\u0061`, "", false},
		{"two high surrogates", `fixture-\ud800\ud800`, "", false},
		{"reversed pair", `fixture-\udc00\ud800`, "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"accountId":"fixture-account","contextId":"` + tc.encoded +
				`","replyTarget":"fixture-reply","originatingEventId":"fixture-event"}`
			path := filepath.Join(t.TempDir(), "private-path.json")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal("could not create test fixture")
			}
			fixture, err := loadDeliveryFixture(path)
			if tc.valid {
				if err != nil || fixture == nil {
					t.Fatal("valid Unicode fixture rejected")
				}
				if fixture.ContextID != tc.want {
					t.Error("loaded Unicode routing identity differs")
				}
			} else if err == nil || fixture != nil {
				t.Error("malformed Unicode fixture accepted")
			} else if err.Error() != "invalid delivery fixture" {
				t.Error("fixture diagnostic is not the fixed sanitized message")
			}
		})
	}
}
