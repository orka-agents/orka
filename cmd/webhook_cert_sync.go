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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	admissionv1 "k8s.io/api/admissionregistration/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	toolscache "k8s.io/client-go/tools/cache"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/certwatcher"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

// webhookCertSyncer bridges the API to a writable, memory-backed CertDir.
// Rotation and CA injection remain owned by cert-controller. Only generated
// mode uses this bridge; operator-mounted certificates keep their file watcher.
type webhookCertSyncer struct {
	options   webhookCertRotationOptions
	cache     cache.Cache
	secretKey types.NamespacedName
	changed   chan struct{}
	mu        sync.RWMutex
	watcher   *certwatcher.CertWatcher
	current   *tls.Certificate
	ca        []byte
	trustErr  error
}

func setupWebhookCertSyncer(mgr manager.Manager, namespace string, s *webhookCertSyncer) error {
	o := s.options
	s.secretKey = types.NamespacedName{Namespace: strings.TrimSpace(namespace), Name: strings.TrimSpace(o.SecretName)}
	c, err := cache.New(mgr.GetConfig(), cache.Options{
		Scheme: mgr.GetScheme(), Mapper: mgr.GetRESTMapper(), ReaderFailOnMissingInformer: true,
		ByObject: map[client.Object]cache.ByObject{
			&corev1.Secret{}: {
				Namespaces: map[string]cache.Config{s.secretKey.Namespace: {}},
				Field:      fields.OneTermEqualSelector("metadata.name", s.secretKey.Name),
			},
			&admissionv1.ValidatingWebhookConfiguration{}: {
				Field: fields.OneTermEqualSelector("metadata.name", strings.TrimSpace(o.WebhookName)),
			},
		},
	})
	if err != nil {
		return fmt.Errorf("create webhook certificate cache: %w", err)
	}
	s.cache = c
	s.changed = make(chan struct{}, 1)
	notify := func(any) {
		select {
		case s.changed <- struct{}{}:
		default:
		}
	}
	for _, object := range []client.Object{&corev1.Secret{}, &admissionv1.ValidatingWebhookConfiguration{}} {
		informer, err := c.GetInformer(context.Background(), object, cache.BlockUntilSynced(false))
		if err != nil {
			return fmt.Errorf("create webhook certificate informer: %w", err)
		}
		if _, err := informer.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
			AddFunc:    notify,
			UpdateFunc: func(_, newObject any) { notify(newObject) },
			DeleteFunc: notify,
		}); err != nil {
			return err
		}
	}
	if err := mgr.Add(c); err != nil {
		return err
	}
	return mgr.Add(s)
}

func (*webhookCertSyncer) NeedLeaderElection() bool { return false }

func (s *webhookCertSyncer) Start(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if !s.cache.WaitForCacheSync(ctx) {
		return errors.New("webhook certificate cache did not synchronize")
	}
	watcherErrors := make(chan error, 1)
	watcherStarted := false
	defer func() {
		cancel()
		if watcherStarted {
			<-watcherErrors
		}
	}()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		// Re-read from the dedicated cache, not the API. The ticker also rechecks
		// certificate validity without a new watch event.
		if err := s.sync(ctx); err != nil {
			return err
		}
		s.mu.RLock()
		watcher := s.watcher
		s.mu.RUnlock()
		if watcher != nil && !watcherStarted {
			watcherStarted = true
			go func() { watcherErrors <- watcher.Start(ctx) }()
		}
		select {
		case <-ctx.Done():
			return nil
		case err := <-watcherErrors:
			watcherStarted = false
			return err
		case <-s.changed:
		case <-ticker.C:
		}
	}
}

