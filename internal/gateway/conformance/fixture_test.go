/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/protocol"
)

func testDeliveryFixture() DeliveryFixture {
	return DeliveryFixture{AccountID: "fixture-account", ContextID: "fixture-context", ThreadID: "fixture-thread", ReplyTarget: "fixture-reply", OriginatingEventID: "fixture-event"}
}

func TestCheckDeliveryFixtureRoutesAndFreshIDs(t *testing.T) {
	for _, fixtureMode := range []bool{false, true} {
		t.Run(map[bool]string{false: "legacy", true: "fixture"}[fixtureMode], func(t *testing.T) {
			fixture := testDeliveryFixture()
			var requests []protocol.DeliveryRequest
			var bodies [][]byte
			var auth []string
			handler := testAdapterHandler("test-auth", defaultCapabilities(), func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var delivery protocol.DeliveryRequest
				if json.Unmarshal(body, &delivery) != nil || len(body) > protocol.MaxHTTPBodyBytes || protocol.ValidateDeliveryRequest(&delivery) != nil {
					writeTestJSON(w, http.StatusBadRequest, protocol.DeliveryResponse{Status: protocol.DeliveryStatusNonRetryableError})
					return
				}
				writeTestJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusDelivered, ProviderMessageID: "provider:" + delivery.DeliveryID})
			})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/v1/deliveries" {
					body, _ := io.ReadAll(r.Body)
					var delivery protocol.DeliveryRequest
					if err := json.Unmarshal(body, &delivery); err != nil {
						t.Error("invalid wire JSON")
					}
					requests = append(requests, delivery)
					bodies = append(bodies, body)
					auth = append(auth, r.Header.Get("Authorization"))
					r.Body = io.NopCloser(bytes.NewReader(body))
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			target := Target{BaseURL: server.URL, AuthorizationValue: "test-auth", HTTPClient: server.Client()}
			if fixtureMode {
				target.DeliveryFixture = &fixture
			}
			for range 2 {
				if result := Check(context.Background(), target); !result.Passed {
					t.Fatalf("Check failed: %s", result.Message)
				}
			}
			if len(requests) != 12 {
				t.Fatalf("delivery requests = %d, want 12 (six per run, no fault sends)", len(requests))
			}
			seenIDs := map[string]bool{}
			for run := range 2 {
				start := run * 6
				if !bytes.Equal(bodies[start], bodies[start+1]) || !bytes.Equal(bodies[start+4], bodies[start+5]) {
					t.Error("auth or duplicate requests differ on wire")
				}
				if auth[start] != "" || auth[start+1] != "Bearer test-auth-invalid" {
					t.Error("authentication probes changed")
				}
				if len(requests[start+2].Text) != protocol.MaxTextBytes+1 || len(bodies[start+2]) > protocol.MaxHTTPBodyBytes {
					t.Error("text probe does not isolate the text bound")
				}
				if len(bodies[start+3]) != protocol.MaxHTTPBodyBytes+1 || protocol.ValidateDeliveryRequest(&requests[start+3]) != nil {
					t.Error("body probe does not isolate the HTTP body bound")
				}
				for index := range 6 {
					delivery := requests[start+index]
					if delivery.ProtocolVersion != protocol.Version || delivery.Kind != protocol.DeliveryKindFinal || delivery.Metadata != nil || delivery.TaskRef != nil || delivery.SessionRef != nil || delivery.DeliveryID != delivery.IdempotencyID {
						t.Error("unexpected delivery shape")
					}
					if index >= 2 && auth[start+index] != "Bearer test-auth" {
						t.Error("authenticated probe lost auth")
					}
				}
				if fixtureMode {
					assertFixtureRun(t, fixture, requests[start:start+6], seenIDs)
				} else {
					assertLegacyRun(t, requests[start:start+6])
				}
			}
		})
	}
}

