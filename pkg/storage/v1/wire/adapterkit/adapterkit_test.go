package adapterkit_test

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/storagetest"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/adapterkit"
)

const serverName = "storage.adapterkit.test"

// memoryArtifacts is an in-memory artifacts.v1 provider. hold, when set,
// runs at the start of SaveArtifact, so a test can block a call or inspect
// its context.
type memoryArtifacts struct {
	dataset string
	mu      sync.Mutex
	files   map[storagev1.ArtifactKey]storedArtifact
	healthy atomic.Bool
	hold    func(ctx context.Context)
}

type storedArtifact struct {
	data        []byte
	contentType string
	createdAt   time.Time
}

func newMemoryArtifacts() *memoryArtifacts {
	provider := &memoryArtifacts{dataset: "memory-" + randomHex(8), files: map[storagev1.ArtifactKey]storedArtifact{}}
	provider.healthy.Store(true)
	return provider
}

func (p *memoryArtifacts) HealthCheck(context.Context) error {
	if !p.healthy.Load() {
		return storagev1.ErrUnavailable
	}
	return nil
}

func (p *memoryArtifacts) Describe(context.Context) (storagev1.ProviderInfo, error) {
	return storagev1.ProviderInfo{
		ContractVersion: storagev1.ContractVersion, Contracts: []string{storagev1.ArtifactsContract},
		DatasetID: p.dataset, DatasetOrigin: storagev1.DatasetOriginNew, SchemaVersion: "1",
		Limits: storagev1.ProviderLimits{MaxArtifactBytes: storagev1.MaxArtifactBytes}, Durability: storagev1.DurabilityLocalBuffered,
	}, nil
}

func ownerKey(owner storagev1.ArtifactOwner) storagev1.ArtifactOwner {
	return storagev1.ArtifactOwner{Namespace: owner.Namespace, TaskName: owner.TaskName}
}

func (p *memoryArtifacts) SaveArtifact(ctx context.Context, request storagev1.SaveArtifactRequest) error {
	if p.hold != nil {
		p.hold(ctx)
	}
	if len(request.Data) > storagev1.MaxArtifactBytes {
		return storagev1.TooLargeError{What: "artifact", Size: int64(len(request.Data)), Limit: storagev1.MaxArtifactBytes}
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	key := storagev1.ArtifactKey{Owner: ownerKey(request.Owner), Filename: request.Filename}
	p.files[key] = storedArtifact{data: bytes.Clone(request.Data), contentType: request.ContentType, createdAt: time.Now().UTC()}
	return nil
}

func (p *memoryArtifacts) GetArtifact(_ context.Context, key storagev1.ArtifactKey) ([]byte, string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	stored, ok := p.files[storagev1.ArtifactKey{Owner: ownerKey(key.Owner), Filename: key.Filename}]
	if !ok {
		return nil, "", storagev1.ErrNotFound
	}
	return bytes.Clone(stored.data), stored.contentType, nil
}

func (p *memoryArtifacts) ListArtifacts(_ context.Context, owner storagev1.ArtifactOwner) ([]storagev1.ArtifactMetadata, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	var listed []storagev1.ArtifactMetadata
	for key, stored := range p.files {
		if key.Owner == ownerKey(owner) {
			listed = append(listed, storagev1.ArtifactMetadata{Filename: key.Filename, ContentType: stored.contentType,
				Size: int64(len(stored.data)), CreatedAt: stored.createdAt})
		}
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].Filename < listed[j].Filename })
	return listed, nil
}

func (p *memoryArtifacts) DeleteArtifacts(_ context.Context, owner storagev1.ArtifactOwner) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key := range p.files {
		if key.Owner == ownerKey(owner) {
			delete(p.files, key)
		}
	}
	return nil
}

func randomHex(size int) string {
	raw := make([]byte, size)
	_, _ = rand.Read(raw)
	return strings.ToLower(new(big.Int).SetBytes(raw).Text(16))
}

// authority is a test certificate authority.
type authority struct {
	cert *x509.Certificate
	key  *ecdsa.PrivateKey
	pem  []byte
}

