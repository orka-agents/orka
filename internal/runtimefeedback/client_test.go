package runtimefeedback

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func testQuery() Query {
	return Query{RunID: strings.Repeat("d", 64), Workload: Workload{Namespace: "workers", PodName: "runtime", PodUID: "pod-uid", ContainerName: "runtime", ContainerID: "containerd://" + strings.Repeat("a", 64), Node: "node"}}
}

func testReport(query Query) Report {
	now := time.Now().UTC()
	return Report{APIVersion: APIVersion, Kind: ReportKind, RunID: query.RunID, Workload: query.Workload, Status: "Collecting", SampledAt: now, Capture: &Capture{StartedAt: now.Add(-time.Minute), ExpiresAt: now.Add(9 * time.Minute)}, Source: Source{Name: "gkr-runtime-observer", InstanceID: "boot"}, AttributionScope: "Container", Completeness: "Partial", Events: []Event{{Timestamp: now.Add(-time.Second), DestinationAddress: "203.0.113.4", DestinationPort: 443, Decision: "deny", KernelEnforced: true}}}
}

func TestReportRejectsStaleCrossExecutionOrUnboundedEvidence(t *testing.T) {
	q := testQuery()
	for _, test := range []struct {
		name   string
		mutate func(*Report)
	}{
		{"different run", func(r *Report) { r.RunID = "other" }},
		{"different container", func(r *Report) { r.Workload.ContainerID = "containerd://" + strings.Repeat("b", 64) }},
		{"restarted container", func(r *Report) { r.Workload.RestartCount++ }},
		{"stale sample", func(r *Report) { r.SampledAt = time.Now().Add(-time.Minute) }},
		{"future sample", func(r *Report) { r.SampledAt = time.Now().Add(time.Minute) }},
		{"missing capture", func(r *Report) { r.Capture = nil }},
		{"too many events", func(r *Report) { r.Events = make([]Event, MaxEvents+1) }},
		{"before capture", func(r *Report) { r.Events[0].Timestamp = r.Capture.StartedAt.Add(-time.Second) }},
		{"after sample", func(r *Report) { r.Events[0].Timestamp = r.SampledAt.Add(time.Second) }},
		{"unavailable with events", func(r *Report) { r.Status = "Unavailable" }},
		{"complete claim", func(r *Report) { r.Completeness = "Complete" }},
		{"tool attribution", func(r *Report) { r.AttributionScope = "Tool" }},
		{"path in address", func(r *Report) { r.Events[0].DestinationAddress = "https://secret.invalid/path?token=not-a-real-token" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := testReport(q)
			test.mutate(&r)
			if err := r.Validate(q, time.Now()); err == nil {
				t.Fatal("invalid evidence accepted")
			}
		})
	}
	if err := testReport(q).Validate(q, time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestReportRetainsBoundedHistoricalCapture(t *testing.T) {
	q := testQuery()
	now := time.Now().UTC()
	for _, status := range []string{Finalized, Expired} {
		t.Run(status, func(t *testing.T) {
			r := testReport(q)
			r.Status = status
			r.SampledAt = now.Add(-5 * time.Minute)
			endedAt := r.SampledAt
			r.Capture = &Capture{StartedAt: endedAt.Add(-10 * time.Minute), EndedAt: &endedAt, ExpiresAt: endedAt}
			r.Events[0].Timestamp = endedAt.Add(-time.Second)
			if err := r.Validate(q, now); err != nil {
				t.Fatalf("bounded historical capture rejected: %v", err)
			}
			r.Events[0].Timestamp = endedAt.Add(time.Second)
			if err := r.Validate(q, now); err == nil {
				t.Fatal("event after historical capture accepted")
			}
			r.Events[0].Timestamp = endedAt.Add(-time.Second)
			if err := r.Validate(q, now.Add(20*time.Minute)); err == nil {
				t.Fatal("capture beyond retention bound accepted")
			}
		})
	}
}

func TestClientUsesMutualTLSAndBoundedExactRead(t *testing.T) {
	q := testQuery()
	var mode atomic.Int32
	var requests atomic.Int32
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.TLS == nil || len(r.TLS.PeerCertificates) != 1 || len(r.TLS.PeerCertificates[0].URIs) != 1 || r.TLS.PeerCertificates[0].URIs[0].String() != "spiffe://orka.ai/controller" {
			t.Error("missing trusted controller mTLS identity")
		}
		if r.Method != http.MethodPost || r.URL.Path != apiPath+"report" || r.Header.Get("Authorization") != "" {
			t.Error("unexpected report transport")
		}
		var got Query
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil || got != q {
			t.Error("request lost execution binding")
		}
		w.Header().Set("Content-Type", "application/json")
		switch mode.Load() {
		case 1:
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte("secret upstream response must not be exposed"))
		case 2:
			_, _ = w.Write([]byte(strings.Repeat("x", MaxResponseBytes+1)))
		case 3:
			w.Header().Set("Location", "https://untrusted.invalid")
			w.WriteHeader(http.StatusTemporaryRedirect)
		case 4:
			_ = json.NewEncoder(w).Encode(testReport(q))
			_, _ = w.Write([]byte(`{}`))
		default:
			_ = json.NewEncoder(w).Encode(testReport(q))
		}
	}))
	config, roots := testClientCertificate(t)
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	server.StartTLS()
	defer server.Close()
	config.URL = server.URL
	serverCA := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	if err := os.WriteFile(config.CAFile, serverCA, 0600); err != nil {
		t.Fatal(err)
	}
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Report(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	for i := int32(1); i <= 4; i++ {
		mode.Store(i)
		_, err := client.Report(context.Background(), q)
		if err == nil || strings.Contains(err.Error(), "secret upstream") || strings.Contains(err.Error(), "untrusted.invalid") {
			t.Fatalf("unsafe response handling for mode %d: %v", i, err)
		}
	}
	if requests.Load() != 5 {
		t.Fatal("unexpected implicit retry or redirect")
	}
}

func testClientCertificate(t *testing.T) (Config, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	uri, _ := url.Parse("spiffe://orka.ai/controller")
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-controller"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, BasicConstraintsValid: true, IsCA: true, URIs: []*url.URL{uri}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	config := Config{CAFile: filepath.Join(dir, "ca.pem"), CertFile: filepath.Join(dir, "client.pem"), KeyFile: filepath.Join(dir, "client.key")}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
	for path, data := range map[string][]byte{config.CAFile: certPEM, config.CertFile: certPEM, config.KeyFile: keyPEM} {
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(certPEM)
	return config, roots
}

func TestClientRequiresConfiguredHTTPSOrigin(t *testing.T) {
	config, _ := testClientCertificate(t)
	config.URL = "https://gkr.invalid"
	if _, err := NewClient(config); err != nil {
		t.Fatalf("valid fixed origin rejected: %v", err)
	}
	for _, endpoint := range []string{"http://gkr.invalid", "https://gkr.invalid/report", "https://user:pass@gkr.invalid", "https://gkr.invalid?key=value", "https://gkr.invalid#fragment"} {
		config.URL = endpoint
		if _, err := NewClient(config); err == nil {
			t.Fatalf("unsafe endpoint accepted: %q", endpoint)
		}
	}
}
