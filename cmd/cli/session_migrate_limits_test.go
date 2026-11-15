//go:build !windows

package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
	"github.com/spf13/cobra"
	"github.com/stretchr/testify/require"
)

func TestMigrationBundleLimitConfiguration(t *testing.T) {
	for _, tc := range []struct {
		name, env, flag string
		want            int
		bad             bool
	}{
		{name: "default", want: 8 << 20},
		{name: "environment", env: "16777216", want: 16 << 20},
		{name: "flag precedence", env: "invalid", flag: "33554432", want: 32 << 20},
		{name: "zero", env: "0", bad: true},
		{name: "negative", env: "-1", bad: true},
		{name: "malformed", env: "8MiB", bad: true},
		{name: "above ceiling", env: "67108865", bad: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("ORKA_NATIVE_SESSION_MAX_BYTES", tc.env)
			} else {
				old, present := os.LookupEnv("ORKA_NATIVE_SESSION_MAX_BYTES")
				require.NoError(t, os.Unsetenv("ORKA_NATIVE_SESSION_MAX_BYTES"))
				t.Cleanup(func() {
					if present {
						require.NoError(t, os.Setenv("ORKA_NATIVE_SESSION_MAX_BYTES", old))
					}
				})
			}
			cmd := &cobra.Command{}
			cmd.Flags().Int("max-bundle-bytes", harnessv2.DefaultMaxNativeSessionBytes, "")
			if tc.flag != "" {
				require.NoError(t, cmd.Flags().Set("max-bundle-bytes", tc.flag))
			}
			limit, err := migrationMaxBundleBytes(cmd)
			if tc.bad {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, limit)
		})
	}
}

func TestMigrationJournalAboveOldLimitAndRetryPolicy(t *testing.T) {
	const limit = 2 << 20
	journal := t.TempDir()
	expected := migrationState{Direction: "import", Server: "http://fixture", Namespace: "team", Session: "imported", Home: "/fixture/home", Thread: "fixture-uuid"}
	saved := expected
	saved.OperationID = "native-test"
	saved.Data = bytes.Repeat([]byte("x"), limit)
	body, err := json.Marshal(saved)
	require.NoError(t, err)
	require.Greater(t, len(body), 1<<20)
	require.NoError(t, os.WriteFile(filepath.Join(journal, "request.json"), body, 0o600))
	actual, err := readMigrationState(journal, expected, limit)
	require.NoError(t, err)
	require.Equal(t, saved, *actual)
	_, err = readMigrationState(journal, expected, limit-1)
	require.Error(t, err)
	expected.Home = "/another/home"
	_, err = readMigrationState(journal, expected, limit)
	require.Error(t, err)
}