func newAuthority(t *testing.T, name string) authority {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)
	cert, err := x509.ParseCertificate(der)
	require.NoError(t, err)
	return authority{cert: cert, key: key, pem: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})}
}

func (a authority) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AddCert(a.cert)
	return pool
}

// issue returns a PEM certificate and key signed by a.
func (a authority) issue(t *testing.T, serial int64, usage x509.ExtKeyUsage, dnsNames []string, uris ...string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "adapterkit test"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, DNSNames: dnsNames,
	}
	for _, raw := range uris {
		parsed, err := url.Parse(raw)
		require.NoError(t, err)
		template.URIs = append(template.URIs, parsed)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, a.cert, &key.PublicKey, a.key)
	require.NoError(t, err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	require.NoError(t, err)
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}

func (a authority) clientCertificate(t *testing.T, uris ...string) *tls.Certificate {
	t.Helper()
	certPEM, keyPEM := a.issue(t, 50, x509.ExtKeyUsageClientAuth, nil, uris...)
	certificate, err := tls.X509KeyPair(certPEM, keyPEM)
	require.NoError(t, err)
	return &certificate
}

func writeFile(t *testing.T, path string, content []byte) {
	t.Helper()
	require.NoError(t, os.WriteFile(path, content, 0o600))
}

// adapter is a running Serve.
type adapter struct {
	address string
	probe   string
	stop    context.CancelFunc
	done    chan error
	roots   *x509.CertPool
}

// fixture holds the files and authorities one adapter serves with.
type fixture struct {
	dir       string
	serverCA  authority
	certFile  string
	keyFile   string
	tokenDir  string
	clientCA  authority
	caFile    string
	clientURI string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	dir := t.TempDir()
	f := &fixture{
		dir: dir, serverCA: newAuthority(t, "server CA"), clientCA: newAuthority(t, "client CA"),
		certFile: filepath.Join(dir, "tls.crt"), keyFile: filepath.Join(dir, "tls.key"),
		tokenDir: filepath.Join(dir, "client-auth"), caFile: filepath.Join(dir, "client-ca.crt"),
		clientURI: "spiffe://orka.test/ns/orka-system/sa/orka-controller",
	}
	f.serveCertificate(t, 100)
	require.NoError(t, os.Mkdir(f.tokenDir, 0o700))
	writeFile(t, filepath.Join(f.tokenDir, "token"), []byte("first-token\n"))
	writeFile(t, f.caFile, f.clientCA.pem)
	return f
}

// serveCertificate writes a serving certificate with serial.
func (f *fixture) serveCertificate(t *testing.T, serial int64) {
	t.Helper()
	certPEM, keyPEM := f.serverCA.issue(t, serial, x509.ExtKeyUsageServerAuth, []string{serverName})
	writeFile(t, f.keyFile, keyPEM)
	writeFile(t, f.certFile, certPEM)
}

func (f *fixture) bearerConfig(provider storagev1.ArtifactProvider) adapterkit.Config {
	return adapterkit.Config{
		Artifacts: provider, AdapterName: "adapterkit-test", AdapterVersion: "test",
		CertFile: f.certFile, KeyFile: f.keyFile, BearerTokenDir: f.tokenDir,
		ReloadInterval: 20 * time.Millisecond, ReadinessInterval: 20 * time.Millisecond, ShutdownTimeout: 5 * time.Second,
	}
}

func (f *fixture) mtlsConfig(provider storagev1.ArtifactProvider) adapterkit.Config {
	config := f.bearerConfig(provider)
	config.BearerTokenDir = ""
	config.ClientCAFile = f.caFile
	config.AllowedClientURIs = []string{f.clientURI}
	return config
}

func start(t *testing.T, f *fixture, config adapterkit.Config) *adapter {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	probe, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	config.Listener, config.ProbeListener = listener, probe
	ctx, cancel := context.WithCancel(t.Context())
	running := &adapter{
		address: listener.Addr().String(), probe: probe.Addr().String(), stop: cancel,
		done: make(chan error, 1), roots: f.serverCA.pool(),
	}
	go func() { running.done <- adapterkit.Serve(ctx, config) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-running.done:
		case <-time.After(10 * time.Second):
			t.Error("the adapter did not stop")
		}
	})
	return running
}

