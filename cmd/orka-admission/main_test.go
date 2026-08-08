package main

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOptionsValidate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		mutate  func(*options)
		wantErr bool
	}{
		{name: "valid"},
		{name: "certificate directory required", mutate: func(o *options) { o.webhookCertPath = "" }, wantErr: true},
		{name: "service DNS name required", mutate: func(o *options) { o.webhookServiceDNSName = "" }, wantErr: true},
		{name: "controller identity required", mutate: func(o *options) { o.controllerUsernames = " , " }, wantErr: true},
		{
			name:   "multiple controller identities",
			mutate: func(o *options) { o.controllerUsernames = "controller-v1,controller-v2" },
		},
		{name: "certificate filename only", mutate: func(o *options) { o.webhookCertName = "../tls.crt" }, wantErr: true},
		{name: "key filename only", mutate: func(o *options) { o.webhookCertKey = "/tls.key" }, wantErr: true},
		{name: "port lower bound", mutate: func(o *options) { o.webhookPort = 0 }, wantErr: true},
		{name: "port upper bound", mutate: func(o *options) { o.webhookPort = 65536 }, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			opts := options{
				webhookCertPath:       "/certs",
				webhookCertName:       "tls.crt",
				webhookCertKey:        "tls.key",
				webhookServiceDNSName: "orka-admission.orka-system.svc",
				webhookPort:           9443,
				controllerUsernames:   "system:serviceaccount:orka-system:orka-controller-manager",
			}
			if tt.mutate != nil {
				tt.mutate(&opts)
			}
			if err := opts.validate(); (err != nil) != tt.wantErr {
				t.Fatalf("validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestServingCertificateFilesChecker(t *testing.T) {
	t.Parallel()
	now := time.Now()
	valid := newServingCertificateFixture(t, now.Add(-time.Hour), now.Add(time.Hour))
	other := newServingCertificateFixture(t, now.Add(-time.Hour), now.Add(time.Hour))
	future := newServingCertificateFixture(t, now.Add(time.Hour), now.Add(2*time.Hour))
	expired := newServingCertificateFixture(t, now.Add(-2*time.Hour), now.Add(-time.Hour))
	wrongDNS := newServingCertificateFixture(t, now.Add(-time.Hour), now.Add(time.Hour), "other.orka-system.svc")

	tests := []struct {
		name            string
		certificatePEM  []byte
		keyPEM          []byte
		wantErrContains string
	}{
		{name: "missing files", wantErrContains: "unavailable"},
		{name: "certificate only", certificatePEM: valid.certificatePEM, wantErrContains: "tls.key is unavailable"},
		{
			name:            "empty certificate",
			certificatePEM:  []byte{},
			keyPEM:          valid.keyPEM,
			wantErrContains: "not a nonempty regular file",
		},
		{
			name:            "malformed certificate",
			certificatePEM:  []byte("certificate"),
			keyPEM:          valid.keyPEM,
			wantErrContains: "load webhook serving certificate",
		},
		{
			name:            "mismatched private key",
			certificatePEM:  valid.certificatePEM,
			keyPEM:          other.keyPEM,
			wantErrContains: "private key does not match",
		},
		{
			name:            "not yet valid",
			certificatePEM:  future.certificatePEM,
			keyPEM:          future.keyPEM,
			wantErrContains: "is not valid before",
		},
		{name: "expired", certificatePEM: expired.certificatePEM, keyPEM: expired.keyPEM, wantErrContains: "expired at"},
		{
			name:            "wrong service DNS name",
			certificatePEM:  wrongDNS.certificatePEM,
			keyPEM:          wrongDNS.keyPEM,
			wantErrContains: "is not valid for orka-admission.orka-system.svc",
		},
		{
			name:            "expired chain certificate",
			certificatePEM:  append(append([]byte{}, valid.certificatePEM...), expired.certificatePEM...),
			keyPEM:          valid.keyPEM,
			wantErrContains: "chain entry 1 expired at",
		},
		{name: "valid", certificatePEM: valid.certificatePEM, keyPEM: valid.keyPEM},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			directory := t.TempDir()
			if tt.certificatePEM != nil {
				if err := os.WriteFile(filepath.Join(directory, "tls.crt"), tt.certificatePEM, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if tt.keyPEM != nil {
				if err := os.WriteFile(filepath.Join(directory, "tls.key"), tt.keyPEM, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			checker := servingCertificateFilesChecker(
				directory, "tls.crt", "tls.key", "orka-admission.orka-system.svc",
			)
			err := checker(httptest.NewRequest("GET", "/readyz", nil))
			if tt.wantErrContains == "" {
				if err != nil {
					t.Fatalf("certificate readiness failed: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErrContains) {
				t.Fatalf("certificate readiness error = %v, want error containing %q", err, tt.wantErrContains)
			}
		})
	}
}

type servingCertificateFixture struct {
	certificatePEM []byte
	keyPEM         []byte
}

func newServingCertificateFixture(
	t *testing.T,
	notBefore, notAfter time.Time,
	dnsNames ...string,
) servingCertificateFixture {
	t.Helper()
	dnsName := "orka-admission.orka-system.svc"
	if len(dnsNames) > 0 {
		dnsName = dnsNames[0]
	}
	privateKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: dnsName},
		DNSNames:     []string{dnsName},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	certificateDER, err := x509.CreateCertificate(rand.Reader, template, template, &privateKey.PublicKey, privateKey)
	if err != nil {
		t.Fatal(err)
	}
	return servingCertificateFixture{
		certificatePEM: pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificateDER}),
		keyPEM: pem.EncodeToMemory(&pem.Block{
			Type:  "RSA PRIVATE KEY",
			Bytes: x509.MarshalPKCS1PrivateKey(privateKey),
		}),
	}
}

func TestSplitCommaList(t *testing.T) {
	t.Parallel()
	got := splitCommaList(" one, ,two , three")
	want := []string{"one", "two", "three"}
	if len(got) != len(want) {
		t.Fatalf("splitCommaList() = %#v, want %#v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("splitCommaList() = %#v, want %#v", got, want)
		}
	}
}
