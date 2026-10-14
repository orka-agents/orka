package nativesession

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

const (
	claudeProviderKind = "claude"
	// claudeCodeVersion is the Claude Code release Orka's runtime runs. It
	// must equal acp.ClaudeCodeVersion.
	claudeCodeVersion  = "2.1.293"
	claudeBundleFormat = "orka/claude-session-v1"

	claudeTranscriptEntry  = "session.jsonl"
	claudeToolResultsEntry = "tool-results/"
	claudeToolResultsDir   = "tool-results"
	maxClaudeToolResults   = 256
	// maxClaudeProjects bounds the project folders capture searches for a
	// session; Claude Code keeps one per working directory.
	maxClaudeProjects = 4096
	// maxClaudeProjectFolder keeps a project folder name within filesystem
	// limits; Claude Code finds a session by ID in any project folder.
	maxClaudeProjectFolder = 200
)

var (
	claudeSessionIDPattern   = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	claudeProjectFolderChars = regexp.MustCompile(`[^A-Za-z0-9]`)
)

// claudeManifest describes a captured Claude Code conversation.
type claudeManifest struct {
	Format string `json:"format"`
	// Version is the Claude Code release that last wrote the transcript.
	Version   string `json:"version"`
	SessionID string `json:"sessionID"`
	// ToolResultsDir is where the captured session spilled large tool
	// results. The transcript refers to those files by absolute path.
	ToolResultsDir string `json:"toolResultsDir"`
}

type claudeBundle struct {
	manifest    claudeManifest
	transcript  []byte
	toolResults map[string][]byte
}

// claudeFormat carries a Claude Code session: its append-only JSONL
// transcript and the tool results it spilled to files. Claude Code finds a
// session by ID in any folder under projects/, and resumes it in whatever
// working directory it is given. Dirs.State is CLAUDE_CONFIG_DIR.
type claudeFormat struct{}

// Later Claude Code releases resume transcripts that earlier ones wrote.
func (claudeFormat) Local() Local {
	return Local{
		Binary: "claude", Version: claudeCodeVersion + " (Claude Code)", NewerReleases: true,
		StateVar: "CLAUDE_CONFIG_DIR", HomeState: ".claude",
	}
}

// The runtime supervisor sets CLAUDE_CONFIG_DIR to HOME/.claude.
func (claudeFormat) runtimeState(home, _ string) string { return filepath.Join(home, ".claude") }

func (claudeFormat) Capture(_ context.Context, dirs Dirs, providerSessionID string, maxBytes int) ([]byte, error) {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return nil, err
	}
	if !claudeSessionIDPattern.MatchString(providerSessionID) {
		return nil, errors.New("the Claude Code session ID is invalid")
	}
	if !absoluteCleanPath(dirs.State) {
		return nil, errors.New("the Claude Code state directory must be an absolute, clean path")
	}
	folder, err := findClaudeSession(filepath.Join(dirs.State, "projects"), providerSessionID)
	if err != nil {
		return nil, err
	}
	transcriptPath := filepath.Join(folder, providerSessionID+".jsonl")
	if err := requireRealPath(transcriptPath, false); err != nil {
		return nil, err
	}
	transcript, err := readBoundedFile(transcriptPath, expandedLimit(limit))
	if err != nil {
		return nil, err
	}
	version, err := inspectClaudeTranscript(transcript, providerSessionID)
	if err != nil && !errors.Is(err, ErrUnsupported) {
		err = fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	if err != nil {
		return nil, err
	}
	toolResultsDir := filepath.Join(folder, providerSessionID, claudeToolResultsDir)
	toolResults, err := readClaudeToolResults(toolResultsDir, expandedLimit(limit)-len(transcript))
	if err != nil {
		return nil, err
	}
	files := make([]bundleFile, 0, 1+len(toolResults))
	files = append(files, bundleFile{claudeTranscriptEntry, transcript})
	for _, name := range slices.Sorted(maps.Keys(toolResults)) {
		files = append(files, bundleFile{claudeToolResultsEntry + name, toolResults[name]})
	}
	data, err := writeBundle(limit, claudeManifest{
		Format: claudeBundleFormat, Version: version, SessionID: providerSessionID, ToolResultsDir: toolResultsDir,
	}, files)
	if err != nil {
		return nil, err
	}
	if len(data) > limit {
		return nil, fmt.Errorf("%w: Claude Code conversation exceeds Orka's %d-byte transport limit", ErrUnsupported, limit)
	}
	return data, nil
}

