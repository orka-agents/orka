package workspace

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

type substrateTLSServer struct {
	ateapipb.UnimplementedControlServer
	bearer     atomic.Value
	repeatPage atomic.Bool
}

func (s *substrateTLSServer) GetActor(ctx context.Context, req *ateapipb.GetActorRequest) (*ateapipb.Actor, error) {
	identity := ""
	if remote, ok := peer.FromContext(ctx); ok {
		if info, ok := remote.AuthInfo.(credentials.TLSInfo); ok && len(info.State.PeerCertificates) > 0 {
			identity = info.State.PeerCertificates[0].Subject.CommonName
		}
	}
	if identity == "" {
		md, _ := metadata.FromIncomingContext(ctx)
		tokens := md.Get("authorization")
		if len(tokens) != 1 || tokens[0] != "Bearer "+s.bearer.Load().(string) {
			return nil, status.Error(codes.Unauthenticated, "invalid test identity")
		}
		identity = "bearer-authenticated"
	}
	return &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: req.Actor.Atespace, Name: req.Actor.Name, Uid: identity}}, nil
}

func (s *substrateTLSServer) ListActorTemplates(_ context.Context, req *ateapipb.ListActorTemplatesRequest) (*ateapipb.ListActorTemplatesResponse, error) {
	if req.PageToken == "" {
		return &ateapipb.ListActorTemplatesResponse{NextPageToken: "second"}, nil
	}
	if req.PageToken != "second" {
		return nil, status.Error(codes.InvalidArgument, "unknown test page")
	}
	page := &ateapipb.ListActorTemplatesResponse{ActorTemplates: []*ateapipb.ActorTemplate{{Metadata: &ateapipb.ResourceMetadata{Atespace: req.Atespace, Name: "template"}}}}
	if s.repeatPage.Load() {
		page.NextPageToken = "second"
	}
	return page, nil
}

type substrateTrackingListener struct {
	net.Listener
	mu          sync.Mutex
	connections []net.Conn
}

func (l *substrateTrackingListener) Accept() (net.Conn, error) {
	conn, err := l.Listener.Accept()
	if err == nil {
		l.mu.Lock()
		l.connections = append(l.connections, conn)
		l.mu.Unlock()
	}
	return conn, err
}
func (l *substrateTrackingListener) disconnect() {
	l.mu.Lock()
	defer l.mu.Unlock()
	for _, conn := range l.connections {
		_ = conn.Close()
	}
	l.connections = nil
}

func substrateTransportFixture(t *testing.T, options ...grpc.ServerOption) (SubstrateConfig, *substrateTLSServer, *substrateTrackingListener, func(string)) {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "substrate-test-root"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	write := func(path string, data []byte) {
		t.Helper()
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	write(filepath.Join(dir, "ca.pem"), caPEM)
	serial := int64(1)
	issue := func(name string, server bool) (tls.Certificate, []byte, []byte) {
		t.Helper()
		serial++
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		leaf := &x509.Certificate{SerialNumber: big.NewInt(serial), Subject: pkix.Name{CommonName: name}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
		if server {
			leaf.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
			leaf.IPAddresses = []net.IP{net.ParseIP("127.0.0.1")}
		}
		der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		private, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		certPEM, keyPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private})
		pair, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			t.Fatal(err)
		}
		return pair, certPEM, keyPEM
	}
	serverPair, _, _ := issue("server", true)
	caPool := x509.NewCertPool()
	caPool.AppendCertsFromPEM(caPEM)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tracking := &substrateTrackingListener{Listener: listener}
	options = append(options, grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: caPool})))
	server := grpc.NewServer(options...)
	service := &substrateTLSServer{}
	service.bearer.Store("initial-test-credential")
	ateapipb.RegisterControlServer(server, service)
	go func() { _ = server.Serve(tracking) }()
	t.Cleanup(server.Stop)
	cfg := SubstrateConfig{APIEndpoint: listener.Addr().String(), APICAFile: filepath.Join(dir, "ca.pem"), APICertFile: filepath.Join(dir, "client.pem"), APIKeyFile: filepath.Join(dir, "client-key.pem"), Atespace: "tenant"}
	rotate := func(name string) {
		_, cert, key := issue(name, false)
		write(cfg.APICertFile, cert)
		write(cfg.APIKeyFile, key)
	}
	rotate("client-one")
	return cfg, service, tracking, rotate
}

