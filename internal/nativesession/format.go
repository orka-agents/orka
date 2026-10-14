// Package nativesession saves and restores an agent's own conversation state
// between RuntimeSessions, and moves it to and from a developer's own agent.
// Each supported provider kind has one Format; the controller, its store, the
// runtime supervisor, and the migration CLI all use the same Format so a
// provider is added in one place.
package nativesession

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
)

// ErrUnsupported identifies a stopped conversation outside the provider's
// supported format or transport bounds. Callers fall back to the canonical
// transcript. I/O failures and uncertain outcomes are deliberately separate.
var ErrUnsupported = errors.New("native session format or transport is unsupported")

// ErrInstallOutcomeUnknown identifies an installation that may have been
// partly published. Callers retain the destination for reconciliation.
var ErrInstallOutcomeUnknown = errors.New("native session installation outcome is unknown")

// Dirs are where one agent keeps its conversations and does its work.
type Dirs struct {
	// State is the directory the agent's state variable (Local.StateVar)
	// names: CODEX_HOME for Codex, XDG_DATA_HOME for OpenCode.
	State string
	// Workspace is the agent's working directory. Capture accepts an empty
	// Workspace to mean the directory the conversation recorded.
	Workspace string
}

// Local describes the agent a Format carries as a developer runs it.
type Local struct {
	// Binary is the agent's executable name.
	Binary string
	// Version is what `Binary --version` prints for the release the Format
	// carries.
	Version string
	// NewerReleases reports whether later releases also resume what the
	// Format installs.
	NewerReleases bool
	// StateVar names the environment variable whose value is Dirs.State.
	StateVar string
	// HomeState is Dirs.State relative to HOME when StateVar is unset.
	HomeState string
}

// Accepts reports whether a local agent whose `--version` printed output can
// resume what the Format installs.
func (l Local) Accepts(output string) bool {
	output = strings.TrimSpace(output)
	if output == l.Version {
		return true
	}
	if !l.NewerReleases {
		return false
	}
	got, want := strings.Fields(output), strings.Fields(l.Version)
	if len(got) == 0 || len(want) == 0 || !slices.Equal(got[1:], want[1:]) {
		return false
	}
	order, ok := compareReleases(got[0], want[0])
	return ok && order >= 0
}

// parseRelease parses a MAJOR.MINOR.PATCH release.
func parseRelease(release string) ([3]int, bool) {
	var parts [3]int
	fields := strings.Split(release, ".")
	if len(fields) != len(parts) {
		return parts, false
	}
	for i, field := range fields {
		n, err := strconv.Atoi(field)
		if err != nil || n < 0 || strconv.Itoa(n) != field {
			return parts, false
		}
		parts[i] = n
	}
	return parts, true
}

// compareReleases orders two MAJOR.MINOR.PATCH releases.
func compareReleases(a, b string) (int, bool) {
	left, okLeft := parseRelease(a)
	right, okRight := parseRelease(b)
	if !okLeft || !okRight {
		return 0, false
	}
	return slices.Compare(left[:], right[:]), true
}

// Summary describes a verified saved conversation.
type Summary struct {
	ProviderSessionID string
	ProviderVersion   string
	DataDigest        string
}

// Format reads, verifies, and installs one provider's saved conversation.
// Capture and Install require that no agent process can write the
// directories: Capture runs after the writer is proven stopped, and Install
// before the agent starts.
type Format interface {
	// Local describes the agent outside Orka.
	Local() Local
	// Capture reads the conversation providerSessionID from dirs.
	Capture(ctx context.Context, dirs Dirs, providerSessionID string, maxBytes int) ([]byte, error)
	// Inspect verifies data without starting an agent.
	Inspect(ctx context.Context, data []byte, maxBytes int) (Summary, error)
	// Install writes data into dirs. journalDir is private, outside dirs, and
	// lets a retry reconcile an earlier attempt for the same destination.
	Install(ctx context.Context, data []byte, dirs Dirs, journalDir string, maxBytes int) error
	// runtimeState returns Dirs.State for a RuntimeSession agent whose HOME
	// is home and XDG_DATA_HOME is data.
	runtimeState(home, data string) string
}

var formats = map[string]Format{
	claudeProviderKind:   claudeFormat{},
	codexProviderKind:    codexFormat{},
	openCodeProviderKind: openCodeFormat{},
}

// For returns the Format for providerKind.
func For(providerKind string) (Format, bool) {
	format, ok := formats[providerKind]
	return format, ok
}

// Supported reports whether providerKind can save and restore conversations.
func Supported(providerKind string) bool {
	_, ok := formats[providerKind]
	return ok
}

// Kinds returns the provider kinds that have a Format, sorted.
func Kinds() []string {
	kinds := make([]string, 0, len(formats))
	for kind := range formats {
		kinds = append(kinds, kind)
	}
	slices.Sort(kinds)
	return kinds
}

// RuntimeDirs returns the Dirs of a RuntimeSession agent, matching the
// environment the runtime supervisor gives it.
func RuntimeDirs(format Format, home, data, workspace string) Dirs {
	return Dirs{State: format.runtimeState(home, data), Workspace: workspace}
}
