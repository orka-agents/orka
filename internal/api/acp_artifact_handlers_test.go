package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/artifactcap"
)

func TestACPArtifactHandlersUploadDownloadAndReplay(t *testing.T) {
	t.Parallel()
	secret := []byte(strings.Repeat("a", artifactcap.MinSecretBytes))
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	service, err := artifactcap.NewService(artifactcap.ServiceConfig{
		Root: t.TempDir(), Secret: secret, MaxObjectBytes: 1 << 20, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := NewACPArtifactHandlers(service)
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{BodyLimit: 1 << 20, StreamRequestBody: true})
	app.Put(acpArtifactRoutePrefix+"/sha256/:digest", handlers.Upload)
	app.Get(acpArtifactRoutePrefix+"/sha256/:digest", handlers.Download)

	data := []byte("streamed durable artifact")
	upload := apiOperationRequest(artifactcap.OperationUpload, data, "api-upload")
	uploadAuthorization, err := artifactcap.Issue(secret, upload, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	uploadResponse := performArtifactRequest(t, app, upload, uploadAuthorization, bytes.NewReader(data))
	defer uploadResponse.Body.Close() //nolint:errcheck
	if uploadResponse.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(uploadResponse.Body)
		t.Fatalf("upload status=%d body=%s", uploadResponse.StatusCode, body)
	}
	var stored artifactcap.Artifact
	if err := json.NewDecoder(uploadResponse.Body).Decode(&stored); err != nil {
		t.Fatal(err)
	}
	if stored.Digest != upload.ObjectDigest || stored.SizeBytes != int64(len(data)) {
		t.Fatalf("stored artifact = %#v", stored)
	}

	replayResponse := performArtifactRequest(t, app, upload, uploadAuthorization, bytes.NewReader(data))
	defer replayResponse.Body.Close() //nolint:errcheck
	if replayResponse.StatusCode != http.StatusConflict {
		t.Fatalf("replay status=%d, want 409", replayResponse.StatusCode)
	}

	download := upload
	download.Operation = artifactcap.OperationDownload
	download.OperationID = "api-download"
	downloadAuthorization, err := artifactcap.Issue(secret, download, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	downloadResponse := performArtifactRequest(t, app, download, downloadAuthorization, nil)
	defer downloadResponse.Body.Close() //nolint:errcheck
	if downloadResponse.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(downloadResponse.Body)
		t.Fatalf("download status=%d body=%s", downloadResponse.StatusCode, body)
	}
	got, err := io.ReadAll(downloadResponse.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, data) {
		t.Fatalf("download = %q, want %q", got, data)
	}
	if downloadResponse.Header.Get(artifactcap.ObjectDigestHeader) != download.ObjectDigest ||
		downloadResponse.Header.Get("Content-Type") != download.MediaType ||
		downloadResponse.ContentLength != int64(len(data)) {
		t.Fatalf("download headers = %#v", downloadResponse.Header)
	}
}

func TestServerInstallsACPArtifactRoutesFromEnvironment(t *testing.T) {
	secret := []byte(strings.Repeat("e", artifactcap.MinSecretBytes))
	secretFile := filepath.Join(t.TempDir(), "artifact-secret")
	if err := os.WriteFile(secretFile, secret, 0o600); err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(t.TempDir(), "artifacts")
	t.Setenv(envACPArtifactSecretFile, secretFile)
	t.Setenv(envACPArtifactRoot, root)
	t.Setenv(envACPArtifactMaxBytes, strconv.Itoa(1<<20))

	server := &Server{app: fiber.New(fiber.Config{BodyLimit: 1024, StreamRequestBody: true})}
	server.installACPArtifactTransport()
	data := []byte("route integration")
	request := apiOperationRequest(artifactcap.OperationUpload, data, "route-install-upload")
	authorization, err := artifactcap.Issue(secret, request, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	response := performArtifactRequest(t, server.app, request, authorization, bytes.NewReader(data))
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("registered route status=%d body=%s", response.StatusCode, body)
	}
}

func TestServerInstallsACPArtifactRoutesWithTrailingNewlineSecret(t *testing.T) {
	secret := []byte(strings.Repeat("n", artifactcap.MinSecretBytes))
	secretFile := filepath.Join(t.TempDir(), "artifact-secret")
	secretWithNewline := append(append([]byte(nil), secret...), '\n')
	if err := os.WriteFile(secretFile, secretWithNewline, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envACPArtifactSecretFile, secretFile)
	t.Setenv(envACPArtifactRoot, filepath.Join(t.TempDir(), "artifacts"))
	t.Setenv(envACPArtifactMaxBytes, strconv.Itoa(1<<20))

	server := &Server{app: fiber.New(fiber.Config{BodyLimit: 1024, StreamRequestBody: true})}
	server.installACPArtifactTransport()
	data := []byte("normalized route secret")
	request := apiOperationRequest(artifactcap.OperationUpload, data, "route-install-normalized-secret")
	authorization, err := artifactcap.Issue(secret, request, time.Now().UTC(), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	response := performArtifactRequest(t, server.app, request, authorization, bytes.NewReader(data))
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("registered route status=%d body=%s", response.StatusCode, body)
	}
}

func TestLoadACPArtifactHandlersRejectsShortNormalizedSecret(t *testing.T) {
	secretFile := filepath.Join(t.TempDir(), "artifact-secret")
	rawSecret := append(bytes.Repeat([]byte("s"), artifactcap.MinSecretBytes-1), '\n')
	if err := os.WriteFile(secretFile, rawSecret, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envACPArtifactSecretFile, secretFile)
	t.Setenv(envACPArtifactRoot, filepath.Join(t.TempDir(), "artifacts"))
	t.Setenv(envACPArtifactMaxBytes, strconv.Itoa(1<<20))

	if _, err := loadACPArtifactHandlersFromEnvironment(); err == nil {
		t.Fatal("expected normalized secret below minimum length to fail closed")
	}
}

func TestACPArtifactHandlerStreamsPastFiberBodyThreshold(t *testing.T) {
	t.Parallel()
	secret := []byte(strings.Repeat("m", artifactcap.MinSecretBytes))
	now := time.Now().UTC()
	service, err := artifactcap.NewService(artifactcap.ServiceConfig{
		Root: t.TempDir(), Secret: secret, MaxObjectBytes: 1 << 20, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := NewACPArtifactHandlers(service)
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{BodyLimit: 1024, StreamRequestBody: true})
	app.Put(acpArtifactRoutePrefix+"/sha256/:digest", handlers.Upload)
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveErrors := make(chan error, 1)
	go func() { serveErrors <- app.Listener(listener, fiber.ListenConfig{DisableStartupMessage: true}) }()
	t.Cleanup(func() {
		_ = app.Shutdown()
		<-serveErrors
	})

	data := bytes.Repeat([]byte("stream-me"), 8192)
	request := apiOperationRequest(artifactcap.OperationUpload, data, "streaming-upload")
	authorization, err := artifactcap.Issue(secret, request, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest, err := http.NewRequest(http.MethodPut, "http://"+listener.Addr().String()+request.Path(), bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	httpRequest.ContentLength = int64(len(data))
	httpRequest.Header.Set("Content-Type", request.MediaType)
	httpRequest.Header.Set(artifactcap.CapabilityHeader, authorization.Capability)
	httpRequest.Header.Set(artifactcap.RequestDigestHeader, authorization.RequestDigest)
	httpRequest.Header.Set(artifactcap.ContentLengthHeader, strconv.FormatInt(request.ContentLength, 10))
	httpRequest.Header.Set(artifactcap.MediaTypeHeader, request.MediaType)
	response, err := http.DefaultClient.Do(httpRequest)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close() //nolint:errcheck
	if response.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(response.Body)
		t.Fatalf("streamed upload status=%d body=%s", response.StatusCode, body)
	}
}

func TestACPArtifactHandlersHostileRequestsAreBoundedAndRedacted(t *testing.T) {
	t.Parallel()
	secretText := strings.Repeat("handler-secret-", 3)
	secret := []byte(secretText)
	now := time.Now().UTC()
	service, err := artifactcap.NewService(artifactcap.ServiceConfig{
		Root: t.TempDir(), Secret: secret, MaxObjectBytes: 4, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	handlers, err := NewACPArtifactHandlers(service)
	if err != nil {
		t.Fatal(err)
	}
	app := fiber.New(fiber.Config{StreamRequestBody: true, BodyLimit: 1 << 20})
	app.Put(acpArtifactRoutePrefix+"/sha256/:digest", handlers.Upload)

	data := []byte("12345")
	request := apiOperationRequest(artifactcap.OperationUpload, data, "api-oversized")
	authorization, err := artifactcap.Issue(secret, request, now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	response := performArtifactRequest(t, app, request, authorization, bytes.NewReader(data))
	defer response.Body.Close() //nolint:errcheck
	body, _ := io.ReadAll(response.Body)
	if response.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d body=%s", response.StatusCode, body)
	}
	if bytes.Contains(body, []byte(authorization.Capability)) || bytes.Contains(body, []byte(secretText)) {
		t.Fatalf("response disclosed capability or secret: %s", body)
	}

	expired, err := artifactcap.Issue(secret, request, now.Add(-2*time.Minute), time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	expiredResponse := performArtifactRequest(t, app, request, expired, bytes.NewReader(data))
	defer expiredResponse.Body.Close() //nolint:errcheck
	if expiredResponse.StatusCode != http.StatusForbidden {
		t.Fatalf("expired status=%d, want 403", expiredResponse.StatusCode)
	}
	expiredBody, _ := io.ReadAll(expiredResponse.Body)
	if bytes.Contains(expiredBody, []byte(expired.Capability)) || bytes.Contains(expiredBody, []byte(secretText)) {
		t.Fatalf("expired response disclosed capability or secret: %s", expiredBody)
	}
}

//nolint:unparam // The stable parameter keeps call sites explicit across related test cases.
func apiOperationRequest(operation artifactcap.Operation, data []byte, operationID string) artifactcap.OperationRequest {
	return artifactcap.OperationRequest{
		Operation: operation, ObjectDigest: artifactcap.DigestBytes(data),
		Identity:      artifactcap.Identity{Namespace: "default", TaskID: "task-uid-api"},
		ContentLength: int64(len(data)), MediaType: "application/octet-stream", OperationID: operationID,
	}
}

func performArtifactRequest(
	t *testing.T,
	app *fiber.App,
	request artifactcap.OperationRequest,
	authorization artifactcap.Authorization,
	body io.Reader,
) *http.Response {
	t.Helper()
	method := request.Method()
	endpoint := "http://example.invalid" + request.Path()
	httpRequest := httptest.NewRequest(method, endpoint, body)
	httpRequest.Header.Set(artifactcap.CapabilityHeader, authorization.Capability)
	httpRequest.Header.Set(artifactcap.RequestDigestHeader, authorization.RequestDigest)
	httpRequest.Header.Set(artifactcap.ContentLengthHeader, strconv.FormatInt(request.ContentLength, 10))
	httpRequest.Header.Set(artifactcap.MediaTypeHeader, request.MediaType)
	if method == http.MethodPut {
		httpRequest.Header.Set("Content-Type", request.MediaType)
		httpRequest.ContentLength = request.ContentLength
	}
	response, err := app.Test(httpRequest, fiber.TestConfig{Timeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	return response
}
