package runtimefeedback

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

const (
	testRoutingNodeA = "node-a"
	testRoutingNodeB = "node-b"
)

type routingTestTransport struct {
	transport http.RoundTripper
	calls     atomic.Int32
}

func (t *routingTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	t.calls.Add(1)
	return t.transport.RoundTrip(request)
}

func TestClientRoutesEntireLifecycleToExactNodeWithMutualTLS(t *testing.T) {
	config, roots := testClientCertificate(t)
	var counts [2][3]atomic.Int32
	endpoints := make(map[string]string)
	serverCAs := make([]byte, 0, 4096)
	for index, node := range []string{testRoutingNodeA, testRoutingNodeB} {
		server := newRoutingTestServer(t, node, roots, &counts[index])
		endpoints[node] = server.URL
		serverCAs = append(serverCAs, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})...)
	}
	if err := os.WriteFile(config.CAFile, serverCAs, 0600); err != nil {
		t.Fatal(err)
	}
	config.NodeURLsFile = filepath.Join(t.TempDir(), "node-urls.json")
	writeTestNodeURLs(t, config.NodeURLsFile, endpoints)
	client, err := NewClient(config)
	if err != nil {
		t.Fatal(err)
	}
	transport := &routingTestTransport{transport: client.http.Transport}
	client.http.Transport = transport
	for _, node := range []string{"", "new-autoscaled-node", "NODE-A"} {
		query := testQuery()
		query.Workload.Node = node
		if err := client.Register(context.Background(), query); err == nil {
			t.Fatal("unmapped registration accepted")
		}
		if _, err := client.Report(context.Background(), query); err == nil {
			t.Fatal("unmapped report accepted")
		}
		if err := client.Complete(context.Background(), query, Completed); err == nil {
			t.Fatal("unmapped completion accepted")
		}
	}
	if transport.calls.Load() != 0 {
		t.Fatal("unmapped node reached transport")
	}
	for _, node := range []string{testRoutingNodeA, testRoutingNodeB} {
		query := testQuery()
		query.Workload.Node = node
		if err := client.Register(context.Background(), query); err != nil {
			t.Fatal(err)
		}
	}
	// Operator file changes cannot reroute the remaining steps of a live capture.
	writeTestNodeURLs(t, config.NodeURLsFile, map[string]string{testRoutingNodeA: endpoints[testRoutingNodeB], testRoutingNodeB: endpoints[testRoutingNodeA]})
	for _, node := range []string{testRoutingNodeA, testRoutingNodeB} {
		query := testQuery()
		query.Workload.Node = node
		if report, err := client.Report(context.Background(), query); err != nil || report.Workload != query.Workload {
			t.Fatalf("report lost node binding: %v", err)
		}
		if err := client.Complete(context.Background(), query, Completed); err != nil {
			t.Fatal(err)
		}
	}
	if transport.calls.Load() != 6 {
		t.Fatal("unexpected retry, fallback, or redirect")
	}
	for index := range counts {
		for operation := range counts[index] {
			if counts[index][operation].Load() != 1 {
				t.Fatalf("node %d operation %d was not routed exactly once", index, operation)
			}
		}
	}
}

func newRoutingTestServer(t *testing.T, node string, roots *x509.CertPool, counts *[3]atomic.Int32) *httptest.Server {
	t.Helper()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.TLS == nil || r.TLS.Version != tls.VersionTLS13 || len(r.TLS.PeerCertificates) != 1 || len(r.TLS.PeerCertificates[0].URIs) != 1 || r.TLS.PeerCertificates[0].URIs[0].String() != "spiffe://orka.ai/controller" {
			t.Error("node backend did not receive trusted TLS 1.3 controller identity")
		}
		var request struct {
			Query
			DurationSeconds int    `json:"durationSeconds"`
			Reason          string `json:"reason"`
		}
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil || request.Workload.Node != node || r.Method != http.MethodPost || r.Header.Get("Authorization") != "" {
			t.Error("lifecycle request reached the wrong node or lost its wire contract")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		report := testReport(request.Query)
		switch r.URL.Path {
		case apiPath + "runs":
			counts[0].Add(1)
			if request.DurationSeconds != CaptureSeconds || request.Reason != "" {
				t.Error("registration changed bounded capture request")
			}
		case apiPath + "report":
			counts[1].Add(1)
			if request.DurationSeconds != 0 || request.Reason != "" {
				t.Error("read request included capture control")
			}
		case apiPath + "complete":
			counts[2].Add(1)
			if request.DurationSeconds != 0 || request.Reason != Completed {
				t.Error("completion changed exact reason")
			}
			report.Status = Finalized
			report.Capture.EndedAt = &report.SampledAt
		default:
			t.Error("unexpected lifecycle endpoint")
			w.WriteHeader(http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(report)
	}))
	server.TLS = &tls.Config{MinVersion: tls.VersionTLS13, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}
	server.StartTLS()
	t.Cleanup(server.Close)
	return server
}

