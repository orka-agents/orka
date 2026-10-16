package nativesession

import (
	"bytes"
	"errors"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/orka-agents/orka/internal/acp"
)

const (
	claudeFixtureSession = "30840f35-9928-4d2d-8785-873a00b244bb"
	claudeFixtureLimit   = 1 << 20
)

// claudeFixture is a stopped Claude Code session in a CLAUDE_CONFIG_DIR
// that also holds another session.
type claudeFixture struct {
	dirs           Dirs
	transcript     string
	toolResultsDir string
	spill          []byte
}

func claudeFixtureEntries(workspace, toolResultsDir, version string) string {
	spillPath := filepath.Join(toolResultsDir, "toolu_01.txt")
	return strings.Join([]string{
		`{"type":"user","sessionId":"` + claudeFixtureSession + `","version":"` + version + `","cwd":"` + workspace + `","message":{"role":"user","content":"Remember nonce-fixture"}}`,
		`{"type":"assistant","sessionId":"` + claudeFixtureSession + `","version":"` + version + `","cwd":"` + workspace + `","message":{"role":"assistant","content":[{"type":"tool_use","id":"toolu_01","name":"Bash","input":{"command":"cat big.txt"}}]}}`,
		`{"type":"user","sessionId":"` + claudeFixtureSession + `","version":"` + version + `","cwd":"` + workspace + `","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_01","content":"Output too large. Full output saved to: ` + spillPath + `"}]},"toolUseResult":{"persistedOutputPath":"` + spillPath + `"}}`,
		`{"type":"last-prompt","sessionId":"` + claudeFixtureSession + `","lastPrompt":"Remember nonce-fixture"}`,
	}, "\n") + "\n"
}

