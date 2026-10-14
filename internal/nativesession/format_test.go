package nativesession

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

const (
	codexFixtureThread = "01a10020-1222-76e3-977d-d5165792ae72"
	codexFixtureName   = "rollout-2026-10-02T21-57-44-" + codexFixtureThread + ".jsonl"
	codexFixtureLimit  = 1 << 20
)

func realDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return dir
}

// codexFixtureDirs writes codexstate's recorded Codex 0.160.0 rollout into a
// CODEX_HOME and returns its Dirs, the rollout path, and its bytes.
func codexFixtureDirs(t *testing.T) (Dirs, string, []byte) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "codexstate", "testdata", codexFixtureName))
	if err != nil {
		t.Fatal(err)
	}
	dirs := Dirs{State: filepath.Join(realDir(t), ".codex"), Workspace: realDir(t)}
	rollout := filepath.Join(dirs.State, "sessions", "2026", "10", "02", codexFixtureName)
	if err := os.MkdirAll(filepath.Dir(rollout), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rollout, data, 0o600); err != nil {
		t.Fatal(err)
	}
	return dirs, rollout, data
}

func TestForRegistersTheNativeFormats(t *testing.T) {
	for _, kind := range []string{"claude", "codex", "copilot", "opencode"} {
		if !Supported(kind) {
			t.Fatalf("%s must save and restore native sessions", kind)
		}
	}
	for _, kind := range []string{"", "agentkit", "Codex"} {
		if _, ok := For(kind); ok || Supported(kind) {
			t.Fatalf("%q must not have a native session format", kind)
		}
	}
	if kinds := Kinds(); !slices.Equal(kinds, []string{"claude", "codex", "copilot", "opencode"}) {
		t.Fatalf("kinds: %v", kinds)
	}
}

// The runtime supervisor sets CODEX_HOME to HOME/.codex, CLAUDE_CONFIG_DIR to
// HOME/.claude, COPILOT_HOME to HOME/.copilot, and XDG_DATA_HOME to
// the session's data directory.
func TestRuntimeDirsMatchTheRuntimeEnvironment(t *testing.T) {
	for kind, want := range map[string]Dirs{
		"codex":    {State: "/s/home/.codex", Workspace: "/s/workspace"},
		"claude":   {State: "/s/home/.claude", Workspace: "/s/workspace"},
		"copilot":  {State: "/s/home/.copilot", Workspace: "/s/workspace"},
		"opencode": {State: "/s/xdg/data", Workspace: "/s/workspace"},
	} {
		format, _ := For(kind)
		if got := RuntimeDirs(format, "/s/home", "/s/xdg/data", "/s/workspace"); got != want {
			t.Errorf("%s: got %+v, want %+v", kind, got, want)
		}
	}
}

func TestCodexFormatCapturesInspectsAndInstalls(t *testing.T) {
	format, _ := For("codex")
	source, _, original := codexFixtureDirs(t)
	data, err := format.Capture(t.Context(), source, codexFixtureThread, codexFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := format.Inspect(t.Context(), data, codexFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProviderSessionID != codexFixtureThread || summary.ProviderVersion != "0.160.0" || summary.DataDigest == "" {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	// The Codex provider's PrepareSession creates the private home first.
	destination := Dirs{State: filepath.Join(realDir(t), ".codex"), Workspace: realDir(t)}
	if err := os.Mkdir(destination.State, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := format.Install(t.Context(), data, destination, realDir(t), codexFixtureLimit); err != nil {
		t.Fatal(err)
	}
	var installed []byte
	err = filepath.WalkDir(filepath.Join(destination.State, "sessions"), func(path string, entry os.DirEntry, err error) error {
		if err == nil && !entry.IsDir() && filepath.Ext(path) == ".jsonl" {
			installed, err = os.ReadFile(path)
		}
		return err
	})
	if err != nil || !bytes.Equal(installed, original) {
		t.Fatalf("installed rollout differs from the captured one: %v", err)
	}
}

func TestCodexFormatMarksUnsupportedContext(t *testing.T) {
	format, _ := For("codex")
	source, rollout, original := codexFixtureDirs(t)
	encrypted := []byte("{\"ordinal\":999,\"timestamp\":\"2026-10-03T04:58:00Z\",\"type\":\"response_item\",\"payload\":{\"type\":\"reasoning\",\"encrypted_content\":\"opaque\",\"summary\":[]}}\n")
	if err := os.WriteFile(rollout, append(original, encrypted...), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := format.Capture(t.Context(), source, codexFixtureThread, codexFixtureLimit); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("encrypted context must be reported as unsupported: %v", err)
	}
	if _, err := format.Inspect(t.Context(), []byte(`{"format":"other"}`), codexFixtureLimit); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("a malformed bundle is invalid, not an unsupported format: %v", err)
	}
}
