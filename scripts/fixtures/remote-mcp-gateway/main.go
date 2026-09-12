// Command remote-mcp-gateway is a LOCAL PROOF FIXTURE, not a production gateway.
// It authenticates a narrowly issued transaction and resource credential, then
// relays MCP unchanged to one fixed upstream. It never implements an MCP tool.
package main

import (
	"bytes"
	"crypto"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

type gateway struct {
	key        *rsa.PublicKey
	credential []byte
	target     *url.URL
	client     *http.Client
	output     io.Writer
	mu         sync.Mutex
}

type claims struct {
	Issuer      string            `json:"iss"`
	Audience    string            `json:"aud"`
	Subject     string            `json:"sub"`
	Workload    string            `json:"req_wl"`
	Transaction string            `json:"txn"`
	Expiry      int64             `json:"exp"`
	Issued      int64             `json:"iat"`
	Scope       string            `json:"scope"`
	Context     map[string]string `json:"tctx"`
}

func (g *gateway) verify(token string) (*claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("invalid authority")
	}
	header, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var h struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}
	if json.Unmarshal(header, &h) != nil || h.Algorithm != "RS256" || h.Type != "txntoken+jwt" {
		return nil, errors.New("invalid authority")
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err = rsa.VerifyPKCS1v15(g.key, crypto.SHA256, digest[:], signature); err != nil {
		return nil, err
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var c claims
	if json.Unmarshal(payload, &c) != nil ||
		c.Issuer != "remote-mcp-proof" || c.Audience != "remote-mcp-proof" ||
		c.Subject != "proof-reader" || c.Workload != "native-ai-worker" || c.Transaction == "" ||
		c.Expiry <= time.Now().Unix() || c.Issued > time.Now().Unix()+30 ||
		c.Context["namespace"] != "orka-system" || c.Context["agent"] != "remote-reader" ||
		c.Context["tool"] != "remote-read" || c.Context["task"] == "" ||
		!strings.Contains(" "+c.Scope+" ", " orka:tools:use ") {
		return nil, errors.New("invalid authority")
	}
	return &c, nil
}

func (g *gateway) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/healthz" && r.Method == http.MethodGet {
		w.WriteHeader(http.StatusOK)
		return
	}
	_, err := g.verify(r.Header.Get("Txn-Token"))
	if err != nil || len(r.Header.Values("Txn-Token")) != 1 || len(r.Header.Values("Authorization")) != 1 ||
		subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), append([]byte("Bearer "), g.credential...)) != 1 ||
		r.Host != "example.com" || r.URL.RequestURI() != "/mcp" ||
		(r.Method != http.MethodPost && r.Method != http.MethodDelete) {
		http.Error(w, "fixture access denied", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 4<<20))
	if err != nil {
		http.Error(w, "invalid body", 400)
		return
	}
	var request struct {
		Method string `json:"method"`
		Params struct {
			Name string `json:"name"`
		} `json:"params"`
	}
	if len(body) > 0 {
		if json.Unmarshal(body, &request) != nil {
			http.Error(w, "invalid request", 400)
			return
		}
		switch request.Method {
		case "initialize", "notifications/initialized", "tools/list":
		case "tools/call":
			if request.Params.Name != "k8s_get_resources" {
				http.Error(w, "tool denied", http.StatusForbidden)
				return
			}
		default:
			http.Error(w, "method denied", http.StatusForbidden)
			return
		}
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, g.target.String()+"/mcp", bytes.NewReader(body))
	if err != nil {
		http.Error(w, "invalid upstream", http.StatusBadGateway)
		return
	}
	req.Header = r.Header.Clone()
	req.Header.Del("Txn-Token")
	req.Header.Del("Connection")
	req.GetBody = nil
	resp, err := g.client.Do(req)
	if err != nil {
		http.Error(w, "upstream failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close() //nolint:errcheck
	result, err := io.ReadAll(io.LimitReader(resp.Body, (4<<20)+1))
	if err != nil || len(result) > 4<<20 {
		http.Error(w, "upstream response too large", http.StatusBadGateway)
		return
	}
	// Only bounded protocol categories and outcomes belong in proof logs. Neither
	// application bodies nor arbitrary caller claims are safe, even in a fixture.
	g.mu.Lock()
	_ = json.NewEncoder(g.output).Encode(map[string]any{
		"event": "exchange", "method": r.Method, "rpc": request.Method,
		"status": resp.StatusCode, "authenticated": true,
		"route": "example.com -> team-mcp/kagent-tools", "transactionStripped": true,
	})
	g.mu.Unlock()
	for _, header := range []string{"Content-Type", "Mcp-Session-Id", "Mcp-Protocol-Version"} {
		if value := resp.Header.Get(header); value != "" {
			w.Header().Set(header, value)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(result)
}

func main() {
	publicPEM, err := os.ReadFile("/credentials/public.pem")
	if err != nil {
		log.Fatal("read public key failed")
	}
	block, _ := pem.Decode(publicPEM)
	if block == nil {
		log.Fatal("invalid public key")
	}
	public, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		log.Fatal("invalid public key")
	}
	key, ok := public.(*rsa.PublicKey)
	if !ok {
		log.Fatal("RSA key required")
	}
	credential, err := os.ReadFile("/credentials/token")
	if err != nil || len(credential) == 0 {
		log.Fatal("read credential failed")
	}
	target, err := url.Parse(os.Getenv("MCP_UPSTREAM"))
	if err != nil || target == nil || target.Scheme != "http" || target.Host == "" ||
		target.User != nil || target.Path != "" || target.RawQuery != "" {
		log.Fatal("invalid fixed upstream")
	}
	g := &gateway{
		key: key, credential: credential, target: target, output: os.Stdout,
		client: &http.Client{
			Timeout: 45 * time.Second, Transport: &http.Transport{Proxy: nil},
			CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") },
		},
	}
	server := &http.Server{
		Addr: ":8080", Handler: g, ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout: 50 * time.Second, WriteTimeout: 50 * time.Second,
		IdleTimeout: 10 * time.Second, MaxHeaderBytes: 16 << 10,
	}
	log.Fatal(server.ListenAndServe())
}
