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
	"slices"
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

const ServerName = "storage.wiretest.invalid"

func State(t testing.TB, provider storagev1.StateProvider) storagev1.StateProvider {
	t.Helper()
	return stateOverlay{StateProvider: provider, client: Serve(t, wire.ServerConfig{State: provider})}
}

func Methods() []string {
	return slices.Clone(overlayMethods)
}

func Artifacts(t testing.TB, provider storagev1.ArtifactProvider) storagev1.ArtifactProvider {
	t.Helper()
	return artifactsOverlay{ArtifactProvider: provider, client: Serve(t, wire.ServerConfig{Artifacts: provider})}
}

func ArtifactsMethods() []string {
	return slices.Clone(artifactsOverlayMethods)
}

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

func Dial(t testing.TB, config wire.ServerConfig) *grpc.ClientConn {
	t.Helper()
	return serve(t, config)()
}

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
		grpc.Creds(credentials.NewTLS(material.serverTLS())), grpc.WaitForHandlers(true))...)
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

type bearerToken string

func (token bearerToken) GetRequestMetadata(context.Context, ...string) (map[string]string, error) {
	return map[string]string{"authorization": "Bearer " + string(token)}, nil
}

func (bearerToken) RequireTransportSecurity() bool { return true }

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