// tokenSource sends a bearer token the test can change.
type tokenSource struct{ token atomic.Pointer[string] }

func newTokenSource(token string) *tokenSource {
	source := &tokenSource{}
	source.token.Store(&token)
	return source
}

func (s *tokenSource) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + *s.token.Load()}, nil
}

func (*tokenSource) RequireTransportSecurity() bool { return true }

// dial connects to the adapter with an optional client certificate and
// bearer token.
func (a *adapter) dial(t *testing.T, certificate *tls.Certificate, token credentials.PerRPCCredentials) *grpc.ClientConn {
	t.Helper()
	config := &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: a.roots, ServerName: serverName}
	if certificate != nil {
		config.Certificates = []tls.Certificate{*certificate}
	}
	conn, err := wire.Dial("passthrough:///"+a.address, wire.DialConfig{
		TransportCredentials: credentials.NewTLS(config), PerRPCCredentials: token,
	})
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// bind describes the adapter and returns a client bound to its dataset.
func bind(t *testing.T, conn *grpc.ClientConn) *wire.Client {
	t.Helper()
	info, server, err := wire.NewClient(conn, wire.ClientConfig{}).DescribeServer(t.Context())
	require.NoError(t, err)
	require.Equal(t, "adapterkit-test", server.AdapterName)
	return wire.NewClient(conn, wire.ClientConfig{Dataset: info.DatasetID, ServerRevision: int(server.ProtocolRevision), Limits: info.Limits})
}

func (a *adapter) get(t *testing.T, path string) (int, string) {
	t.Helper()
	response, err := http.Get("http://" + a.probe + path)
	if err != nil {
		return 0, err.Error()
	}
	defer func() { _ = response.Body.Close() }()
	body, _ := io.ReadAll(response.Body)
	return response.StatusCode, string(body)
}

func (a *adapter) waitReady(t *testing.T, ready bool) {
	t.Helper()
	want := http.StatusServiceUnavailable
	if ready {
		want = http.StatusOK
	}
	require.Eventually(t, func() bool {
		code, _ := a.get(t, "/readyz")
		return code == want
	}, 5*time.Second, 10*time.Millisecond, "readiness %v", ready)
}

// The adapter serves the whole artifacts.v1 suite over TLS with bearer
// authentication, through the generated client.
func TestServeRunsTheArtifactSuite(t *testing.T) {
	f := newFixture(t)
	storagetest.RunArtifacts(t, func(t *testing.T) storagev1.ArtifactProvider {
		running := start(t, f, f.bearerConfig(newMemoryArtifacts()))
		return bind(t, running.dial(t, nil, newTokenSource("first-token")))
	})
}

// Bearer tokens rotate by adding a second key, moving clients to it and
// removing the first, without restarting the adapter.
func TestServeRotatesBearerTokens(t *testing.T) {
	f := newFixture(t)
	running := start(t, f, f.bearerConfig(newMemoryArtifacts()))
	token := newTokenSource("first-token")
	client := bind(t, running.dial(t, nil, token))
	require.NoError(t, client.HealthCheck(t.Context()))

	wrong := newTokenSource("wrong-token")
	err := wire.NewClient(running.dial(t, nil, wrong), wire.ClientConfig{}).HealthCheck(t.Context())
	require.ErrorIs(t, err, wire.ErrAuthentication)
	require.ErrorIs(t, err, storagev1.ErrUnavailable)

	writeFile(t, filepath.Join(f.tokenDir, "token-2"), []byte("second-token"))
	token.token.Store(new("second-token"))
	require.Eventually(t, func() bool { return client.HealthCheck(t.Context()) == nil }, 5*time.Second, 10*time.Millisecond,
		"the added token is accepted once the adapter reloads")

	require.NoError(t, os.Remove(filepath.Join(f.tokenDir, "token")))
	old := wire.NewClient(running.dial(t, nil, newTokenSource("first-token")), wire.ClientConfig{})
	require.Eventually(t, func() bool { return errors.Is(old.HealthCheck(t.Context()), wire.ErrAuthentication) },
		5*time.Second, 10*time.Millisecond, "the removed token is refused")
	require.NoError(t, client.HealthCheck(t.Context()), "the remaining token still works")

	require.NoError(t, os.Remove(filepath.Join(f.tokenDir, "token-2")))
	require.Eventually(t, func() bool { return errors.Is(client.HealthCheck(t.Context()), wire.ErrAuthentication) },
		5*time.Second, 10*time.Millisecond, "with no tokens left every call is refused")
}

