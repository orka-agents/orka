package main

import (
	"context"
	"encoding/json"
	"encoding/pem"
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
	"github.com/orka-agents/orka/internal/tools"
	"github.com/orka-agents/orka/internal/transactiontoken"
	"github.com/orka-agents/orka/internal/worker"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Keep the real resolver and its side effects; inject changes only at the
// Kubernetes read boundary, after native binding but during request preparation.
type remotePreparationReader struct {
	client.Client
	beforePolicyRead func()
}

func (r *remotePreparationReader) Get(
	ctx context.Context, key client.ObjectKey, object client.Object, options ...client.GetOption,
) error {
	if _, policy := object.(*corev1alpha1.OutboundAccessPolicy); policy && r.beforePolicyRead != nil {
		mutate := r.beforePolicyRead
		r.beforePolicyRead = nil
		mutate()
	}
	return r.Client.Get(ctx, key, object, options...)
}

type nativeRouteFixture struct {
	*nativeRemoteFixture
	resolverReader *remotePreparationReader
	beforeAuthRead func()
	traffic        atomic.Int32
	credentials    atomic.Int32
}

func newNativeRouteFixture(t *testing.T) *nativeRouteFixture {
	t.Helper()
	f := &nativeRouteFixture{nativeRemoteFixture: newNativeRemoteFixture(t)}
	backend, _ := url.Parse(f.gatewayURL)
	proxy := httputil.NewSingleHostReverseProxy(backend)
	gateway := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.traffic.Add(1)
		if r.Header.Get("Authorization") != "" || r.Header.Get(transactiontoken.HeaderName) != "" {
			f.credentials.Add(1)
		}
		proxy.ServeHTTP(w, r)
	}))
	t.Cleanup(gateway.Close)
	gatewayURL, _ := url.Parse(gateway.URL)
	secret := &corev1.Secret{}
	readNativeRouteObject(t, f.client, "auth", secret)
	secret.Data["ca.crt"] = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: gateway.Certificate().Raw})
	updateNativeRouteObject(t, f.client, secret)
	policy := &corev1alpha1.OutboundAccessPolicy{}
	readNativeRouteObject(t, f.client, "egress", policy)
	policy.Spec.Gateway.Scheme = "https"
	policy.Spec.Gateway.TLS = &corev1alpha1.OutboundTLSConfig{
		ServerName:  "example.com",
		CASecretRef: &corev1alpha1.NamespacedSecretKeySelector{Name: "auth", Key: "ca.crt"},
	}
	policy.Status.ObservedGeneration = policy.Generation
	for _, kind := range []string{
		corev1alpha1.OutboundAccessPolicyConditionAccepted, corev1alpha1.OutboundAccessPolicyConditionResolvedRefs,
	} {
		policy.Status.Conditions = append(policy.Status.Conditions, metav1.Condition{
			Type: kind, Status: metav1.ConditionTrue, Reason: "Accepted", ObservedGeneration: policy.Generation,
		})
	}
	updateNativeRouteObject(t, f.client, policy)
	service := &corev1.Service{}
	readNativeRouteObject(t, f.client, "gateway", service)
	service.Spec.Ports = []corev1.ServicePort{{Port: 8080}}
	updateNativeRouteObject(t, f.client, service)
	other := service.DeepCopy()
	other.Name, other.UID, other.ResourceVersion = "other-gateway", "other-gateway-uid", ""
	if err := f.client.Create(t.Context(), other); err != nil {
		t.Fatal(err)
	}
	f.resolverReader = &remotePreparationReader{Client: f.client}
	resolver := &outboundaccess.KubernetesResolver{Reader: f.resolverReader}
	kube := kubefake.NewSimpleClientset()
	kube.PrependReactor("get", "secrets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		if f.beforeAuthRead != nil {
			mutate := f.beforeAuthRead
			f.beforeAuthRead = nil
			mutate()
		}
		value := &corev1.Secret{}
		key := client.ObjectKey{Namespace: action.GetNamespace(), Name: action.(k8stesting.GetAction).GetName()}
		err := f.client.Get(t.Context(), key, value)
		return true, value, err
	})
	transport := &http.Transport{DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		if address != "gateway.team.svc:8080" && address != "other-gateway.team.svc:8080" {
			return nil, fmt.Errorf("unexpected non-gateway dial")
		}
		return (&net.Dialer{}).DialContext(ctx, network, gatewayURL.Host)
	}}
	t.Cleanup(transport.CloseIdleConnections)
	f.executor = worker.NewToolExecutorForNamespace("team", kube, &http.Client{Transport: transport}, resolver)
	return f
}