func TestProbeDeliveryFixtureTransport(t *testing.T) {
	for _, mode := range []string{
		"legacy", "default", "configured", "custom", "no proxy", "configured no proxy",
	} {
		t.Run(mode, func(t *testing.T) {
			handler := testAdapterHandler("test-auth", defaultCapabilities(), nil)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Close != (mode != "legacy") {
					t.Error("unexpected HTTP connection reuse policy")
				}
				handler.ServeHTTP(w, r)
			}))
			defer server.Close()
			fixture := testDeliveryFixture()
			target := Target{BaseURL: server.URL, AuthorizationValue: "test-auth", DeliveryFixture: &fixture}
			if mode == "legacy" {
				target.DeliveryFixture = nil
			}
			transport := http.DefaultTransport.(*http.Transport).Clone()
			defer transport.CloseIdleConnections()
			var originalTransport http.RoundTripper
			switch mode {
			case "custom":
				originalTransport = fixtureRoundTripper(transport.RoundTrip)
			case "configured", "configured no proxy":
				originalTransport = transport
			}
			client := &http.Client{Transport: originalTransport}
			target.HTTPClient = client
			target.DisableProxy = strings.Contains(mode, "no proxy")
			if result := Probe(context.Background(), target); !result.Passed {
				t.Fatalf("fixture probe failed: %s", result.Message)
			}
			if transport.DisableKeepAlives || client.Timeout != 0 || client.CheckRedirect != nil {
				t.Error("fixture probe mutated caller HTTP configuration")
			}
		})
	}
}

func assertFixtureRun(t *testing.T, fixture DeliveryFixture, deliveries []protocol.DeliveryRequest, seenIDs map[string]bool) {
	t.Helper()
	prefix := strings.TrimSuffix(deliveries[0].DeliveryID, "-auth")
	for index, delivery := range deliveries {
		suffix := []string{"-auth", "-auth", "-size-text", "-size-body", "-idempotency", "-idempotency"}[index]
		if delivery.DeliveryID != prefix+suffix {
			t.Error("fixture probe IDs do not share the run prefix")
		}
		if delivery.AccountID != fixture.AccountID || delivery.ContextID != fixture.ContextID ||
			delivery.ThreadID != fixture.ThreadID || delivery.ReplyTarget != fixture.ReplyTarget ||
			delivery.OriginatingEvent != fixture.OriginatingEventID {
			t.Error("fixture routing not propagated")
		}
		if index != 2 && delivery.Text != "[Orka conformance check] No action required." {
			t.Error("fixture probe text is not the fixed safe text")
		}
		if index != 1 && index != 5 {
			if delivery.DeliveryID == "" || seenIDs[delivery.DeliveryID] {
				t.Error("fixture IDs reused across probes or runs")
			}
			seenIDs[delivery.DeliveryID] = true
		}
	}
}

func assertLegacyRun(t *testing.T, deliveries []protocol.DeliveryRequest) {
	t.Helper()
	wantIDs := []string{
		"conformance-auth", "conformance-auth", "conformance-size-text", "conformance-size-body",
		"conformance-idempotency", "conformance-idempotency",
	}
	for index, delivery := range deliveries {
		if delivery.DeliveryID != wantIDs[index] || delivery.AccountID != "conformance" ||
			delivery.ContextID != "conformance" || delivery.ReplyTarget != "conformance" ||
			delivery.ThreadID != "" || delivery.OriginatingEvent != "conformance-event" {
			t.Error("legacy IDs or route changed")
		}
		if index != 2 && delivery.Text != "conformance authentication probe" {
			t.Error("legacy text changed")
		}
	}
}

