package daemonprotocol

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"testing"
)

func TestClientRoutesTypedRequestsThroughActorHost(t *testing.T) {
	seen := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "actor-1.actors.test" {
			t.Fatalf("Host = %q, want actor route host", r.Host)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer auth-value" {
			t.Fatalf("Authorization = %q, want bearer auth", got)
		}
		path := strings.TrimPrefix(r.URL.Path, "/router")
		switch r.Method + " " + path {
		case http.MethodGet + " " + HealthPath:
			seen["health"] = true
			w.WriteHeader(http.StatusNoContent)
		case http.MethodPost + " " + ExecPath:
			seen["exec"] = true
			var req ExecRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode exec request: %v", err)
			}
			if len(req.Command) != 1 || req.Command[0] != "echo" || !req.Detach {
				t.Fatalf("exec request = %#v", req)
			}
			_ = json.NewEncoder(w).Encode(ExecResponse{ExecID: "exec-1", Running: true})
		case http.MethodGet + " " + ExecStatusPath("exec-1"):
			seen["execStatus"] = true
			_ = json.NewEncoder(w).Encode(ExecResponse{Stdout: "ok"})
		case http.MethodPut + " " + FilesPath:
			seen["upload"] = true
			_ = json.NewEncoder(w).Encode(UploadResponse{Artifacts: []Artifact{{Path: "file"}}})
		case http.MethodPost + " " + FilesDownloadPath:
			seen["download"] = true
			_ = json.NewEncoder(w).Encode(DownloadResponse{Artifacts: []DownloadedArtifact{{Artifact: Artifact{Path: "file"}, Data: []byte("ok")}}})
		case http.MethodPost + " " + ScrubPath:
			seen["scrub"] = true
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	client := HTTPClient{RouterURL: server.URL + "/router", ActorDNSSuffix: "actors.test", HTTPClient: server.Client()}
	actor := ActorRequest{ActorID: "actor-1", AuthValue: "auth-value"}
	if err := client.Health(context.Background(), actor); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if exec, err := client.Exec(context.Background(), actor, ExecRequest{Command: []string{"echo"}, Detach: true}); err != nil || exec.ExecID != "exec-1" {
		t.Fatalf("Exec() = %#v, %v", exec, err)
	}
	if status, err := client.ExecStatus(context.Background(), actor, "exec-1"); err != nil || status.Stdout != "ok" {
		t.Fatalf("ExecStatus() = %#v, %v", status, err)
	}
	if upload, err := client.Upload(context.Background(), actor, UploadRequest{Files: []UploadFile{{Path: "file", Data: []byte("ok")}}}); err != nil || len(upload.Artifacts) != 1 {
		t.Fatalf("Upload() = %#v, %v", upload, err)
	}
	if download, err := client.Download(context.Background(), actor, DownloadRequest{Paths: []string{"file"}}); err != nil || string(download.Artifacts[0].Data) != "ok" {
		t.Fatalf("Download() = %#v, %v", download, err)
	}
	if err := client.Scrub(context.Background(), actor, ScrubRequest{Paths: []string{"file"}}); err != nil {
		t.Fatalf("Scrub() error = %v", err)
	}
	for _, key := range []string{"health", "exec", "execStatus", "upload", "download", "scrub"} {
		if !seen[key] {
			t.Fatalf("request %q was not observed", key)
		}
	}
}

func TestClientStatusErrorCarriesRetryability(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "try later", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	client := HTTPClient{RouterURL: server.URL, ActorDNSSuffix: "actors.test", HTTPClient: server.Client()}
	err := client.Health(context.Background(), ActorRequest{ActorID: "actor-1"})
	var daemonErr *Error
	if !errors.As(err, &daemonErr) {
		t.Fatalf("Health() error = %T %[1]v, want daemon Error", err)
	}
	if daemonErr.Reason != ErrorReasonStatus || daemonErr.StatusCode != http.StatusServiceUnavailable || !daemonErr.Retryable {
		t.Fatalf("daemon error = %#v, want retryable status", daemonErr)
	}
}

func TestClientHealthUsesNoBodyAndNoAuthWhenEmpty(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != HealthPath {
			t.Fatalf("request = %s %s, want GET %s", r.Method, r.URL.Path, HealthPath)
		}
		if r.Host != "actor-1.actors.test" {
			t.Fatalf("Host = %q, want actor route host", r.Host)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("Authorization = %q, want omitted", got)
		}
		if got := r.Header.Get("Content-Type"); got != "" {
			t.Fatalf("Content-Type = %q, want omitted", got)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := HTTPClient{RouterURL: server.URL, ActorDNSSuffix: "actors.test", HTTPClient: server.Client()}
	if err := client.Health(context.Background(), ActorRequest{ActorID: "actor-1"}); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
}

func TestClientDecodeErrorIsNonRetryable(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not-json"))
	}))
	defer server.Close()

	client := HTTPClient{RouterURL: server.URL, ActorDNSSuffix: "actors.test", HTTPClient: server.Client()}
	_, err := client.Exec(context.Background(), ActorRequest{ActorID: "actor-1"}, ExecRequest{Command: []string{"echo"}})
	var daemonErr *Error
	if !errors.As(err, &daemonErr) {
		t.Fatalf("Exec() error = %T %[1]v, want daemon Error", err)
	}
	if daemonErr.Reason != ErrorReasonDecodeResponse || daemonErr.Retryable {
		t.Fatalf("daemon error = %#v, want non-retryable decode error", daemonErr)
	}
}