func (claudeFormat) Inspect(_ context.Context, data []byte, maxBytes int) (Summary, error) {
	bundle, err := decodeClaudeBundle(data, maxBytes)
	if err != nil {
		return Summary{}, err
	}
	return Summary{ProviderSessionID: bundle.manifest.SessionID, ProviderVersion: bundle.manifest.Version, DataDigest: dataDigest(data)}, nil
}

// Install writes the conversation into a Claude Code state directory that
// does not hold the session, under the project folder of the new working
// directory, and points the transcript's spilled tool results at their new
// location. A failed install removes what it created, so journalDir is
// unused.
func (claudeFormat) Install(_ context.Context, data []byte, dirs Dirs, _ string, maxBytes int) (err error) {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return err
	}
	bundle, err := decodeClaudeBundle(data, limit)
	if err != nil {
		return err
	}
	if !absoluteCleanPath(dirs.State) {
		return errors.New("the Claude Code state directory must be an absolute, clean path")
	}
	for _, dir := range []string{filepath.Dir(dirs.State), dirs.Workspace} {
		if err := requireRealPath(dir, true); err != nil {
			return err
		}
	}
	var created []string
	defer func() {
		if err != nil {
			for _, path := range slices.Backward(created) {
				_ = os.Remove(path)
			}
		}
	}()
	mkdir := func(path string) error {
		if err := os.Mkdir(path, 0o700); err == nil {
			created = append(created, path)
		} else if !errors.Is(err, os.ErrExist) {
			return err
		}
		return requireRealPath(path, true)
	}
	projects := filepath.Join(dirs.State, "projects")
	if err = mkdir(dirs.State); err != nil {
		return err
	}
	if err = mkdir(projects); err != nil {
		return err
	}
	if _, findErr := findClaudeSession(projects, bundle.manifest.SessionID); findErr == nil {
		return errors.New("the Claude Code state directory already holds the session")
	} else if !errors.Is(findErr, os.ErrNotExist) {
		return findErr
	}
	folder := filepath.Join(projects, claudeProjectFolder(dirs.Workspace))
	if err = mkdir(folder); err != nil {
		return err
	}
	toolResultsDir := filepath.Join(folder, bundle.manifest.SessionID, claudeToolResultsDir)
	if len(bundle.toolResults) > 0 {
		for _, dir := range []string{filepath.Dir(toolResultsDir), toolResultsDir} {
			if err = os.Mkdir(dir, 0o700); err != nil {
				return err
			}
			created = append(created, dir)
		}
		for name, content := range bundle.toolResults {
			path := filepath.Join(toolResultsDir, name)
			if err = writeNewFile(path, content); err != nil {
				return err
			}
			created = append(created, path)
		}
	}
	carried := len(bundle.transcript)
	for _, content := range bundle.toolResults {
		carried += len(content)
	}
	transcript, err := relocateClaudeToolResults(bundle.transcript, bundle.manifest.ToolResultsDir, toolResultsDir, expandedLimit(limit)-carried)
	if err != nil {
		return err
	}
	staged := filepath.Join(folder, "."+bundle.manifest.SessionID+".jsonl.restore")
	if err = writeNewFile(staged, transcript); err != nil {
		return err
	}
	created = append(created, staged)
	destination := filepath.Join(folder, bundle.manifest.SessionID+".jsonl")
	if err = os.Rename(staged, destination); err != nil {
		return err
	}
	// Until the directories are synced, a failure still removes the published
	// transcript, so a retry finds no partial install.
	created[len(created)-1] = destination
	synced := []string{folder, projects, dirs.State, filepath.Dir(dirs.State)}
	if len(bundle.toolResults) > 0 {
		synced = append([]string{toolResultsDir, filepath.Dir(toolResultsDir)}, synced...)
	}
	if err = syncDirs(synced...); err != nil {
		return err
	}
	created = nil
	return nil
}

