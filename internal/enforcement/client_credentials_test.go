package enforcement

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type identityRecorder struct {
	enforcementv1.UnimplementedEnforcementProviderServer
	serials chan string
}

func (r *identityRecorder) EnsureEnforced(ctx context.Context, _ *enforcementv1.EnsureRequest) (*enforcementv1.EnsureResponse, error) {
	caller, ok := peer.FromContext(ctx)
	if !ok {
		return nil, status.Error(codes.Unauthenticated, "missing authenticated peer")
	}
	tlsInfo, ok := caller.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 {
		return nil, status.Error(codes.Unauthenticated, "missing verified client certificate")
	}
	r.serials <- tlsInfo.State.PeerCertificates[0].SerialNumber.String()
	return &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Pending{
		Pending: &enforcementv1.Pending{Reason: "transport-only test"},
	}}, nil
}

func TestClientReloadsRenewedIdentityOnNewConnection(t *testing.T) {
	directory := t.TempDir()
	rootPublic, rootPrivate, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	root := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "renewal CA"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		BasicConstraintsValid: true, IsCA: true, KeyUsage: x509.KeyUsageCertSign,
	}
	rootDER, err := x509.CreateCertificate(rand.Reader, root, root, rootPublic, rootPrivate)
	require.NoError(t, err)
	rootPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: rootDER})
	issue := func(serial int64, server bool) ([]byte, []byte) {
		t.Helper()
		public, private, err := ed25519.GenerateKey(rand.Reader)
		require.NoError(t, err)
		leaf := &x509.Certificate{
			SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: "renewed peer"},
			NotBefore: root.NotBefore, NotAfter: root.NotAfter,
			KeyUsage: x509.KeyUsageDigitalSignature,
			ExtKeyUsage: []x509.ExtKeyUsage{
				x509.ExtKeyUsageClientAuth,
			},
		}
		if server {
			leaf.DNSNames = []string{"node.provider.test"}
			leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, root, public, rootPrivate)
		require.NoError(t, err)
		key, err := x509.MarshalPKCS8PrivateKey(private)
		require.NoError(t, err)
		return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
			pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: key})
	}
	serverCert, serverKey := issue(2, true)
	serverPair, err := tls.X509KeyPair(serverCert, serverKey)
	require.NoError(t, err)
	roots := x509.NewCertPool()
	require.True(t, roots.AppendCertsFromPEM(rootPEM))
	recorder := &identityRecorder{serials: make(chan string, 2)}
	startServer := func(address string) (*grpc.Server, string) {
		t.Helper()
		listener, err := net.Listen("tcp", address)
		require.NoError(t, err)
		server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert,
			ClientCAs: roots, Certificates: []tls.Certificate{serverPair},
		})))
		enforcementv1.RegisterEnforcementProviderServer(server, recorder)
		done := make(chan error, 1)
		go func() { done <- server.Serve(listener) }()
		t.Cleanup(func() {
			server.Stop()
			require.NoError(t, <-done)
		})
		return server, listener.Addr().String()
	}
	server, address := startServer("127.0.0.1:0")
	config := ProviderConfig{
		Name: "filesystem", Endpoint: "passthrough:///" + address, ServerName: "node.provider.test",
		CAFile: filepath.Join(directory, "ca.pem"), CertificateFile: filepath.Join(directory, "client.pem"),
		KeyFile: filepath.Join(directory, "client-key.pem"),
	}
	require.NoError(t, os.WriteFile(config.CAFile, rootPEM, 0600))
	writeIdentity := func(serial int64) {
		t.Helper()
		certificate, key := issue(serial, false)
		require.NoError(t, os.WriteFile(config.CertificateFile, certificate, 0600))
		require.NoError(t, os.WriteFile(config.KeyFile, key, 0600))
	}
	writeIdentity(3)
	client, err := NewClient(config)
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, client.Close()) })
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	_, err = client.rpc.EnsureEnforced(ctx, exampleRequest(), grpc.WaitForReady(true))
	require.NoError(t, err)
	require.Equal(t, "3", <-recorder.serials)

	writeIdentity(4)
	server.GracefulStop()
	startServer(address)
	_, err = client.rpc.EnsureEnforced(ctx, exampleRequest(), grpc.WaitForReady(true))
	require.NoError(t, err)
	require.Equal(t, "4", <-recorder.serials, "renewal must not require recreating the coordinator")
}
