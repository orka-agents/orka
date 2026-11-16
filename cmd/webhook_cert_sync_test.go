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
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/open-policy-agent/cert-controller/pkg/rotator"
	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func webhookTestSecret(t *testing.T, begin, end time.Time) *corev1.Secret {
	t.Helper()
	return webhookTestSecretWithCAValidity(t, begin, end, begin, time.Now().Add(24*time.Hour))
}

func webhookTestSecretWithCAValidity(t *testing.T, begin, end, caBegin, caEnd time.Time) *corev1.Secret {
	t.Helper()
	usages := []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	r := &rotator.CertRotator{
		CAName: "test-ca", CAOrganization: "test", DNSName: "webhook.test.svc", ExtKeyUsages: &usages,
	}
	ca, err := r.CreateCACert(caBegin, caEnd)
	if err != nil {
		t.Fatal(err)
	}
	cert, key, err := r.CreateCertPEM(ca, begin, end)
	if err != nil {
		t.Fatal(err)
	}
	return &corev1.Secret{Data: map[string][]byte{"tls.crt": cert, "tls.key": key, "ca.crt": ca.CertPEM}}
}

func TestWebhookCertificateSourceValidation(t *testing.T) {
	now := time.Now()
	valid := webhookTestSecret(t, now.Add(-time.Hour), now.Add(time.Hour))
	rotated := webhookTestSecret(t, now.Add(-time.Hour), now.Add(time.Hour))
	expired := webhookTestSecret(t, now.Add(-2*time.Hour), now.Add(-time.Hour))
	future := webhookTestSecret(t, now.Add(time.Hour), now.Add(2*time.Hour))
	options := webhookCertRotationOptions{DNSName: "webhook.test.svc", CertName: "tls.crt", KeyName: "tls.key"}
	tests := []struct {
		name    string
		secret  *corev1.Secret
		mutate  func(*corev1.Secret)
		wantErr bool
	}{
		{name: "missing", secret: &corev1.Secret{}, wantErr: true},
		{name: "valid", secret: valid},
		{
			name: "malformed", secret: valid,
			mutate: func(s *corev1.Secret) { s.Data["tls.crt"] = []byte("broken") }, wantErr: true,
		},
		{
			name: "mismatched pair", secret: valid,
			mutate: func(s *corev1.Secret) { s.Data["tls.key"] = rotated.Data["tls.key"] }, wantErr: true,
		},
		{name: "rotation swap", secret: rotated},
		{name: "expired", secret: expired, wantErr: true},
		{name: "not yet valid", secret: future, wantErr: true},
		{
			name: "wrong CA", secret: valid,
			mutate: func(s *corev1.Secret) { s.Data["ca.crt"] = rotated.Data["ca.crt"] }, wantErr: true,
		},
		{name: "missing CA", secret: valid, mutate: func(s *corev1.Secret) { delete(s.Data, "ca.crt") }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := tt.secret.DeepCopy()
			if tt.mutate != nil {
				tt.mutate(secret)
			}
			cert, err := parseWebhookCertificate(secret, options, now)
			if (err != nil) != tt.wantErr {
				t.Fatalf("validation error = %v, want error %t", err, tt.wantErr)
			}
			if err == nil && len(cert.Certificate) == 0 {
				t.Fatal("valid pair was not loaded")
			}
		})
	}
}