func TestCheckDeliveryFixtureRejectsBeforeNetwork(t *testing.T) {
	cases := map[string]func(*DeliveryFixture){
		"account missing":                 func(f *DeliveryFixture) { f.AccountID = "" },
		"context blank":                   func(f *DeliveryFixture) { f.ContextID = " \t" },
		"reply missing":                   func(f *DeliveryFixture) { f.ReplyTarget = "" },
		"event missing":                   func(f *DeliveryFixture) { f.OriginatingEventID = "" },
		"account too long":                func(f *DeliveryFixture) { f.AccountID = strings.Repeat("a", protocol.MaxIdentityBytes+1) },
		"account leading control":         func(f *DeliveryFixture) { f.AccountID = "\n" + f.AccountID },
		"account whitespace padding":      func(f *DeliveryFixture) { f.AccountID = strings.Repeat(" ", protocol.MaxIdentityBytes) + f.AccountID },
		"context trailing control":        func(f *DeliveryFixture) { f.ContextID += "\t" },
		"reply leading control":           func(f *DeliveryFixture) { f.ReplyTarget = "\r" + f.ReplyTarget },
		"event whitespace padding":        func(f *DeliveryFixture) { f.OriginatingEventID += strings.Repeat(" ", protocol.MaxIdentityBytes) },
		"context control":                 func(f *DeliveryFixture) { f.ContextID = "bad\x00context" },
		"reply invalid UTF8":              func(f *DeliveryFixture) { f.ReplyTarget = "bad\xffreply" },
		"thread too long":                 func(f *DeliveryFixture) { f.ThreadID = strings.Repeat("t", protocol.MaxIdentityBytes+1) },
		"thread control":                  func(f *DeliveryFixture) { f.ThreadID = "bad\nthread" },
		"thread invalid UTF8":             func(f *DeliveryFixture) { f.ThreadID = "bad\xffthread" },
		"reference fixtures incompatible": func(*DeliveryFixture) {},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			fixture := testDeliveryFixture()
			mutate(&fixture)
			calls := 0
			client := &http.Client{Transport: fixtureRoundTripper(func(*http.Request) (*http.Response, error) {
				calls++
				return nil, errors.New("network must not be used")
			})}
			result := Check(context.Background(), Target{BaseURL: "https://adapter.invalid", AuthorizationValue: "test-auth", HTTPClient: client, DeliveryFixture: &fixture, ReferenceFixtures: name == "reference fixtures incompatible"})
			if calls != 0 {
				t.Errorf("made %d network calls before fixture rejection", calls)
			}
			if result.Passed || !strings.Contains(result.Message, "fixture") {
				t.Error("expected sanitized fixture rejection")
			}
		})
	}
}

func TestCheckDeliveryFixtureOutputPrivacy(t *testing.T) {
	for _, mode := range []string{"capabilities", "protocol version", "transport error"} {
		t.Run(mode, func(t *testing.T) {
			fixture := testDeliveryFixture()
			if mode == "protocol version" {
				fixture.AccountID = protocol.Version
			}
			identities := []string{fixture.AccountID, fixture.ContextID, fixture.ThreadID, fixture.ReplyTarget, fixture.OriginatingEventID}
			fail := mode == "transport error"
			caps := defaultCapabilities()
			caps.AdapterName = strings.Join(identities, " ")
			caps.AdapterVersion = fixture.OriginatingEventID
			server := httptest.NewServer(testAdapterHandler("test-auth", caps, func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				var delivery protocol.DeliveryRequest
				if json.Unmarshal(body, &delivery) != nil || len(body) > protocol.MaxHTTPBodyBytes || protocol.ValidateDeliveryRequest(&delivery) != nil {
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				writeTestJSON(w, http.StatusOK, protocol.DeliveryResponse{Status: protocol.DeliveryStatusDelivered, ProviderMessageID: "test-message"})
			}))
			defer server.Close()
			client := server.Client()
			if fail {
				client = &http.Client{Transport: fixtureRoundTripper(func(*http.Request) (*http.Response, error) { return nil, errors.New(strings.Join(identities, " ")) })}
			}
			result := Check(context.Background(), Target{BaseURL: server.URL, AuthorizationValue: "test-auth", HTTPClient: client, DeliveryFixture: &fixture})
			encoded, err := json.Marshal(result)
			if err != nil {
				t.Fatal(err)
			}
			for _, identity := range identities {
				if bytes.Contains(encoded, []byte(identity)) {
					t.Error("result exposed a fixture identity")
				}
			}
			if result.Passed == fail || (!fail && result.Capabilities == nil) {
				t.Error("privacy test did not reach the expected success or error path")
			}
			if !bytes.Contains(encoded, []byte("[REDACTED]")) {
				t.Error("expected redaction marker")
			}
		})
	}
}

