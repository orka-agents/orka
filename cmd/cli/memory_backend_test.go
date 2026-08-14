package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

const (
	memoryBackendUpdateRootName            = "memory"
	memoryBackendUpdateGroupName           = "backend"
	memoryBackendUpdateCommandName         = "update"
	memoryBackendUpdateServerFlag          = "--server"
	memoryBackendUpdateNamespaceFlag       = "--namespace"
	memoryBackendUpdateFileFlag            = "--file"
	memoryBackendUpdateReasonFlag          = "--reason"
	memoryBackendUpdateConfirmFlag         = "--yes"
	memoryBackendUpdatePath                = "/api/v1/memory-backends/default"
	memoryBackendUpdateManifestNamespace   = "team-a"
	memoryBackendUpdateConfiguredNamespace = "configured-default"
	memoryBackendUpdateExplicitNamespace   = "team-b"
	memoryBackendUpdateReason              = "update backend configuration"
	memoryBackendUpdateDefaultName         = defaultNamespace
	memoryBackendUpdateMetadataKey         = "metadata"
	memoryBackendUpdateNameKey             = "name"
	memoryBackendUpdateNamespaceKey        = "namespace"
	memoryBackendUpdateReasonKey           = "reason"
	memoryBackendUpdateSpecKey             = "spec"
	memoryBackendUpdateLifecycleStateKey   = "lifecycleState"
)

func TestMemoryBackendUpdateManifestNamespaceRouting(t *testing.T) {
	tests := []struct {
		name                string
		manifestNamespace   string
		configuredNamespace string
		explicitNamespace   string
		wantNamespace       string
		wantError           string
	}{
		{
			name:                "manifest overrides configured default when flag omitted",
			manifestNamespace:   memoryBackendUpdateManifestNamespace,
			configuredNamespace: memoryBackendUpdateConfiguredNamespace,
			wantNamespace:       memoryBackendUpdateManifestNamespace,
		},
		{
			name:              "matching explicit namespace",
			manifestNamespace: memoryBackendUpdateManifestNamespace,
			explicitNamespace: memoryBackendUpdateManifestNamespace,
			wantNamespace:     memoryBackendUpdateManifestNamespace,
		},
		{
			name:              "explicit namespace fills omitted manifest namespace",
			explicitNamespace: memoryBackendUpdateExplicitNamespace,
			wantNamespace:     memoryBackendUpdateExplicitNamespace,
		},
		{
			name:              "mismatched explicit namespace",
			manifestNamespace: memoryBackendUpdateManifestNamespace,
			explicitNamespace: memoryBackendUpdateExplicitNamespace,
			wantError:         "manifest namespace \"team-a\" does not match --namespace \"team-b\"",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			manifestPath := writeMemoryBackendUpdateManifest(t, test.manifestNamespace)
			var requests int
			var gotNamespace, gotReason, gotBodyNamespace string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPut || r.URL.Path != memoryBackendUpdatePath {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				gotNamespace = r.URL.Query().Get(memoryBackendUpdateNamespaceKey)
				gotReason = r.URL.Query().Get(memoryBackendUpdateReasonKey)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				metadata, _ := body[memoryBackendUpdateMetadataKey].(map[string]any)
				gotBodyNamespace = strings.TrimSpace(anyString(metadata[memoryBackendUpdateNamespaceKey]))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()

			if test.configuredNamespace != "" {
				if err := saveConfig(orkaConfig{Namespace: test.configuredNamespace}); err != nil {
					t.Fatal(err)
				}
			}
			root := newRootCmd()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			args := []string{
				memoryBackendUpdateRootName, memoryBackendUpdateGroupName, memoryBackendUpdateCommandName,
				memoryBackendUpdateServerFlag, server.URL,
				memoryBackendUpdateFileFlag, manifestPath,
				memoryBackendUpdateReasonFlag, memoryBackendUpdateReason,
				memoryBackendUpdateConfirmFlag,
			}
			if test.explicitNamespace != "" {
				args = append(args, memoryBackendUpdateNamespaceFlag, test.explicitNamespace)
			}
			root.SetArgs(args)
			err := root.Execute()
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Execute() error = %v, want %q", err, test.wantError)
				}
				if requests != 0 {
					t.Fatalf("requests = %d, want no request after namespace mismatch", requests)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute() error: %v\n%s", err, output.String())
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1", requests)
			}
			if gotNamespace != test.wantNamespace || gotBodyNamespace != test.wantNamespace {
				t.Fatalf("query/body namespace = %q/%q, want %q", gotNamespace, gotBodyNamespace, test.wantNamespace)
			}
			if gotReason != memoryBackendUpdateReason {
				t.Fatalf("reason query = %q, want %q", gotReason, memoryBackendUpdateReason)
			}
		})
	}
}