// The fake replaces only API/cache transport; publication and certwatcher's
// last-good cache are real. Invalid observed state must never serve that cache.
func TestWebhookCertSyncerRejectsInvalidStateAndLoadsRotation(t *testing.T) {
	now := time.Now()
	valid := webhookTestSecret(t, now.Add(-time.Hour), now.Add(time.Hour))
	rotated := webhookTestSecret(t, now.Add(-time.Hour), now.Add(time.Hour))
	expired := webhookTestSecret(t, now.Add(-2*time.Hour), now.Add(-time.Hour))
	tests := []struct {
		name    string
		secret  *corev1.Secret
		bundles [][]byte
		wantErr bool
	}{
		{name: "valid", secret: valid, bundles: [][]byte{valid.Data["ca.crt"]}},
		{name: "CA not injected", secret: valid, bundles: [][]byte{nil}, wantErr: true},
		{
			name: "one webhook has wrong CA", secret: valid,
			bundles: [][]byte{valid.Data["ca.crt"], rotated.Data["ca.crt"]}, wantErr: true,
		},
		{name: "empty configuration", secret: valid, wantErr: true},
		{name: "missing pair", secret: &corev1.Secret{}, wantErr: true},
		{name: "expired cannot serve last good", secret: expired, bundles: [][]byte{expired.Data["ca.crt"]}, wantErr: true},
		{name: "rotation reloads watcher", secret: rotated, bundles: [][]byte{rotated.Data["ca.crt"]}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secret := valid.DeepCopy()
			secret.ObjectMeta = metav1.ObjectMeta{Namespace: "test", Name: "tls"}
			configuration := &admissionv1.ValidatingWebhookConfiguration{
				ObjectMeta: metav1.ObjectMeta{Name: "webhook"},
				Webhooks: []admissionv1.ValidatingWebhook{{
					ClientConfig: admissionv1.WebhookClientConfig{CABundle: valid.Data["ca.crt"]},
				}},
			}
			api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret, configuration).Build()
			s := &webhookCertSyncer{
				options: webhookCertRotationOptions{
					SecretName: "tls", WebhookName: "webhook", DNSName: "webhook.test.svc",
					CertDir: t.TempDir(), CertName: "tls.crt", KeyName: "tls.key",
				},
				secretKey: types.NamespacedName{Namespace: "test", Name: "tls"},
				cache:     webhookCertificateTestCache{reader: api},
			}
			if err := s.sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.watcher.Start(ctx) }()
			t.Cleanup(func() { cancel(); <-done })
			if _, err := s.GetCertificate(nil); err != nil {
				t.Fatalf("initial valid certificate: %v", err)
			}
			secret.Data = tt.secret.DeepCopy().Data
			if err := api.Update(context.Background(), secret); err != nil {
				t.Fatal(err)
			}
			configuration.Webhooks = nil
			for _, bundle := range tt.bundles {
				configuration.Webhooks = append(configuration.Webhooks, admissionv1.ValidatingWebhook{
					ClientConfig: admissionv1.WebhookClientConfig{CABundle: bundle},
				})
			}
			if err := api.Update(context.Background(), configuration); err != nil {
				t.Fatal(err)
			}
			if err := s.sync(context.Background()); err != nil {
				t.Fatal(err)
			}
			cert, err := s.GetCertificate(nil)
			if (err != nil) != tt.wantErr {
				t.Fatalf("serving error = %v, want error %t", err, tt.wantErr)
			}
			ready := s.ReadyChecker(func(*http.Request) error { return nil })(nil)
			if (ready != nil) != tt.wantErr {
				t.Fatalf("readiness = %v, want error %t", ready, tt.wantErr)
			}
			if err == nil {
				expected, err := tls.X509KeyPair(tt.secret.Data["tls.crt"], tt.secret.Data["tls.key"])
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(cert.Certificate[0], expected.Certificate[0]) {
					t.Fatal("watcher returned old certificate")
				}
			}
		})
	}
}

// Model previously valid pairs that expired since the last sync. The TLS gate
// and readiness must check wall-clock validity even with matching CA state.
func TestWebhookCertSyncerRejectsExpiredLoadedCertificate(t *testing.T) {
	now := time.Now()
	for _, tt := range []struct {
		name   string
		secret *corev1.Secret
	}{
		{
			name:   "expired leaf",
			secret: webhookTestSecret(t, now.Add(-2*time.Hour), now.Add(-time.Hour)),
		},
		{
			name: "expired CA",
			secret: webhookTestSecretWithCAValidity(t, now.Add(-2*time.Hour), now.Add(time.Hour),
				now.Add(-3*time.Hour), now.Add(-time.Hour)),
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			options := webhookCertRotationOptions{
				SecretName: "tls", DNSName: "webhook.test.svc",
				CertDir: t.TempDir(), CertName: "tls.crt", KeyName: "tls.key",
			}
			if err := publishWebhookCertificate(tt.secret, options); err != nil {
				t.Fatal(err)
			}
			watcher, err := certwatcher.New(filepath.Join(options.CertDir, options.CertName),
				filepath.Join(options.CertDir, options.KeyName))
			if err != nil {
				t.Fatal(err)
			}
			stopWebhookTestWatcher(t, watcher)
			current, err := parseWebhookCertificate(tt.secret, options, now.Add(-90*time.Minute))
			if err != nil {
				t.Fatalf("fixture was not valid before expiry: %v", err)
			}
			s := &webhookCertSyncer{options: options, watcher: watcher, current: current, ca: tt.secret.Data["ca.crt"]}
			_, servingErr := s.GetCertificate(nil)
			readyErr := s.ReadyChecker(func(*http.Request) error { return nil })(nil)
			for _, err := range []error{servingErr, readyErr} {
				var invalid x509.CertificateInvalidError
				if !errors.As(err, &invalid) || invalid.Reason != x509.Expired {
					t.Fatalf("expired loaded pair rejection = %v, want certificate expiry", err)
				}
			}
		})
	}
}