func readNativeRouteObject(t *testing.T, reader client.Client, name string, object client.Object) {
	t.Helper()
	if err := reader.Get(t.Context(), client.ObjectKey{Namespace: "team", Name: name}, object); err != nil {
		t.Fatal(err)
	}
}

func updateNativeRouteObject(t *testing.T, reader client.Client, object client.Object) {
	t.Helper()
	if err := reader.Update(t.Context(), object); err != nil {
		t.Fatal(err)
	}
}

func mutateNativeRouteDependency(t *testing.T, f *nativeRouteFixture, dependency string) {
	t.Helper()
	switch dependency {
	case "policy":
		policy := &corev1alpha1.OutboundAccessPolicy{}
		readNativeRouteObject(t, f.client, "egress", policy)
		policy.Spec.Gateway.ServiceRef.Name = "other-gateway"
		policy.Generation++
		policy.Status.ObservedGeneration = policy.Generation
		for i := range policy.Status.Conditions {
			policy.Status.Conditions[i].ObservedGeneration = policy.Generation
		}
		updateNativeRouteObject(t, f.client, policy)
	case "Service version", "Service UID":
		service := &corev1.Service{}
		readNativeRouteObject(t, f.client, "gateway", service)
		service.Spec.Selector = map[string]string{"app": "changed-gateway"}
		if dependency == "Service UID" {
			service.UID = "replacement-gateway"
		}
		updateNativeRouteObject(t, f.client, service)
	case "TLS Secret", "credential", "credential version":
		secret := &corev1.Secret{}
		readNativeRouteObject(t, f.client, "auth", secret)
		switch dependency {
		case "TLS Secret":
			secret.Data["ca.crt"] = append(secret.Data["ca.crt"], '\n')
		case "credential":
			secret.Data["token"] = []byte("rotated-fixture-credential")
		case "credential version":
			secret.Labels = map[string]string{"rotation": "changed"}
		}
		updateNativeRouteObject(t, f.client, secret)
	}
}

func TestNativeRemoteMCPPreparationDriftSendsNoCredentials(t *testing.T) {
	for _, phase := range []string{"startup", "invocation"} {
		for _, dependency := range []string{
			"unchanged", "policy", "Service version", "Service UID", "TLS Secret", "credential", "credential version",
		} {
			t.Run(phase+"/"+dependency, func(t *testing.T) {
				f := newNativeRouteFixture(t)
				ctx := t.Context()
				var loaded map[string]*corev1alpha1.Tool
				var err error
				if phase == "invocation" {
					ctx, loaded, err = f.prepare(t)
					if err != nil {
						t.Fatal(err)
					}
				}
				beforeTraffic, beforeCredentials := f.traffic.Load(), f.credentials.Load()
				if dependency != "unchanged" {
					mutate := func() { mutateNativeRouteDependency(t, f, dependency) }
					if dependency == "credential" || dependency == "credential version" {
						f.beforeAuthRead = mutate
					} else {
						f.resolverReader.beforePolicyRead = mutate
					}
				}
				if phase == "startup" {
					_, _, err = f.prepare(t)
				} else {
					_, err = executeNativeRemoteTool(ctx, &tools.ToolContext{
						Client: f.client, Namespace: "team", TaskID: "task", TaskUID: "task-uid",
					}, loaded["health"], json.RawMessage(`{}`))
				}
				if dependency == "unchanged" {
					if err != nil || f.traffic.Load() == beforeTraffic || f.credentials.Load() == beforeCredentials {
						t.Fatalf("unchanged real gateway transport failed: %v", err)
					}
					return
				}
				if err == nil {
					t.Error("dependency changed during preparation was accepted")
				}
				if got := f.traffic.Load() - beforeTraffic; got != 0 {
					t.Errorf("changed binding received %d protocol requests", got)
				}
				if got := f.credentials.Load() - beforeCredentials; got != 0 {
					t.Errorf("changed binding received credentials on %d requests", got)
				}
			})
		}
	}
}
