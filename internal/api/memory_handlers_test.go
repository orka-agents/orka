/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

func TestHandlers_UpdateMemoryRejectsQueryBodyNamespaceMismatch(t *testing.T) {
	tests := []struct {
		name             string
		enforceIsolation bool
		user             *UserInfo
	}{
		{
			name: "cluster-scoped caller",
			user: &UserInfo{
				AuthType: AuthTypeTokenReview,
				Username: "cluster-admin",
			},
		},
		{
			name:             "namespace-bound caller",
			enforceIsolation: true,
			user: &UserInfo{
				AuthType:  AuthTypeTokenReview,
				Username:  "system:serviceaccount:team-a:operator",
				Namespace: "team-a",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memory := store.Memory{
				ID:        "mem-namespace-mismatch",
				Namespace: "team-a",
				Content:   "original content",
				Source:    "manual",
			}
			app, memoryStore := setupPublicMemoryUpdateTest(t, tt.enforceIsolation, tt.user, memory)

			response := testJSONRequest(t, app, http.MethodPut,
				"/memories/"+memory.ID+"?namespace=team-a",
				map[string]any{"namespace": "team-b", "content": "mutated content"})
			require.Equal(t, http.StatusBadRequest, response.StatusCode)

			stored, err := memoryStore.GetMemory(context.Background(), memory.Namespace, memory.ID)
			require.NoError(t, err)
			require.Equal(t, "original content", stored.Content)
		})
	}
}

func TestHandlers_UpdateMemoryPreservesNamespaceDefaulting(t *testing.T) {
	tests := []struct {
		name          string
		query         string
		bodyNamespace string
	}{
		{name: "empty body namespace uses query", query: "?namespace=team-a"},
		{name: "matching body namespace uses query", query: "?namespace=team-a", bodyNamespace: "team-a"},
		{name: "empty body and query use caller namespace"},
		{name: "body namespace is used without query", bodyNamespace: "team-a"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			memory := store.Memory{
				ID:        "mem-namespace-defaulting",
				Namespace: "team-a",
				Content:   "original content",
				Source:    "manual",
			}
			app, memoryStore := setupPublicMemoryUpdateTest(t, true, &UserInfo{
				AuthType:  AuthTypeTokenReview,
				Username:  "system:serviceaccount:team-a:operator",
				Namespace: "team-a",
			}, memory)

			body := map[string]any{"content": "updated content"}
			if tt.bodyNamespace != "" {
				body["namespace"] = tt.bodyNamespace
			}
			response := testJSONRequest(t, app, http.MethodPut, "/memories/"+memory.ID+tt.query, body)
			require.Equal(t, http.StatusOK, response.StatusCode)

			stored, err := memoryStore.GetMemory(context.Background(), memory.Namespace, memory.ID)
			require.NoError(t, err)
			require.Equal(t, "updated content", stored.Content)
		})
	}
}

func setupPublicMemoryUpdateTest(
	t *testing.T,
	enforceIsolation bool,
	user *UserInfo,
	memories ...store.Memory,
) (*fiber.App, *sqlite.Store) {
	t.Helper()

	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	memoryStore := sqlite.NewStore(db, ":memory:")
	for i := range memories {
		memory := memories[i]
		require.NoError(t, memoryStore.CreateMemory(context.Background(), &memory))
	}

	handlers := NewHandlers(HandlersConfig{
		EnforceNamespaceIsolation: enforceIsolation,
		MemoryStore:               memoryStore,
	})
	app := fiber.New()
	if user != nil {
		app.Use(func(c fiber.Ctx) error {
			c.Locals(UserInfoContextKey, user)
			return c.Next()
		})
	}
	app.Put("/memories/:id", handlers.UpdateMemory)
	return app, memoryStore
}