// findClaudeSession returns the one project folder under projects that holds
// providerSessionID's transcript, or an error wrapping os.ErrNotExist.
func findClaudeSession(projects, providerSessionID string) (string, error) {
	if err := requireRealPath(projects, true); err != nil {
		return "", err
	}
	handle, err := os.Open(projects)
	if err != nil {
		return "", err
	}
	defer func() { _ = handle.Close() }()
	entries, err := handle.ReadDir(maxClaudeProjects + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	if len(entries) > maxClaudeProjects {
		return "", fmt.Errorf("%w: Claude Code keeps more than %d project folders", ErrUnsupported, maxClaudeProjects)
	}
	var found []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		folder := filepath.Join(projects, entry.Name())
		if _, err := os.Lstat(filepath.Join(folder, providerSessionID+".jsonl")); err == nil {
			found = append(found, folder)
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
	}
	switch len(found) {
	case 0:
		return "", fmt.Errorf("the Claude Code state directory does not hold session %s: %w", providerSessionID, os.ErrNotExist)
	case 1:
		return found[0], nil
	default:
		return "", fmt.Errorf("%w: Claude Code keeps session %s in more than one project folder", ErrUnsupported, providerSessionID)
	}
}

// claudeProjectFolder names the project folder Claude Code uses for a
// working directory: every character other than a letter or digit becomes a
// hyphen.
func claudeProjectFolder(workspace string) string {
	folder := claudeProjectFolderChars.ReplaceAllString(workspace, "-")
	if len(folder) > maxClaudeProjectFolder {
		folder = folder[:maxClaudeProjectFolder]
	}
	return folder
}

// readClaudeToolResults reads the session's spilled tool results, which only
// it writes, within budget bytes. Files with other names are not carried.
func readClaudeToolResults(dir string, budget int) (map[string][]byte, error) {
	if _, err := os.Lstat(dir); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err := requireRealPath(dir, true); err != nil {
		return nil, err
	}
	handle, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	defer func() { _ = handle.Close() }()
	entries, err := handle.ReadDir(maxClaudeToolResults + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	if len(entries) > maxClaudeToolResults {
		return nil, fmt.Errorf("%w: Claude Code session spilled more than %d tool results", ErrUnsupported, maxClaudeToolResults)
	}
	files := map[string][]byte{}
	total := 0
	for _, entry := range entries {
		if !spilledFilePattern.MatchString(entry.Name()) {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if err := requireRealPath(path, false); err != nil {
			return nil, err
		}
		content, err := readBoundedFile(path, budget-total)
		if err != nil {
			return nil, err
		}
		total += len(content)
		files[entry.Name()] = content
	}
	return files, nil
}

// claudeEntryTypes are the transcript entry types Claude Code 2.1.293 reads,
// from its own classification of transcript entries.
var claudeEntryTypes = map[string]bool{
	"user": true, "assistant": true, "system": true, "attachment": true, "progress": true,
	"file-history-snapshot": true, "file-history-delta": true, "last-prompt": true, "continued-in": true,
	"content-replacement": true, "api-request-shape": true, "api-request-blob": true, "api-request": true,
	"fork-context-ref": true, "frame-link": true, "summary": true, "custom-title": true, "ended-by-model": true,
	"ai-title": true, "tag": true, "relocated": true, "agent-name": true, "agent-color": true, "agent-setting": true,
	"pr-link": true, "artifact-comment-monitor": true, "artifact-autoreact-ledger": true, "bridge-session": true,
	"history-suppression": true, "attribution-snapshot": true, "mode": true, "permission-mode": true,
	"isolation-latch": true, "dev-mods": true, "memory-mode": true, "atis-latch": true, "worktree-state": true,
	"cost-state": true, "queue-operation": true, "observer-ref": true,
}

// inspectClaudeTranscript checks that transcript is JSON lines of entries of
// providerSessionID that Claude Code claudeCodeVersion reads, and returns the
// latest Claude Code release that wrote it. A later release's transcript is
// unsupported.
func inspectClaudeTranscript(transcript []byte, providerSessionID string) (string, error) {
	if len(transcript) == 0 || transcript[len(transcript)-1] != '\n' {
		return "", errors.New("the Claude Code transcript is empty or ends in a partial entry")
	}
	version, owned, unknown := "", false, ""
	for line := range bytes.Lines(transcript) {
		var entry struct {
			Type      string  `json:"type"`
			SessionID *string `json:"sessionId"`
			Version   string  `json:"version"`
		}
		decoder := json.NewDecoder(bytes.NewReader(line))
		if !bytes.HasPrefix(line, []byte("{")) || decoder.Decode(&entry) != nil || decoder.More() {
			return "", errors.New("the Claude Code transcript holds an entry that is not one JSON object")
		}
		if !claudeEntryTypes[entry.Type] && unknown == "" {
			unknown = entry.Type
		}
		if entry.SessionID != nil {
			if *entry.SessionID != providerSessionID {
				return "", errors.New("the Claude Code transcript holds entries of another session")
			}
			owned = true
		}
		if entry.Version != "" {
			if _, ok := parseRelease(entry.Version); !ok {
				return "", fmt.Errorf("the Claude Code transcript records release %q", entry.Version)
			}
			if order, _ := compareReleases(entry.Version, version); version == "" || order > 0 {
				version = entry.Version
			}
		}
	}
	if !owned {
		return "", errors.New("the Claude Code transcript holds no entries of the session")
	}
	if version == "" {
		return "", errors.New("the Claude Code transcript does not record a release")
	}
	if err := requireSupportedClaudeVersion(version); err != nil {
		return "", err
	}
	if unknown != "" {
		return "", fmt.Errorf("the Claude Code transcript holds an entry of type %q, which Claude Code %s does not read", unknown, claudeCodeVersion)
	}
	return version, nil
}

// requireSupportedClaudeVersion refuses a transcript from a later Claude Code
// release than Orka runs, which may hold entries that release cannot read.
func requireSupportedClaudeVersion(version string) error {
	if order, ok := compareReleases(version, claudeCodeVersion); !ok || order > 0 {
		return fmt.Errorf("%w: Claude Code %s wrote the conversation; Orka runs %s", ErrUnsupported, version, claudeCodeVersion)
	}
	return nil
}

// relocateClaudeToolResults points the transcript's references to spilled
// tool results at their new directory, adding at most budget bytes. The
// transcript holds each path as a JSON string.
func relocateClaudeToolResults(transcript []byte, previous, current string, budget int) ([]byte, error) {
	from, to := jsonStringContent(previous), jsonStringContent(current)
	if bytes.Equal(from, to) {
		return transcript, nil
	}
	if growth := bytes.Count(transcript, from) * (len(to) - len(from)); growth > budget {
		return nil, errors.New("relocating the Claude Code session would exceed its expanded budget")
	}
	return bytes.ReplaceAll(transcript, from, to), nil
}

// jsonStringContent returns value as it appears between the quotes of a JSON
// string written without HTML escaping, as Claude Code writes its transcript.
func jsonStringContent(value string) []byte {
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	_ = encoder.Encode(value)
	return bytes.TrimSuffix(bytes.TrimPrefix(bytes.TrimSpace(encoded.Bytes()), []byte(`"`)), []byte(`"`))
}

// decodeClaudeBundle reads and verifies a bundle holding a manifest, the
// transcript, and at most maxClaudeToolResults spilled tool results.
func decodeClaudeBundle(data []byte, maxBytes int) (claudeBundle, error) {
	toolResults := 0
	manifest, files, err := readBundle(data, maxBytes, func(name string) bool {
		if name == claudeTranscriptEntry {
			return true
		}
		toolResult, ok := strings.CutPrefix(name, claudeToolResultsEntry)
		toolResults++
		return ok && spilledFilePattern.MatchString(toolResult) && toolResults <= maxClaudeToolResults
	})
	if err != nil {
		return claudeBundle{}, err
	}
	bundle := claudeBundle{transcript: files[claudeTranscriptEntry], toolResults: map[string][]byte{}}
	for name, content := range files {
		if toolResult, ok := strings.CutPrefix(name, claudeToolResultsEntry); ok {
			bundle.toolResults[toolResult] = content
		}
	}
	if err := decodeManifest(manifest, &bundle.manifest); err != nil {
		return claudeBundle{}, err
	}
	if bundle.manifest.Format != claudeBundleFormat || !claudeSessionIDPattern.MatchString(bundle.manifest.SessionID) ||
		!absoluteCleanPath(bundle.manifest.ToolResultsDir) {
		return claudeBundle{}, errors.New("the Claude Code bundle manifest is invalid")
	}
	version, err := inspectClaudeTranscript(bundle.transcript, bundle.manifest.SessionID)
	if err != nil {
		return claudeBundle{}, err
	}
	if version != bundle.manifest.Version {
		return claudeBundle{}, errors.New("the Claude Code bundle manifest names another release than its transcript")
	}
	return bundle, nil
}
