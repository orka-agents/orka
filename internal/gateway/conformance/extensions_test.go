package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/gateway/protocol"
)

func TestProbeRejectsCredentialEchoInExtensions(t *testing.T) {
	for _, credential := range []string{"extension-echo-sentinel", "ghp_abcdefghijklmnopqrstuvwxyz"} {
		for _, escaped := range []bool{false, true} {
			name := credential
			if escaped {
				name += "/escaped JSON"
			}
			t.Run(name, func(t *testing.T) {
				caps := defaultCapabilities()
				caps.Capabilities.Extensions = []string{"future.safe.v1", "echo-" + credential}
				body, err := json.Marshal(caps)
				if err != nil {
					t.Fatal(err)
				}
				if escaped {
					first := `\u0065`
					if credential[0] == 'g' {
						first = `\u0067`
					}
					body = bytes.ReplaceAll(body, []byte(credential), []byte(first+credential[1:]))
				}
				base := testAdapterHandler(credential, caps, nil)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/v1/capabilities" && r.Header.Get("Authorization") == "Bearer "+credential {
						w.Header().Set("Content-Type", "application/json")
						_, _ = w.Write(body)
						return
					}
					base.ServeHTTP(w, r)
				}))
				defer server.Close()
				target := Target{BaseURL: server.URL, AuthorizationValue: credential, HTTPClient: server.Client()}
				for _, result := range []ProbeResult{
					Probe(context.Background(), target),
					probeResultFromCheck(Check(context.Background(), target)),
				} {
					if result.Passed || result.Message != "capability probe returned sensitive data" {
						t.Fatalf("credential-bearing extension did not fail the capability probe: %+v", result)
					}
					encoded, err := json.Marshal(result)
					if err != nil {
						t.Fatal(err)
					}
					if bytes.Contains(encoded, []byte(credential)) || result.Capabilities != nil {
						t.Fatal("rejected capability output retained credential-bearing extensions")
					}
				}
			})
		}
	}
}

func probeResultFromCheck(result CheckResult) ProbeResult {
	return ProbeResult(result)
}

func TestCapabilitiesContainCredentialChecksAllExtensions(t *testing.T) {
	caps := defaultCapabilities()
	caps.Capabilities.Extensions = make([]string, 20)
	caps.Capabilities.Extensions[19] = "echo-fixture-credential"
	if !capabilitiesContainCredential(&caps, " fixture-credential ") {
		t.Fatal("credential detector ignored extension outside output bound")
	}
	if capabilitiesContainCredential(&caps, "") || capabilitiesContainCredential(nil, "fixture-credential") {
		t.Fatal("credential detector rejected empty credential or absent capabilities")
	}
}

func TestResultSanitizersDropUnsafeExtensionsAndCopySlices(t *testing.T) {
	for name, sanitize := range map[string]func(*protocol.CapabilitiesResponse) *protocol.CapabilitiesResponse{
		"check": func(caps *protocol.CapabilitiesResponse) *protocol.CapabilitiesResponse {
			return SanitizeCheckResult(CheckResult{Capabilities: caps}, "fixture-credential").Capabilities
		},
		"probe": func(caps *protocol.CapabilitiesResponse) *protocol.CapabilitiesResponse {
			return sanitizeProbeResult(ProbeResult{Capabilities: caps}, "fixture-credential").Capabilities
		},
	} {
		t.Run(name, func(t *testing.T) {
			caps := defaultCapabilities()
			caps.Capabilities.Extensions = []string{
				"future.safe.v1", "echo-fixture-credential", "future.safe.v1", " Future.v1", "future.\x00v1",
				"token=fixture-value", "[REDACTED]", string([]byte{'f', 0xff}), strings.Repeat("a", 257),
				"未知の拡張/v1", "Future Extension v1",
			}
			original := append([]string(nil), caps.Capabilities.Extensions...)
			got := sanitize(&caps)
			want := []string{"future.safe.v1", "未知の拡張/v1", "Future Extension v1"}
			if got == nil || !reflect.DeepEqual(got.Capabilities.Extensions, want) {
				t.Fatalf("sanitized extensions = %+v, want %q", got, want)
			}
			if !reflect.DeepEqual(caps.Capabilities.Extensions, original) || got == &caps {
				t.Fatal("sanitizer mutated original capabilities")
			}
			got.Capabilities.Extensions[0] = "changed-output"
			if !reflect.DeepEqual(caps.Capabilities.Extensions, original) {
				t.Fatal("sanitizer aliased original extension storage")
			}
			caps.Capabilities.Extensions[9] = "changed-input"
			if got.Capabilities.Extensions[1] != "未知の拡張/v1" {
				t.Fatal("sanitizer retained caller-owned extension storage")
			}
		})
	}
}

