package main

import (
	"bytes"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestGatewayLogsMetadataWithoutApplicationData(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	const requestBody = `{"jsonrpc":"2.0","id":"private-id","method":"tools/call",` +
		`"params":{"name":"k8s_get_resources","arguments":{"password":"unrelated-private-password"}}}`
	const responseBody = `{"jsonrpc":"2.0","id":"private-id","result":{"content":[` +
		`{"type":"text","text":"unrelated-private-result"}],"structuredContent":{"api_key":"other-private-key"}}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(responseBody))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	var output bytes.Buffer
	g := &gateway{
		key: &key.PublicKey, credential: []byte("fixture-resource"), target: target,
		client: upstream.Client(), output: &output,
	}
	req := httptest.NewRequest(http.MethodPost, "http://example.com/mcp", strings.NewReader(requestBody))
	req.Header.Set("Txn-Token", signedToken(t, key, time.Now().Add(time.Minute).Unix()))
	req.Header.Set("Authorization", "Bearer fixture-resource")
	rr := httptest.NewRecorder()
	g.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK || rr.Body.String() != responseBody {
		t.Fatal("gateway must relay the response without projecting application data")
	}
	var record map[string]any
	if err := json.Unmarshal(output.Bytes(), &record); err != nil {
		t.Fatalf("missing structural exchange record: %v", err)
	}
	for _, field := range []string{"request", "response", "task"} {
		if _, exists := record[field]; exists {
			t.Errorf("log must not contain application or caller data field %q", field)
		}
	}
	for _, value := range []string{
		"unrelated-private-password", "unrelated-private-result", "other-private-key", "private-id", "fixture-resource",
	} {
		if strings.Contains(output.String(), value) {
			t.Error("application or credential data reached the log")
		}
	}
	if record["method"] != http.MethodPost || record["rpc"] != "tools/call" || record["status"] != float64(http.StatusOK) {
		t.Fatal("log must retain protocol method and HTTP outcome")
	}
}
