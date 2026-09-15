package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/outboundaccess"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

type remoteProtocolFixture struct {
	calls, lists, requests, cleanup atomic.Int32
	initialize                      string
	list                            func(int, json.RawMessage) string
	result                          string
	call                            func(json.RawMessage)
	intercept                       func(http.ResponseWriter, *http.Request, string) bool
}

func (f *remoteProtocolFixture) serve(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.requests.Add(1)
		if r.Method == http.MethodDelete {
			f.cleanup.Add(1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			Method, ID string
			Params     json.RawMessage
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		if req.Method == "tools/call" {
			f.calls.Add(1)
		}
		w.Header().Set("Content-Type", "application/json")
		if f.intercept != nil && f.intercept(w, r, req.Method) {
			return
		}
		result := ""
		switch req.Method {
		case "initialize":
			w.Header().Set(mcpSessionIDHeader, "fixture-session")
			result = f.initialize
			if result == "" {
				result = `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"fixture","version":"1"}}`
			}
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
			return
		case "tools/list":
			page := int(f.lists.Add(1)) - 1
			if f.list != nil {
				result = f.list(page, req.Params)
			} else {
				result = `{"tools":[{"name":"service_health","inputSchema":{"type":"object","properties":{"nested":{"type":"object"}}}}]}`
			}
		case "tools/call":
			if f.call != nil {
				f.call(req.Params)
			}
			result = f.result
			if result == "" {
				result = `{"content":[{"type":"text","text":"ok"}]}`
			}
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
		_, _ = fmt.Fprintf(w, `{"jsonrpc":"2.0","id":%q,"result":%s}`, req.ID, result)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestRemoteMCPRejectsProtocolAndContent(t *testing.T) {
	for _, tc := range []struct {
		name, initialize, result, wire string
		method                         string
		attempted                      bool
	}{
		{name: "unsupported negotiated version", initialize: `{"protocolVersion":"2024-11-05","capabilities":{"tools":{}}}`},
		{name: "missing capabilities", initialize: `{"protocolVersion":"2025-06-18","capabilities":{}}`},
		{name: "missing server identity", initialize: `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}}}`},
		{name: "malformed tools capability", initialize: `{"protocolVersion":"2025-06-18","capabilities":{"tools":null},"serverInfo":{"name":"test","version":"1"}}`},
		{name: "initialize metadata", initialize: `{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"_meta":{"instruction":"bad"}}`},
		{name: "result metadata", result: `{"content":[],"_meta":{"secret":"bad"}}`, attempted: true},
		{name: "audience annotation", result: `{"content":[{"type":"text","text":"private","annotations":{"audience":["user"]}}]}`, attempted: true},
		{name: "content metadata", result: `{"content":[{"type":"text","text":"private","_meta":{}}]}`, attempted: true},
		{name: "image", result: `{"content":[{"type":"image","data":"AA==","mimeType":"image/png"}]}`, attempted: true},
		{name: "resource link", result: `{"content":[{"type":"resource_link","uri":"file:///secret"}]}`, attempted: true},
		{name: "missing text", result: `{"content":[{"type":"text"}]}`, attempted: true},
		{name: "structured array", result: `{"content":[],"structuredContent":[1]}`, attempted: true},
		{name: "null structured", result: `{"content":[],"structuredContent":null}`, attempted: true},
		{name: "missing content", result: `{"structuredContent":{}}`, attempted: true},
		{name: "tool error", result: `{"content":[{"type":"text","text":"test-resource-credential"}],"isError":true}`, attempted: true},
		{name: "wrong RPC version", method: "tools/list", wire: `{"jsonrpc":"1.0","id":"list-0","result":{"tools":[]}}`},
		{name: "wrong id", method: "tools/list", wire: `{"jsonrpc":"2.0","id":"other","result":{"tools":[]}}`},
		{name: "JSON RPC interaction", method: "tools/list", wire: `{"jsonrpc":"2.0","id":"list-0","method":"sampling/createMessage","params":{}}`},
		{name: "RPC error", method: "tools/list", wire: `{"jsonrpc":"2.0","id":"list-0","error":{"code":-1,"message":"test-resource-credential"}}`},
		{name: "duplicate JSON key", method: "tools/list", wire: `{"jsonrpc":"2.0","id":"other","id":"list-0","result":{"tools":[{"name":"service_health","inputSchema":{"type":"object","properties":{"nested":{"type":"object"}}}}]}}`},
		{name: "case folded field", method: "tools/list", wire: `{"jsonrpc":"2.0","ID":"list-0","result":{"tools":[{"name":"service_health","inputSchema":{"type":"object","properties":{"nested":{"type":"object"}}}}]}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &remoteProtocolFixture{initialize: tc.initialize, result: tc.result}
			if tc.wire != "" {
				f.intercept = func(w http.ResponseWriter, r *http.Request, method string) bool {
					if method != tc.method {
						return false
					}
					_, _ = w.Write([]byte(tc.wire))
					return true
				}
			}
			e := remoteTestExecutor(t, f.serve(t))
			result, err := e.Execute(t.Context(), remoteTestTool(t), json.RawMessage(`{}`))
			if err == nil {
				t.Fatalf("accepted unsupported protocol/content: %s", result)
			}
			if strings.Contains(err.Error(), "test-resource-credential") {
				t.Fatal("credential leaked in error")
			}
			if ToolRequestWasAttempted(err) != tc.attempted {
				t.Fatalf("attempted = %t; want %t; err=%v", ToolRequestWasAttempted(err), tc.attempted, err)
			}
			expected := int32(0)
			if tc.attempted {
				expected = 1
			}
			if f.calls.Load() != expected {
				t.Fatalf("tools/call count %d want %d", f.calls.Load(), expected)
			}
		})
	}
}

func TestRemoteMCPDiscoveryPaginationAndDrift(t *testing.T) {
	descriptor := `{"name":"service_health","description":"untrusted","inputSchema":{"properties":{"nested":{"type":"object"}},"type":"object"}}`
	for _, name := range []string{"selected on last page", "duplicate selected", "duplicate unrelated", "missing selected", "schema drift", "selected metadata", "invalid schema", "page bound", "cursor loop", "size bound", "tool count bound"} {
		t.Run(name, func(t *testing.T) {
			f := &remoteProtocolFixture{list: func(page int, params json.RawMessage) string {
				if name == "size bound" {
					return `{"tools":[{"name":"other","description":"` + strings.Repeat("x", remoteMCPResponseLimit) + `","inputSchema":{}}]}`
				}
				if name == "tool count bound" {
					var descriptors []string
					for i := range 1025 {
						descriptors = append(descriptors, fmt.Sprintf(`{"name":"other-%d","inputSchema":{}}`, i))
					}
					return `{"tools":[` + strings.Join(descriptors, ",") + `]}`
				}
				if name == "page bound" {
					return fmt.Sprintf(`{"tools":[],"nextCursor":"page-%d"}`, page)
				}
				if name == "cursor loop" {
					return `{"tools":[],"nextCursor":"repeat"}`
				}
				if page == 0 {
					return `{"tools":[{"name":"other","inputSchema":{"type":"object"}}],"nextCursor":"next"}`
				}
				if !strings.Contains(string(params), `"cursor":"next"`) {
					t.Error("missing pagination cursor")
				}
				switch name {
				case "selected on last page":
					return `{"tools":[` + descriptor + `]}`
				case "duplicate selected":
					return `{"tools":[` + descriptor + `,` + descriptor + `]}`
				case "duplicate unrelated":
					return `{"tools":[` + descriptor + `,{"name":"other","inputSchema":{}}]}`
				case "missing selected":
					return `{"tools":[]}`
				case "schema drift":
					return `{"tools":[{"name":"service_health","inputSchema":{"type":"object","required":["new"]}}]}`
				case "selected metadata":
					return `{"tools":[{"name":"service_health","_meta":{"audience":"user"},"inputSchema":{"type":"object","properties":{"nested":{"type":"object"}}}}]}`
				default:
					return `{"tools":[{"name":"service_health","inputSchema":null}]}`
				}
			}}
			e := remoteTestExecutor(t, f.serve(t))
			err := e.VerifyRemoteMCPTool(t.Context(), remoteTestTool(t))
			if name == "selected on last page" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("discovery accepted invalid catalog")
			}
			if name == "schema drift" && !strings.Contains(err.Error(), "schema differs") {
				t.Fatalf("safe schema drift context was lost: %v", err)
			}
			if f.calls.Load() != 0 {
				t.Fatal("discovery made a tools/call request")
			}
			if f.lists.Load() > 8 {
				t.Fatal("discovery was unbounded")
			}
			if f.cleanup.Load() != 1 {
				t.Fatal("discovery did not terminate session")
			}
		})
	}
}

func TestRemoteMCPNoRedirectProxyCookieFallbackOrReplay(t *testing.T) {
	for _, mode := range []string{"redirect initialize", "redirect list", "redirect call", "HTTP failure call", "connection failure call", "ambient proxy and cookie"} {
		t.Run(mode, func(t *testing.T) {
			var otherCalls atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { otherCalls.Add(1); w.WriteHeader(http.StatusOK) }))
			defer other.Close()
			f := &remoteProtocolFixture{intercept: func(w http.ResponseWriter, r *http.Request, method string) bool {
				if mode == "ambient proxy and cookie" {
					if r.Header.Get("Cookie") != "" {
						t.Error("ambient cookie reached MCP gateway")
					}
					return false
				}
				target := "tools/call"
				if mode == "redirect initialize" {
					target = "initialize"
				}
				if mode == "redirect list" {
					target = "tools/list"
				}
				if method != target {
					return false
				}
				if strings.HasPrefix(mode, "redirect") {
					http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
				} else if mode == "connection failure call" {
					conn, _, err := w.(http.Hijacker).Hijack()
					if err != nil {
						t.Error(err)
					} else {
						_ = conn.Close()
					}
				} else {
					w.WriteHeader(http.StatusInternalServerError)
				}
				return true
			}}
			gateway := f.serve(t)
			e := remoteTestExecutor(t, gateway)
			proxyURL, _ := url.Parse(other.URL)
			e.client.Transport = &http.Transport{Proxy: http.ProxyURL(proxyURL)}
			jar, _ := cookiejar.New(nil)
			gatewayURL, _ := url.Parse(gateway.URL)
			jar.SetCookies(gatewayURL, []*http.Cookie{{Name: "ambient", Value: "sensitive"}})
			originalURL, _ := url.Parse("https://8.8.8.8/mcp")
			jar.SetCookies(originalURL, []*http.Cookie{{Name: "ambient", Value: "sensitive"}})
			e.client.Jar = jar
			_, err := e.Execute(t.Context(), remoteTestTool(t), json.RawMessage(`{}`))
			if mode == "ambient proxy and cookie" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatal("failed remote call succeeded")
			}
			if otherCalls.Load() != 0 {
				t.Fatal("redirect, proxy or fallback escaped gateway")
			}
			if f.calls.Load() > 1 {
				t.Fatal("tools/call was retried")
			}
		})
	}
}

func TestRemoteMCPRechecksSchemaAndFreezesTransport(t *testing.T) {
	for _, mode := range []string{"schema drift", "credential drift", "gateway redirection"} {
		t.Run(mode, func(t *testing.T) {
			f := &remoteProtocolFixture{}
			gateway := f.serve(t)
			e := remoteTestExecutor(t, gateway)
			tool := remoteTestTool(t)
			if err := e.VerifyRemoteMCPTool(t.Context(), tool); err != nil {
				t.Fatal(err)
			}
			before := f.requests.Load()
			switch mode {
			case "schema drift":
				f.list = func(_ int, _ json.RawMessage) string { return `{"tools":[]}` }
			case "credential drift":
				secret, err := e.k8sClient.CoreV1().Secrets("team").Get(t.Context(), "remote-auth", metav1.GetOptions{})
				if err != nil {
					t.Fatal(err)
				}
				secret.Data["token"] = []byte("replacement")
				_, err = e.k8sClient.CoreV1().Secrets("team").Update(t.Context(), secret, metav1.UpdateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			case "gateway redirection":
				e.outboundResolver.(*fakeOutboundAccessResolver).resolution.GatewayHost = "127.0.0.1:1"
			}
			if _, err := e.Execute(t.Context(), tool, json.RawMessage(`{}`)); err == nil {
				t.Fatal("observed drift did not fail closed")
			}
			if f.calls.Load() != 0 {
				t.Fatal("drift reached tools/call")
			}
			if mode != "schema drift" && f.requests.Load() != before {
				t.Fatal("binding drift sent requests")
			}
		})
	}
}

type remoteBlockingResolver struct{}

func (remoteBlockingResolver) Resolve(ctx context.Context, _ outboundaccess.ResolveRequest) (outboundaccess.Resolution, error) {
	<-ctx.Done()
	return outboundaccess.Resolution{}, ctx.Err()
}

func TestRemoteMCPDeadlineIncludesPreparationAndDiscovery(t *testing.T) {
	for _, mode := range []string{"preparation", "discovery", "SSE discovery"} {
		t.Run(mode, func(t *testing.T) {
			f := &remoteProtocolFixture{intercept: func(w http.ResponseWriter, r *http.Request, method string) bool {
				if method == "tools/list" {
					if mode == "SSE discovery" {
						w.Header().Set("Content-Type", "text/event-stream")
						w.WriteHeader(http.StatusOK)
						w.(http.Flusher).Flush()
					}
					<-r.Context().Done()
					return true
				}
				return false
			}}
			e := remoteTestExecutor(t, f.serve(t))
			if mode == "preparation" {
				e.outboundResolver = remoteBlockingResolver{}
			}
			tool := remoteTestTool(t)
			tool.Spec.HTTP.Timeout = &metav1.Duration{Duration: 20 * time.Millisecond}
			outer, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			start := time.Now()
			err := e.VerifyRemoteMCPTool(outer, tool)
			if err == nil || !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline not preserved: %v", err)
			}
			if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
				t.Fatalf("configured operation deadline ignored: %s", elapsed)
			}
		})
	}
}

func TestRemoteMCPSchemaComparisonPreservesExactNumbers(t *testing.T) {
	for _, remoteMaximum := range []string{"9007199254740993.00", "9007199254740992"} {
		t.Run(remoteMaximum, func(t *testing.T) {
			f := &remoteProtocolFixture{list: func(_ int, _ json.RawMessage) string {
				return `{"tools":[{"name":"service_health","inputSchema":{"type":"object","properties":{"n":{"type":"number","maximum":` + remoteMaximum + `}}}}]}`
			}}
			e := remoteTestExecutor(t, f.serve(t))
			tool := remoteTestTool(t)
			tool.Spec.Parameters.Raw = []byte(`{"properties":{"n":{"maximum":9007199254740993,"type":"number"}},"type":"object"}`)
			err := e.VerifyRemoteMCPTool(t.Context(), tool)
			if (err == nil) != (remoteMaximum == "9007199254740993.00") {
				t.Fatalf("exact numeric schema comparison: %v", err)
			}
		})
	}
}

func TestRemoteMCPRejectsMissingReviewedParametersBeforeNetwork(t *testing.T) {
	f := &remoteProtocolFixture{}
	e := remoteTestExecutor(t, f.serve(t))
	tool := remoteTestTool(t)
	tool.Spec.Parameters = nil
	if err := e.VerifyRemoteMCPTool(t.Context(), tool); err == nil {
		t.Fatal("accepted absent reviewed parameters")
	}
	if f.requests.Load() != 0 {
		t.Fatal("missing reviewed parameters made network requests")
	}
}

func TestRemoteMCPSSERejectsInteractions(t *testing.T) {
	for _, interaction := range []bool{false, true} {
		t.Run(fmt.Sprint(interaction), func(t *testing.T) {
			f := &remoteProtocolFixture{intercept: func(w http.ResponseWriter, r *http.Request, method string) bool {
				if method != "tools/list" {
					return false
				}
				w.Header().Set("Content-Type", "text/event-stream")
				if interaction {
					_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":\"interactive\",\"method\":\"elicitation/create\",\"params\":{}}\n\n"))
				}
				_, _ = w.Write([]byte("data: {\"jsonrpc\":\"2.0\",\"id\":\"list-0\",\"result\":{\"tools\":[{\"name\":\"service_health\",\"inputSchema\":{\"type\":\"object\",\"properties\":{\"nested\":{\"type\":\"object\"}}}}]}}\n\n"))
				return true
			}}
			e := remoteTestExecutor(t, f.serve(t))
			err := e.VerifyRemoteMCPTool(t.Context(), remoteTestTool(t))
			if (err != nil) != interaction {
				t.Fatalf("SSE verification error: %v", err)
			}
		})
	}
}