func TestWebhookCertSyncerPreservesNotYetValidReason(t *testing.T) {
	now := time.Now()
	secret := webhookTestSecret(t, now.Add(time.Hour), now.Add(2*time.Hour))
	secret.ObjectMeta = metav1.ObjectMeta{Namespace: "test", Name: "tls"}
	api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret).Build()
	s := &webhookCertSyncer{
		options: webhookCertRotationOptions{
			SecretName: "tls", DNSName: "webhook.test.svc",
			CertDir: t.TempDir(), CertName: "tls.crt", KeyName: "tls.key",
		},
		secretKey: types.NamespacedName{Namespace: "test", Name: "tls"},
		cache:     webhookCertificateTestCache{reader: api},
	}
	if err := s.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	_, servingErr := s.GetCertificate(nil)
	readyErr := s.ReadyChecker(func(*http.Request) error { return nil })(nil)
	for _, err := range []error{s.trustErr, servingErr, readyErr} {
		var invalid x509.CertificateInvalidError
		if !errors.As(err, &invalid) || invalid.Reason != x509.Expired || !strings.Contains(err.Error(), "is before") {
			t.Fatalf("future pair rejection = %v, want not-yet-valid cause", err)
		}
		if strings.Contains(err.Error(), "-----BEGIN") {
			t.Fatal("validation error contains PEM data")
		}
	}
}

func TestWebhookCertSyncerReloadsStaleWatcherOnUnchangedSecret(t *testing.T) {
	now := time.Now()
	old := webhookTestSecret(t, now.Add(-time.Hour), now.Add(time.Hour))
	secret := webhookTestSecret(t, now.Add(-time.Hour), now.Add(time.Hour))
	secret.ObjectMeta = metav1.ObjectMeta{Namespace: "test", Name: "tls"}
	configuration := &admissionv1.ValidatingWebhookConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: "webhook"},
		Webhooks: []admissionv1.ValidatingWebhook{{
			ClientConfig: admissionv1.WebhookClientConfig{CABundle: secret.Data["ca.crt"]},
		}},
	}
	api := fake.NewClientBuilder().WithScheme(scheme).WithObjects(secret, configuration).Build()
	s := &webhookCertSyncer{
		options: webhookCertRotationOptions{
			SecretName: "tls", WebhookName: "webhook", DNSName: "webhook.test.svc",
			CertDir: t.TempDir(), CertName: "tls.crt", KeyName: "tls.key",
		},
		secretKey: types.NamespacedName{Namespace: "test", Name: "tls"},
		cache:     webhookCertificateTestCache{reader: api},
	}
	if err := s.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	stopWebhookTestWatcher(t, s.watcher)
	// Model a late watcher read that replaces its cache with an old generation.
	// Keep its background loop stopped so recovery can only come from sync.
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.WriteFile(filepath.Join(s.options.CertDir, name), old.Data[name], 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.watcher.ReadCertificate(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"tls.crt", "tls.key"} {
		if err := os.WriteFile(filepath.Join(s.options.CertDir, name), secret.Data[name], 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.GetCertificate(nil); err == nil || !strings.Contains(err.Error(), "current pair") {
		t.Fatalf("stale watcher was not rejected: %v", err)
	}
	if err := s.sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	cert, err := s.GetCertificate(nil)
	if err != nil {
		t.Fatalf("unchanged Secret did not repair stale watcher: %v", err)
	}
	if !bytes.Equal(cert.Certificate[0], s.current.Certificate[0]) {
		t.Fatal("reloaded watcher returned old certificate")
	}
	if err := s.ReadyChecker(func(*http.Request) error { return nil })(nil); err != nil {
		t.Fatalf("recovered watcher not ready: %v", err)
	}
}

// New allocates an fsnotify watcher. Start only during cleanup to close it,
// leaving explicit reads deterministic in tests that simulate stale state.
func stopWebhookTestWatcher(t *testing.T, watcher *certwatcher.CertWatcher) {
	t.Helper()
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := watcher.Start(ctx); err != nil {
			t.Error(err)
		}
	})
}

type webhookCertificateTestCache struct {
	cache.Cache
	reader client.Reader
}

func (c webhookCertificateTestCache) Get(
	ctx context.Context,
	key client.ObjectKey,
	obj client.Object,
	opts ...client.GetOption,
) error {
	return c.reader.Get(ctx, key, obj, opts...)
}