// peerSerial returns the serial of the certificate the adapter presents to a
// new connection.
func (a *adapter) peerSerial(t *testing.T) int64 {
	t.Helper()
	conn, err := tls.Dial("tcp", a.address, &tls.Config{
		MinVersion: tls.VersionTLS13, RootCAs: a.roots, ServerName: serverName, NextProtos: []string{"h2"},
	})
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	return conn.ConnectionState().PeerCertificates[0].SerialNumber.Int64()
}

// The adapter serves a renewed certificate to new connections without a
// restart, and keeps its certificate when the files hold an invalid one.
func TestServeReloadsTheServingCertificate(t *testing.T) {
	f := newFixture(t)
	running := start(t, f, f.bearerConfig(newMemoryArtifacts()))
	require.Equal(t, int64(100), running.peerSerial(t))
	client := bind(t, running.dial(t, nil, newTokenSource("first-token")))

	f.serveCertificate(t, 200)
	require.Eventually(t, func() bool { return running.peerSerial(t) == 200 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, client.HealthCheck(t.Context()), "an existing connection keeps working")

	writeFile(t, f.certFile, []byte("not a certificate"))
	require.Eventually(t, func() bool {
		_, metrics := running.get(t, "/metrics")
		return strings.Contains(metrics, `orka_storage_server_credential_reloads_total{credential="certificate",result="failure"}`)
	}, 5*time.Second, 10*time.Millisecond)
	require.Equal(t, int64(200), running.peerSerial(t), "an invalid certificate never replaces a valid one")
}

// With mTLS, a client needs a certificate from the client CA that names an
// allowed client, and the CA bundle can rotate without a restart.
func TestServeAuthenticatesClientCertificates(t *testing.T) {
	f := newFixture(t)
	running := start(t, f, f.mtlsConfig(newMemoryArtifacts()))

	allowed := f.clientCA.clientCertificate(t, f.clientURI)
	client := bind(t, running.dial(t, allowed, nil))
	require.NoError(t, client.HealthCheck(t.Context()))

	other := f.clientCA.clientCertificate(t, "spiffe://orka.test/ns/other/sa/other")
	err := wire.NewClient(running.dial(t, other, nil), wire.ClientConfig{}).HealthCheck(t.Context())
	require.ErrorIs(t, err, wire.ErrAuthentication, "a certificate that names no allowed client is refused")

	err = wire.NewClient(running.dial(t, nil, nil), wire.ClientConfig{}).HealthCheck(t.Context())
	require.ErrorIs(t, err, storagev1.ErrUnavailable, "a client without a certificate cannot connect")
	require.False(t, errors.Is(err, storagev1.ErrOutcomeUnknown))

	rotated := newAuthority(t, "rotated client CA")
	fromRotated := rotated.clientCertificate(t, f.clientURI)
	err = wire.NewClient(running.dial(t, fromRotated, nil), wire.ClientConfig{}).HealthCheck(t.Context())
	require.ErrorIs(t, err, storagev1.ErrUnavailable, "a certificate from an untrusted CA cannot connect")

	writeFile(t, f.caFile, append(append([]byte(nil), f.clientCA.pem...), rotated.pem...))
	require.Eventually(t, func() bool {
		return wire.NewClient(running.dial(t, fromRotated, nil), wire.ClientConfig{}).HealthCheck(t.Context()) == nil
	}, 5*time.Second, 10*time.Millisecond, "the new CA is trusted once the bundle holds it")
	require.NoError(t, wire.NewClient(running.dial(t, allowed, nil), wire.ClientConfig{}).HealthCheck(t.Context()),
		"the old CA stays trusted while the bundle holds both")
}

// With both methods configured, a call must pass both.
func TestServeRequiresBothMethodsWhenBothAreConfigured(t *testing.T) {
	f := newFixture(t)
	config := f.mtlsConfig(newMemoryArtifacts())
	config.BearerTokenDir = f.tokenDir
	running := start(t, f, config)
	allowed := f.clientCA.clientCertificate(t, f.clientURI)

	require.NoError(t, wire.NewClient(running.dial(t, allowed, newTokenSource("first-token")), wire.ClientConfig{}).HealthCheck(t.Context()))
	err := wire.NewClient(running.dial(t, allowed, nil), wire.ClientConfig{}).HealthCheck(t.Context())
	require.ErrorIs(t, err, wire.ErrAuthentication, "a certificate without a token is refused")
	err = wire.NewClient(running.dial(t, allowed, newTokenSource("wrong-token")), wire.ClientConfig{}).HealthCheck(t.Context())
	require.ErrorIs(t, err, wire.ErrAuthentication)
	err = wire.NewClient(running.dial(t, nil, newTokenSource("first-token")), wire.ClientConfig{}).HealthCheck(t.Context())
	require.ErrorIs(t, err, storagev1.ErrUnavailable, "a token without a certificate cannot connect")
}

// The probe port reports liveness, readiness from the provider's health and
// metrics without request content.
func TestServeProbesAndMetrics(t *testing.T) {
	f := newFixture(t)
	provider := newMemoryArtifacts()
	running := start(t, f, f.bearerConfig(provider))
	code, _ := running.get(t, "/healthz")
	require.Equal(t, http.StatusOK, code)
	running.waitReady(t, true)

	client := bind(t, running.dial(t, nil, newTokenSource("first-token")))
	require.NoError(t, client.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{
		Owner: storagev1.ArtifactOwner{Namespace: "n", TaskName: "t"}, Filename: "secret-file-name", Data: []byte("secret-content"),
	}))
	_, _, err := client.GetArtifact(t.Context(), storagev1.ArtifactKey{Owner: storagev1.ArtifactOwner{Namespace: "n", TaskName: "t"}, Filename: "missing"})
	require.ErrorIs(t, err, storagev1.ErrNotFound)
	_ = wire.NewClient(running.dial(t, nil, newTokenSource("wrong-token")), wire.ClientConfig{}).HealthCheck(t.Context())

	_, metrics := running.get(t, "/metrics")
	for _, line := range []string{
		`orka_storage_server_requests_total{method="SaveArtifact",code="OK",reason=""} 1`,
		`orka_storage_server_requests_total{method="GetArtifact",code="NotFound",reason="NOT_FOUND"} 1`,
		`orka_storage_server_requests_total{method="HealthCheck",code="Unauthenticated",reason="UNAUTHENTICATED"} 1`,
		`orka_storage_server_request_duration_seconds_count{method="SaveArtifact"} 1`,
		`orka_storage_server_ready 1`,
	} {
		require.Contains(t, metrics, line)
	}
	require.NotContains(t, metrics, "secret-file-name")
	require.NotContains(t, metrics, "secret-content")
	require.NotContains(t, metrics, "first-token")

	provider.healthy.Store(false)
	running.waitReady(t, false)
	_, metrics = running.get(t, "/metrics")
	require.Contains(t, metrics, "orka_storage_server_ready 0")
	provider.healthy.Store(true)
	running.waitReady(t, true)
}

