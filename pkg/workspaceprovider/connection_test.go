package workspaceprovider

import (
	"bytes"
	"crypto/x509"
	"encoding/pem"
	"net/http/httptest"
	"testing"
)

func TestConnectionDataRoundTrip(t *testing.T) {
	server := httptest.NewTLSServer(nil)
	defer server.Close()
	certificate, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse test certificate: %v", err)
	}
	caData := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certificate.Raw})
	original := ConnectionData{
		Endpoint:    server.URL,
		CAData:      caData,
		HostHeader:  "workspace-agent.internal",
		ControlAuth: "example-control-value",
	}
	encoded, err := EncodeConnectionData(original)
	if err != nil {
		t.Fatalf("EncodeConnectionData: %v", err)
	}
	decoded, err := ParseConnectionData(encoded)
	if err != nil {
		t.Fatalf("ParseConnectionData: %v", err)
	}
	if decoded.Endpoint != original.Endpoint || decoded.HostHeader != original.HostHeader ||
		decoded.ControlAuth != original.ControlAuth || !bytes.Equal(decoded.CAData, original.CAData) {
		t.Fatalf("decoded connection data = %#v", decoded)
	}
	config := decoded.ClientConfig()
	if config.Endpoint != original.Endpoint || config.ControlAuth != original.ControlAuth {
		t.Fatalf("client config = %#v", config)
	}
}

func TestConnectionDataRejectsMissingVersionOrControlAuth(t *testing.T) {
	values := map[string][]byte{
		ConnectionDataEndpointKey: []byte("https://workspace-agent.example"),
	}
	if _, err := ParseConnectionData(values); err == nil {
		t.Fatal("missing protocol version passed")
	}
	values[ConnectionDataVersionKey] = []byte(ConnectionDataVersion)
	if _, err := ParseConnectionData(values); err == nil {
		t.Fatal("missing control auth passed")
	}
}