func TestPublishWebhookCertificateReclaimsOrphans(t *testing.T) {
	dir := t.TempDir()
	options := webhookCertRotationOptions{CertDir: dir, CertName: "tls.crt", KeyName: "tls.key"}
	now := time.Now()
	secret := webhookTestSecret(t, now.Add(-time.Hour), now.Add(time.Hour))
	if err := publishWebhookCertificate(secret, options); err != nil {
		t.Fatal(err)
	}
	active, err := os.Readlink(filepath.Join(dir, "..data"))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"..cert-interrupted-1", "..cert-interrupted-2", "operator-owned"} {
		path := filepath.Join(dir, name)
		if err := os.Mkdir(path, 0700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(path, "keep"), []byte("fixture"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("operator-owned", filepath.Join(dir, "..cert-not-a-directory")); err != nil {
		t.Fatal(err)
	}
	// Fail after pre-publication cleanup: the active pair must remain available.
	if err := os.Mkdir(filepath.Join(dir, "..data-next"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "..data-next", "keep"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := publishWebhookCertificate(secret, options); err == nil {
		t.Fatal("publication unexpectedly replaced a nonempty ..data-next directory")
	}
	for _, name := range []string{"..cert-interrupted-1", "..cert-interrupted-2"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("orphan %s was not reclaimed: %v", name, err)
		}
	}
	for _, name := range []string{active, "operator-owned", "..cert-not-a-directory"} {
		if _, err := os.Lstat(filepath.Join(dir, name)); err != nil {
			t.Fatalf("cleanup removed retained entry %s: %v", name, err)
		}
	}
	if _, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")); err != nil {
		t.Fatalf("active pair unavailable after cleanup: %v", err)
	}
	if err := os.RemoveAll(filepath.Join(dir, "..data-next")); err != nil {
		t.Fatal(err)
	}
	if err := publishWebhookCertificate(secret, options); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, active)); !os.IsNotExist(err) {
		t.Fatalf("previous generation was not removed after success: %v", err)
	}
}

func TestPublishWebhookCertificateRejectsUnreadableDataLink(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "..data"), []byte("not a symlink"), 0600); err != nil {
		t.Fatal(err)
	}
	options := webhookCertRotationOptions{CertDir: dir, CertName: "tls.crt", KeyName: "tls.key"}
	now := time.Now()
	secret := webhookTestSecret(t, now.Add(-time.Hour), now.Add(time.Hour))
	err := publishWebhookCertificate(secret, options)
	var pathErr *os.PathError
	if !errors.As(err, &pathErr) || pathErr.Op != "readlink" {
		t.Fatalf("invalid ..data rejection = %v, want readlink error", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatal("generation allocated despite unreadable ..data link")
	}
}

func TestPublishWebhookCertificatePropagatesOrphanCleanupError(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can remove unreadable directories")
	}
	dir := t.TempDir()
	orphan := filepath.Join(dir, "..cert-interrupted")
	if err := os.Mkdir(orphan, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(orphan, "keep"), []byte("fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(orphan, 0000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(orphan, 0700); err != nil && !os.IsNotExist(err) {
			t.Error(err)
		}
	})
	options := webhookCertRotationOptions{CertDir: dir, CertName: "tls.crt", KeyName: "tls.key"}
	now := time.Now()
	secret := webhookTestSecret(t, now.Add(-time.Hour), now.Add(time.Hour))
	if err := publishWebhookCertificate(secret, options); !errors.Is(err, os.ErrPermission) {
		t.Fatalf("orphan cleanup error = %v, want permission denied", err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "..data")); !os.IsNotExist(err) {
		t.Fatalf("publication continued after cleanup failure: %v", err)
	}
}

func TestPublishWebhookCertificateGeneration(t *testing.T) {
	dir := t.TempDir()
	options := webhookCertRotationOptions{CertDir: dir, CertName: "tls.crt", KeyName: "tls.key"}
	now := time.Now()
	for range 2 {
		secret := webhookTestSecret(t, now.Add(-time.Hour), now.Add(time.Hour))
		if err := publishWebhookCertificate(secret, options); err != nil {
			t.Fatal(err)
		}
		cert, err := tls.LoadX509KeyPair(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := tls.X509KeyPair(secret.Data["tls.crt"], secret.Data["tls.key"])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(cert.Certificate[0], parsed.Certificate[0]) {
			t.Fatal("published generation differs from Secret")
		}
		info, err := os.Stat(filepath.Join(dir, "tls.key"))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatalf("key permissions = %v", info.Mode())
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 4 {
		t.Fatalf("stale generations remain: %d entries", len(entries))
	}
}
