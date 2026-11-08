/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package controller

import (
	"context"
	"testing"

	"github.com/orka-agents/orka/internal/taskwebhook"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestIsAllowedWebhookURL_ValidURLs(t *testing.T) {
	validURLs := []string{
		"https://example.com/webhook",
		"http://public.example.org/notify",
		"https://api.example.com:8080/hook",
	}

	for _, url := range validURLs {
		t.Run(url, func(t *testing.T) {
			err := isAllowedWebhookURL(context.Background(), nil, url)
			if err != nil {
				t.Errorf("isAllowedWebhookURL(%q) returned error: %v", url, err)
			}
		})
	}
}

func TestIsAllowedWebhookURL_AllowsSameNamespaceClusterIPService(t *testing.T) {
	kubeClient := newWebhookValidationClient(t, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "receiver", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.96.0.10",
			Selector:  map[string]string{"app": "receiver"},
		},
	})

	for _, url := range []string{
		"http://receiver.default.svc.cluster.local/webhook",
		"http://receiver.default.svc/webhook",
	} {
		t.Run(url, func(t *testing.T) {
			err := isAllowedWebhookURL(context.Background(), kubeClient, url)
			if err != nil {
				t.Fatalf("isAllowedWebhookURL(%q) returned error: %v", url, err)
			}
		})
	}
}

func TestIsAllowedWebhookURL_BlockedURLs(t *testing.T) {
	tests := []struct {
		name string
		url  string
	}{
		{"metadata endpoint", "http://169.254.169.254/latest/meta-data"},
		{"google metadata", "http://metadata.google.internal/computeMetadata/v1/"},
		{"kubernetes default", "https://kubernetes.default/api"},
		{"kubernetes default svc", "https://kubernetes.default.svc/api"},
		{"kubernetes default svc fqdn", "https://kubernetes.default.svc.cluster.local/api"},
		{"localhost", "http://localhost:8080/webhook"},
		{"127.0.0.1", "http://127.0.0.1/webhook"},
		{"loopback IPv6", "http://[::1]/webhook"},
		{"private IP 10.x", "http://10.0.0.1/webhook"},
		{"private IP 172.16.x", "http://172.16.0.1/webhook"},
		{"private IP 192.168.x", "http://192.168.1.1/webhook"},
		{"localhost subdomain", "http://hook.localhost/webhook"},
		{"unspecified IPv4", "http://0.0.0.0:8080/webhook"},
		{"unspecified IPv6", "http://[::]/webhook"},
		{"CGNAT", "http://100.64.0.1/webhook"},
		{"broadcast", "http://255.255.255.255/webhook"},
		{"multicast", "http://224.0.0.1/webhook"},
		{"IPv6 ULA", "http://[fd00::1]/webhook"},
		{"IPv6 link-local", "http://[fe80::1]/webhook"},
		{"IPv6 link-local zone", "http://[fe80::1%25eth0]/webhook"},
		{"IPv4-mapped private", "http://[::ffff:10.0.0.1]/webhook"},
		{"IPv4-mapped loopback", "http://[::ffff:127.0.0.1]/webhook"},
		{"IPv4-mapped metadata", "http://[::ffff:169.254.169.254]/webhook"},
		{"NAT64 private", "http://[64:ff9b::a00:1]/webhook"},
		{"file scheme", "file:///etc/passwd"},
		{"ftp scheme", "ftp://internal.example.com/data"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := isAllowedWebhookURL(context.Background(), nil, tt.url)
			if err == nil {
				t.Errorf("isAllowedWebhookURL(%q) should have returned error but didn't", tt.url)
			}
		})
	}
}

func TestIsAllowedWebhookURL_InvalidURLs(t *testing.T) {
	invalidURLs := []string{
		"not-a-url",
		"://missing-scheme",
		"",
	}

	for _, url := range invalidURLs {
		t.Run(url, func(t *testing.T) {
			err := isAllowedWebhookURL(context.Background(), nil, url)
			if err == nil {
				t.Errorf("isAllowedWebhookURL(%q) should have returned error for invalid URL", url)
			}
		})
	}
}

func TestIsAllowedWebhookURL_BlocksOtherNamespaceServices(t *testing.T) {
	url := "http://receiver.other.svc.cluster.local/webhook"
	if err := isAllowedWebhookURL(context.Background(), nil, url); err == nil {
		t.Fatalf("isAllowedWebhookURL(%q) should reject services outside the task namespace", url)
	}
}

func TestIsAllowedWebhookURL_BlocksExternalNameServices(t *testing.T) {
	kubeClient := newWebhookValidationClient(t, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "receiver", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:         corev1.ServiceTypeExternalName,
			ExternalName: "169.254.169.254",
		},
	})

	err := isAllowedWebhookURL(context.Background(), kubeClient, "http://receiver.default.svc.cluster.local/webhook")
	if err == nil {
		t.Fatal("expected ExternalName service to be rejected")
	}
}

func TestIsAllowedWebhookURL_BlocksSelectorlessServices(t *testing.T) {
	kubeClient := newWebhookValidationClient(t, &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "receiver", Namespace: "default"},
		Spec: corev1.ServiceSpec{
			Type:      corev1.ServiceTypeClusterIP,
			ClusterIP: "10.96.0.10",
		},
	})

	err := isAllowedWebhookURL(context.Background(), kubeClient, "http://receiver.default.svc.cluster.local/webhook")
	if err == nil {
		t.Fatal("expected selector-less service to be rejected")
	}
}

// isAllowedWebhookURL applies the production destination policy that runs
// before any network I/O; public names are checked again at dial time.
// Every case runs as a Task in the default namespace.
func isAllowedWebhookURL(ctx context.Context, kubeClient ctrlclient.Reader, rawURL string) error {
	_, err := resolveWebhookTarget(ctx, kubeClient, rawURL, defaultNS, taskwebhook.PublicAddressPolicy)
	return err
}

func newWebhookValidationClient(t *testing.T, objs ...ctrlclient.Object) ctrlclient.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 to scheme: %v", err)
	}

	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}