// Stopping reports not ready at once, lets a call in progress finish and
// then returns.
func TestServeStopsGracefully(t *testing.T) {
	f := newFixture(t)
	provider := newMemoryArtifacts()
	entered, release := make(chan struct{}), make(chan struct{})
	provider.hold = func(context.Context) {
		close(entered)
		<-release
	}
	running := start(t, f, f.bearerConfig(provider))
	running.waitReady(t, true)
	client := bind(t, running.dial(t, nil, newTokenSource("first-token")))

	saved := make(chan error, 1)
	go func() {
		saved <- client.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{
			Owner: storagev1.ArtifactOwner{Namespace: "n", TaskName: "t"}, Filename: "f", Data: []byte("x"),
		})
	}()
	<-entered
	running.stop()
	running.waitReady(t, false)
	select {
	case err := <-running.done:
		t.Fatalf("the adapter stopped with a call in progress: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	require.NoError(t, <-saved, "the call in progress finishes")
	select {
	case err := <-running.done:
		require.NoError(t, err)
		running.done <- nil
	case <-time.After(10 * time.Second):
		t.Fatal("the adapter did not stop")
	}
	_, err := net.DialTimeout("tcp", running.address, time.Second)
	require.Error(t, err, "the storage port is closed")
}

// A provider can settle a commit it already sent after the caller's context
// ends, within the adapter's settle timeout.
func TestSettleContextOutlivesTheCaller(t *testing.T) {
	f := newFixture(t)
	provider := newMemoryArtifacts()
	type settled struct {
		err      error
		remains  time.Duration
		canceled bool
	}
	result := make(chan settled, 1)
	entered := make(chan struct{})
	provider.hold = func(ctx context.Context) {
		close(entered)
		<-ctx.Done()
		settleCtx, cancel := adapterkit.SettleContext(ctx)
		defer cancel()
		deadline, _ := settleCtx.Deadline()
		result <- settled{err: settleCtx.Err(), remains: time.Until(deadline), canceled: ctx.Err() != nil}
	}
	config := f.bearerConfig(provider)
	config.SettleTimeout = 3 * time.Second
	running := start(t, f, config)
	client := bind(t, running.dial(t, nil, newTokenSource("first-token")))

	ctx, cancel := context.WithCancel(t.Context())
	saved := make(chan error, 1)
	go func() {
		saved <- client.SaveArtifact(ctx, storagev1.SaveArtifactRequest{
			Owner: storagev1.ArtifactOwner{Namespace: "n", TaskName: "t"}, Filename: "f", Data: []byte("x"),
		})
	}()
	<-entered
	cancel()
	got := <-result
	require.True(t, got.canceled, "the caller's context ended")
	require.NoError(t, got.err, "the settle context is still live")
	require.Greater(t, got.remains, 2*time.Second)
	require.LessOrEqual(t, got.remains, 3*time.Second)
	require.ErrorIs(t, <-saved, storagev1.ErrOutcomeUnknown, "the caller cannot know whether the commit settled")

	settleCtx, cancelSettle := adapterkit.SettleContext(context.Background())
	defer cancelSettle()
	deadline, _ := settleCtx.Deadline()
	require.WithinDuration(t, time.Now().Add(adapterkit.DefaultSettleTimeout), deadline, time.Second,
		"outside Serve the default settle timeout applies")
}

// Serve refuses an incomplete or insecure configuration before it listens.
func TestServeValidatesItsConfiguration(t *testing.T) {
	f := newFixture(t)
	provider := newMemoryArtifacts()
	emptyDir := t.TempDir()
	for name, edit := range map[string]func(*adapterkit.Config){
		"no provider":               func(c *adapterkit.Config) { c.Artifacts = nil },
		"no certificate":            func(c *adapterkit.Config) { c.CertFile = "" },
		"no client authentication":  func(c *adapterkit.Config) { c.BearerTokenDir = "" },
		"mTLS without allowed name": func(c *adapterkit.Config) { c.BearerTokenDir, c.ClientCAFile = "", f.caFile },
		"allowed names without CA":  func(c *adapterkit.Config) { c.AllowedClientDNSNames = []string{"orka"} },
		"invalid certificate":       func(c *adapterkit.Config) { c.KeyFile = f.caFile },
		"no tokens":                 func(c *adapterkit.Config) { c.BearerTokenDir = emptyDir },
		"missing CA bundle": func(c *adapterkit.Config) {
			c.ClientCAFile, c.AllowedClientURIs = filepath.Join(emptyDir, "missing"), []string{f.clientURI}
		},
	} {
		t.Run(name, func(t *testing.T) {
			config := f.bearerConfig(provider)
			edit(&config)
			config.Address, config.ProbeAddress = "127.0.0.1:0", "127.0.0.1:0"
			done := make(chan error, 1)
			go func() { done <- adapterkit.Serve(t.Context(), config) }()
			select {
			case err := <-done:
				require.Error(t, err)
				require.NotContains(t, err.Error(), "first-token")
			case <-time.After(5 * time.Second):
				t.Fatal("Serve started with an invalid configuration")
			}
		})
	}
}
