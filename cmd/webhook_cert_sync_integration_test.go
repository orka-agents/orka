/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
)

// Exercise startup, publication, and rotation without a projected Secret mount.
// CA rejection is covered by unit tests; the rotator promptly repairs CA drift.
func TestWebhookCertificateReadyWithoutSecretProjection(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run the envtest certificate integration")
	}
	environment := &envtest.Environment{}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	api, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "webhook-cert-test"}}
	if err := api.Create(ctx, ns); err != nil {
		t.Fatal(err)
	}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "tls", Namespace: ns.Name}}
	if err := api.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	fail := admissionv1.Fail
	sideEffects := admissionv1.SideEffectClassNone
	url := "https://webhook.test.svc/test"
	configuration := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "webhook-cert-test"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			Name:                    "test.webhook.test",
			ClientConfig:            admissionv1.WebhookClientConfig{URL: &url},
			FailurePolicy:           &fail,
			SideEffects:             &sideEffects,
			AdmissionReviewVersions: []string{"v1"},
		}},
	}
	if err := api.Create(ctx, configuration); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	options := webhookCertRotationOptions{
		SecretName: secret.Name, WebhookName: configuration.Name, DNSName: "webhook.test.svc",
		CertDir: dir, CertName: "tls.crt", KeyName: "tls.key",
	}
	syncer := &webhookCertSyncer{options: options}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	server := webhook.NewServer(webhook.Options{
		Host: "127.0.0.1", Port: port, CertDir: dir,
		TLSOpts: []func(*tls.Config){func(c *tls.Config) {
			c.GetCertificate = syncer.GetCertificate
			c.SessionTicketsDisabled = true
		}},
	})
	mgr, err := ctrl.NewManager(config, ctrl.Options{
		Scheme: scheme, Metrics: metricsserver.Options{BindAddress: "0"},
		HealthProbeBindAddress: "0", WebhookServer: server,
	})
	if err != nil {
		t.Fatal(err)
	}
	ready, err := setupWebhookCertRotation(mgr, ns.Name, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := setupWebhookCertSyncer(mgr, ns.Name, syncer); err != nil {
		t.Fatal(err)
	}
	check := webhookReadyChecker(ready, syncer.ReadyChecker(server.StartedChecker()))
	if err := check(nil); err == nil {
		t.Fatal("empty Secret was ready")
	}
	if _, err := os.Stat(filepath.Join(dir, "tls.crt")); !os.IsNotExist(err) {
		t.Fatalf("CertDir was not empty: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- mgr.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Error(err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop")
		}
	})
	select {
	case <-ready:
	case <-time.After(15 * time.Second):
		t.Fatal("rotator did not become ready without projection")
	}
	server.Register("/test", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	mgr.GetWebhookServer()
	waitForWebhookTest(t, func() bool { return check(nil) == nil })
	original, err := syncer.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	// Exercise recovery from CA drift using real informers and the rotator.
	if err := api.Get(ctx, client.ObjectKeyFromObject(configuration), configuration); err != nil {
		t.Fatal(err)
	}
	configuration.Webhooks[0].ClientConfig.CABundle = []byte("wrong CA")
	if err := api.Update(ctx, configuration); err != nil {
		t.Fatal(err)
	}
	// Upstream immediately repairs drift, so assert steady-state recovery rather
	// than depending on the scheduling of its repair versus the probe.
	waitForWebhookTest(t, func() bool {
		if err := api.Get(ctx, client.ObjectKeyFromObject(configuration), configuration); err != nil {
			return false
		}
		return !bytes.Equal(configuration.Webhooks[0].ClientConfig.CABundle, []byte("wrong CA")) && check(nil) == nil
	})
	// Invalidating the key asks the unchanged rotator to generate a new pair.
	if err := api.Get(ctx, client.ObjectKeyFromObject(secret), secret); err != nil {
		t.Fatal(err)
	}
	secret.Data["tls.key"] = nil
	if err := api.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	waitForWebhookTest(t, func() bool {
		current, err := syncer.GetCertificate(nil)
		return err == nil && !bytes.Equal(current.Certificate[0], original.Certificate[0]) && check(nil) == nil
	})
	// A trusted TLS client sees the rotated leaf, not merely readiness state.
	assertWebhookTestServingCertificate(t, syncer, port)
}

func assertWebhookTestServingCertificate(t *testing.T, syncer *webhookCertSyncer, port int) {
	t.Helper()
	syncer.mu.RLock()
	ca := bytes.Clone(syncer.ca)
	syncer.mu.RUnlock()
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca)
	conn, err := tls.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &tls.Config{
		RootCAs: roots, ServerName: syncer.options.DNSName, MinVersion: tls.VersionTLS12,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := conn.Close(); err != nil {
			t.Error(err)
		}
	}()
	current, err := syncer.GetCertificate(nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(conn.ConnectionState().PeerCertificates[0].Raw, current.Certificate[0]) {
		t.Fatal("server did not pick up the rotated certificate")
	}
}

func waitForWebhookTest(t *testing.T, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("webhook certificate condition did not converge")
}