func newClaudeFixture(t *testing.T) claudeFixture {
	t.Helper()
	root := realDir(t)
	dirs := Dirs{State: filepath.Join(root, "home", ".claude"), Workspace: filepath.Join(root, "workspace")}
	folder := filepath.Join(dirs.State, "projects", claudeProjectFolder(dirs.Workspace))
	toolResultsDir := filepath.Join(folder, claudeFixtureSession, "tool-results")
	other := filepath.Join(dirs.State, "projects", "-src-other")
	for _, dir := range []string{toolResultsDir, dirs.Workspace, other} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	spill := []byte(strings.Repeat("spilled tool output\n", 64))
	transcript := filepath.Join(folder, claudeFixtureSession+".jsonl")
	for path, content := range map[string][]byte{
		filepath.Join(toolResultsDir, "toolu_01.txt"): spill,
		transcript: []byte(claudeFixtureEntries(dirs.Workspace, toolResultsDir, claudeCodeVersion)),
		filepath.Join(other, "8f1d0c3e-0000-4000-8000-000000000000.jsonl"): []byte(`{"type":"user","sessionId":"8f1d0c3e-0000-4000-8000-000000000000","message":"other-private"}` + "\n"),
	} {
		if err := os.WriteFile(path, content, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return claudeFixture{dirs: dirs, transcript: transcript, toolResultsDir: toolResultsDir, spill: spill}
}

func freshClaudeDirs(t *testing.T) Dirs {
	t.Helper()
	root := realDir(t)
	dirs := Dirs{State: filepath.Join(root, "home", ".claude"), Workspace: filepath.Join(root, "workspace")}
	for _, dir := range []string{filepath.Dir(dirs.State), dirs.Workspace} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	return dirs
}

func TestClaudeVersionMatchesPinnedRuntime(t *testing.T) {
	if claudeCodeVersion != acp.ClaudeCodeVersion {
		t.Fatalf("native Claude Code format %s does not match the pinned Claude Code %s", claudeCodeVersion, acp.ClaudeCodeVersion)
	}
}

func TestClaudeFormatCarriesTheSessionIntoANewHome(t *testing.T) {
	format, ok := For("claude")
	if !ok {
		t.Fatal("claude must save and restore native sessions")
	}
	source := newClaudeFixture(t)
	data, err := format.Capture(t.Context(), source.dirs, claudeFixtureSession, claudeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(gunzipForTest(t, data), []byte("other-private")) {
		t.Fatal("capture carried another session")
	}
	summary, err := format.Inspect(t.Context(), data, claudeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProviderSessionID != claudeFixtureSession || summary.ProviderVersion != claudeCodeVersion || summary.DataDigest != dataDigest(data) {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	destination := freshClaudeDirs(t)
	if err := format.Install(t.Context(), data, destination, realDir(t), claudeFixtureLimit); err != nil {
		t.Fatal(err)
	}
	folder := filepath.Join(destination.State, "projects", claudeProjectFolder(destination.Workspace))
	newToolResults := filepath.Join(folder, claudeFixtureSession, "tool-results")
	installed, err := os.ReadFile(filepath.Join(folder, claudeFixtureSession+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if want := claudeFixtureEntries(source.dirs.Workspace, newToolResults, claudeCodeVersion); string(installed) != want {
		t.Fatalf("installed transcript:\n%s\nwant:\n%s", installed, want)
	}
	spill, err := os.ReadFile(filepath.Join(newToolResults, "toolu_01.txt"))
	if err != nil || !bytes.Equal(spill, source.spill) {
		t.Fatalf("spilled tool result was not carried: %v", err)
	}
	// A retry after an unacknowledged install finds exactly what it wrote.
	if err := format.Install(t.Context(), data, destination, realDir(t), claudeFixtureLimit); err != nil {
		t.Fatalf("repeating a completed install must succeed: %v", err)
	}
	appendForTest(t, source.transcript, `{"type":"user","sessionId":"`+claudeFixtureSession+`","message":"later turn"}`+"\n")
	later, err := format.Capture(t.Context(), source.dirs, claudeFixtureSession, claudeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	if err := format.Install(t.Context(), later, destination, realDir(t), claudeFixtureLimit); err == nil {
		t.Fatal("install must not overwrite another conversation of the session")
	}
}

func TestClaudeInstallReclaimsAKilledInstall(t *testing.T) {
	format, _ := For("claude")
	data, err := format.Capture(t.Context(), newClaudeFixture(t).dirs, claudeFixtureSession, claudeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	stage := func(t *testing.T, extra string) (Dirs, string) {
		t.Helper()
		destination := freshClaudeDirs(t)
		folder := filepath.Join(destination.State, "projects", claudeProjectFolder(destination.Workspace))
		toolResults := filepath.Join(folder, claudeFixtureSession, "tool-results")
		if err := os.MkdirAll(toolResults, 0o700); err != nil {
			t.Fatal(err)
		}
		// The install was killed while it wrote the tool result and the
		// staged transcript, before it published the transcript.
		writeForTest(t, filepath.Join(toolResults, "toolu_01.txt"), "par")
		writeForTest(t, filepath.Join(folder, "."+claudeFixtureSession+".jsonl.restore"), `{"type":"us`)
		if extra != "" {
			writeForTest(t, filepath.Join(folder, claudeFixtureSession, extra), "not staging")
		}
		return destination, toolResults
	}
	destination, toolResults := stage(t, "")
	if err := format.Install(t.Context(), data, destination, realDir(t), claudeFixtureLimit); err != nil {
		t.Fatalf("a retry must replace a killed install's staging: %v", err)
	}
	if spill, err := os.ReadFile(filepath.Join(toolResults, "toolu_01.txt")); err != nil || string(spill) == "par" {
		t.Fatalf("the retry kept the partial tool result: %v", err)
	}
	destination, toolResults = stage(t, "subagents")
	if err := format.Install(t.Context(), data, destination, realDir(t), claudeFixtureLimit); err == nil {
		t.Fatal("a retry must not remove session state an install did not stage")
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(toolResults), "subagents")); err != nil {
		t.Fatalf("the refused retry removed state it did not stage: %v", err)
	}
}

func TestClaudeCaptureFallsBackOnStateItDoesNotCarry(t *testing.T) {
	format, _ := For("claude")
	for name, test := range map[string]struct {
		mutate func(*testing.T, claudeFixture)
		reason string
	}{
		"newer Claude Code": {reason: "Claude Code 2.1.999 wrote", mutate: func(t *testing.T, f claudeFixture) {
			writeForTest(t, f.transcript, claudeFixtureEntries(f.dirs.Workspace, f.toolResultsDir, "2.1.999"))
		}},
		"newer Claude Code earlier in the transcript": {reason: "Claude Code 2.1.999 wrote", mutate: func(t *testing.T, f claudeFixture) {
			writeForTest(t, f.transcript, claudeFixtureEntries(f.dirs.Workspace, f.toolResultsDir, "2.1.999")+
				claudeFixtureEntries(f.dirs.Workspace, f.toolResultsDir, claudeCodeVersion))
		}},
		"unknown entry type": {reason: `type "future-entry", which Claude Code 2.1.293 does not read`, mutate: func(t *testing.T, f claudeFixture) {
			appendForTest(t, f.transcript, `{"type":"future-entry","sessionId":"`+claudeFixtureSession+`"}`+"\n")
		}},
		"missing entry type": {reason: `type "", which Claude Code 2.1.293 does not read`, mutate: func(t *testing.T, f claudeFixture) {
			appendForTest(t, f.transcript, `{"sessionId":"`+claudeFixtureSession+`"}`+"\n")
		}},
		"null entry type": {reason: `type "", which Claude Code 2.1.293 does not read`, mutate: func(t *testing.T, f claudeFixture) {
			appendForTest(t, f.transcript, `{"type":null,"sessionId":"`+claudeFixtureSession+`"}`+"\n")
		}},
		"empty entry type": {reason: `type "", which Claude Code 2.1.293 does not read`, mutate: func(t *testing.T, f claudeFixture) {
			appendForTest(t, f.transcript, `{"type":"","sessionId":"`+claudeFixtureSession+`"}`+"\n")
		}},
		"unparseable release": {reason: `records release "2.1"`, mutate: func(t *testing.T, f claudeFixture) {
			appendForTest(t, f.transcript, `{"type":"user","sessionId":"`+claudeFixtureSession+`","version":"2.1"}`+"\n")
		}},
		"entry with trailing data": {reason: "not one JSON object", mutate: func(t *testing.T, f claudeFixture) {
			appendForTest(t, f.transcript, `{"type":"user","sessionId":"`+claudeFixtureSession+`"}]`+"\n")
		}},
		"entry of no session": {reason: "of another session or of none", mutate: func(t *testing.T, f claudeFixture) {
			appendForTest(t, f.transcript, `{"type":"user","message":"unowned"}`+"\n")
		}},
		"another session's entry": {reason: "another session", mutate: func(t *testing.T, f claudeFixture) {
			appendForTest(t, f.transcript, `{"type":"user","sessionId":"8f1d0c3e-0000-4000-8000-000000000000"}`+"\n")
		}},
		"partial entry": {reason: "partial entry", mutate: func(t *testing.T, f claudeFixture) {
			appendForTest(t, f.transcript, `{"type":"user","sessionId":"`)
		}},
		"entry that is not an object": {reason: "not one JSON object", mutate: func(t *testing.T, f claudeFixture) {
			appendForTest(t, f.transcript, "null\n")
		}},
		"session in two folders": {reason: "more than one project folder", mutate: func(t *testing.T, f claudeFixture) {
			copyPath := filepath.Join(f.dirs.State, "projects", "-src-other", claudeFixtureSession+".jsonl")
			writeForTest(t, copyPath, claudeFixtureEntries(f.dirs.Workspace, f.toolResultsDir, claudeCodeVersion))
		}},
		"too many spilled results": {reason: "more than 256 tool results", mutate: func(t *testing.T, f claudeFixture) {
			for i := range maxClaudeToolResults {
				writeForTest(t, filepath.Join(f.toolResultsDir, "toolu_extra"+strings.Repeat("x", i%3)+string(rune('a'+i%26))+string(rune('a'+i/26))+".txt"), "x")
			}
		}},
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newClaudeFixture(t)
			test.mutate(t, fixture)
			_, err := format.Capture(t.Context(), fixture.dirs, claudeFixtureSession, claudeFixtureLimit)
			if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("capture must fall back to the transcript because of %q: %v", test.reason, err)
			}
		})
	}
	fixture := newClaudeFixture(t)
	// Claude Code identifies a summary by its leaf and writes no session ID.
	appendForTest(t, fixture.transcript, `{"type":"summary","summary":"Read LICENSE","leafUuid":"u2"}`+"\n")
	if _, err := format.Capture(t.Context(), fixture.dirs, claudeFixtureSession, claudeFixtureLimit); err != nil {
		t.Fatalf("a summary entry must be carried: %v", err)
	}
	if _, err := format.Capture(t.Context(), fixture.dirs, "8f1d0c3e-0000-4000-8000-00000000ffff", claudeFixtureLimit); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("a missing session is a capture failure, not an unsupported conversation: %v", err)
	}
	if _, err := format.Capture(t.Context(), fixture.dirs, claudeFixtureSession, 256); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("an oversized conversation must fall back to the transcript: %v", err)
	}
}

func TestClaudeCaptureRejectsSymlinks(t *testing.T) {
	format, _ := For("claude")
	outside := filepath.Join(realDir(t), "secret")
	writeForTest(t, outside, "not a transcript\n")
	for name, plant := range map[string]func(claudeFixture) string{
		"transcript":  func(f claudeFixture) string { return f.transcript },
		"tool result": func(f claudeFixture) string { return filepath.Join(f.toolResultsDir, "toolu_01.txt") },
	} {
		t.Run(name, func(t *testing.T) {
			fixture := newClaudeFixture(t)
			target := plant(fixture)
			_ = os.Remove(target)
			if err := os.Symlink(outside, target); err != nil {
				t.Fatal(err)
			}
			if _, err := format.Capture(t.Context(), fixture.dirs, claudeFixtureSession, claudeFixtureLimit); err == nil {
				t.Fatal("capture followed a symlink")
			}
		})
	}
}

func TestClaudeInspectRejectsTamperedBundles(t *testing.T) {
	format, _ := For("claude")
	fixture := newClaudeFixture(t)
	data, err := format.Capture(t.Context(), fixture.dirs, claudeFixtureSession, claudeFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	entries := tarEntriesForTest(t, data)
	rebuild := func(change func(map[string][]byte)) []byte {
		copied := map[string][]byte{}
		maps.Copy(copied, entries)
		change(copied)
		return tarGzipForTest(t, copied)
	}
	if _, err := format.Inspect(t.Context(), rebuild(func(map[string][]byte) {}), claudeFixtureLimit); err != nil {
		t.Fatalf("a faithful rebuild must inspect: %v", err)
	}
	newer := rebuild(func(e map[string][]byte) {
		e["session.jsonl"] = []byte(claudeFixtureEntries(fixture.dirs.Workspace, fixture.toolResultsDir, "2.1.999"))
		e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(`"`+claudeCodeVersion+`"`), []byte(`"2.1.999"`), 1)
	})
	if _, err := format.Inspect(t.Context(), newer, claudeFixtureLimit); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a later Claude Code's transcript must be unsupported: %v", err)
	}
	for name, tampered := range map[string][]byte{
		"unknown entry": rebuild(func(e map[string][]byte) { e["extra"] = []byte("x") }),
		"other session": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(claudeFixtureSession), []byte("8f1d0c3e-0000-4000-8000-000000000000"), 1)
		}),
		"manifest release": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(`"`+claudeCodeVersion+`"`), []byte(`"2.1.200"`), 1)
		}),
		"no transcript": rebuild(func(e map[string][]byte) { delete(e, "session.jsonl") }),
		"no manifest":   rebuild(func(e map[string][]byte) { delete(e, "manifest.json") }),
		"invalid entry": rebuild(func(e map[string][]byte) { e["session.jsonl"] = append(e["session.jsonl"], "[]\n"...) }),
		"unknown entry type": rebuild(func(e map[string][]byte) {
			e["session.jsonl"] = append(e["session.jsonl"], `{"type":"future-entry"}`+"\n"...)
		}),
		"missing entry type": rebuild(func(e map[string][]byte) {
			e["session.jsonl"] = append(e["session.jsonl"], `{"sessionId":"`+claudeFixtureSession+`"}`+"\n"...)
		}),
		"null entry type": rebuild(func(e map[string][]byte) {
			e["session.jsonl"] = append(e["session.jsonl"], `{"type":null,"sessionId":"`+claudeFixtureSession+`"}`+"\n"...)
		}),
		"empty entry type": rebuild(func(e map[string][]byte) {
			e["session.jsonl"] = append(e["session.jsonl"], `{"type":"","sessionId":"`+claudeFixtureSession+`"}`+"\n"...)
		}),
		"relative spills": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(`"/`), []byte(`"`), 1)
		}),
		"spills outside the session": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(`/tool-results"`), []byte(`"`), 1)
		}),
		"another session's spills": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(claudeFixtureSession+`/tool-results`), []byte(`8f1d0c3e-0000-4000-8000-000000000000/tool-results`), 1)
		}),
		"spills outside a projects folder": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(`/projects/`), []byte(`/elsewhere/`), 1)
		}),
		"manifest field": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(`{`), []byte(`{"extra":1,`), 1)
		}),
		"manifest tail": rebuild(func(e map[string][]byte) { e["manifest.json"] = append(e["manifest.json"], ']') }),
		"trailing data": append(append([]byte(nil), data...), data...),
		"not gzip":      []byte("plain"),
	} {
		if _, err := format.Inspect(t.Context(), tampered, claudeFixtureLimit); err == nil || errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: must be rejected as invalid: %v", name, err)
		}
	}
}

