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
	copilotFixtureSession = "bff1f96a-be09-426e-933b-ec3720e975ac"
	copilotFixtureLimit   = 1 << 20
)

func copilotFixtureEvents(sessionID, version string) string {
	return strings.Join([]string{
		`{"type":"session.start","data":{"sessionId":"` + sessionID + `","version":1,"producer":"copilot-agent","copilotVersion":"` + version + `","context":{"cwd":"/runtime-a/workspace"}},"id":"e1","timestamp":"2026-10-13T04:57:00.000Z","parentId":null}`,
		`{"type":"user.message","data":{"content":"Remember nonce-fixture","messageId":"m1"},"id":"e2","timestamp":"2026-10-13T04:57:01.000Z","parentId":"e1"}`,
		`{"type":"tool.execution_complete","data":{"toolCallId":"t1","result":{"content":"The secret fact is ZEPHYR-7731."}},"id":"e3","timestamp":"2026-10-13T04:57:02.000Z","parentId":"e2"}`,
		`{"type":"assistant.message","data":{"content":"Noted.","encryptedContent":"opaque"},"id":"e4","timestamp":"2026-10-13T04:57:03.000Z","parentId":"e3"}`,
	}, "\n") + "\n"
}

// newCopilotFixture is a COPILOT_HOME holding the session and another one.
func newCopilotFixture(t *testing.T) (Dirs, string) {
	t.Helper()
	dirs := Dirs{State: filepath.Join(realDir(t), "home", ".copilot"), Workspace: realDir(t)}
	events := copilotEventsPath(dirs.State, copilotFixtureSession)
	other := copilotEventsPath(dirs.State, "8f1d0c3e-0000-4000-8000-000000000000")
	for path, content := range map[string]string{
		events: copilotFixtureEvents(copilotFixtureSession, copilotVersion),
		other:  copilotFixtureEvents("8f1d0c3e-0000-4000-8000-000000000000", copilotVersion),
	} {
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		writeForTest(t, path, content)
	}
	return dirs, events
}

func TestCopilotVersionMatchesPinnedRuntime(t *testing.T) {
	if copilotVersion != acp.CopilotCLIVersion {
		t.Fatalf("native Copilot CLI format %s does not match the pinned Copilot CLI %s", copilotVersion, acp.CopilotCLIVersion)
	}
}

