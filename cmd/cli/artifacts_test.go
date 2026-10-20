/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/cli/client"
	sigsyaml "sigs.k8s.io/yaml"
)

const artifactsAPIPath = "/api/v1/tasks/example-task/artifacts"

func artifactsAPIServer(artifacts []client.ArtifactMetadata) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != artifactsAPIPath {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprintf(w, "not found: %s %s", r.Method, r.URL.Path) //nolint:errcheck
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"artifacts": artifacts}) //nolint:errcheck
	}))
}

func runTaskArtifacts(t *testing.T, srv *httptest.Server, extraArgs ...string) (string, error) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	root := newRootCmd()
	var out bytes.Buffer
	root.SetOut(&out)
	args := append([]string{"task", "artifacts", "example-task", "--server", srv.URL}, extraArgs...)
	root.SetArgs(args)
	err := root.Execute()
	return out.String(), err
}

func TestTaskArtifactsJSON(t *testing.T) {
	fixture := []client.ArtifactMetadata{{
		Filename:    "report.txt",
		ContentType: "text/plain",
		Size:        512,
		CreatedAt:   "2026-09-09T12:00:00Z",
	}}
	srv := artifactsAPIServer(fixture)
	defer srv.Close()

	out, err := runTaskArtifacts(t, srv, "-o", "json")
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	var got []client.ArtifactMetadata
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding JSON output: %v\noutput: %s", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("decoded %d artifacts, want 1\noutput: %s", len(got), out)
	}
	if got[0] != fixture[0] {
		t.Fatalf("decoded artifact %+v, want %+v", got[0], fixture[0])
	}
}

func TestTaskArtifactsYAML(t *testing.T) {
	fixture := []client.ArtifactMetadata{{
		Filename:    "report.txt",
		ContentType: "text/plain",
		Size:        512,
		CreatedAt:   "2026-09-09T12:00:00Z",
	}}
	srv := artifactsAPIServer(fixture)
	defer srv.Close()

	out, err := runTaskArtifacts(t, srv, "--output", "yaml")
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}

	var got []client.ArtifactMetadata
	if err := sigsyaml.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("decoding YAML output: %v\noutput: %s", err, out)
	}
	if len(got) != 1 {
		t.Fatalf("decoded %d artifacts, want 1\noutput: %s", len(got), out)
	}
	if got[0] != fixture[0] {
		t.Fatalf("decoded artifact %+v, want %+v", got[0], fixture[0])
	}
}

func TestTaskArtifactsEmptyStructuredOutput(t *testing.T) {
	srv := artifactsAPIServer([]client.ArtifactMetadata{})
	defer srv.Close()

	out, err := runTaskArtifacts(t, srv, "-o", "json")
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if got := strings.TrimSpace(out); got != "[]" {
		t.Fatalf("empty JSON output = %q, want []", got)
	}

	out, err = runTaskArtifacts(t, srv, "-o", "yaml")
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if got := strings.TrimSpace(out); got != "[]" {
		t.Fatalf("empty YAML output = %q, want []", got)
	}
}

func TestTaskArtifactsDefaultTable(t *testing.T) {
	fixture := []client.ArtifactMetadata{{
		Filename:    "report.txt",
		ContentType: "text/plain",
		Size:        512,
		CreatedAt:   "2026-09-09T12:00:00Z",
	}, {
		Filename:    "video.mp4",
		ContentType: "video/mp4",
		Size:        5 * 1024 * 1024,
		CreatedAt:   "2026-09-09T12:00:00Z",
	}}
	srv := artifactsAPIServer(fixture)
	defer srv.Close()

	out, err := runTaskArtifacts(t, srv)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	for _, want := range []string{"FILENAME", "TYPE", "SIZE", "report.txt", "text/plain", "512 B", "video.mp4", "5.0 MB"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table output missing %q:\n%s", want, out)
		}
	}
}

func TestTaskArtifactsEmptyTable(t *testing.T) {
	srv := artifactsAPIServer([]client.ArtifactMetadata{})
	defer srv.Close()

	out, err := runTaskArtifacts(t, srv)
	if err != nil {
		t.Fatalf("Execute() error: %v", err)
	}
	if got := strings.TrimSpace(out); got != "No artifacts found." {
		t.Fatalf("empty table output = %q, want No artifacts found.", got)
	}
}

func TestTaskArtifactsInvalidFormat(t *testing.T) {
	srv := artifactsAPIServer([]client.ArtifactMetadata{})
	defer srv.Close()

	_, err := runTaskArtifacts(t, srv, "-o", "xml")
	if err == nil {
		t.Fatal("Execute() error = nil, want unsupported format error")
	}
	if !strings.Contains(err.Error(), "unsupported output format") {
		t.Fatalf("Execute() error = %v, want unsupported output format", err)
	}
}