func TestClaudeInstallBoundsRelocationGrowth(t *testing.T) {
	format, _ := For("claude")
	source := newClaudeFixture(t)
	// Many references to the spilled results, each of which grows when the
	// new location is much longer.
	var transcript strings.Builder
	transcript.WriteString(claudeFixtureEntries(source.dirs.Workspace, source.toolResultsDir, claudeCodeVersion))
	reference := `{"type":"user","sessionId":"` + claudeFixtureSession + `","message":"` + strings.Repeat(source.toolResultsDir+" ", 60) + `"}` + "\n"
	for range 40 {
		transcript.WriteString(reference)
	}
	writeForTest(t, source.transcript, transcript.String())
	const limit = 128 << 10
	data, err := format.Capture(t.Context(), source.dirs, claudeFixtureSession, limit)
	if err != nil {
		t.Fatal(err)
	}
	destination := freshClaudeDirs(t)
	destination.State = filepath.Join(filepath.Dir(destination.State), strings.Repeat("h", 120), ".claude")
	destination.Workspace = filepath.Join(destination.Workspace, strings.Repeat("w", 150))
	for _, dir := range []string{filepath.Dir(destination.State), destination.Workspace} {
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := format.Install(t.Context(), data, destination, realDir(t), limit); err == nil || !strings.Contains(err.Error(), "expanded budget") {
		t.Fatalf("install must refuse to grow the conversation past its expanded budget: %v", err)
	}
	if _, err := os.Lstat(destination.State); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a failed install left %s behind: %v", destination.State, err)
	}
}

func TestLocalAcceptsTheReleasesTheFormatResumes(t *testing.T) {
	codex, _ := For("codex")
	claude, _ := For("claude")
	for _, test := range []struct {
		local  Local
		output string
		want   bool
	}{
		{codex.Local(), "codex-cli 0.160.0\n", true},
		{codex.Local(), "codex-cli 0.161.0", false},
		{claude.Local(), "2.1.293 (Claude Code)\n", true},
		{claude.Local(), "2.1.300 (Claude Code)", true},
		{claude.Local(), "2.2.0 (Claude Code)", true},
		{claude.Local(), "2.1.200 (Claude Code)", false},
		{claude.Local(), "2.1.300", false},
		{claude.Local(), "2.1.300 (Other)", false},
		{claude.Local(), "2.1.x (Claude Code)", false},
		{claude.Local(), "", false},
	} {
		if got := test.local.Accepts(test.output); got != test.want {
			t.Errorf("%s accepts %q = %v, want %v", test.local.Binary, test.output, got, test.want)
		}
	}
}

func writeForTest(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendForTest(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err := file.WriteString(content); err != nil {
		t.Fatal(err)
	}
}
