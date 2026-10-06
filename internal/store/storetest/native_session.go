package storetest

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/orka-agents/orka/internal/codexstate"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/stretchr/testify/require"
)

// NativeSessionSnapshot builds a verified paginated bundle from a synthetic
// provider rollout, with no external client, credentials, or network access.
func NativeSessionSnapshot(t *testing.T, text string) harnessv2.NativeSessionSnapshot {
	t.Helper()
	const threadID = "0195e76b-7c5e-7123-8123-456789abcdef"
	home, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	file := filepath.Join(home, "sessions", "2025", "03", "31", "rollout-2025-04-01T00-30-00-"+threadID+".jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(file), 0o700))
	rollout := fmt.Sprintf(`{"timestamp":"2025-04-01T00:30:00Z","ordinal":0,"type":"session_meta","payload":{"id":%q,"cli_version":"0.160.0","history_mode":"paginated","history_base":null,"cwd":"/source/work","source":"exec","model_provider":"stub","runtime_workspace_roots":["/source/work"]}}
{"timestamp":"2025-04-01T00:30:01Z","ordinal":1,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":%q}]}}
`, threadID, text)
	require.NoError(t, os.WriteFile(file, []byte(rollout), 0o600))
	data, err := codexstate.Capture(t.Context(), home, threadID)
	require.NoError(t, err)
	summary, err := codexstate.Inspect(t.Context(), data)
	require.NoError(t, err)
	return harnessv2.NativeSessionSnapshot{
		Data: data, DataDigest: summary.DataDigest, ProviderSessionID: threadID,
		ProviderKind: "codex", ProviderVersion: "0.160.0",
		RuntimeSessionUID: "native-test-runtime", RuntimeProfileDigest: harnessv2.ProfileDigest("sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"),
		WorkingDirectory: "/source/work",
	}
}
