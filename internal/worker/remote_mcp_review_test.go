package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/contexttoken"
)

func TestRemoteMCPDiscoveredSchemaNumericBudget(t *testing.T) {
	for _, tc := range []struct {
		name, reviewed, observed string
		valid                    bool
	}{
		{"unchanged", `"maximum":1`, `"maximum":1`, true},
		{"equivalent exact number", `"maximum":9007199254740993`, `"maximum":9007199254740993.0`, true},
		{"equivalent exponent boundary", `"maximum":1e-65535`, `"maximum":1E-065535`, true},
		{"equivalent aggregate boundary", `"minimum":1e-32767,"maximum":1e-32767`, `"maximum":1E-032767,"minimum":1E-032767`, true},
		{"oversized exponent", `"maximum":1`, `"maximum":1e` + strings.Repeat("9", 8192), false},
		{"equivalent beyond exponent budget", `"maximum":1e-65535`, `"maximum":10e-65536`, false},
		{"equivalent beyond aggregate budget", `"minimum":1e-32767,"maximum":1e-32767`, `"minimum":10e-32768,"maximum":1e-32767`, false},
	} {
		for _, phase := range []string{"startup", "invocation"} {
			t.Run(tc.name+"/"+phase, func(t *testing.T) {
				f := &remoteProtocolFixture{list: func(_ int, _ json.RawMessage) string {
					return `{"tools":[{"name":"service_health","inputSchema":{"type":"object","properties":{"value":{` + tc.observed + `}}}}]}`
				}}
				e := remoteTestExecutor(t, f.serve(t))
				tool := remoteTestTool(t)
				tool.Spec.Parameters.Raw = []byte(`{"type":"object","properties":{"value":{` + tc.reviewed + `}}}`)
				var err error
				if phase == "startup" {
					err = e.VerifyRemoteMCPTool(t.Context(), tool)
				} else {
					_, err = e.Execute(t.Context(), tool, json.RawMessage(`{}`))
				}
				if tc.valid {
					if err != nil {
						t.Fatal("valid-equivalent discovered schema was rejected")
					}
				} else if err == nil || err.Error() != "remote MCP session: remote MCP numeric expansion exceeds limit" {
					t.Errorf("overbudget discovered schema did not return the generic numeric budget error: %v", err)
				}
				wantCalls := int32(0)
				if tc.valid && phase == "invocation" {
					wantCalls = 1
				}
				if f.calls.Load() != wantCalls {
					t.Errorf("tools/call count = %d; want %d", f.calls.Load(), wantCalls)
				}
				if f.lists.Load() != 1 || f.cleanup.Load() != 1 {
					t.Error("discovery did not inspect and clean up its bounded session")
				}
			})
		}
	}
}

func TestRemoteMCPSSEOptionalFieldSpace(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		valid       bool
	}{
		{"space", "event: message", true},
		{"no space", "event:message", true},
		{"extra space", "event:  message", false},
		{"different event", "event: sampling", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := readRemoteMCPEvent(strings.NewReader(tc.event + "\ndata:{}\n\n"))
			if (err == nil) != tc.valid {
				t.Fatalf("SSE event acceptance = %t; want %t", err == nil, tc.valid)
			}
		})
	}
}

func TestRemoteMCPSSETerminalResponseBoundary(t *testing.T) {
	response := "data: {\"jsonrpc\":\"2.0\",\"id\":\"1\",\"result\":{\"ok\":true}}\n\n"
	interaction := "data: {\"jsonrpc\":\"2.0\",\"id\":\"interaction\",\"method\":\"sampling/createMessage\",\"params\":{}}\n\n"
	for _, mode := range []string{"open after response", "interaction after response", "interaction before response"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if mode == "interaction before response" {
					_, _ = w.Write([]byte(interaction))
				}
				_, _ = w.Write([]byte(response))
				if mode == "interaction after response" {
					_, _ = w.Write([]byte(interaction))
				}
				if mode == "open after response" {
					w.(http.Flusher).Flush()
					<-r.Context().Done()
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = remoteMCPExchange(server.Client(), req, "1")
			if (err != nil) != (mode == "interaction before response") {
				t.Fatal("SSE did not stop at the validated terminal response or reject a preceding interaction")
			}
		})
	}
}

func TestRemoteMCPResponseIDsCompareExactStringValues(t *testing.T) {
	for _, tc := range []struct {
		name, id string
		valid    bool
	}{
		{"plain", `"1"`, true},
		{"escaped", `"\u0031"`, true},
		{"number", `1`, false},
		{"null", `null`, false},
		{"boolean", `true`, false},
		{"object", `{}`, false},
		{"array", `["1"]`, false},
		{"different", `"2"`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":` + tc.id + `,"result":{"ok":true}}`))
			}))
			defer server.Close()
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL, nil)
			if err != nil {
				t.Fatal(err)
			}
			_, _, err = remoteMCPExchange(server.Client(), req, "1")
			if (err == nil) != tc.valid {
				t.Fatalf("response identity acceptance = %t; want %t", err == nil, tc.valid)
			}
		})
	}
}

func TestRemoteMCPRedactsBoundAndExchangedTransactionTokens(t *testing.T) {
	for _, exchange := range []bool{false, true} {
		name := "bound"
		if exchange {
			name = "exchanged"
		}
		t.Run(name, func(t *testing.T) {
			f := &remoteProtocolFixture{result: `{"content":[{"type":"text","text":"test-task-authority transaction-token test-resource-credential"}],"structuredContent":{"test-task-authority":{"nested":["test-task-authority","transaction-token","test-resource-credential"]}}}`}
			e := remoteTestExecutor(t, f.serve(t))
			if exchange {
				e.SetTransactionExchangeConfig(&TransactionExchangeConfig{
					TTS:       contexttoken.TTSConfig{Endpoint: "https://issuer.example.test/token", TokenSource: contexttoken.TTSTokenSourceIncoming},
					Exchanger: fakeContextTokenExchanger{},
				})
			}
			result, err := e.Execute(t.Context(), remoteTestTool(t), json.RawMessage(`{}`))
			if err != nil {
				t.Fatal("remote execution failed")
			}
			secrets := []string{"test-task-authority", "test-resource-credential"}
			if exchange {
				secrets = append(secrets, "transaction-token")
			}
			for _, secret := range secrets {
				if strings.Contains(result, secret) {
					t.Fatal("remote result exposed a bound credential")
				}
			}
		})
	}
}
