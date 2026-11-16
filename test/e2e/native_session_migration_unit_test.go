//go:build e2e && !windows

package e2e

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/orka-agents/orka/internal/codexstate"
	"github.com/orka-agents/orka/test/utils"
	"github.com/orka-agents/sessionkit/harness/codex/writerlock"
)

func TestNativeMigrationSourceNonceOnlyInToolOutput(t *testing.T) {
	projectDir, err := utils.GetProjectDir()
	if err != nil {
		t.Fatal(err)
	}
	// TestE2E's command runner leaves the process at the repository root.
	// Exercise that full-binary ordering even in focused unit runs.
	t.Chdir(projectDir)
	fixture := "rollout-2026-10-02T21-57-44-" + nativeMigrationThread + ".jsonl"
	original, err := os.ReadFile(filepath.Join(projectDir, "internal/codexstate/testdata", fixture))
	if err != nil {
		t.Fatal(err)
	}
	rollout, err := nativeMigrationSourceRollout(original)
	if err != nil {
		t.Fatal(err)
	}
	var outputs int
	for line := range bytes.SplitSeq(bytes.TrimSpace(rollout), []byte("\n")) {
		var record struct {
			Type    string `json:"type"`
			Payload struct {
				Type      string          `json:"type"`
				Arguments string          `json:"arguments"`
				Output    string          `json:"output"`
				Content   json.RawMessage `json:"content"`
			} `json:"payload"`
		}
		if err := json.Unmarshal(line, &record); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(record.Payload.Arguments, nativeMigrationNonce) ||
			bytes.Contains(record.Payload.Content, []byte(nativeMigrationNonce)) {
			t.Fatal("source nonce is present in a prompt, assistant message, or tool argument")
		}
		if record.Type == "response_item" && record.Payload.Type == "function_call_output" &&
			strings.Contains(record.Payload.Output, nativeMigrationNonce) {
			outputs++
		}
	}
	if outputs != 1 {
		t.Fatalf("got %d native nonce tool outputs, want exactly one", outputs)
	}
	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(home, "sessions/2026/10/02", fixture)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, rollout, 0o600); err != nil {
		t.Fatal(err)
	}
	lock, err := writerlock.Source(t.Context(), home, nativeMigrationThread)
	if err != nil {
		t.Fatal(err)
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	before, err := nativeMigrationHomeFiles(home)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := codexstate.Capture(t.Context(), home, nativeMigrationThread)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := codexstate.Inspect(t.Context(), bundle)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ThreadID != nativeMigrationThread {
		t.Fatal("source transformation changed the native thread identity")
	}
	after, err := nativeMigrationHomeFiles(home)
	if err != nil {
		t.Fatal(err)
	}
	if !nativeMigrationHomesEqual(before, after) {
		t.Fatal("capture changed the stopped source files or native lock namespace")
	}
}

func TestNativeMigrationSourceRejectsMalformedRecord(t *testing.T) {
	for _, data := range []string{`{`, `{}`, `{"payload":null}`} {
		if _, err := nativeMigrationSourceRollout([]byte(data)); err == nil {
			t.Fatal("malformed source record was accepted")
		}
	}
}

