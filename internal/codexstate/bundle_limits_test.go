package codexstate

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/stretchr/testify/require"
)

func TestNativeBundleConfiguredLimits(t *testing.T) {
	home, original := fixtureHome(t)
	lines := bytes.Split(bytes.TrimSuffix(original, []byte("\n")), []byte("\n"))
	changed := false
	for i, line := range lines {
		var record map[string]any
		require.NoError(t, json.Unmarshal(line, &record))
		payload, _ := record["payload"].(map[string]any)
		if record["type"] == "response_item" && payload["type"] == "message" && payload["role"] == "user" && !changed {
			payload["content"] = []any{map[string]any{"type": "input_text", "text": string(bytes.Repeat([]byte("x"), 1<<20))}}
			var err error
			lines[i], err = json.Marshal(record)
			require.NoError(t, err)
			changed = true
		}
	}
	require.True(t, changed)
	large := append(bytes.Join(lines, []byte("\n")), '\n')
	source := filepath.Join(home, "sessions", "2026", "10", "02", fixtureName)
	require.NoError(t, os.WriteFile(source, large, 0o600))
	data, err := Capture(t.Context(), home, fixtureThread)
	require.NoError(t, err)
	require.Greater(t, len(data), harnessv2.LegacyMaxNativeSessionBytes)
	require.Less(t, len(data), 2<<20)
	_, err = Capture(t.Context(), home, fixtureThread, harnessv2.LegacyMaxNativeSessionBytes)
	require.Error(t, err)
	exact := len(data)
	_, err = Inspect(t.Context(), data, exact)
	require.NoError(t, err)
	_, err = Inspect(t.Context(), data, exact-1)
	require.Error(t, err)
	destination, cwd, journal := realDir(t), realDir(t), realDir(t)
	receipt, err := Install(t.Context(), data, destination, cwd, journal, exact)
	require.NoError(t, err)
	require.NotEmpty(t, receipt)
	retry, err := Install(t.Context(), data, destination, cwd, journal, exact)
	require.NoError(t, err)
	require.Equal(t, receipt.OperationID, retry.OperationID)
	installed, err := os.ReadFile(filepath.Join(destination, "sessions", "2026", "10", "02", fixtureName))
	require.NoError(t, err)
	require.Equal(t, large, installed)
	_, err = Install(t.Context(), data, destination, cwd, journal, exact-1)
	require.Error(t, err)
	unchanged, err := os.ReadFile(source)
	require.NoError(t, err)
	require.Equal(t, large, unchanged)
	for _, invalid := range []int{-1, harnessv2.MaxNativeSessionBytes + 1} {
		_, err = Capture(t.Context(), home, fixtureThread, invalid)
		require.Error(t, err)
		_, err = Inspect(t.Context(), data, invalid)
		require.Error(t, err)
		_, err = Install(t.Context(), data, destination, cwd, journal, invalid)
		require.Error(t, err)
	}
}