func TestMemoryBackendCreateManifestNamespaceRouting(t *testing.T) {
	tests := []struct {
		name                string
		manifestNamespace   string
		configuredNamespace string
		explicitNamespace   string
		wantNamespace       string
		wantError           string
	}{
		{
			name:                "manifest overrides configured default when flag omitted",
			manifestNamespace:   memoryBackendUpdateManifestNamespace,
			configuredNamespace: memoryBackendUpdateConfiguredNamespace,
			wantNamespace:       memoryBackendUpdateManifestNamespace,
		},
		{
			name:              "matching explicit namespace",
			manifestNamespace: memoryBackendUpdateManifestNamespace,
			explicitNamespace: memoryBackendUpdateManifestNamespace,
			wantNamespace:     memoryBackendUpdateManifestNamespace,
		},
		{
			name:              "explicit namespace fills omitted manifest namespace",
			explicitNamespace: memoryBackendUpdateExplicitNamespace,
			wantNamespace:     memoryBackendUpdateExplicitNamespace,
		},
		{
			name:              "mismatched explicit namespace",
			manifestNamespace: memoryBackendUpdateManifestNamespace,
			explicitNamespace: memoryBackendUpdateExplicitNamespace,
			wantError:         "manifest namespace \"team-a\" does not match --namespace \"team-b\"",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			manifestPath := writeMemoryBackendUpdateManifest(t, test.manifestNamespace)
			var requests int
			var gotNamespace, gotReason, gotBodyNamespace string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests++
				if r.Method != http.MethodPost || r.URL.Path != "/api/v1/memory-backends" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				gotNamespace = r.URL.Query().Get(memoryBackendUpdateNamespaceKey)
				gotReason = r.URL.Query().Get(memoryBackendUpdateReasonKey)
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode request: %v", err)
				}
				metadata, _ := body[memoryBackendUpdateMetadataKey].(map[string]any)
				gotBodyNamespace = strings.TrimSpace(anyString(metadata[memoryBackendUpdateNamespaceKey]))
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(body)
			}))
			defer server.Close()

			if test.configuredNamespace != "" {
				if err := saveConfig(orkaConfig{Namespace: test.configuredNamespace}); err != nil {
					t.Fatal(err)
				}
			}
			root := newRootCmd()
			var output bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&output)
			args := []string{
				memoryBackendUpdateRootName, memoryBackendUpdateGroupName, "create",
				memoryBackendUpdateServerFlag, server.URL,
				memoryBackendUpdateFileFlag, manifestPath,
				memoryBackendUpdateReasonFlag, "create backend configuration",
				memoryBackendUpdateConfirmFlag,
			}
			if test.explicitNamespace != "" {
				args = append(args, memoryBackendUpdateNamespaceFlag, test.explicitNamespace)
			}
			root.SetArgs(args)
			err := root.Execute()
			if test.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantError) {
					t.Fatalf("Execute() error = %v, want %q", err, test.wantError)
				}
				if requests != 0 {
					t.Fatalf("requests = %d, want no request after namespace mismatch", requests)
				}
				return
			}
			if err != nil {
				t.Fatalf("Execute() error: %v\n%s", err, output.String())
			}
			if requests != 1 {
				t.Fatalf("requests = %d, want 1", requests)
			}
			if gotNamespace != test.wantNamespace || gotBodyNamespace != test.wantNamespace {
				t.Fatalf("query/body namespace = %q/%q, want %q", gotNamespace, gotBodyNamespace, test.wantNamespace)
			}
			if gotReason != "create backend configuration" {
				t.Fatalf("reason query = %q, want create backend configuration", gotReason)
			}
		})
	}
}

