package adapterkit

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
)

type credentialStore struct {
	config  *Config
	metrics *metrics

	certificate atomic.Pointer[tls.Certificate]
	tokens      atomic.Pointer[[][]byte]
	clientCAs   atomic.Pointer[x509.CertPool]

	mu        sync.Mutex
	loadedSum map[string][sha256.Size]byte
}

func loadCredentials(config *Config, metrics *metrics) (*credentialStore, error) {
	c := &credentialStore{config: config, metrics: metrics, loadedSum: map[string][sha256.Size]byte{}}
	if err := c.reloadCertificate(); err != nil {
		return nil, fmt.Errorf("load the serving certificate: %w", err)
	}
	if config.BearerTokenDir != "" {
		if err := c.reloadTokens(); err != nil {
			return nil, fmt.Errorf("load bearer tokens: %w", err)
		}
		if len(*c.tokens.Load()) == 0 {
			return nil, fmt.Errorf("load bearer tokens: %s holds no file named token*", config.BearerTokenDir)
		}
	}
	if config.ClientCAFile != "" {
		if err := c.reloadClientCAs(); err != nil {
			return nil, fmt.Errorf("load the client CA bundle: %w", err)
		}
	}
	return c, nil
}

func (c *credentialStore) watch(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		c.reload(ctx)
	}
}

func (c *credentialStore) reload(ctx context.Context) {
	check := func(name string, reload func() error) {
		if err := reload(); err != nil {
			c.config.Logger.ErrorContext(ctx, "storage adapter credential reload failed; keeping the previous value",
				"credential", name, "error", err)
		}
	}
	check("certificate", c.reloadCertificate)
	if c.config.BearerTokenDir != "" {
		check("bearer-tokens", c.reloadTokens)
	}
	if c.config.ClientCAFile != "" {
		check("client-ca", c.reloadClientCAs)
	}
}

func (c *credentialStore) changed(name string, content []byte) ([sha256.Size]byte, bool) {
	sum := sha256.Sum256(content)
	c.mu.Lock()
	defer c.mu.Unlock()
	previous, ok := c.loadedSum[name]
	return sum, !ok || previous != sum
}

func (c *credentialStore) loaded(name string, sum [sha256.Size]byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadedSum[name] = sum
}

func (c *credentialStore) reloadCertificate() error {
	certPEM, err := os.ReadFile(c.config.CertFile)
	if err != nil {
		return c.failed("certificate", err)
	}
	keyPEM, err := os.ReadFile(c.config.KeyFile)
	if err != nil {
		return c.failed("certificate", err)
	}
	sum, changed := c.changed("certificate", append(append([]byte(nil), certPEM...), keyPEM...))
	if !changed {
		return nil
	}
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return c.failed("certificate", errors.New("the certificate and key do not form a valid pair"))
	}
	c.certificate.Store(&certificate)
	c.loaded("certificate", sum)
	c.metrics.credentialReload("certificate", true)
	return nil
}

func (c *credentialStore) reloadTokens() error {
	entries, err := os.ReadDir(c.config.BearerTokenDir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return c.failed("bearer-tokens", err)
		}
		entries = nil
	}
	var names []string
	var tokens [][]byte
	var content bytes.Buffer
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), "token") {
			continue
		}
		path := filepath.Join(c.config.BearerTokenDir, entry.Name())
		info, err := os.Stat(path)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return c.failed("bearer-tokens", err)
		}
		if !info.Mode().IsRegular() {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return c.failed("bearer-tokens", err)
		}
		token := bytes.TrimSpace(raw)
		if len(token) == 0 {
			continue
		}
		names = append(names, entry.Name())
		tokens = append(tokens, token)
	}
	for index, name := range names {
		fmt.Fprintf(&content, "%s\x00%d\x00", name, len(tokens[index]))
	}
	for _, token := range tokens {
		content.Write(token)
	}
	sum, changed := c.changed("bearer-tokens", content.Bytes())
	if !changed {
		return nil
	}
	if len(tokens) == 0 && c.tokens.Load() != nil {
		c.config.Logger.Error("storage adapter has no bearer tokens left; every bearer-authenticated call is refused",
			"directory", c.config.BearerTokenDir)
	}
	c.tokens.Store(&tokens)
	c.loaded("bearer-tokens", sum)
	c.metrics.credentialReload("bearer-tokens", true)
	return nil
}