func writeTestNodeURLs(t *testing.T, path string, endpoints map[string]string) {
	t.Helper()
	data, err := json.Marshal(endpoints)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestClientRejectsAmbiguousMalformedOrUnboundedNodeRouting(t *testing.T) {
	config, _ := testClientCertificate(t)
	config.NodeURLsFile = filepath.Join(t.TempDir(), "node-urls.json")
	writeTestNodeURLs(t, config.NodeURLsFile, map[string]string{testRoutingNodeA: "https://gkr.invalid"})
	config.URL = "https://gkr.invalid"
	if _, err := NewClient(config); err == nil {
		t.Fatal("ambiguous fixed and per-node routing accepted")
	}
	config.URL = ""
	if _, err := NewClient(config); err != nil {
		t.Fatalf("valid mapping rejected: %v", err)
	}
	for _, data := range []string{
		``, `null`, `[]`, `{}`, `{"node-a":`, `{"node-a":123}`, `{"node-a":null}`,
		`{"node-a":"https://gkr.invalid"} {}`, `{"node-a":"https://gkr.invalid","node-a":"https://other.invalid"}`,
		`{"":"https://gkr.invalid"}`, `{"Node-A":"https://gkr.invalid"}`, `{"node/a":"https://gkr.invalid"}`,
	} {
		if err := os.WriteFile(config.NodeURLsFile, []byte(data), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := NewClient(config); err == nil {
			t.Fatal("malformed or unbounded mapping accepted")
		}
	}
	for _, origin := range []string{"http://gkr.invalid", "https://gkr.invalid/report", "https://user:pass@gkr.invalid", "https://gkr.invalid?key=value", "https://gkr.invalid?", "https://gkr.invalid#fragment", "https://gkr.invalid/%2F"} {
		writeTestNodeURLs(t, config.NodeURLsFile, map[string]string{testRoutingNodeA: "https://valid.invalid", testRoutingNodeB: origin})
		if _, err := NewClient(config); err == nil {
			t.Fatalf("invalid origin accepted: %q", origin)
		}
	}
	const mapping = `{"node-a":"https://gkr.invalid"}`
	bounded := strings.Repeat(" ", MaxNodeURLsFileBytes-len(mapping)) + mapping
	if err := os.WriteFile(config.NodeURLsFile, []byte(bounded), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(config); err != nil {
		t.Fatalf("mapping exactly at the file limit rejected: %v", err)
	}
	if err := os.WriteFile(config.NodeURLsFile, []byte(bounded+" "), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(config); err == nil {
		t.Fatal("valid JSON exceeding the file limit accepted")
	}
	endpoints := make(map[string]string)
	for index := range MaxNodeURLs {
		endpoints[fmt.Sprintf("node-%d", index)] = "https://gkr.invalid"
	}
	writeTestNodeURLs(t, config.NodeURLsFile, endpoints)
	if _, err := NewClient(config); err != nil {
		t.Fatalf("bounded entry count rejected: %v", err)
	}
	endpoints["one-too-many"] = "https://gkr.invalid"
	writeTestNodeURLs(t, config.NodeURLsFile, endpoints)
	if _, err := NewClient(config); err == nil {
		t.Fatal("excess entries accepted")
	}
	if err := os.Remove(config.NodeURLsFile); err != nil {
		t.Fatal(err)
	}
	if _, err := NewClient(config); err == nil {
		t.Fatal("missing mapping accepted")
	}
	config.NodeURLsFile = ""
	if _, err := NewClient(config); err == nil {
		t.Fatal("absent routing accepted")
	}
}