func (s *webhookCertSyncer) sync(ctx context.Context) error {
	secret := &corev1.Secret{}
	err := s.cache.Get(ctx, s.secretKey, secret)
	var cert *tls.Certificate
	if err != nil {
		err = errors.New("webhook certificate Secret is missing")
	} else {
		cert, err = parseWebhookCertificate(secret, s.options, time.Now())
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err != nil {
		s.current = nil
		s.trustErr = err
		return nil // cert-controller repairs invalid data; never publish it.
	}
	publish := s.current == nil || !bytes.Equal(cert.Certificate[0], s.current.Certificate[0]) ||
		!bytes.Equal(secret.Data["ca.crt"], s.ca)
	if publish {
		if err := publishWebhookCertificate(secret, s.options); err != nil {
			return fmt.Errorf("publish webhook serving certificate: %w", err)
		}
	}
	if s.watcher == nil {
		s.watcher, err = certwatcher.New(
			filepath.Join(s.options.CertDir, s.options.CertName),
			filepath.Join(s.options.CertDir, s.options.KeyName),
		)
	} else {
		// A background read of the old generation can finish after our reload.
		// Recheck even unchanged Secrets so recovery need not wait for its poll.
		var loaded *tls.Certificate
		loaded, err = s.watcher.GetCertificate(nil)
		if err == nil && (publish || loaded == nil || !bytes.Equal(loaded.Certificate[0], cert.Certificate[0])) {
			err = s.watcher.ReadCertificate()
		}
	}
	if err != nil {
		return fmt.Errorf("load published webhook serving certificate: %w", err)
	}
	s.current = cert
	s.ca = bytes.Clone(secret.Data["ca.crt"])
	configuration := &admissionv1.ValidatingWebhookConfiguration{}
	s.trustErr = s.cache.Get(ctx, types.NamespacedName{Name: strings.TrimSpace(s.options.WebhookName)}, configuration)
	if s.trustErr != nil {
		s.trustErr = errors.New("webhook CA configuration is missing")
		return nil
	}
	if !configuration.DeletionTimestamp.IsZero() || len(configuration.Webhooks) == 0 {
		s.trustErr = errors.New("webhook CA configuration is empty or deleting")
		return nil
	}
	for _, wh := range configuration.Webhooks {
		if !bytes.Equal(wh.ClientConfig.CABundle, s.ca) {
			s.trustErr = errors.New("webhook serving CA has not been injected")
			return nil
		}
	}
	return nil
}

// GetCertificate also gates new TLS connections: readiness alone cannot stop
// certwatcher's last-good cache from serving an expired or untrusted pair.
func (s *webhookCertSyncer) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.trustErr != nil {
		return nil, s.trustErr
	}
	if s.current == nil || s.watcher == nil {
		return nil, errors.New("webhook serving certificate is not ready")
	}
	cert, err := s.watcher.GetCertificate(hello)
	if err != nil {
		return nil, err
	}
	if cert == nil || !bytes.Equal(cert.Certificate[0], s.current.Certificate[0]) {
		return nil, errors.New("webhook certificate watcher has not loaded the current pair")
	}
	if err := verifyWebhookCertificate(cert, s.ca, s.options.DNSName, time.Now()); err != nil {
		return nil, err
	}
	return cert, nil
}

func (s *webhookCertSyncer) ReadyChecker(started healthz.Checker) healthz.Checker {
	if !s.options.enabled() {
		return started
	}
	return func(req *http.Request) error {
		if _, err := s.GetCertificate(nil); err != nil {
			return err
		}
		return started(req)
	}
}

func parseWebhookCertificate(
	secret *corev1.Secret,
	o webhookCertRotationOptions,
	now time.Time,
) (*tls.Certificate, error) {
	if !secret.DeletionTimestamp.IsZero() {
		return nil, errors.New("webhook certificate Secret is deleting")
	}
	cert, err := tls.X509KeyPair(secret.Data[o.CertName], secret.Data[o.KeyName])
	if err != nil {
		return nil, errors.New("webhook certificate Secret has an invalid key pair")
	}
	if err := verifyWebhookCertificate(&cert, secret.Data["ca.crt"], o.DNSName, now); err != nil {
		return nil, err
	}
	return &cert, nil
}

func verifyWebhookCertificate(cert *tls.Certificate, ca []byte, dnsName string, now time.Time) error {
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca) {
		return errors.New("webhook certificate CA is invalid")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return errors.New("webhook serving certificate is malformed")
	}
	_, err = leaf.Verify(x509.VerifyOptions{
		Roots:       roots,
		DNSName:     strings.TrimSpace(dnsName),
		CurrentTime: now,
		KeyUsages:   []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	})
	if err != nil {
		return fmt.Errorf("validate webhook serving certificate: %w", err)
	}
	return nil
}

// Publish a complete generation before swapping ..data, just like a projected
// Secret volume. Independent renames of cert/key could expose a mixed pair.
func publishWebhookCertificate(secret *corev1.Secret, o webhookCertRotationOptions) error {
	if err := os.MkdirAll(o.CertDir, 0700); err != nil {
		return err
	}
	current := filepath.Join(o.CertDir, "..data")
	previous, err := os.Readlink(current)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	entries, err := os.ReadDir(o.CertDir)
	if err != nil {
		return err
	}
	// Interrupted publications can survive container restarts in emptyDir.
	// Reclaim only our inactive directories before allocating another pair.
	for _, entry := range entries {
		if entry.IsDir() && strings.HasPrefix(entry.Name(), "..cert-") && entry.Name() != previous {
			if err := os.RemoveAll(filepath.Join(o.CertDir, entry.Name())); err != nil {
				return err
			}
		}
	}
	generation, err := os.MkdirTemp(o.CertDir, "..cert-")
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = os.RemoveAll(generation)
		}
	}()
	for _, name := range []string{o.CertName, o.KeyName} {
		if err := os.WriteFile(filepath.Join(generation, name), secret.Data[name], 0600); err != nil {
			return err
		}
		path := filepath.Join(o.CertDir, name)
		if _, err := os.Lstat(path); os.IsNotExist(err) {
			if err := os.Symlink(filepath.Join("..data", name), path); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
	}
	temporary := filepath.Join(o.CertDir, "..data-next")
	if err := os.Remove(temporary); err != nil && !os.IsNotExist(err) {
		return err
	}
	if err := os.Symlink(filepath.Base(generation), temporary); err != nil {
		return err
	}
	if err := os.Rename(temporary, current); err != nil {
		return err
	}
	published = true
	// Removing the old inode also wakes certwatcher's file watches. Only remove
	// generation directories owned by this writer, including across Pod restarts.
	if strings.HasPrefix(previous, "..cert-") && filepath.Base(previous) == previous {
		return os.RemoveAll(filepath.Join(o.CertDir, previous))
	}
	return nil
}
