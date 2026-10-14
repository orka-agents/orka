package nativesession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
)

const (
	copilotProviderKind = "copilot"
	// copilotVersion is the Copilot CLI release Orka's runtime runs. It must
	// equal acp.CopilotCLIVersion.
	copilotVersion      = "1.0.96"
	copilotBundleFormat = "orka/copilot-session-v1"
	copilotEventsEntry  = "events.jsonl"
	// copilotEventsSchema is the events.jsonl schema that session.start
	// records.
	copilotEventsSchema = 1
)

// copilotManifest describes a captured Copilot CLI conversation.
type copilotManifest struct {
	Format string `json:"format"`
	// Version is the Copilot CLI release that started the session.
	Version   string `json:"version"`
	SessionID string `json:"sessionID"`
}

// copilotFormat carries a Copilot CLI session: its append-only events.jsonl.
// Copilot cannot resume a session, but session/load replays the file in any
// working directory. Copilot spills large tool output to TMPDIR, which is
// not carried; the restored conversation keeps its preview. Dirs.State is
// COPILOT_HOME.
type copilotFormat struct{}

// Later Copilot CLI releases load sessions that earlier ones wrote.
func (copilotFormat) Local() Local {
	return Local{
		Binary: "copilot", VersionLine: regexp.MustCompile(`^GitHub Copilot CLI ([0-9]+\.[0-9]+\.[0-9]+)\.$`),
		Release: copilotVersion, NewerReleases: true, StateVar: "COPILOT_HOME", HomeState: ".copilot",
	}
}

// The runtime supervisor sets COPILOT_HOME to HOME/.copilot.
func (copilotFormat) runtimeState(home, _ string) string { return filepath.Join(home, ".copilot") }

func copilotEventsPath(state, providerSessionID string) string {
	return filepath.Join(state, "session-state", providerSessionID, copilotEventsEntry)
}

func (copilotFormat) Capture(_ context.Context, dirs Dirs, providerSessionID string, maxBytes int) ([]byte, error) {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return nil, err
	}
	if !uuidSessionIDPattern.MatchString(providerSessionID) {
		return nil, errors.New("the Copilot CLI session ID is invalid")
	}
	if !absoluteCleanPath(dirs.State) {
		return nil, errors.New("the Copilot CLI state directory must be an absolute, clean path")
	}
	path := copilotEventsPath(dirs.State, providerSessionID)
	if err := requireRealPath(path, false); err != nil {
		return nil, err
	}
	events, err := readBoundedFile(path, expandedLimit(limit))
	if err != nil {
		return nil, err
	}
	version, err := inspectCopilotEvents(events, providerSessionID)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if err := requireSupportedCopilotVersion(version); err != nil {
		return nil, err
	}
	data, err := writeBundle(limit, copilotManifest{Format: copilotBundleFormat, Version: version, SessionID: providerSessionID},
		[]bundleFile{{copilotEventsEntry, events}})
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%w: Copilot CLI conversation exceeds Orka's %d-byte transport limit", ErrUnsupported, limit)
	}
	return data, nil
}

func (copilotFormat) Inspect(_ context.Context, data []byte, maxBytes int) (Summary, error) {
	manifest, _, err := decodeCopilotBundle(data, maxBytes)
	if err != nil {
		return Summary{}, err
	}
	return Summary{ProviderSessionID: manifest.SessionID, ProviderVersion: manifest.Version, DataDigest: dataDigest(data)}, nil
}