func TestMemoryCommandsWithoutOperandsRejectPositionalArgs(t *testing.T) {
	tests := []struct {
		name string
		new  func() *cobra.Command
	}{
		{name: "memory list", new: newMemoryListCmd},
		{name: "memory create", new: newMemoryCreateCmd},
		{name: "memory proposal list", new: newMemoryProposalListCmd},
		{name: "memory operation list", new: newMemoryOperationListCmd},
		{name: "memory backend list", new: newMemoryBackendListCmd},
		{name: "memory backend get", new: func() *cobra.Command { return newMemoryBackendGetCmd(false) }},
		{name: "memory backend status", new: func() *cobra.Command { return newMemoryBackendGetCmd(true) }},
		{name: "memory backend create", new: newMemoryBackendCreateCmd},
		{name: "memory backend update", new: newMemoryBackendUpdateCmd},
		{name: "memory backend delete", new: newMemoryBackendDeleteCmd},
		{name: "memory backend checkpoint", new: newMemoryBackendCheckpointCmd},
		{name: "memory backend purge", new: newMemoryBackendPurgeCmd},
		{name: "memory backend activate", new: func() *cobra.Command { return newMemoryBackendActionCmd("activate") }},
		{
			name: "memory backend decommission",
			new:  func() *cobra.Command { return newMemoryBackendActionCmd("decommission") },
		},
		{
			name: "memory backend force-orphan",
			new:  func() *cobra.Command { return newMemoryBackendActionCmd("force-orphan") },
		},
		{
			name: "memory backend restore-legacy",
			new:  func() *cobra.Command { return newMemoryBackendActionCmd("restore-legacy") },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cmd := test.new()
			if cmd.Args == nil {
				t.Fatal("Args validator is nil")
			}
			if err := cmd.Args(cmd, nil); err != nil {
				t.Fatalf("Args(nil) error = %v", err)
			}
			if err := cmd.Args(cmd, []string{"unexpected"}); err == nil {
				t.Fatal("unexpected positional argument was accepted")
			}
		})
	}
}

func TestMemoryBackendDestructiveCommandsRejectPositionalArgsBeforeRequest(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var requests int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{}`))
	}))
	defer server.Close()

	tests := []struct {
		name string
		args []string
	}{
		{name: "delete", args: []string{"delete", "wrong-target", "--reason", "test", "--yes"}},
		{name: "checkpoint", args: []string{
			"checkpoint", "wrong-target", "--manifest-digest", "sha256:test", "--reason", "test", "--yes",
		}},
		{name: "purge", args: []string{
			"purge", "wrong-target", "--checkpoint-id", "checkpoint-1",
			"--before", "2026-08-01T00:00:00Z", "--payloads", "--reason", "test", "--yes",
		}},
		{name: "force-orphan", args: []string{"force-orphan", "wrong-target", "--reason", "test", "--yes"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := newRootCmd()
			root.SetArgs(append([]string{"memory", "backend"}, append(test.args, "--server", server.URL)...))
			if err := root.Execute(); err == nil {
				t.Fatal("unexpected positional argument was accepted")
			}
		})
	}
	if requests != 0 {
		t.Fatalf("requests = %d, want no requests after positional-argument rejection", requests)
	}
}

func writeMemoryBackendUpdateManifest(t *testing.T, namespace string) string {
	t.Helper()
	metadata := map[string]any{memoryBackendUpdateNameKey: memoryBackendUpdateDefaultName}
	if namespace != "" {
		metadata[memoryBackendUpdateNamespaceKey] = namespace
	}
	body, err := json.Marshal(map[string]any{
		"apiVersion":                   "core.orka.ai/v1alpha1",
		"kind":                         "MemoryBackend",
		memoryBackendUpdateMetadataKey: metadata,
		memoryBackendUpdateSpecKey: map[string]any{
			memoryBackendUpdateLifecycleStateKey: "Staged",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "memory-backend.json")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
