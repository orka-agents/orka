package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	gatewayv1alpha1 "github.com/orka-agents/orka/api/gateway/v1alpha1"
)

func TestEndpointResolverRequiresTLSForServiceRef(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{"app": "adapter"},
			Ports:    []corev1.ServicePort{{Port: 8090}},
		},
	}
	resolver := EndpointResolver{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()}
	object := &gatewayv1alpha1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default"},
		Spec: gatewayv1alpha1.GatewaySpec{Adapter: gatewayv1alpha1.GatewayAdapterLocation{
			ServiceRef: &gatewayv1alpha1.GatewayServiceReference{Name: "adapter", Port: 8090},
		}},
	}
	endpoint, safe, err := resolver.Resolve(context.Background(), object)
	if err != nil {
		t.Fatalf("Resolve() error = %v", err)
	}
	if endpoint != "https://adapter.default.svc:8090" || safe != endpoint {
		t.Fatalf("resolved endpoint = %q, safe = %q", endpoint, safe)
	}

	object.Spec.Adapter = gatewayv1alpha1.GatewayAdapterLocation{Endpoint: "http://adapter.example.com"}
	if _, _, err := resolver.Resolve(context.Background(), object); err == nil || !strings.Contains(err.Error(), "https") {
		t.Fatalf("Resolve(insecure external) error = %v", err)
	}
	object.Spec.Adapter = gatewayv1alpha1.GatewayAdapterLocation{Endpoint: "https://user" + ":pass@adapter.example.com?value=x"}
	if _, _, err := resolver.Resolve(context.Background(), object); err == nil || !strings.Contains(err.Error(), "credentials") {
		t.Fatalf("Resolve(credentials) error = %v", err)
	}
}

func TestEndpointResolverRejectsExternalNameService(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	service := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "adapter", Namespace: "default"},
		Spec:       corev1.ServiceSpec{Type: corev1.ServiceTypeExternalName, ExternalName: "example.com"},
	}
	resolver := EndpointResolver{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(service).Build()}
	object := &gatewayv1alpha1.Gateway{
		ObjectMeta: metav1.ObjectMeta{Name: "chat", Namespace: "default"},
		Spec: gatewayv1alpha1.GatewaySpec{Adapter: gatewayv1alpha1.GatewayAdapterLocation{
			ServiceRef: &gatewayv1alpha1.GatewayServiceReference{Name: "adapter"},
		}},
	}
	if _, _, err := resolver.Resolve(context.Background(), object); err == nil || !strings.Contains(err.Error(), "selector-backed") {
		t.Fatalf("Resolve(ExternalName) error = %v", err)
	}
}

func TestGatewayHTTPClientDisablesProxyForServiceTraffic(t *testing.T) {
	client, err := gatewayHTTPClient(nil, time.Second, true, false)
	if err != nil {
		t.Fatal(err)
	}
	transport, ok := client.Transport.(*http.Transport)
	if !ok || transport.Proxy != nil {
		t.Fatalf("transport = %#v, want explicit no-proxy transport", client.Transport)
	}
}

func TestEndpointResolverRejectsUnsafeDirectTargets(t *testing.T) {
	resolver := EndpointResolver{}
	for _, endpoint := range []string{
		"https://127.0.0.1:8443",
		"https://10.0.0.5",
		"https://169.254.169.254",
		"https://adapter.other.svc:8443",
		"https://metadata.local",
	} {
		object := &gatewayv1alpha1.Gateway{Spec: gatewayv1alpha1.GatewaySpec{
			Adapter: gatewayv1alpha1.GatewayAdapterLocation{Endpoint: endpoint},
		}}
		if _, _, err := resolver.Resolve(context.Background(), object); err == nil {
			t.Fatalf("Resolve(%q) unexpectedly succeeded", endpoint)
		}
	}
}

func TestPublicGatewayDialerRejectsLoopbackBeforeRequest(t *testing.T) {
	requests := 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	client, err := NewAdapterHTTPClient(server.Client(), time.Second, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Get(server.URL); err == nil {
		t.Fatal("public-only adapter client unexpectedly reached loopback")
	}
	if requests != 0 {
		t.Fatalf("loopback server received %d requests", requests)
	}
}

func TestIsPublicGatewayAddress(t *testing.T) {
	for raw, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700:4700::1111": true,
		"127.0.0.1": false, "10.0.0.1": false, "100.64.0.1": false,
		"169.254.169.254": false, "::1": false, "fc00::1": false,
		"64:ff9b:1::1": false, "64:ff9b::a00:1": false, "fec0::1": false,
		"2002:a00:1::1": false,
	} {
		address := netip.MustParseAddr(raw)
		if got := isPublicGatewayAddress(address); got != want {
			t.Errorf("isPublicGatewayAddress(%s) = %v, want %v", raw, got, want)
		}
	}
}
