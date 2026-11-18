/* Copyright (c) 2026. MIT License - see LICENSE file for details. */

package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

const ahpTestToken = "orka_ahp_fake-test-token"

type ahpTestServer struct {
	*httptest.Server
	mu      sync.Mutex
	minted  map[string]string
	deleted []string
	tokens  []map[string]any
}

func newAHPTestServer(t *testing.T) *ahpTestServer {
	t.Helper()
	s := &ahpTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if r.Header.Get("Authorization") != "Bearer person-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/ahp-tokens":
			if err := json.NewDecoder(r.Body).Decode(&s.minted); err != nil {
				t.Errorf("decode mint body: %v", err)
			}
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
				"id": "ahpt_0123456789abcdef", "token": ahpTestToken, "description": s.minted["description"],
				"namespace": "team", "expiresAt": time.Now().Add(720 * time.Hour).UTC(),
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/ahp-tokens":
			json.NewEncoder(w).Encode(map[string]any{"items": s.tokens}) //nolint:errcheck
		case r.Method == http.MethodDelete && strings.HasPrefix(r.URL.Path, "/api/v1/ahp-tokens/"):
			id := strings.TrimPrefix(r.URL.Path, "/api/v1/ahp-tokens/")
			if id == "ahpt_missing" {
				w.WriteHeader(http.StatusNotFound)
				json.NewEncoder(w).Encode(map[string]string{"error": "AHP token not found"}) //nolint:errcheck
				return
			}
			s.deleted = append(s.deleted, id)
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(s.Close)
	return s
}

func runAHPCommand(t *testing.T, args ...string) (string, string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	root := newRootCmd()
	var out, errOut bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errOut)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), errOut.String(), err
}

func TestAHPConnectPrintsOnlyTheURLOnStandardOutput(t *testing.T) {
	srv := newAHPTestServer(t)
	out, errOut, err := runAHPCommand(t, "ahp", "connect", "--server", srv.URL, "--token", "person-token",
		"--name", "work laptop", "--ttl", "2160h", "--url", "https://orka.example.com/orka/")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, errOut)
	}
	if srv.minted["description"] != "work laptop" || srv.minted["ttl"] != "2160h" {
		t.Fatalf("mint body = %v", srv.minted)
	}
	want := "wss://orka.example.com/orka/ahp?tkn=" + ahpTestToken + "\n"
	if out != want {
		t.Fatalf("stdout = %q, want only %q", out, want)
	}
	if strings.Contains(errOut, ahpTestToken) {
		t.Fatal("the instructions must not repeat the token")
	}
	for _, want := range []string{"ahpt_0123456789abcdef (work laptop)", "remote agent host", "orka ahp tokens revoke ahpt_0123456789abcdef"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("instructions are missing %q:\n%s", want, errOut)
		}
	}
	if strings.Contains(errOut, "port-forward") {
		t.Errorf("a public URL needs no loopback warning:\n%s", errOut)
	}
}

func TestAHPConnectDefaultsToTheServerAndWarnsForLoopback(t *testing.T) {
	srv := newAHPTestServer(t)
	out, errOut, err := runAHPCommand(t, "ahp", "connect", "--server", srv.URL, "--token", "person-token")
	if err != nil {
		t.Fatalf("connect: %v\n%s", err, errOut)
	}
	server, _ := url.Parse(srv.URL)
	if want := "ws://" + server.Host + "/ahp?tkn=" + ahpTestToken + "\n"; out != want {
		t.Fatalf("stdout = %q, want %q", out, want)
	}
	if srv.minted["description"] != defaultAHPDeviceName() {
		t.Fatalf("description = %q, want the host name %q", srv.minted["description"], defaultAHPDeviceName())
	}
	if !strings.Contains(errOut, "port-forward") {
		t.Fatalf("a loopback URL must say it works only on this machine:\n%s", errOut)
	}
}

