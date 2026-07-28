package workspaceagent

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewClientRejectsInsecureTransportByDefault(t *testing.T) {
	t.Parallel()

	_, err := NewClient(ClientConfig{Endpoint: "http://workspace-agent.example"})
	if err == nil || !strings.Contains(err.Error(), "insecure workspace-agent transport") {
		t.Fatalf("NewClient error = %v, want insecure transport rejection", err)
	}
	if _, err := NewClient(ClientConfig{Endpoint: "http://workspace-agent.example", AllowInsecure: true}); err != nil {
		t.Fatalf("NewClient with explicit insecure opt-in: %v", err)
	}
}

func TestClientValidatesTLSWithConfiguredCA(t *testing.T) {
	t.Parallel()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != HealthPath {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(HealthResponse{Versioned: NewVersioned(), Status: "ok"})
	}))
	defer server.Close()

	cert, err := x509.ParseCertificate(server.TLS.Certificates[0].Certificate[0])
	if err != nil {
		t.Fatalf("parse server certificate: %v", err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Raw})
	customTransport := http.DefaultTransport.(*http.Transport).Clone() //nolint:forcetypeassert
	client, err := NewClient(ClientConfig{
		Endpoint:   server.URL,
		CAData:     caPEM,
		HTTPClient: &http.Client{Transport: customTransport},
		Timeout:    2 * time.Second,
	})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	response, err := client.Health(context.Background())
	if err != nil {
		t.Fatalf("Health: %v", err)
	}
	if response.Status != "ok" || response.ProtocolVersion != ProtocolVersion {
		t.Fatalf("health response = %#v", response)
	}
	configured := client.httpClient
	if configured.Timeout != 2*time.Second {
		t.Fatalf("custom client timeout = %v, want 2s", configured.Timeout)
	}
}