func TestClientUploadNoResponseAcceptsNoContent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != FilesPath {
			t.Fatalf("request = %s %s, want PUT %s", r.Method, r.URL.Path, FilesPath)
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	client := HTTPClient{RouterURL: server.URL, ActorDNSSuffix: "actors.test", HTTPClient: server.Client()}
	if err := client.UploadNoResponse(context.Background(), ActorRequest{ActorID: "actor-1"}, UploadRequest{Files: []UploadFile{{Path: "file"}}}); err != nil {
		t.Fatalf("UploadNoResponse() error = %v", err)
	}
}

func TestClientReusesConnectionAfterIgnoredJSONResponse(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			if r.Method != http.MethodPut || r.URL.Path != FilesPath {
				t.Errorf("first request = %s %s, want PUT %s", r.Method, r.URL.Path, FilesPath)
			}
			_, _ = io.Copy(io.Discard, r.Body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ignored":true}`)
		case 2:
			if r.Method != http.MethodGet || r.URL.Path != HealthPath {
				t.Errorf("second request = %s %s, want GET %s", r.Method, r.URL.Path, HealthPath)
			}
			w.WriteHeader(http.StatusNoContent)
		default:
			t.Errorf("unexpected request %d: %s %s", requestCount, r.Method, r.URL.Path)
		}
	}))
	defer server.Close()

	var traceMu sync.Mutex
	gotConnections := make([]httptrace.GotConnInfo, 0, 2)
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			traceMu.Lock()
			gotConnections = append(gotConnections, info)
			traceMu.Unlock()
		},
	})
	client := HTTPClient{RouterURL: server.URL, ActorDNSSuffix: "actors.test", HTTPClient: server.Client()}
	actor := ActorRequest{ActorID: "actor-1"}
	if err := client.UploadNoResponse(ctx, actor, UploadRequest{Files: []UploadFile{{Path: "file"}}}); err != nil {
		t.Fatalf("UploadNoResponse() error = %v", err)
	}
	if err := client.Health(ctx, actor); err != nil {
		t.Fatalf("Health() error = %v", err)
	}

	traceMu.Lock()
	connections := append([]httptrace.GotConnInfo(nil), gotConnections...)
	traceMu.Unlock()
	if len(connections) != 2 {
		t.Fatalf("GotConn callbacks = %d, want 2", len(connections))
	}
	if !connections[1].Reused {
		t.Fatal("second daemon request did not reuse the first connection")
	}
}

func TestClientReusesConnectionAfterLargeErrorResponse(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.Method != http.MethodGet || r.URL.Path != HealthPath {
			t.Errorf("request = %s %s, want GET %s", r.Method, r.URL.Path, HealthPath)
		}
		if requestCount == 1 {
			http.Error(w, strings.Repeat("x", 16<<10), http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	var traceMu sync.Mutex
	gotConnections := make([]httptrace.GotConnInfo, 0, 2)
	ctx := httptrace.WithClientTrace(t.Context(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			traceMu.Lock()
			gotConnections = append(gotConnections, info)
			traceMu.Unlock()
		},
	})
	client := HTTPClient{RouterURL: server.URL, ActorDNSSuffix: "actors.test", HTTPClient: server.Client()}
	actor := ActorRequest{ActorID: "actor-1"}
	if err := client.Health(ctx, actor); err == nil {
		t.Fatal("first Health() error = nil, want status error")
	}
	if err := client.Health(ctx, actor); err != nil {
		t.Fatalf("second Health() error = %v", err)
	}

	traceMu.Lock()
	connections := append([]httptrace.GotConnInfo(nil), gotConnections...)
	traceMu.Unlock()
	if len(connections) != 2 {
		t.Fatalf("GotConn callbacks = %d, want 2", len(connections))
	}
	if !connections[1].Reused {
		t.Fatal("request after large daemon error did not reuse the first connection")
	}
}

func TestClientBoundsIgnoredResponseDrain(t *testing.T) {
	const (
		bodyBytes             = 1 << 20
		maxExpectedDrainBytes = 64 << 10
	)
	body := &trackingReadCloser{reader: strings.NewReader(strings.Repeat("x", bodyBytes))}
	client := HTTPClient{
		RouterURL:      "http://router.test",
		ActorDNSSuffix: "actors.test",
		HTTPClient: httpDoerFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusNoContent,
				Header:     make(http.Header),
				Body:       body,
			}, nil
		}),
	}

	if err := client.Health(t.Context(), ActorRequest{ActorID: "actor-1"}); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
	if !body.closed {
		t.Fatal("response body was not closed")
	}
	if body.bytesRead == 0 {
		t.Fatal("ignored response body was not drained")
	}
	if body.bytesRead > maxExpectedDrainBytes {
		t.Fatalf("ignored response drain read %d bytes, want at most %d", body.bytesRead, maxExpectedDrainBytes)
	}
}

type httpDoerFunc func(*http.Request) (*http.Response, error)

func (f httpDoerFunc) Do(req *http.Request) (*http.Response, error) {
	return f(req)
}

type trackingReadCloser struct {
	reader    io.Reader
	bytesRead int
	closed    bool
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += n
	return n, err
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}
