package nativesession

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/orka-agents/sessionkit"

	"github.com/orka-agents/orka/internal/codexstate"
)

const (
	codexProviderKind = "codex"
	// codexVersion is the Codex release codexstate carries.
	codexVersion = "0.160.0"
)

// codexFormat carries Codex's SessionKit bundle; codexstate owns the format.
// Dirs.State is CODEX_HOME.
type codexFormat struct{}

func (codexFormat) Local() Local {
	return Local{
		Binary: "codex", VersionLine: regexp.MustCompile(`^codex-cli ([0-9]+\.[0-9]+\.[0-9]+)$`), Release: codexVersion,
		StateVar: "CODEX_HOME", HomeState: ".codex",
	}
}

// The runtime supervisor sets CODEX_HOME to HOME/.codex.
func (codexFormat) runtimeState(home, _ string) string { return filepath.Join(home, ".codex") }

func (codexFormat) Capture(ctx context.Context, dirs Dirs, providerSessionID string, maxBytes int) ([]byte, error) {
	data, err := codexstate.Capture(ctx, dirs.State, providerSessionID, maxBytes)
	return data, codexError(err)
}

func (codexFormat) Inspect(ctx context.Context, data []byte, maxBytes int) (Summary, error) {
	summary, err := codexstate.Inspect(ctx, data, maxBytes)
	if err != nil {
		return Summary{}, codexError(err)
	}
	return Summary{
		ProviderSessionID: summary.ThreadID, ProviderVersion: summary.Manifest.SourceCLIVersion, DataDigest: summary.DataDigest,
	}, nil
}

func (codexFormat) Install(ctx context.Context, data []byte, dirs Dirs, journalDir string, maxBytes int) error {
	_, err := codexstate.Install(ctx, data, dirs.State, dirs.Workspace, journalDir, maxBytes)
	if _, unknown := errors.AsType[*sessionkit.UnknownOutcomeError](err); unknown {
		return fmt.Errorf("%w: %w", ErrInstallOutcomeUnknown, err)
	}
	return codexError(err)
}

func codexError(err error) error {
	if errors.Is(err, codexstate.ErrUnsupported) {
		return fmt.Errorf("%w: %w", ErrUnsupported, err)
	}
	return err
}