func TestNativeMigrationFixturesMatchContinuationMarkersAndPreferReturnLeg(t *testing.T) {
	projectDir, err := utils.GetProjectDir()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(projectDir)
	data, err := os.ReadFile(filepath.Join(projectDir, mockLLMFixtureDir, "native_session_migration.json"))
	if err != nil {
		t.Fatal(err)
	}
	var file struct {
		Fixtures []struct {
			Match    map[string]any `json:"match"`
			Response struct {
				Content string `json:"content"`
			} `json:"response"`
		} `json:"fixtures"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	if len(file.Fixtures) != 2 {
		t.Fatal("expected both native migration legs")
	}
	for i, want := range []struct{ marker, reply string }{
		{nativeMigrationLocalMarker, nativeMigrationLocalReply},
		{nativeMigrationClusterMarker, nativeMigrationClusterReply},
	} {
		fixture := file.Fixtures[i]
		// A continuation ends in a new user message. aimock's toolResultContains
		// checks only the final message, not the historical tool output. The
		// E2E journal assertion, not a last-message matcher, proves native replay.
		if len(fixture.Match) != 1 || fixture.Match["userMessage"] != want.marker ||
			fixture.Response.Content != want.reply {
			t.Fatal("native fixture must match its continuation marker, with the local return first")
		}
		if strings.Contains(fixture.Response.Content, nativeMigrationNonce) {
			t.Fatal("response must not duplicate the native-only nonce")
		}
	}
}

func TestNativeMigrationLocalProviderRequiresCurrentCredential(t *testing.T) {
	var forwarded int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		forwarded++
		if r.URL.Path != "/v1/responses" {
			t.Error("provider proxy changed the request path")
		}
		_, _ = io.WriteString(w, "scripted reply")
	}))
	t.Cleanup(upstream.Close)
	const currentKey = "native-return-current-test-key"
	provider, err := nativeMigrationLocalProvider(upstream.URL, currentKey)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(provider.Close)
	for _, test := range []struct {
		name, key string
		status    int
	}{
		{"missing", "", http.StatusUnauthorized},
		{"source", "revoked-native-source-test-key", http.StatusUnauthorized},
		{"current", currentKey, http.StatusOK},
	} {
		t.Run(test.name, func(t *testing.T) {
			req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, provider.URL+"/v1/responses", nil)
			if err != nil {
				t.Fatal(err)
			}
			if test.key != "" {
				req.Header.Set("Authorization", "Bearer "+test.key)
			}
			response, err := provider.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != test.status {
				t.Fatalf("HTTP %d, want %d", response.StatusCode, test.status)
			}
		})
	}
	if forwarded != 1 {
		t.Fatalf("upstream got %d requests, want only the current credential", forwarded)
	}
}

func TestNativeMigrationCurrentPolicyMustComeFromReturnedTurn(t *testing.T) {
	current := []byte(`{"type":"turn_context","payload":{"cwd":"/current","model":"gpt-4.1",` +
		`"approval_policy":"never","sandbox_policy":{"type":"read-only"}}}`)
	if !nativeMigrationCurrentPolicy(current, "/current") {
		t.Fatal("current destination policy not recognized")
	}
	for _, stale := range [][]byte{
		bytes.ReplaceAll(current, []byte("/current"), []byte("/source")),
		bytes.ReplaceAll(current, []byte("gpt-4.1"), []byte("source-model")),
		bytes.ReplaceAll(current, []byte("never"), []byte("untrusted")),
		bytes.ReplaceAll(current, []byte("read-only"), []byte("danger-full-access")),
	} {
		if nativeMigrationCurrentPolicy(stale, "/current") {
			t.Fatal("source policy was accepted as current destination policy")
		}
	}
}

func TestNativeMigrationSourceSnapshotDetectsMutation(t *testing.T) {
	for _, change := range []string{"unchanged", "contents", "replacement", "mode", "mtime", "added", "deleted"} {
		t.Run(change, func(t *testing.T) {
			home := t.TempDir()
			path := filepath.Join(home, "source.jsonl")
			original := []byte("original native history\n")
			if err := os.WriteFile(path, original, 0o600); err != nil {
				t.Fatal(err)
			}
			before, err := nativeMigrationHomeFiles(home)
			if err != nil {
				t.Fatal(err)
			}
			switch change {
			case "contents":
				err = os.WriteFile(path, []byte("changed native history\n"), 0o600)
			case "replacement":
				replacement := path + ".new"
				if err := os.WriteFile(replacement, original, 0o600); err != nil {
					t.Fatal(err)
				}
				stamp := before["source.jsonl"].Info.ModTime()
				if err := os.Chtimes(replacement, stamp, stamp); err != nil {
					t.Fatal(err)
				}
				err = os.Rename(replacement, path)
			case "mode":
				err = os.Chmod(path, 0o400)
			case "mtime":
				stamp := before["source.jsonl"].Info.ModTime().Add(time.Hour)
				err = os.Chtimes(path, stamp, stamp)
			case "added":
				err = os.WriteFile(filepath.Join(home, "added"), nil, 0o600)
			case "deleted":
				err = os.Remove(path)
			}
			if err != nil {
				t.Fatal(err)
			}
			after, err := nativeMigrationHomeFiles(home)
			if err != nil {
				t.Fatal(err)
			}
			if nativeMigrationHomesEqual(before, after) != (change == "unchanged") {
				t.Fatal("source snapshot failed to distinguish unchanged files from a mutation")
			}
		})
	}
}