func (c *credentialStore) reloadClientCAs() error {
	bundle, err := os.ReadFile(c.config.ClientCAFile)
	if err != nil {
		return c.failed("client-ca", err)
	}
	sum, changed := c.changed("client-ca", bundle)
	if !changed {
		return nil
	}
	pool, err := parseCABundle(bundle)
	if err != nil {
		return c.failed("client-ca", err)
	}
	c.clientCAs.Store(pool)
	c.loaded("client-ca", sum)
	c.metrics.credentialReload("client-ca", true)
	return nil
}

func parseCABundle(bundle []byte) (*x509.CertPool, error) {
	pool := x509.NewCertPool()
	parsed := 0
	for rest := bundle; ; {
		var block *pem.Block
		block, rest = pem.Decode(rest)
		if block == nil {
			break
		}
		if block.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("the bundle holds a %q block, not a certificate", block.Type)
		}
		certificate, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return nil, fmt.Errorf("the bundle holds an invalid certificate: %w", err)
		}
		pool.AddCert(certificate)
		parsed++
	}
	if begun := bytes.Count(bundle, []byte("-----BEGIN ")); begun != parsed {
		return nil, fmt.Errorf("the bundle holds %d PEM blocks, of which only %d decode", begun, parsed)
	}
	if parsed == 0 {
		return nil, errors.New("the bundle holds no PEM certificate")
	}
	return pool, nil
}

func (c *credentialStore) failed(name string, err error) error {
	c.metrics.credentialReload(name, false)
	return err
}

func (c *credentialStore) serverTLS() *tls.Config {
	config := &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			return c.certificate.Load(), nil
		},
	}
	if c.config.ClientCAFile != "" {
		base := config.Clone()
		config.GetConfigForClient = func(*tls.ClientHelloInfo) (*tls.Config, error) {
			handshake := base.Clone()
			handshake.ClientAuth = tls.RequireAndVerifyClientCert
			handshake.ClientCAs = c.clientCAs.Load()
			return handshake, nil
		}
	}
	return config
}

func (c *credentialStore) Authenticate(ctx context.Context, _ string, header metadata.MD) error {
	if c.config.BearerTokenDir != "" {
		if err := c.authenticateBearer(header); err != nil {
			return err
		}
	}
	if c.config.ClientCAFile != "" {
		if err := c.authenticateCertificate(ctx); err != nil {
			return err
		}
	}
	return nil
}

func (c *credentialStore) authenticateBearer(header metadata.MD) error {
	values := header.Get("authorization")
	if len(values) != 1 {
		return errors.New("the call must carry exactly one bearer token")
	}
	presented, ok := strings.CutPrefix(values[0], "Bearer ")
	if !ok || presented == "" {
		return errors.New("the call carries no bearer token")
	}
	accepted := c.tokens.Load()
	if accepted == nil {
		return errors.New("no bearer token is accepted")
	}
	match := 0
	for _, token := range *accepted {
		match |= subtle.ConstantTimeCompare([]byte(presented), token)
	}
	if match != 1 {
		return errors.New("the bearer token is not accepted")
	}
	return nil
}

func (c *credentialStore) authenticateCertificate(ctx context.Context) error {
	caller, ok := peer.FromContext(ctx)
	if !ok {
		return errors.New("the call has no peer")
	}
	info, ok := caller.AuthInfo.(credentials.TLSInfo)
	if !ok || len(info.State.VerifiedChains) == 0 || len(info.State.VerifiedChains[0]) == 0 {
		return errors.New("the call presents no verified client certificate")
	}
	leaf := info.State.VerifiedChains[0][0]
	intermediates := x509.NewCertPool()
	for _, presented := range info.State.PeerCertificates[1:] {
		intermediates.AddCert(presented)
	}
	if _, err := leaf.Verify(x509.VerifyOptions{
		Roots: c.clientCAs.Load(), Intermediates: intermediates, CurrentTime: time.Now(),
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}); err != nil {
		return errors.New("the client certificate is no longer trusted")
	}
	for _, uri := range leaf.URIs {
		if slices.Contains(c.config.AllowedClientURIs, uri.String()) {
			return nil
		}
	}
	for _, name := range leaf.DNSNames {
		if slices.Contains(c.config.AllowedClientDNSNames, name) {
			return nil
		}
	}
	return errors.New("the client certificate names no allowed client")
}
