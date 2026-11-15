package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/transactiontoken"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

func remoteTestTool(t *testing.T) *corev1alpha1.Tool {
	t.Helper()
	var tool corev1alpha1.Tool
	err := json.Unmarshal([]byte(`{"metadata":{"name":"health","namespace":"team","uid":"tool-1","generation":1},"spec":{"description":"Operator reviewed health lookup","parameters":{"type":"object","properties":{"nested":{"type":"object"}}},"mcp":{"remote":{"url":"https://8.8.8.8/mcp?route=health","toolName":"service_health"}},"http":{"authSecretRef":{"name":"remote-auth","key":"token"},"outboundAccessPolicyRef":{"name":"remote-egress"}}}}`), &tool)
	if err != nil {
		t.Fatal(err)
	}
	return &tool
}

func remoteTestExecutor(t *testing.T, gateway *httptest.Server) *ToolExecutor {
	t.Helper()
	u, err := url.Parse(gateway.URL)
	if err != nil {
		t.Fatal(err)
	}
	resolver := &fakeOutboundAccessResolver{resolution: outboundaccess.Resolution{Adapter: outboundaccess.AdapterGateway, GatewayScheme: u.Scheme, GatewayHost: u.Host}}
	kube := fake.NewSimpleClientset(&corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "remote-auth", Namespace: "team"}, Data: map[string][]byte{"token": []byte("test-resource-credential")}}, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "other-auth", Namespace: "team"}, Data: map[string][]byte{"token": []byte("wrong-credential")}})
	e := NewToolExecutorForNamespace("team", kube, gateway.Client(), resolver)
	e.SetTransactionAuthority("test-task-authority", []string{outboundaccess.DefaultCredentialReadScope})
	e.SetTransactionCredentialAuthority(true, true, "remote-auth")
	return e
}

