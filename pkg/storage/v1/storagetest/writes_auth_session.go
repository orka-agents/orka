package storagetest

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// The GitHub sign-in session write cases.

var catalogAuthSessionAt = time.Date(2026, time.October, 22, 8, 0, 0, 0, time.UTC)

func observeAuthSessions(t *testing.T, p storagev1.StateProvider) any {
	t.Helper()
	sessions := make([]any, 0, 2)
	for _, hash := range []string{"hash-1", "hash-2"} {
		session, err := p.GetAuthSessionByHash(t.Context(), hash)
		sessions = append(sessions, observed(t, session, err))
	}
	return sessions
}

func init() {
	type authSessionCase = writeCase[storagev1.StateProvider]
	create := func(t *testing.T, p storagev1.StateProvider) {
		require.NoError(t, p.CreateAuthSession(t.Context(), testAuthSession("sess-1", "hash-1", catalogAuthSessionAt)))
	}
	stateWriteCases = append(stateWriteCases,
		authSessionCase{
			method:  "CreateAuthSession",
			args:    fixed(testAuthSession("sess-1", "hash-1", catalogAuthSessionAt)),
			observe: observeAuthSessions,
		},
		authSessionCase{
			method:  "RevokeAuthSession",
			setup:   create,
			args:    fixed("sess-1", catalogAuthSessionAt.Add(time.Minute)),
			observe: observeAuthSessions,
		},
		authSessionCase{
			method: "RenewAuthSession",
			setup:  create,
			args: fixed("sess-1", testAuthSession("sess-2", "hash-2", catalogAuthSessionAt.Add(time.Hour)),
				catalogAuthSessionAt.Add(time.Hour)),
			observe: observeAuthSessions,
		},
	)
}