func TestClientSendsControlAndAttachmentCredentials(t *testing.T) {
	t.Parallel()

	const (
		controlAuth = "control-secret"
		workerAuth  = "worker-secret"
	)
	var sawActivate, sawExec bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case AttachmentControlPath:
			if r.Header.Get("Authorization") != "Bearer "+controlAuth {
				t.Errorf("control authorization = %q", r.Header.Get("Authorization"))
			}
			var request AttachmentControlRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode activation: %v", err)
			}
			if request.ProtocolVersion != ProtocolVersion {
				t.Errorf("activation protocol version = %q", request.ProtocolVersion)
			}
			sawActivate = true
			_ = json.NewEncoder(w).Encode(AttachmentControlResponse{
				Versioned:    NewVersioned(),
				WorkspaceUID: request.WorkspaceUID,
				ActiveEpoch:  request.Epoch,
				Active:       true,
			})
		case ExecPath:
			wrongToken := r.Header.Get("Authorization") != "Bearer "+workerAuth
			wrongWorkspace := r.Header.Get(WorkspaceUIDHeader) != "workspace-uid"
			wrongEpoch := r.Header.Get(AttachmentEpochHeader) != "7"
			if wrongToken || wrongWorkspace || wrongEpoch {
				t.Errorf("attachment headers = %#v", r.Header)
			}
			var request ExecRequest
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				t.Errorf("decode exec: %v", err)
			}
			if request.ProtocolVersion != ProtocolVersion || request.OperationID != "operation-7" {
				t.Errorf("exec request = %#v", request)
			}
			sawExec = true
			_ = json.NewEncoder(w).Encode(ExecResponse{
				Versioned:   NewVersioned(),
				OperationID: request.OperationID,
				State:       OperationStateRunning,
				Running:     true,
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{Endpoint: server.URL, AllowInsecure: true, ControlAuth: controlAuth})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.ActivateAttachment(context.Background(), AttachmentControlRequest{
		WorkspaceUID:      "workspace-uid",
		BindingGeneration: "generation-1",
		TaskUID:           "task-uid",
		Epoch:             7,
		TokenSHA256:       "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		ExpiresAt:         time.Now().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("ActivateAttachment: %v", err)
	}
	_, err = client.Exec(
		context.Background(),
		AttachmentCredentials{WorkspaceUID: "workspace-uid", Epoch: 7, Bearer: workerAuth},
		ExecRequest{
			OperationID: "operation-7",
			Command:     []string{"true"},
		},
	)
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if !sawActivate || !sawExec {
		t.Fatalf("sawActivate=%t sawExec=%t", sawActivate, sawExec)
	}
}

func TestClientRedactsCredentialsFromErrors(t *testing.T) {
	t.Parallel()

	const authValue = "do-not-leak-this-token"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "rejected "+authValue, http.StatusUnauthorized)
	}))
	defer server.Close()

	client, err := NewClient(ClientConfig{Endpoint: server.URL, AllowInsecure: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Exec(
		context.Background(),
		AttachmentCredentials{WorkspaceUID: "workspace", Epoch: 1, Bearer: authValue},
		ExecRequest{OperationID: "op", Command: []string{"true"}},
	)
	if err == nil {
		t.Fatal("Exec succeeded, want error")
	}
	if strings.Contains(err.Error(), authValue) || !strings.Contains(err.Error(), "[redacted]") {
		t.Fatalf("error = %q, want redacted token", err)
	}
}

func TestClientRejectsIncompatibleProtocolResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(HealthResponse{
			Versioned: Versioned{ProtocolVersion: "workspace.orka.ai/v2"},
			Status:    "ok",
		})
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{Endpoint: server.URL, AllowInsecure: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Health(context.Background()); err == nil || !strings.Contains(err.Error(), "protocol version") {
		t.Fatalf("Health error = %v, want protocol version failure", err)
	}
}

func TestClientDoesNotForwardCredentialsAcrossRedirects(t *testing.T) {
	t.Parallel()

	const authValue = "redirect-sensitive-auth"
	forwarded := false
	target := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		forwarded = r.Header.Get("Authorization") != ""
	}))
	defer target.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, nil, target.URL, http.StatusTemporaryRedirect)
	}))
	defer origin.Close()

	client, err := NewClient(ClientConfig{Endpoint: origin.URL, AllowInsecure: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Exec(
		context.Background(),
		AttachmentCredentials{WorkspaceUID: "workspace", Epoch: 1, Bearer: authValue},
		ExecRequest{OperationID: "redirect", Command: []string{"true"}},
	)
	if err == nil {
		t.Fatal("Exec followed redirect")
	}
	if forwarded {
		t.Fatal("workspace-agent credential was forwarded across redirect")
	}
}

func TestClientRejectsNoContentForVersionedResponse(t *testing.T) {
	t.Parallel()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{Endpoint: server.URL, AllowInsecure: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Health(context.Background()); err == nil {
		t.Fatal("Health accepted a bodyless versioned response")
	}
}

func TestClientRejectsTLSVerificationOverrides(t *testing.T) {
	t.Parallel()

	_, err := NewClient(ClientConfig{
		Endpoint: "https://workspace-agent.example",
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			InsecureSkipVerify: true,
		}}},
	})
	if err == nil {
		t.Fatal("NewClient accepted InsecureSkipVerify")
	}
	_, err = NewClient(ClientConfig{
		Endpoint: "https://workspace-agent.example",
		HTTPClient: &http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{
			ServerName: "other.example",
		}}},
	})
	if err == nil {
		t.Fatal("NewClient accepted mismatched TLS ServerName")
	}
}

func TestClientMarksTruncatedSuccessResponseRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"protocolVersion":"workspace.orka.ai/v1"`))
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{Endpoint: server.URL, AllowInsecure: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Health(context.Background())
	var clientErr *Error
	if !errors.As(err, &clientErr) || !clientErr.Retryable || clientErr.Reason != ErrorReasonDecodeResponse {
		t.Fatalf("truncated response error = %#v", err)
	}
}

func TestClientKeepsMalformedCompleteResponseNonRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`not-json`))
	}))
	defer server.Close()
	client, err := NewClient(ClientConfig{Endpoint: server.URL, AllowInsecure: true})
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, err = client.Health(context.Background())
	var clientErr *Error
	if !errors.As(err, &clientErr) || clientErr.Retryable {
		t.Fatalf("malformed response error = %#v", err)
	}
}
