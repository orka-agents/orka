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
	"runtime"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"

	"golang.org/x/text/unicode/norm"
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
	// maxClaudeProjectFolder is where Claude Code cuts a longer project
	// folder name and appends a hash of the working directory.
	maxClaudeProjectFolder = 200
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
		Binary: "claude", VersionLine: regexp.MustCompile(`^([0-9]+\.[0-9]+\.[0-9]+) \(Claude Code\)$`), Release: claudeCodeVersion,
		NewerReleases: true,
		StateVar:      "CLAUDE_CONFIG_DIR", HomeState: ".claude",
	}
}

// The runtime supervisor sets CLAUDE_CONFIG_DIR to HOME/.claude.
func (claudeFormat) runtimeState(home, _ string) string { return filepath.Join(home, ".claude") }

func (claudeFormat) Capture(_ context.Context, dirs Dirs, providerSessionID string, maxBytes int) ([]byte, error) {
	limit, err := normalizeLimit(maxBytes)
	if err != nil {
		return nil, err
	}
	if !uuidSessionIDPattern.MatchString(providerSessionID) {
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
// unused. Installs into one destination run one at a time.
func (claudeFormat) Install(ctx context.Context, data []byte, dirs Dirs, _ string, maxBytes int) error {
	unlock, err := lockInstallDestination(ctx, dirs.State)
	if err != nil {
		return err
	}
	defer unlock()
	return installClaude(data, dirs, maxBytes)
}

func installClaude(data []byte, dirs Dirs, maxBytes int) (err error) {
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
	projects := filepath.Join(dirs.State, "projects")
	folder := filepath.Join(projects, claudeProjectFolder(dirs.Workspace))
	toolResultsDir := filepath.Join(folder, bundle.manifest.SessionID, claudeToolResultsDir)
	destination := filepath.Join(folder, bundle.manifest.SessionID+".jsonl")
	carried := len(bundle.transcript)
	for _, content := range bundle.toolResults {
		carried += len(content)
	}
	transcript, err := relocateClaudeToolResults(bundle.transcript, bundle.manifest.ToolResultsDir, toolResultsDir, expandedLimit(limit)-carried)
	if err != nil {
		return err
	}
	synced := []string{folder, projects, dirs.State, filepath.Dir(dirs.State)}
	if len(bundle.toolResults) > 0 {
		synced = append([]string{toolResultsDir, filepath.Dir(toolResultsDir)}, synced...)
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
	if err = mkdir(dirs.State); err != nil {
		return err
	}
	if err = mkdir(projects); err != nil {
		return err
	}
	if found, findErr := findClaudeSession(projects, bundle.manifest.SessionID); findErr == nil {
		// A retry after an install that completed but was not acknowledged
		// finds exactly what it would write.
		if found == folder && holdsFile(destination, transcript) && holdsFiles(toolResultsDir, bundle.toolResults) {
			return syncDirs(synced...)
		}
		return errors.New("the Claude Code state directory already holds the session")
	} else if !errors.Is(findErr, os.ErrNotExist) {
		return findErr
	}
	if err = mkdir(folder); err != nil {
		return err
	}
	staged := filepath.Join(folder, "."+bundle.manifest.SessionID+".jsonl.restore")
	if err = reclaimClaudeStaging(staged, filepath.Dir(toolResultsDir), bundle.toolResults); err != nil {
		return err
	}
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
	if err = writeNewFile(staged, transcript); err != nil {
		return err
	}
	created = append(created, staged)
	if err = os.Rename(staged, destination); err != nil {
		return err
	}
	// Until the directories are synced, a failure still removes the published
	// transcript, so a retry finds no partial install.
	created[len(created)-1] = destination
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

// claudeNormalizesPaths reports whether Claude Code composes a working
// directory to NFC before naming its folder, as it does on macOS.
var claudeNormalizesPaths = runtime.GOOS == "darwin"

// claudeProjectFolder names the project folder Claude Code uses for a
// working directory: every UTF-16 code unit other than an ASCII letter or
// digit becomes a hyphen. A longer name is cut and gets the base-36 absolute
// value of the directory's 32-bit JavaScript string hash, which the pinned
// adapter requires to list the folder's sessions.
func claudeProjectFolder(workspace string) string {
	if claudeNormalizesPaths {
		workspace = norm.NFC.String(workspace)
	}
	units := utf16.Encode([]rune(workspace))
	folder := make([]byte, len(units))
	var hash int32
	for i, unit := range units {
		folder[i] = '-'
		if unit >= 'a' && unit <= 'z' || unit >= 'A' && unit <= 'Z' || unit >= '0' && unit <= '9' {
			folder[i] = byte(unit)
		}
		hash = hash*31 + int32(unit)
	}
	if len(folder) <= maxClaudeProjectFolder {
		return string(folder)
	}
	return string(folder[:maxClaudeProjectFolder]) + "-" + strconv.FormatInt(max(int64(hash), -int64(hash)), 36)
}

// claudeToolResultsPath reports whether dir is where Claude Code spills the
// session's tool results, <config>/projects/<folder>/<session>/tool-results,
// so relocation rewrites only references to that session's spill directory.
func claudeToolResultsPath(dir, sessionID string) bool {
	session := filepath.Dir(dir)
	return absoluteCleanPath(dir) && filepath.Base(dir) == claudeToolResultsDir && filepath.Base(session) == sessionID &&
		filepath.Base(filepath.Dir(filepath.Dir(session))) == "projects"
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

// claudeUnownedEntryTypes are the entry types Claude Code identifies by
// something other than the session: a summary by leafUuid and a file history
// snapshot by messageId. Every other entry names its session.
var claudeUnownedEntryTypes = map[string]bool{"summary": true, "file-history-snapshot": true}

// inspectClaudeTranscript checks that transcript is JSON lines of entries of
// providerSessionID that Claude Code claudeCodeVersion reads, and returns the
// latest Claude Code release that wrote it. A later release's transcript is
// unsupported.
func inspectClaudeTranscript(transcript []byte, providerSessionID string) (string, error) {
	if len(transcript) == 0 || transcript[len(transcript)-1] != '\n' {
		return "", errors.New("the Claude Code transcript is empty or ends in a partial entry")
	}
	// A missing, null, or empty type decodes to "", so unread records the
	// first unread entry separately from its type.
	version, owned, unknown, unread := "", false, "", false
	for line := range bytes.Lines(transcript) {
		var entry struct {
			Type      string
			SessionID *string
			Version   string
		}
		fields, ok := exactFields(line)
		if !ok || exactField(fields, "type", &entry.Type) != nil || exactField(fields, "sessionId", &entry.SessionID) != nil ||
			exactField(fields, "version", &entry.Version) != nil {
			return "", errors.New("the Claude Code transcript holds an entry that is not one JSON object")
		}
		if !claudeEntryTypes[entry.Type] && !unread {
			unknown, unread = entry.Type, true
		}
		switch {
		case entry.SessionID != nil && *entry.SessionID == providerSessionID:
			owned = true
		case entry.SessionID == nil && claudeUnownedEntryTypes[entry.Type]:
		default:
			return "", errors.New("the Claude Code transcript holds an entry of another session or of none")
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
	if unread {
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

// reclaimClaudeStaging removes what an install killed before it published the
// transcript left behind: the staged transcript, and a session directory that
// holds only tool results the bundle carries. Anything else there is not an
// install's staging, so the install refuses it.
func reclaimClaudeStaging(staged, sessionDir string, toolResults map[string][]byte) error {
	if err := os.Remove(staged); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := requireRealPathIfPresent(sessionDir, true); err != nil {
		return err
	}
	entries, err := os.ReadDir(sessionDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	occupied := errors.New("the Claude Code state directory already holds the session")
	var leftovers []string
	for _, entry := range entries {
		if entry.Name() != claudeToolResultsDir || !entry.IsDir() {
			return occupied
		}
		toolResultsDir := filepath.Join(sessionDir, claudeToolResultsDir)
		results, err := os.ReadDir(toolResultsDir)
		if err != nil {
			return err
		}
		for _, result := range results {
			if _, carried := toolResults[result.Name()]; !carried || !result.Type().IsRegular() {
				return occupied
			}
			leftovers = append(leftovers, filepath.Join(toolResultsDir, result.Name()))
		}
		leftovers = append(leftovers, toolResultsDir)
	}
	for _, path := range append(leftovers, sessionDir) {
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return nil
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
	if bundle.manifest.Format != claudeBundleFormat || !uuidSessionIDPattern.MatchString(bundle.manifest.SessionID) ||
		!claudeToolResultsPath(bundle.manifest.ToolResultsDir, bundle.manifest.SessionID) {
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