func TestSanitizeCheckResultOmitsAllRejectedExtensions(t *testing.T) {
	caps := defaultCapabilities()
	caps.Capabilities.Extensions = []string{"fixture-credential", "[REDACTED]"}
	got := SanitizeCheckResult(CheckResult{Capabilities: &caps}, "fixture-credential")
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if got.Capabilities.Capabilities.Extensions != nil || bytes.Contains(encoded, []byte(`"extensions"`)) {
		t.Fatalf("rejected extension list was not omitted: %s", encoded)
	}
}

func TestDeliveryFixtureMaskDropsExtensionsAndCopiesSlices(t *testing.T) {
	fixture := testDeliveryFixture()
	caps := defaultCapabilities()
	caps.Capabilities.Extensions = []string{
		"future.safe.v1", fixture.AccountID, "echo-" + fixture.ContextID, fixture.ThreadID,
		fixture.ReplyTarget, fixture.OriginatingEventID, url.QueryEscape(fixture.ContextID),
		"future.safe.v2",
	}
	original := append([]string(nil), caps.Capabilities.Extensions...)
	message, got := fixture.maskResultFields("safe diagnostic", &caps)
	want := []string{"future.safe.v1", "future.safe.v2"}
	if message != "safe diagnostic" || got == nil || !reflect.DeepEqual(got.Capabilities.Extensions, want) {
		t.Fatalf("fixture mask retained private extensions or changed safe output: %+v", got)
	}
	got.Capabilities.Extensions[0] = "changed-output"
	if !reflect.DeepEqual(caps.Capabilities.Extensions, original) {
		t.Fatal("fixture mask changed caller-owned extension storage")
	}
	caps.Capabilities.Extensions[7] = "changed-input"
	if got.Capabilities.Extensions[1] != "future.safe.v2" {
		t.Fatal("fixture mask retained caller-owned extension storage")
	}
}

func TestProbeDeliveryFixtureMasksEscapedExtensionIdentities(t *testing.T) {
	fixture := testDeliveryFixture()
	fixture.ContextID = "private&<route>/é😀"
	for _, value := range []string{
		fixture.ContextID, `private\u0026\u003croute\u003e\/\u00e9\ud83d\ude00`, url.PathEscape(fixture.ContextID),
	} {
		t.Run(value, func(t *testing.T) {
			caps := defaultCapabilities()
			caps.Capabilities.Extensions = []string{value, "future.safe.v1"}
			server := httptest.NewServer(testAdapterHandler("test-auth", caps, nil))
			defer server.Close()
			result := Probe(context.Background(), Target{
				BaseURL: server.URL, AuthorizationValue: "test-auth", HTTPClient: server.Client(), DeliveryFixture: &fixture,
			})
			if !result.Passed || result.Capabilities == nil {
				t.Fatalf("positive fixture probe failed: %s", result.Message)
			}
			if got := result.Capabilities.Capabilities.Extensions; !reflect.DeepEqual(got, []string{"future.safe.v1"}) {
				t.Fatalf("fixture-bearing extensions retained: %q", got)
			}
		})
	}
}
