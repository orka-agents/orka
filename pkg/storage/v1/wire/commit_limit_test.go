package wire_test

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
	"github.com/orka-agents/orka/pkg/storage/v1/wire"
	"github.com/orka-agents/orka/pkg/storage/v1/wire/wiretest"
)

// serveWithBulkLimit serves provider with a short bulk duration limit.
func serveWithBulkLimit(t *testing.T, provider *recordingProvider) *wire.Client {
	t.Helper()
	config := serveBoth(provider)
	config.Limits = wire.Limits{MaxDuration: map[wire.Class]time.Duration{wire.ClassBulk: 300 * time.Millisecond}}
	return wiretest.Serve(t, config)
}

// An upload whose provider commits after the bulk duration limit has passed
// may have committed, so the client never reports it as safely failed: it
// either succeeds or is an unknown outcome.
func TestUploadsThatCommitAfterTheLimitAreNeverSafelyFailed(t *testing.T) {
	for name, save := range map[string]func(*wire.Client) error{
		"SaveArtifact": func(client *wire.Client) error {
			return client.SaveArtifact(t.Context(), storagev1.SaveArtifactRequest{Filename: "f", Data: []byte("x")})
		},
		"SaveResult": func(client *wire.Client) error {
			return client.SaveResult(t.Context(), "n", "t", []byte("r"))
		},
		"SavePromptResultReceipt": func(client *wire.Client) error {
			return client.SavePromptResultReceipt(t.Context(), storagev1.PromptResultReceipt{AttemptID: "a", Data: []byte("r")})
		},
	} {
		t.Run(name, func(t *testing.T) {
			provider := newRecordingProvider()
			client := serveWithBulkLimit(t, provider)
			release := make(chan struct{})
			provider.mu.Lock()
			provider.block[name] = release
			provider.mu.Unlock()
			go func() {
				provider.waitEntered(t, name)
				// Commit after the server's limit has passed, while the
				// caller is still waiting.
				time.Sleep(500 * time.Millisecond)
				close(release)
			}()
			err := save(client)
			require.Len(t, provider.callsTo(name), 1, "the provider committed")
			if err != nil {
				require.ErrorIs(t, err, storagev1.ErrOutcomeUnknown,
					"a write that committed is never reported as safely failed: %v", err)
			}
			require.False(t, errors.Is(err, storagev1.ErrUnavailable) && !errors.Is(err, storagev1.ErrOutcomeUnknown))
		})
	}
}