func TestAHPConnectRefusesPlaintextURLsForOtherHosts(t *testing.T) {
	srv := newAHPTestServer(t)
	_, _, err := runAHPCommand(t, "ahp", "connect", "--server", srv.URL, "--token", "person-token",
		"--url", "http://orka.example.com")
	if err == nil || !strings.Contains(err.Error(), "plaintext ws://") {
		t.Fatalf("connect error = %v, want a plaintext refusal", err)
	}
	if srv.minted != nil {
		t.Fatal("no token may be minted for a URL that is refused")
	}
	out, _, err := runAHPCommand(t, "ahp", "connect", "--server", srv.URL, "--token", "person-token",
		"--url", "http://orka.example.com", "--insecure")
	if err != nil || !strings.HasPrefix(out, "ws://orka.example.com/ahp?tkn=") {
		t.Fatalf("connect --insecure = %q, %v", out, err)
	}
}

func TestAHPConnectStructuredOutput(t *testing.T) {
	srv := newAHPTestServer(t)
	out, _, err := runAHPCommand(t, "ahp", "connect", "--server", srv.URL, "--token", "person-token",
		"--name", "desk", "--url", "https://orka.example.com", "-o", "json")
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	var result ahpConnectResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	if result.ID != "ahpt_0123456789abcdef" || result.Name != "desk" || result.Namespace != "team" ||
		result.URL != "wss://orka.example.com/ahp?tkn="+ahpTestToken || result.ExpiresAt.IsZero() {
		t.Fatalf("result = %+v", result)
	}
}

func TestAHPTokensListShowsActiveTokensWithoutSecrets(t *testing.T) {
	srv := newAHPTestServer(t)
	now := time.Now().UTC()
	srv.tokens = []map[string]any{
		{"id": "ahpt_active", "description": "laptop", "namespace": "team", "createdAt": now.Add(-time.Hour), "expiresAt": now.Add(time.Hour)},
		{"id": "ahpt_expired", "description": "old", "namespace": "team", "createdAt": now.Add(-48 * time.Hour), "expiresAt": now.Add(-time.Hour)},
		{"id": "ahpt_revoked", "description": "lost phone", "namespace": "team", "createdAt": now.Add(-time.Hour), "expiresAt": now.Add(time.Hour), "revokedAt": now},
		{"id": "ahpt_desktop", "namespace": "team", "createdAt": now, "expiresAt": now.Add(time.Hour), "token": "orka_ahp_never-printed"},
	}
	out, _, err := runAHPCommand(t, "ahp", "tokens", "list", "--server", srv.URL, "--token", "person-token")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if !strings.Contains(out, "ahpt_active") || !strings.Contains(out, "laptop") || !strings.Contains(out, "ahpt_desktop") ||
		strings.Contains(out, "ahpt_expired") || strings.Contains(out, "ahpt_revoked") || strings.Contains(out, "never-printed") {
		t.Fatalf("default listing:\n%s", out)
	}

	out, _, err = runAHPCommand(t, "ahp", "tokens", "list", "--all", "-o", "json", "--server", srv.URL, "--token", "person-token")
	if err != nil {
		t.Fatalf("list --all: %v", err)
	}
	var list struct {
		Items []ahpTokenListItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(out), &list); err != nil {
		t.Fatalf("decode %q: %v", out, err)
	}
	statuses := map[string]string{}
	for _, item := range list.Items {
		statuses[item.ID] = item.Status
	}
	want := map[string]string{"ahpt_active": ahpStatusActive, "ahpt_expired": ahpStatusExpired, "ahpt_revoked": ahpStatusRevoked, "ahpt_desktop": ahpStatusActive}
	if len(statuses) != len(want) {
		t.Fatalf("statuses = %v, want %v", statuses, want)
	}
	for id, status := range want {
		if statuses[id] != status {
			t.Errorf("status of %s = %q, want %q", id, statuses[id], status)
		}
	}
	if strings.Contains(out, "never-printed") {
		t.Fatal("structured output must never carry a token secret")
	}
}

