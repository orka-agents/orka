package worker

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"

	"github.com/orka-agents/orka/internal/outboundaccess"
)

func TestRemoteMCPDuplicateHeadersRejectBeforeCredentialsAndSend(t *testing.T) {
	for _, discovery := range []bool{true, false} {
		t.Run(map[bool]string{true: "discovery", false: "invocation"}[discovery], func(t *testing.T) {
			f := &remoteProtocolFixture{}
			e := remoteTestExecutor(t, f.serve(t))
			tool := remoteTestTool(t)
			tool.Spec.HTTP.Headers = map[string]string{"X-Tenant": "first", "x-tenant": "second"}
			var err error
			if discovery {
				err = e.VerifyRemoteMCPTool(t.Context(), tool)
			} else {
				_, err = e.Execute(t.Context(), tool, json.RawMessage(`{}`))
			}
			if err == nil || !strings.Contains(err.Error(), "duplicate custom header") {
				t.Errorf("header error = %v, want canonical duplicate rejection", err)
			}
			if actions := e.k8sClient.(*kubefake.Clientset).Actions(); len(actions) != 0 {
				t.Errorf("duplicate headers caused %d Kubernetes credential actions", len(actions))
			}
			if e.outboundResolver.(*fakeOutboundAccessResolver).request.PolicyName != "" {
				t.Error("duplicate headers reached outbound policy resolution")
			}
			if f.requests.Load() != 0 {
				t.Fatalf("duplicate headers sent %d MCP requests", f.requests.Load())
			}
		})
	}
}

func TestRemoteMCPSendGateBlocksDiscoveryAndInvocation(t *testing.T) {
	for _, discovery := range []bool{true, false} {
		t.Run(map[bool]string{true: "discovery", false: "invocation"}[discovery], func(t *testing.T) {
			f := &remoteProtocolFixture{}
			e := remoteTestExecutor(t, f.serve(t))
			tool := remoteTestTool(t)
			denied := errors.New("authority withdrawn before send")
			gates := 0
			e.SetSendGate(func(context.Context) error {
				gates++
				request := e.outboundResolver.(*fakeOutboundAccessResolver).request
				if request.PolicyName != "remote-egress" || !request.MCPBacked {
					t.Error("send gate ran before MCP policy resolution")
				}
				return denied
			})
			var err error
			if discovery {
				err = e.VerifyRemoteMCPTool(t.Context(), tool)
			} else {
				_, err = e.Execute(t.Context(), tool, json.RawMessage(`{}`))
			}
			if !errors.Is(err, denied) || ToolRequestWasAttempted(err) {
				t.Errorf("error = %v, want unattempted send-gate rejection", err)
			}
			if gates != 1 || f.requests.Load() != 0 {
				t.Fatalf("gates = %d, MCP requests = %d, want one gate and zero sends", gates, f.requests.Load())
			}
		})
	}
}

func TestRemoteMCPConfiguredTimeoutOverridesInheritedClientTimeout(t *testing.T) {
	for _, discovery := range []bool{true, false} {
		t.Run(map[bool]string{true: "discovery", false: "invocation"}[discovery], func(t *testing.T) {
			f := &remoteProtocolFixture{intercept: func(_ http.ResponseWriter, _ *http.Request, method string) bool {
				if method == "initialize" {
					// Bounded real HTTP proof: outlast the inherited timeout without
					// making the test wait for the executor's production 30 seconds.
					time.Sleep(30 * time.Millisecond)
				}
				return false
			}}
			e := remoteTestExecutor(t, f.serve(t))
			e.client.Timeout = 5 * time.Millisecond
			tool := remoteTestTool(t)
			tool.Spec.HTTP.Timeout = &metav1.Duration{Duration: 2 * time.Minute}
			var err error
			if discovery {
				err = e.VerifyRemoteMCPTool(t.Context(), tool)
			} else {
				_, err = e.Execute(t.Context(), tool, json.RawMessage(`{}`))
			}
			if err != nil {
				t.Fatalf("configured remote budget was cut short by the inherited client: %v", err)
			}
		})
	}
}

func TestRemoteMCPResolverReceivesArgumentsAndNoConnectorCustody(t *testing.T) {
	f := &remoteProtocolFixture{}
	e := remoteTestExecutor(t, f.serve(t))
	args := json.RawMessage(`{"nested":{"integer":9007199254740993}}`)
	if _, err := e.Execute(t.Context(), remoteTestTool(t), args); err != nil {
		t.Fatal(err)
	}
	request := e.outboundResolver.(*fakeOutboundAccessResolver).request
	if string(request.Arguments) != string(args) || !request.MCPBacked {
		t.Fatalf("policy request lost exact MCP arguments: %+v", request)
	}
	if request.Requester != nil || len(request.FrozenConnections) != 0 {
		t.Fatal("remote executor acquired connector custody")
	}
	if request.CredentialSecret != "remote-auth" || !request.CredentialAuthorityEnforced ||
		len(request.ParentTransactionScopes) != 1 || request.ParentTransactionScopes[0] != outboundaccess.DefaultCredentialReadScope {
		t.Fatal("remote executor lost explicit Task authority")
	}
}