func TestCopilotFormatCarriesTheSessionIntoANewHome(t *testing.T) {
	format, ok := For("copilot")
	if !ok {
		t.Fatal("copilot must save and restore native sessions")
	}
	source, events := newCopilotFixture(t)
	data, err := format.Capture(t.Context(), source, copilotFixtureSession, copilotFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	summary, err := format.Inspect(t.Context(), data, copilotFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	if summary.ProviderSessionID != copilotFixtureSession || summary.ProviderVersion != copilotVersion || summary.DataDigest != dataDigest(data) {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	destination := Dirs{State: filepath.Join(realDir(t), ".copilot"), Workspace: realDir(t)}
	// An install killed before it published the event log leaves only its
	// staging behind, and the retry replaces it.
	sessionDir := filepath.Dir(copilotEventsPath(destination.State, copilotFixtureSession))
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	writeForTest(t, filepath.Join(sessionDir, ".events.jsonl.restore"), `{"type":"session.st`)
	if err := format.Install(t.Context(), data, destination, realDir(t), copilotFixtureLimit); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(copilotEventsPath(destination.State, copilotFixtureSession))
	if err != nil || !bytes.Equal(installed, original) {
		t.Fatalf("installed events differ from the captured ones: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(destination.State, "session-state"))
	if err != nil || len(entries) != 1 {
		t.Fatalf("install must carry only the session: %v %v", entries, err)
	}
	// A retry after an unacknowledged install finds exactly what it wrote.
	if err := format.Install(t.Context(), data, destination, realDir(t), copilotFixtureLimit); err != nil {
		t.Fatalf("repeating a completed install must succeed: %v", err)
	}
	workspace := filepath.Join(filepath.Dir(copilotEventsPath(destination.State, copilotFixtureSession)), "workspace.yaml")
	writeForTest(t, workspace, "cwd: /elsewhere\n")
	if err := format.Install(t.Context(), data, destination, realDir(t), copilotFixtureLimit); err == nil {
		t.Fatal("a retry must not accept a session directory holding state the install did not write")
	}
	if err := os.Remove(workspace); err != nil {
		t.Fatal(err)
	}
	appendForTest(t, events, `{"type":"user.message","data":{"content":"later turn"},"id":"e5","timestamp":"2026-10-13T04:58:00.000Z","parentId":"e4"}`+"\n")
	later, err := format.Capture(t.Context(), source, copilotFixtureSession, copilotFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	if err := format.Install(t.Context(), later, destination, realDir(t), copilotFixtureLimit); err == nil {
		t.Fatal("install must not overwrite another conversation of the session")
	}
}

func TestCopilotInstallWaitsForAConcurrentInstall(t *testing.T) {
	format, _ := For("copilot")
	source, events := newCopilotFixture(t)
	data, err := format.Capture(t.Context(), source, copilotFixtureSession, copilotFixtureLimit)
	if err != nil {
		t.Fatal(err)
	}
	destination := Dirs{State: filepath.Join(realDir(t), ".copilot"), Workspace: realDir(t)}
	sessionDir := filepath.Dir(copilotEventsPath(destination.State, copilotFixtureSession))
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Another install holds the destination and has staged its event log.
	unlock, err := lockInstallDestination(t.Context(), destination.State)
	if err != nil {
		t.Fatal(err)
	}
	staged := filepath.Join(sessionDir, ".events.jsonl.restore")
	writeForTest(t, staged, "the other install's checkpoint")
	expectWaitsForConcurrentInstall(t, format, data, destination, staged, copilotFixtureLimit, unlock)
	original, err := os.ReadFile(events)
	if err != nil {
		t.Fatal(err)
	}
	if !holdsFile(copilotEventsPath(destination.State, copilotFixtureSession), original) {
		t.Fatal("the install did not publish its own event log")
	}
}

func TestCopilotCaptureFallsBackOnStateItDoesNotCarry(t *testing.T) {
	format, _ := For("copilot")
	for name, test := range map[string]struct {
		events string
		reason string
	}{
		"newer Copilot CLI":        {copilotFixtureEvents(copilotFixtureSession, "1.0.120"), "Copilot CLI 1.0.120 started"},
		"another session":          {copilotFixtureEvents("8f1d0c3e-0000-4000-8000-000000000000", copilotVersion), "starts another session"},
		"no session.start":         {strings.SplitN(copilotFixtureEvents(copilotFixtureSession, copilotVersion), "\n", 2)[1], "does not begin with session.start"},
		"second session":           {copilotFixtureEvents(copilotFixtureSession, copilotVersion) + strings.SplitN(copilotFixtureEvents(copilotFixtureSession, copilotVersion), "\n", 2)[0] + "\n", "more than one session"},
		"another schema":           {strings.Replace(copilotFixtureEvents(copilotFixtureSession, copilotVersion), `"version":1`, `"version":2`, 1), "schema 2"},
		"partial event":            {copilotFixtureEvents(copilotFixtureSession, copilotVersion) + `{"type":"user.message"`, "partial event"},
		"untyped event":            {copilotFixtureEvents(copilotFixtureSession, copilotVersion) + `{"data":{}}` + "\n", "not one event"},
		"event with trailing data": {copilotFixtureEvents(copilotFixtureSession, copilotVersion) + `{"type":"user.message","data":{}}]` + "\n", "not one event"},
		"unknown event":            {copilotFixtureEvents(copilotFixtureSession, copilotVersion) + `{"type":"session.future","data":{}}` + "\n", `"session.future" event, which Copilot CLI 1.0.96 does not record`},
		"event not a object":       {copilotFixtureEvents(copilotFixtureSession, copilotVersion) + "[]\n", "not one event"},
	} {
		t.Run(name, func(t *testing.T) {
			source, events := newCopilotFixture(t)
			writeForTest(t, events, test.events)
			_, err := format.Capture(t.Context(), source, copilotFixtureSession, copilotFixtureLimit)
			if !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("capture must fall back to the transcript because of %q: %v", test.reason, err)
			}
		})
	}
	source, events := newCopilotFixture(t)
	if _, err := format.Capture(t.Context(), source, "8f1d0c3e-0000-4000-8000-00000000ffff", copilotFixtureLimit); err == nil || errors.Is(err, ErrUnsupported) {
		t.Fatalf("a missing session is a capture failure, not an unsupported conversation: %v", err)
	}
	if _, err := format.Capture(t.Context(), source, copilotFixtureSession, 256); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("an oversized conversation must fall back to the transcript: %v", err)
	}
	outside := filepath.Join(realDir(t), "secret")
	writeForTest(t, outside, copilotFixtureEvents(copilotFixtureSession, copilotVersion))
	if err := os.Remove(events); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, events); err != nil {
		t.Fatal(err)
	}
	if _, err := format.Capture(t.Context(), source, copilotFixtureSession, copilotFixtureLimit); err == nil {
		t.Fatal("capture followed a symlink")
	}
}

func TestCopilotInspectRejectsTamperedBundles(t *testing.T) {
	format, _ := For("copilot")
	source, _ := newCopilotFixture(t)
	data, err := format.Capture(t.Context(), source, copilotFixtureSession, copilotFixtureLimit)
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
	if _, err := format.Inspect(t.Context(), rebuild(func(map[string][]byte) {}), copilotFixtureLimit); err != nil {
		t.Fatalf("a faithful rebuild must inspect: %v", err)
	}
	newer := rebuild(func(e map[string][]byte) {
		e["events.jsonl"] = []byte(copilotFixtureEvents(copilotFixtureSession, "1.0.120"))
		e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(`"`+copilotVersion+`"`), []byte(`"1.0.120"`), 1)
	})
	if _, err := format.Inspect(t.Context(), newer, copilotFixtureLimit); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("a later Copilot CLI's session must be unsupported: %v", err)
	}
	for name, tampered := range map[string][]byte{
		"unknown entry": rebuild(func(e map[string][]byte) { e["extra"] = []byte("x") }),
		"other session": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(copilotFixtureSession), []byte("8f1d0c3e-0000-4000-8000-000000000000"), 1)
		}),
		"manifest release": rebuild(func(e map[string][]byte) {
			e["manifest.json"] = bytes.Replace(e["manifest.json"], []byte(`"`+copilotVersion+`"`), []byte(`"1.0.90"`), 1)
		}),
		"no events":     rebuild(func(e map[string][]byte) { delete(e, "events.jsonl") }),
		"invalid event": rebuild(func(e map[string][]byte) { e["events.jsonl"] = append(e["events.jsonl"], "[]\n"...) }),
		"unknown event": rebuild(func(e map[string][]byte) {
			e["events.jsonl"] = append(e["events.jsonl"], `{"type":"session.future","data":{}}`+"\n"...)
		}),
		"trailing data": append(append([]byte(nil), data...), data...),
		"not gzip":      []byte("plain"),
	} {
		if _, err := format.Inspect(t.Context(), tampered, copilotFixtureLimit); err == nil || errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: must be rejected as invalid: %v", name, err)
		}
	}
}