func TestRemoteMCPGatewaySequenceAndNumericFidelity(t *testing.T) {
	var sequence []string
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "8.8.8.8" || r.URL.RequestURI() != "/mcp?route=health" {
			t.Error("gateway lost original authority/path")
		}
		if r.Header.Get("Authorization") != "Bearer test-resource-credential" || r.Header.Get(transactiontoken.HeaderName) != "test-task-authority" {
			t.Error("missing exact credential or task authority")
		}
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodDelete {
			sequence = append(sequence, "DELETE")
			w.WriteHeader(http.StatusNoContent)
			return
		}
		var req struct {
			Method string          `json:"method"`
			ID     string          `json:"id"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		sequence = append(sequence, req.Method)
		if req.Method != "initialize" && (r.Header.Get(mcpSessionIDHeader) != "session-test" || r.Header.Get(mcpProtocolVersionHeader) != "2025-06-18") {
			t.Error("session/version missing")
		}
		switch req.Method {
		case "initialize":
			w.Header().Set(mcpSessionIDHeader, "session-test")
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"initialize","result":{"protocolVersion":"2025-06-18","capabilities":{"tools":{}},"serverInfo":{"name":"untrusted","version":"1"},"instructions":"NEVER COPY THIS INTO THE MODEL"}}`))
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted)
		case "tools/list":
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"` + req.ID + `","result":{"tools":[{"name":"service_health","description":"NEVER COPY THIS EITHER","inputSchema":{"properties":{"nested":{"type":"object"}},"type":"object"}}]}}`))
		case "tools/call":
			if !strings.Contains(string(req.Params), `"name":"service_health"`) || !strings.Contains(string(req.Params), `9007199254740993`) || !strings.Contains(string(req.Params), `0.12345678901234567890123456789`) {
				t.Errorf("wrong call: %s", req.Params)
			}
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":"1","result":{"content":[{"type":"text","text":"healthy"}],"structuredContent":{"nested":[9007199254740993,{"decimal":0.12345678901234567890123456789,"negative":-9007199254740995}]}}}`))
		default:
			t.Errorf("unexpected method %s", req.Method)
		}
	}))
	defer gateway.Close()
	e := remoteTestExecutor(t, gateway)
	result, err := e.Execute(context.Background(), remoteTestTool(t), json.RawMessage(`{"nested":{"integer":9007199254740993,"decimal":0.12345678901234567890123456789}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"healthy", "9007199254740993", "0.12345678901234567890123456789", "-9007199254740995"} {
		if !strings.Contains(result, value) {
			t.Errorf("missing %s from result %s", value, result)
		}
	}
	if !reflect.DeepEqual(sequence, []string{"initialize", "notifications/initialized", "tools/list", "tools/call", "DELETE"}) {
		t.Fatalf("sequence = %v", sequence)
	}
}

func TestRemoteMCPCredentialSelectorIgnoresMountedAndInjectedFallbacks(t *testing.T) {
	f := &remoteProtocolFixture{intercept: func(w http.ResponseWriter, r *http.Request, method string) bool {
		if r.Header.Get("Authorization") != "Bearer test-resource-credential" {
			t.Error("credential not selected by exact namespace/name/key")
		}
		return false
	}}
	e := remoteTestExecutor(t, f.serve(t))
	e.secretPath = t.TempDir()
	mounted := filepath.Join(e.secretPath, "remote-auth")
	if err := os.MkdirAll(mounted, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(mounted, "token"), []byte("wrong-mounted-value"), 0600); err != nil {
		t.Fatal(err)
	}
	e.SetAuthSecretValue("remote-auth", "token", "wrong-injected-value")
	if err := e.VerifyRemoteMCPTool(t.Context(), remoteTestTool(t)); err != nil {
		t.Fatal(err)
	}
	before := f.requests.Load()
	if err := e.k8sClient.CoreV1().Secrets("team").Delete(t.Context(), "remote-auth", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	if err := e.VerifyRemoteMCPTool(t.Context(), remoteTestTool(t)); err == nil {
		t.Fatal("missing exact Secret fell back to mounted or injected credential")
	}
	if f.requests.Load() != before {
		t.Fatal("missing exact selector sent protocol traffic")
	}
}

func TestRemoteMCPInvalidAuthorityMakesNoRequests(t *testing.T) {
	for _, name := range []string{"empty token", "implicit authority", "empty scopes", "denied credential", "wrong credential", "missing exact secret", "empty secret", "no gateway", "direct policy", "private original"} {
		t.Run(name, func(t *testing.T) {
			var calls atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1); w.WriteHeader(http.StatusBadGateway) }))
			defer gateway.Close()
			e := remoteTestExecutor(t, gateway)
			tool := remoteTestTool(t)
			switch name {
			case "empty token":
				e.SetTransactionAuthority("", []string{outboundaccess.DefaultCredentialReadScope})
			case "implicit authority":
				e.transactionAuthoritySet = false
			case "empty scopes":
				e.SetTransactionAuthority("test-task-authority", nil)
			case "denied credential":
				e.SetTransactionCredentialAuthority(true, false, "")
			case "wrong credential":
				e.SetTransactionCredentialAuthority(true, true, "other-auth")
			case "missing exact secret":
				tool.Spec.HTTP.AuthSecretRef.Name = "absent"
				e.SetTransactionCredentialAuthority(true, true, "")
			case "empty secret":
				_, err := e.k8sClient.CoreV1().Secrets("team").Update(t.Context(), &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "remote-auth", Namespace: "team"}, Data: map[string][]byte{"token": []byte(" ")}}, metav1.UpdateOptions{})
				if err != nil {
					t.Fatal(err)
				}
			case "no gateway":
				e.outboundResolver = nil
			case "direct policy":
				e.outboundResolver.(*fakeOutboundAccessResolver).resolution.Adapter = outboundaccess.AdapterDirect
			case "private original":
				raw, _ := json.Marshal(tool)
				raw = []byte(strings.ReplaceAll(string(raw), "8.8.8.8", "127.0.0.1"))
				if err := json.Unmarshal(raw, tool); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.Execute(t.Context(), tool, json.RawMessage(`{}`)); err == nil {
				t.Fatal("accepted invalid authority")
			}
			if calls.Load() != 0 {
				t.Fatalf("made %d gateway requests", calls.Load())
			}
		})
	}
}