func TestCheckDeliveryFixtureMasksOverlappingOutputFields(t *testing.T) {
	for _, mode := range []string{
		"fixture overlap", "token overlap", "probe token overlap", "truncated probe error", "truncated delivery error",
	} {
		t.Run(mode, func(t *testing.T) {
			fixture := testDeliveryFixture()
			fixture.AccountID = "account42"
			fixture.ContextID = "context-account42-suffix"
			fixture.ThreadID = protocol.Version
			caps := defaultCapabilities()
			if mode == "fixture overlap" {
				caps.AdapterName = fixture.ContextID
				caps.AdapterVersion = "build-" + fixture.ContextID
			} else {
				fixture.ContextID = "context-test-auth-suffix"
			}
			server := httptest.NewServer(testAdapterHandler("test-auth", caps, func(w http.ResponseWriter, r *http.Request) {
				// Finish the size probes before injecting the final delivery error.
				_, _ = io.Copy(io.Discard, r.Body)
				w.WriteHeader(http.StatusBadRequest)
			}))
			defer server.Close()
			transport := server.Client().Transport
			client := &http.Client{Transport: fixtureRoundTripper(func(r *http.Request) (*http.Response, error) {
				fail := r.Method == http.MethodPost || mode == "probe token overlap" || mode == "truncated probe error"
				if mode == "truncated delivery error" {
					fail = r.Method == http.MethodPost && r.Header.Get("Authorization") == "Bearer test-auth" &&
						r.ContentLength < protocol.MaxTextBytes
				}
				if fail {
					message := "failed route " + fixture.ContextID
					if strings.HasPrefix(mode, "truncated") {
						message = strings.Repeat("x", conformanceMessageLimit-90) + message
					}
					return nil, errors.New(message)
				}
				return transport.RoundTrip(r)
			})}
			result := Check(context.Background(), Target{
				BaseURL: server.URL, AuthorizationValue: "test-auth", HTTPClient: client, DeliveryFixture: &fixture,
			})
			if result.Passed || result.Message != "[REDACTED]" {
				t.Error("fixture-bearing error was not masked as a whole field")
			}
			if mode == "probe token overlap" || mode == "truncated probe error" {
				return
			}
			if result.Capabilities == nil {
				t.Fatal("positive control did not reach capabilities")
			}
			if result.Capabilities.ProtocolVersion != "[REDACTED]" {
				t.Error("fixture-bearing protocol version was not masked")
			}
			if mode == "fixture overlap" {
				if result.Capabilities.AdapterName != "[REDACTED]" || result.Capabilities.AdapterVersion != "[REDACTED]" {
					t.Error("overlapping capability identities were not masked as whole fields")
				}
			} else if result.Capabilities.AdapterName != caps.AdapterName || result.Capabilities.AdapterVersion != caps.AdapterVersion {
				t.Error("unrelated capability fields changed")
			}
		})
	}
}

func TestCheckDeliveryFixtureMasksQuotedErrors(t *testing.T) {
	for name, identity := range map[string]string{
		"quote":             "fixture-chat\"segment",
		"backslash":         "fixture-chat\\segment",
		"unicode separator": "fixture-chat\u2028segment",
		"nested quote":      "fixture-chat\\\"segment/é%41\u2028",
		"deep quote":        "fixture-chat\\\"segment/é%41\u2028",
	} {
		encoded := identity
		depth := map[string]int{"nested quote": 1, "deep quote": 9}[name]
		for range depth {
			encoded = strconv.Quote(encoded)
		}
		for _, mode := range []string{"capabilities", "transport", "truncated transport"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				fixture := testDeliveryFixture()
				fixture.ContextID = identity
				caps := defaultCapabilities()
				caps.ProtocolVersion = encoded
				handler := testAdapterHandler("test-auth", caps, nil)
				if mode != "capabilities" {
					handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						conn, writer, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error("could not hijack test connection")
							return
						}
						defer conn.Close() //nolint:errcheck
						header := "invalid-header " + encoded
						if mode == "truncated transport" {
							header = strings.Repeat("x", conformanceMessageLimit) + header
						}
						if _, err := writer.WriteString("HTTP/1.1 200 OK\r\n" + header + "\r\n\r\n"); err != nil {
							t.Error("could not write malformed test response")
							return
						}
						if err := writer.Flush(); err != nil {
							t.Error("could not flush malformed test response")
						}
					})
				}
				server := httptest.NewServer(handler)
				defer server.Close()
				result := Check(context.Background(), Target{
					BaseURL: server.URL, AuthorizationValue: "test-auth",
					HTTPClient: server.Client(), DeliveryFixture: &fixture,
				})
				if result.Passed || result.Message != redactedValue {
					t.Error("quoted fixture identity was not masked before output truncation")
				}
			})
		}
	}
}