func TestAHPTokensListWhenNoneAreActive(t *testing.T) {
	srv := newAHPTestServer(t)
	out, _, err := runAHPCommand(t, "ahp", "tokens", "list", "--server", srv.URL, "--token", "person-token")
	if err != nil || !strings.Contains(out, "orka ahp connect") {
		t.Fatalf("list = %q, %v", out, err)
	}
}

func TestAHPTokensRevoke(t *testing.T) {
	srv := newAHPTestServer(t)
	out, _, err := runAHPCommand(t, "ahp", "tokens", "revoke", "ahpt_one", "ahpt_missing", "ahpt_two",
		"--server", srv.URL, "--token", "person-token")
	if err == nil || !strings.Contains(err.Error(), "AHP token ahpt_missing not found") {
		t.Fatalf("revoke error = %v, want the missing token named", err)
	}
	if strings.Join(srv.deleted, ",") != "ahpt_one,ahpt_two" {
		t.Fatalf("deleted = %v, want every other token revoked", srv.deleted)
	}
	if !strings.Contains(out, "Revoked AHP token ahpt_one") || !strings.Contains(out, "Revoked AHP token ahpt_two") {
		t.Fatalf("output = %q", out)
	}
}

func TestAHPHostURL(t *testing.T) {
	for _, tt := range []struct {
		base, want string
		insecure   bool
		loopback   bool
		wantErr    string
	}{
		{base: "https://orka.example.com", want: "wss://orka.example.com/ahp"},
		{base: "https://orka.example.com/", want: "wss://orka.example.com/ahp"},
		{base: "https://orka.example.com:8443/api?x=1#y", want: "wss://orka.example.com:8443/api/ahp"},
		{base: "wss://orka.example.com/ahp-base", want: "wss://orka.example.com/ahp-base/ahp"},
		{base: "http://localhost:8080", want: "ws://localhost:8080/ahp", loopback: true},
		{base: "http://127.0.0.1:8080", want: "ws://127.0.0.1:8080/ahp", loopback: true},
		{base: "http://[::1]:8080", want: "ws://[::1]:8080/ahp", loopback: true},
		{base: "http://orka.internal", want: "ws://orka.internal/ahp", insecure: true},
		{base: "http://orka.internal", wantErr: "plaintext ws://"},
		{base: "https://user:pw@orka.example.com", wantErr: "must not carry credentials"},
		{base: "ftp://orka.example.com", wantErr: "http(s) or ws(s)"},
		{base: "orka.example.com", wantErr: "invalid Orka API address"},
	} {
		got, loopback, err := ahpHostURL(tt.base, tt.insecure)
		if tt.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("ahpHostURL(%q) error = %v, want %q", tt.base, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got.String() != tt.want || loopback != tt.loopback {
			t.Errorf("ahpHostURL(%q) = %v, %t, %v; want %s, %t", tt.base, got, loopback, err, tt.want, tt.loopback)
		}
	}
}

func TestAHPErrorExplainsCommonFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Header.Get("Authorization") {
		case "Bearer service-account":
			w.WriteHeader(http.StatusForbidden)
			w.Write([]byte(`{"error":"AHP tokens require the person's own OIDC sign-in; delegated context tokens are not accepted"}`)) //nolint:errcheck
		default:
			w.WriteHeader(http.StatusNotImplemented)
			w.Write([]byte(`{"error":"the AHP API is not enabled on this controller"}`)) //nolint:errcheck
		}
	}))
	defer srv.Close()
	_, _, err := runAHPCommand(t, "ahp", "tokens", "list", "--server", srv.URL, "--token", "service-account")
	if err == nil || !strings.Contains(err.Error(), "belong to a person signed in with OIDC") {
		t.Fatalf("ServiceAccount error = %v", err)
	}
	_, _, err = runAHPCommand(t, "ahp", "connect", "--server", srv.URL, "--token", "person-token")
	if err == nil || !strings.Contains(err.Error(), "controller.ahp.enabled") {
		t.Fatalf("disabled error = %v", err)
	}
}
