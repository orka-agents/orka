package main

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func signedToken(t *testing.T, key *rsa.PrivateKey, expiry int64) string {
	t.Helper()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"txntoken+jwt"}`))
	payload, err := json.Marshal(map[string]any{
		"iss": "remote-mcp-proof", "aud": "remote-mcp-proof", "sub": "proof-reader",
		"req_wl": "native-ai-worker", "txn": "task-proof", "iat": time.Now().Unix(),
		"exp": expiry, "scope": "orka:tools:use",
		"tctx": map[string]string{
			"namespace": "orka-system", "agent": "remote-reader", "tool": "remote-read", "task": "proof-task",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	unsigned := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	digest := sha256.Sum256([]byte(unsigned))
	signature, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return unsigned + "." + base64.RawURLEncoding.EncodeToString(signature)
}

func TestGatewayAuthenticatesAndRoutesWithoutProtocolTranslation(t *testing.T) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.Header.Get("Txn-Token") != "" {
			t.Error("transaction authority leaked upstream")
		}
		if r.Header.Get("Authorization") != "Bearer fixture-resource" {
			t.Error("resource credential missing")
		}
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_get_resources"}}` {
			t.Errorf("body changed: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"real-output"}]}}`))
	}))
	defer upstream.Close()
	target, _ := url.Parse(upstream.URL)
	g := &gateway{
		key: &key.PublicKey, credential: []byte("fixture-resource"), target: target,
		client: upstream.Client(), output: io.Discard,
	}
	token := signedToken(t, key, time.Now().Add(time.Minute).Unix())
	for _, tc := range []struct {
		name, txn, auth, host string
		want                  int
	}{
		{"missing identity", "", "Bearer fixture-resource", "example.com", 403},
		{"missing resource", token, "", "example.com", 403},
		{"tampered identity", token + "x", "Bearer fixture-resource", "example.com", 403},
		{"expired", signedToken(t, key, time.Now().Add(-time.Minute).Unix()), "Bearer fixture-resource", "example.com", 403},
		{"wrong route", token, "Bearer fixture-resource", "other.example", 403},
		{"authorized", token, "Bearer fixture-resource", "example.com", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"k8s_get_resources"}}`
			req := httptest.NewRequest(http.MethodPost, "http://example.com/mcp", strings.NewReader(body))
			req.Host = tc.host
			req.Header.Set("Txn-Token", tc.txn)
			req.Header.Set("Authorization", tc.auth)
			rr := httptest.NewRecorder()
			g.ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status %d, want %d", rr.Code, tc.want)
			}
		})
	}
	if calls != 1 {
		t.Fatalf("upstream calls=%d want 1", calls)
	}
}