// Install writes the conversation into a Copilot CLI state directory that
// does not hold the session. Copilot loads it in the cwd session/load names,
// so nothing is rewritten. A failed install removes what it created, so
// journalDir is unused.
func (copilotFormat) Install(_ context.Context, data []byte, dirs Dirs, _ string, maxBytes int) (err error) {
	manifest, events, err := decodeCopilotBundle(data, maxBytes)
	if err != nil {
		return err
	}
	if !absoluteCleanPath(dirs.State) {
		return errors.New("the Copilot CLI state directory must be an absolute, clean path")
	}
	if err := requireRealPath(filepath.Dir(dirs.State), true); err != nil {
		return err
	}
	var created []string
	defer func() {
		if err != nil {
			for _, path := range slices.Backward(created) {
				_ = os.Remove(path)
			}
		}
	}()
	sessionDir := filepath.Dir(copilotEventsPath(dirs.State, manifest.SessionID))
	for _, dir := range []string{dirs.State, filepath.Dir(sessionDir)} {
		if err = os.Mkdir(dir, 0o700); err == nil {
			created = append(created, dir)
		} else if !errors.Is(err, os.ErrExist) {
			return err
		}
		if err = requireRealPath(dir, true); err != nil {
			return err
		}
	}
	if err = os.Mkdir(sessionDir, 0o700); err != nil {
		if errors.Is(err, os.ErrExist) {
			return errors.New("the Copilot CLI state directory already holds the session")
		}
		return err
	}
	created = append(created, sessionDir)
	staged := filepath.Join(sessionDir, ".events.jsonl.restore")
	if err = writeNewFile(staged, events); err != nil {
		return err
	}
	created = append(created, staged)
	if err = os.Rename(staged, filepath.Join(sessionDir, copilotEventsEntry)); err != nil {
		return err
	}
	created = nil
	return syncDir(sessionDir)
}

// inspectCopilotEvents checks that events is JSON lines of events that start
// with providerSessionID's session.start, and returns the Copilot CLI release
// that started the session.
func inspectCopilotEvents(events []byte, providerSessionID string) (string, error) {
	if len(events) == 0 || events[len(events)-1] != '\n' {
		return "", errors.New("the Copilot CLI event log is empty or ends in a partial event")
	}
	version := ""
	first := true
	for line := range bytes.Lines(events) {
		var event struct {
			Type string          `json:"type"`
			Data json.RawMessage `json:"data"`
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		if !bytes.HasPrefix(line, []byte("{")) || decoder.Decode(&event) != nil || decoder.More() || event.Type == "" {
			return "", errors.New("the Copilot CLI event log holds an entry that is not one event")
		}
		switch {
		case first:
			var start struct {
				SessionID      string `json:"sessionId"`
				CopilotVersion string `json:"copilotVersion"`
				Version        int    `json:"version"`
			}
			if event.Type != "session.start" || json.Unmarshal(event.Data, &start) != nil {
				return "", errors.New("the Copilot CLI event log does not begin with session.start")
			}
			if start.SessionID != providerSessionID {
				return "", errors.New("the Copilot CLI event log starts another session")
			}
			if start.Version != copilotEventsSchema {
				return "", fmt.Errorf("the Copilot CLI event log uses schema %d, not %d", start.Version, copilotEventsSchema)
			}
			if _, ok := parseRelease(start.CopilotVersion); !ok {
				return "", errors.New("the Copilot CLI event log does not record a release")
			}
			version, first = start.CopilotVersion, false
		case event.Type == "session.start":
			return "", errors.New("the Copilot CLI event log starts more than one session")
		}
	}
	return version, nil
}

// requireSupportedCopilotVersion refuses a session a later Copilot CLI
// release started than Orka runs, which may hold events that release cannot
// load.
func requireSupportedCopilotVersion(version string) error {
	if order, ok := compareReleases(version, copilotVersion); !ok || order > 0 {
		return fmt.Errorf("%w: Copilot CLI %s started the conversation; Orka runs %s", ErrUnsupported, version, copilotVersion)
	}
	return nil
}

// decodeCopilotBundle reads and verifies a bundle holding a manifest and the
// event log.
func decodeCopilotBundle(data []byte, maxBytes int) (copilotManifest, []byte, error) {
	encoded, files, err := readBundle(data, maxBytes, func(name string) bool { return name == copilotEventsEntry })
	if err != nil {
		return copilotManifest{}, nil, err
	}
	var manifest copilotManifest
	if err := decodeManifest(encoded, &manifest); err != nil {
		return copilotManifest{}, nil, err
	}
	if manifest.Format != copilotBundleFormat || !uuidSessionIDPattern.MatchString(manifest.SessionID) {
		return copilotManifest{}, nil, errors.New("the Copilot CLI bundle manifest is invalid")
	}
	events := files[copilotEventsEntry]
	version, err := inspectCopilotEvents(events, manifest.SessionID)
	if err != nil {
		return copilotManifest{}, nil, err
	}
	if version != manifest.Version {
		return copilotManifest{}, nil, errors.New("the Copilot CLI bundle manifest names another release than its event log")
	}
	if err := requireSupportedCopilotVersion(version); err != nil {
		return copilotManifest{}, nil, err
	}
	return manifest, events, nil
}
