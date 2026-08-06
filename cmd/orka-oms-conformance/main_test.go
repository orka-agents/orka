package main

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orka-agents/orka/pkg/oms/conformance"
)

func TestWriteResultRedactsRawTokenAndAuthorizationValue(t *testing.T) {
	rawToken := "marker-" + "value"
	authorizationValue := "Bearer " + rawToken
	var output bytes.Buffer
	result := conformance.CheckResult{Message: "raw=" + rawToken + " header=" + authorizationValue}
	if err := writeResult(&output, result, rawToken); err != nil {
		t.Fatalf("writeResult(): %v", err)
	}
	if strings.Contains(output.String(), rawToken) || strings.Contains(output.String(), authorizationValue) {
		t.Fatalf("writeResult() leaked configured credential: %s", output.String())
	}
}

func TestRunSendsOneCompleteBearerAuthorizationValue(t *testing.T) {
	const rawToken = "conformance-secret-token"
	var gotAuthorization string
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		gotAuthorization = request.Header.Get("Authorization")
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusUnauthorized)
		_, _ = writer.Write([]byte(
			`{"protocolVersion":"orka.oms.v0alpha1","binding":null,` +
				`"code":"OMS_UNAUTHORIZED","message":"unauthorized",` +
				`"retryable":false,"retryAfterSeconds":0}`,
		))
	}))
	t.Cleanup(server.Close)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		[]string{
			"--endpoint=" + server.URL,
			"--insecure-loopback-only",
			"--provider-commit-gap-proof",
			"--overall-timeout=1s",
		},
		&stdout,
		&stderr,
		func(name string) string {
			if name == "ORKA_OMS_BEARER_TOKEN" {
				return rawToken
			}
			return ""
		},
	)
	if code != 1 {
		t.Fatalf("run() code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if gotAuthorization != "Bearer "+rawToken {
		t.Fatalf("Authorization = %q, want one complete Bearer value", gotAuthorization)
	}
	if strings.Contains(stdout.String(), rawToken) || strings.Contains(stderr.String(), rawToken) {
		t.Fatalf("run() leaked raw token; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
}

func TestReadCheckpointRejectsUnknownAndTrailingJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	for name, content := range map[string]string{
		"unknown":  `{"unknown":true}`,
		"trailing": `{} {}`,
	} {
		t.Run(name, func(t *testing.T) {
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readCheckpoint(path); err == nil {
				t.Fatalf("readCheckpoint() accepted %s JSON", name)
			}
		})
	}
}

func TestReadCheckpointRejectsOversizedFileWithBoundedRead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "checkpoint.json")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Truncate(int64(maxCheckpointBytes) * 64); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := readCheckpoint(path); err == nil || !strings.Contains(err.Error(), "size is invalid") {
		t.Fatalf("readCheckpoint() error = %v, want size rejection", err)
	}
}

func TestRunRejectsPlaintextHTTPWithoutExplicitLoopbackFlag(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		[]string{"--endpoint=http://127.0.0.1:8080", "--overall-timeout=1s"},
		&stdout,
		&stderr,
		func(name string) string {
			if name == "ORKA_OMS_BEARER_TOKEN" {
				return "test-token"
			}
			return ""
		},
	)
	if code != 1 {
		t.Fatalf("run() code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "HTTPS") {
		t.Fatalf("run() output = %q, want HTTPS rejection", stdout.String())
	}
}
