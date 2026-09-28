package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"sync/atomic"
	"testing"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/outboundaccess"
	"github.com/orka-agents/orka/internal/transactiontoken"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestRemoteMCPRealResolverAndFixedRouteGateway(t *testing.T) {
	for _, deny := range []bool{false, true} {
		t.Run(fmt.Sprint(deny), func(t *testing.T) {
			backendFixture := &remoteProtocolFixture{intercept: func(w http.ResponseWriter, r *http.Request, method string) bool {
				if r.Header.Get(transactiontoken.HeaderName) != "" {
					t.Error("gateway forwarded Task authority to upstream")
				}
				return false
			}}
			backend := backendFixture.serve(t)
			target, _ := url.Parse(backend.URL)
			proxy := httputil.NewSingleHostReverseProxy(target)
			var forwarded, direct atomic.Int32
			gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Host != "8.8.8.8" || r.URL.Path != "/mcp" || r.Header.Get(transactiontoken.HeaderName) != "task-authority" || r.Header.Get("Authorization") != "Bearer exact-resource" {
					t.Error("unbound gateway request")
					w.WriteHeader(http.StatusForbidden)
					return
				}
				forwarded.Add(1)
				r.Header.Del(transactiontoken.HeaderName)
				proxy.ServeHTTP(w, r)
			}))
			defer gateway.Close()
			gatewayURL, _ := url.Parse(gateway.URL)
			policy := &corev1alpha1.OutboundAccessPolicy{ObjectMeta: metav1.ObjectMeta{Name: "remote-egress", Namespace: "team", UID: "policy", Generation: 1}, Spec: corev1alpha1.OutboundAccessPolicySpec{Gateway: &corev1alpha1.GatewayOutboundAccess{ServiceRef: corev1alpha1.OutboundServiceReference{Name: "gateway", Port: 8080}}}}
			policy.Status.ObservedGeneration = 1
			if !deny {
				for _, typ := range []string{corev1alpha1.OutboundAccessPolicyConditionAccepted, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs} {
					policy.Status.Conditions = append(policy.Status.Conditions, metav1.Condition{Type: typ, Status: metav1.ConditionTrue, Reason: "Accepted", ObservedGeneration: 1})
				}
			}
			service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: "gateway", Namespace: "team"}, Spec: corev1.ServiceSpec{Ports: []corev1.ServicePort{{Port: 8080}}}}
			secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "remote-auth", Namespace: "team"}, Data: map[string][]byte{"token": []byte("exact-resource")}}
			scheme := runtime.NewScheme()
			_ = corev1.AddToScheme(scheme)
			_ = corev1alpha1.AddToScheme(scheme)
			reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(policy, service, secret).Build()
			resolver := &outboundaccess.KubernetesResolver{Reader: reader}
			transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
				if address != "gateway.team.svc:8080" {
					direct.Add(1)
					return nil, fmt.Errorf("non-gateway dial rejected")
				}
				return (&net.Dialer{}).DialContext(ctx, network, gatewayURL.Host)
			}}
			e := NewToolExecutorForNamespace("team", kubefake.NewSimpleClientset(secret), &http.Client{Transport: transport}, resolver)
			e.SetTransactionAuthority("task-authority", []string{outboundaccess.DefaultCredentialReadScope})
			e.SetTransactionCredentialAuthority(true, true, "remote-auth")
			tool := remoteTestTool(t)
			err := e.VerifyRemoteMCPTool(t.Context(), tool)
			if deny {
				if err == nil || forwarded.Load() != 0 || backendFixture.requests.Load() != 0 {
					t.Fatal("unadmitted policy reached MCP")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := e.Execute(t.Context(), tool, json.RawMessage(`{}`)); err != nil {
				t.Fatal(err)
			}
			if direct.Load() != 0 || forwarded.Load() != 9 || backendFixture.requests.Load() != 9 || backendFixture.calls.Load() != 1 || backendFixture.lists.Load() != 2 {
				t.Fatalf("gateway=%d backend=%d calls=%d lists=%d direct=%d", forwarded.Load(), backendFixture.requests.Load(), backendFixture.calls.Load(), backendFixture.lists.Load(), direct.Load())
			}
		})
	}
}
