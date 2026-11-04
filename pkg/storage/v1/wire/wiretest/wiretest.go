// Package wiretest serves storage providers over a real gRPC loopback for
// tests: TLS 1.3 with a test certificate authority, bearer authentication,
// the server library and the generated client, over an in-memory listener.
// Streaming calls use a second connection, as a remote client's bulk
// connection does. There is no plaintext mode.
package wiretest

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"errors"
	"math/big"
	"net"
	"sync"
	"testing"
	"time"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

// ServerName is the TLS server name of the loopback server.
const ServerName = "storage.wiretest.invalid"

// State serves provider over the loopback and returns a state provider whose
// families with wire RPCs go through the generated client and server. Every
// other family is served by provider in process until it gains its RPCs.
func State(t testing.TB, provider storagev1.StateProvider) storagev1.StateProvider {
	t.Helper()
	return stateOverlay{StateProvider: provider, client: Serve(t, wire.ServerConfig{State: provider})}
}

// Artifacts serves provider over the loopback and returns an artifacts.v1
// provider whose every method goes through the generated client and server.
func Artifacts(t testing.TB, provider storagev1.ArtifactProvider) storagev1.ArtifactProvider {
	t.Helper()
	return Serve(t, wire.ServerConfig{Artifacts: provider})
}

// Serve serves the providers in config over the loopback and returns a
// client bound to their dataset. Streaming RPCs use a second connection, as a
// remote client's bulk connection does. An unset Info or Authenticator
// defaults to the loopback's adapter name and bearer authentication. The
// server and connections stop when the test ends.
func Serve(t testing.TB, config wire.ServerConfig) *wire.Client {
	t.Helper()
	dial := serve(t, config)
	conn := dial()
	info, server, err := wire.NewClient(conn, wire.ClientConfig{}).DescribeServer(t.Context())
	if err != nil {
		t.Fatalf("describe the storage server: %v", err)
	}
	return wire.NewClient(conn, wire.ClientConfig{
		Dataset: info.DatasetID, ServerRevision: int(server.ProtocolRevision), Bulk: dial(), Limits: info.Limits,
	})
}

// Dial serves the providers in config over the loopback, as Serve does, and
// returns an authenticated client connection to them.
func Dial(t testing.TB, config wire.ServerConfig) *grpc.ClientConn {
	t.Helper()
	return serve(t, config)()
}

// serve starts a loopback server for config and returns a function that
// opens authenticated connections to it.
func serve(t testing.TB, config wire.ServerConfig) func() *grpc.ClientConn {
	t.Helper()
	material, err := loadMaterial()
	if err != nil {
		t.Fatalf("create loopback TLS material: %v", err)
	}
	if config.Info == (wire.ServerInfo{}) {
		config.Info = wire.ServerInfo{AdapterName: "wiretest", AdapterVersion: "test"}
	}
	if config.Authenticator == nil {
		config.Authenticator = bearerAuthenticator{bearer: material.bearer}
	}
	storageServer, err := wire.NewServer(config)
	if err != nil {
		t.Fatalf("create storage server: %v", err)
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer(append(storageServer.ServerOptions(),
		grpc.Creds(credentials.NewTLS(material.serverTLS())))...)
	storageServer.Register(grpcServer)
	served := make(chan error, 1)
	go func() { served <- grpcServer.Serve(listener) }()
	t.Cleanup(func() {
		grpcServer.Stop()
		if err := <-served; err != nil && !errors.Is(err, grpc.ErrServerStopped) {
			t.Errorf("storage server: %v", err)
		}
	})
	return func() *grpc.ClientConn {
		t.Helper()
		conn, err := wire.Dial("passthrough:///"+ServerName, wire.DialConfig{
			TransportCredentials: credentials.NewTLS(material.clientTLS()),
			PerRPCCredentials:    bearerToken(material.bearer),
			ContextDialer:        func(ctx context.Context, _ string) (net.Conn, error) { return listener.DialContext(ctx) },
		})
		if err != nil {
			t.Fatalf("dial storage server: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}
}

// material is the process-wide test certificate authority, server
// certificate and bearer token. Creating them once keeps per-case loopbacks
// cheap.
type material struct {
	roots       *x509.CertPool
	certificate tls.Certificate
	bearer      string
}

var (
	materialOnce sync.Once
	materialData *material
	materialErr  error
)

func loadMaterial() (*material, error) {
	materialOnce.Do(func() { materialData, materialErr = newMaterial() })
	return materialData, materialErr
}

func newMaterial() (*material, error) {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	now := time.Now()
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "orka storage wiretest CA"},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		return nil, err
	}
	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: ServerName},
		DNSNames:     []string{ServerName},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		return nil, err
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		return nil, err
	}
	roots := x509.NewCertPool()
	roots.AddCert(caCert)
	return &material{
		roots:       roots,
		certificate: tls.Certificate{Certificate: [][]byte{serverDER}, PrivateKey: serverKey},
		bearer:      hex.EncodeToString(random[:]),
	}, nil
}

func (m *material) serverTLS() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{m.certificate}}
}

func (m *material) clientTLS() *tls.Config {
	return &tls.Config{MinVersion: tls.VersionTLS13, RootCAs: m.roots, ServerName: ServerName}
}

// bearerToken sends the loopback's bearer token, only over TLS.
type bearerToken string

func (token bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(token)}, nil
}

func (bearerToken) RequireTransportSecurity() bool { return true }

// bearerAuthenticator accepts exactly the loopback's bearer token.
type bearerAuthenticator struct {
	bearer string
}

func (a bearerAuthenticator) Authenticate(_ context.Context, _ string, header metadata.MD) error {
	values := header.Get("authorization")
	want := "Bearer " + a.bearer
	if len(values) != 1 || subtle.ConstantTimeCompare([]byte(values[0]), []byte(want)) != 1 {
		return errors.New("invalid bearer token")
	}
	return nil
}