func TestDeliveryFixtureMaskCopiesCapabilities(t *testing.T) {
	fixture := testDeliveryFixture()
	fixture.ThreadID = ""
	caps := defaultCapabilities()
	caps.AdapterName = "adapter-" + fixture.ContextID
	original := caps
	message, masked := fixture.maskResultFields("safe diagnostic", &caps)
	if message != "safe diagnostic" || masked.AdapterName != "[REDACTED]" ||
		masked.ProtocolVersion != caps.ProtocolVersion || masked.AdapterVersion != caps.AdapterVersion {
		t.Error("masking changed safe fields or retained a fixture-bearing field")
	}
	if caps != original || masked == &caps {
		t.Error("masking did not preserve caller-owned capabilities")
	}
}

func TestCheckDeliveryFixtureMasksQuotedEscapes(t *testing.T) {
	fixture := testDeliveryFixture()
	fixture.ContextID = "private&<route>/é😀\\%41"
	encoded, err := json.Marshal(fixture.ContextID)
	if err != nil {
		t.Fatal("could not encode test identity")
	}
	for name, value := range map[string]string{
		"JSON":           string(encoded),
		"ASCII JSON":     `"private\u0026\u003Croute\u003e\/\u00E9\uD83D\uDE00\\%41"`,
		"JSON in query":  url.QueryEscape(string(encoded)),
		"JSON in quotes": strconv.Quote(string(encoded)),
		"ASCII Go":       strconv.QuoteToASCII(fixture.ContextID),
		"Go bytes":       `"private&<route>/\xc3\xa9\xf0\x9f\x98\x80\\%41"`,
		"Go octal":       `"private&<route>/\303\251\360\237\230\200\\%41"`,
	} {
		for _, mode := range []string{"capability field", "capability error"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				caps := defaultCapabilities()
				if mode == "capability error" {
					caps.ProtocolVersion = value
				} else {
					caps.AdapterName = "adapter-" + value
				}
				server := httptest.NewServer(testAdapterHandler("test-auth", caps, nil))
				defer server.Close()
				result := Check(context.Background(), Target{
					BaseURL: server.URL, AuthorizationValue: "test-auth",
					HTTPClient: server.Client(), DeliveryFixture: &fixture,
				})
				if mode == "capability error" {
					if result.Passed || result.Message != redactedValue {
						t.Error("quoted fixture identity was not masked in the error")
					}
				} else if result.Capabilities == nil || result.Capabilities.AdapterName != redactedValue {
					t.Error("quoted fixture identity was not masked in capabilities")
				}
			})
		}
	}
}