func TestSubstrateNativeTLSAndCertificateRotation(t *testing.T) {
	cfg, _, connections, rotate := substrateTransportFixture(t)
	api, err := NewSubstrateNativeClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close() //nolint:errcheck
	get := func(want string) {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
		defer cancel()
		for {
			actor, err := api.Control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: &ateapipb.ObjectRef{Atespace: "tenant", Name: "same"}}, grpc.WaitForReady(true))
			// Disconnect can race an RPC already assigned to the old transport.
			// Retry this read only; WaitForReady does not replay in-flight RPCs.
			if status.Code(err) == codes.Unavailable && ctx.Err() == nil {
				time.Sleep(10 * time.Millisecond)
				continue
			}
			if err != nil || actor.GetMetadata().GetUid() != want {
				t.Fatalf("authenticated Actor read: identity=%q err=%v", actor.GetMetadata().GetUid(), err)
			}
			break
		}
	}
	get("client-one")
	rotate("client-two")
	connections.disconnect()
	get("client-two")
	if err := os.WriteFile(cfg.APIKeyFile, []byte("not a private key"), 0o600); err != nil {
		t.Fatal(err)
	}
	connections.disconnect()
	ctx, cancel := context.WithTimeout(t.Context(), 150*time.Millisecond)
	defer cancel()
	if _, err := api.Control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: &ateapipb.ObjectRef{Name: "same", Atespace: "tenant"}}, grpc.WaitForReady(true)); err == nil {
		t.Fatal("invalid rotated key was admitted")
	}
}

func TestSubstrateNativeBearerRotationNamespaceAndPagination(t *testing.T) {
	cfg, server, _, _ := substrateTransportFixture(t)
	cfg.APICertFile, cfg.APIKeyFile = "", ""
	cfg.APIBearerTokenFile = filepath.Join(t.TempDir(), "credential")
	write := func(value string) {
		t.Helper()
		if err := os.WriteFile(cfg.APIBearerTokenFile, []byte(value), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("initial-test-credential")
	api, err := newGRPCSubstrateControlClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer api.Close() //nolint:errcheck
	for _, space := range []string{"tenant", "another"} {
		actor, err := api.GetActor(t.Context(), SubstrateActorKey(space, "same"))
		if err != nil || actor.Atespace != space {
			t.Fatalf("namespace-qualified Actor read failed: %v", err)
		}
	}
	server.bearer.Store("rotated-test-credential")
	if _, err := api.GetActor(t.Context(), "same"); err == nil {
		t.Fatal("stale bearer was admitted")
	}
	write("rotated-test-credential")
	if _, err := api.GetActor(t.Context(), "same"); err != nil {
		t.Fatal("rotated bearer was not loaded for the next RPC")
	}
	native := &SubstrateNativeClient{Control: api.client}
	list, err := native.ListActorTemplates(t.Context(), "tenant")
	if err != nil || len(list) != 1 || list[0].GetMetadata().GetAtespace() != "tenant" {
		t.Fatalf("paginated native templates: %v", err)
	}
	server.repeatPage.Store(true)
	if _, err := native.ListActorTemplates(t.Context(), "tenant"); err == nil {
		t.Fatal("repeated native page token caused incomplete success")
	}
}

func TestSubstrateNativeRejectsAmbiguousControlAuth(t *testing.T) {
	cfg, _, _, _ := substrateTransportFixture(t)
	cfg.APIBearerTokenFile = "unused"
	if _, err := NewSubstrateNativeClient(cfg); err == nil {
		t.Fatal("client accepted simultaneous bearer and mTLS")
	}
	cfg.APICertFile, cfg.APIKeyFile, cfg.APIBearerTokenFile = "", "", ""
	if _, err := NewSubstrateNativeClient(cfg); err == nil {
		t.Fatal("client accepted anonymous native control")
	}
}