func TestCheckDeliveryFixtureMasksEscapedRoutes(t *testing.T) {
	identity := "private account/é"
	escaped := url.PathEscape(identity)
	for name, encoded := range map[string]string{
		"path segment": escaped,
		"lowercase":    strings.ToLower(escaped),
		"mixed case":   strings.ReplaceAll(escaped, "%2F", "%2f"),
		"whole path":   (&url.URL{Path: identity}).EscapedPath(),
		"nested":       url.PathEscape(escaped),
		"query":        url.QueryEscape(identity),
		"nested query": url.QueryEscape(url.QueryEscape(identity)),
		"path query":   url.PathEscape(url.QueryEscape(identity)),
		"query path":   url.QueryEscape(escaped),
	} {
		for _, mode := range []string{"URL", "header", "truncated header", "capabilities"} {
			t.Run(name+"/"+mode, func(t *testing.T) {
				fixture := testDeliveryFixture()
				fixture.AccountID = identity
				caps := defaultCapabilities()
				caps.AdapterName = encoded
				handler := testAdapterHandler("test-auth", caps, nil)
				if mode != "capabilities" {
					handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
						conn, writer, err := w.(http.Hijacker).Hijack()
						if err != nil {
							t.Error("could not hijack test connection")
							return
						}
						defer conn.Close() //nolint:errcheck
						if mode == "URL" {
							return
						}
						header := "invalid-header 100% complete %ZZ " + encoded
						if mode == "truncated header" {
							header = strings.Repeat("x", conformanceMessageLimit) + header
						}
						if _, err := writer.WriteString("HTTP/1.1 200 OK\r\n" + header + "\r\n\r\n"); err != nil {
							t.Error("could not write malformed response")
							return
						}
						if err := writer.Flush(); err != nil {
							t.Error("could not flush malformed response")
						}
					})
				}
				server := httptest.NewServer(handler)
				defer server.Close()
				endpoint := server.URL
				if mode == "URL" {
					endpoint += "/accounts/" + encoded + "/adapter"
				}
				result := Check(context.Background(), Target{
					BaseURL: endpoint, AuthorizationValue: "test-auth",
					HTTPClient: server.Client(), DeliveryFixture: &fixture,
				})
				if mode == "capabilities" {
					if result.Capabilities == nil || result.Capabilities.AdapterName != redactedValue {
						t.Error("escaped fixture identity was not masked in capabilities")
					}
					return
				}
				if result.Passed || result.Message != redactedValue {
					t.Error("escaped fixture identity was not masked before output truncation")
				}
			})
		}
	}
}

func TestDeliveryFixtureMaskEscapingBoundsAndSafeFields(t *testing.T) {
	fixture := testDeliveryFixture()
	fixture.ContextID = "private account/é"
	encoded := fixture.ContextID
	for range 20 {
		encoded = url.PathEscape(encoded)
	}
	message, _ := fixture.maskResultFields("failed route "+encoded, nil)
	if message != redactedValue {
		t.Error("deeply escaped fixture identity was not masked")
	}
	caps := defaultCapabilities()
	caps.AdapterName = `safe%2Fadapter\u0026\uD83D\uDE00\U0001f680`
	safe := "safe%20route is 100% ready, a+b"
	message, masked := fixture.maskResultFields(safe, &caps)
	if message != safe || *masked != caps {
		t.Error("safe escaped fields changed")
	}
}

func TestDeliveryFixtureMaskQueryEncodingPreservesLiteralPlus(t *testing.T) {
	for _, identity := range []string{"private account", "private+account /é", "private+account"} {
		for name, encode := range map[string]func(string) string{
			"path":  url.PathEscape,
			"query": url.QueryEscape,
			"path query": func(value string) string {
				return url.PathEscape(url.QueryEscape(value))
			},
			"query path": func(value string) string {
				return url.QueryEscape(url.PathEscape(value))
			},
		} {
			t.Run(identity+"/"+name, func(t *testing.T) {
				fixture := testDeliveryFixture()
				fixture.AccountID = identity
				message, _ := fixture.maskResultFields("failed route "+encode(identity), nil)
				if message != redactedValue {
					t.Error("query or path escaped identity was not masked")
				}
			})
		}
	}
}

func TestDeliveryFixtureAcceptsIdentityBoundsAndOptionalThread(t *testing.T) {
	for _, thread := range []string{"", strings.Repeat("t", protocol.MaxIdentityBytes)} {
		fixture := DeliveryFixture{
			AccountID:          strings.Repeat("a", protocol.MaxIdentityBytes),
			ContextID:          strings.Repeat("c", protocol.MaxIdentityBytes),
			ReplyTarget:        strings.Repeat("r", protocol.MaxIdentityBytes),
			OriginatingEventID: strings.Repeat("e", protocol.MaxIdentityBytes),
			ThreadID:           thread,
		}
		delivery, err := fixture.deliveryRequest()
		if err != nil || protocol.ValidateDeliveryRequest(&delivery) != nil || delivery.ThreadID != thread {
			t.Error("valid boundary fixture rejected or thread changed")
		}
	}
}

type fixtureRoundTripper func(*http.Request) (*http.Response, error)

func (f fixtureRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
