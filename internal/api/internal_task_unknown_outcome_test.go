package api

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

type committedMessageLostReplyProvider struct {
	*sqlite.Store
	afterCommit func() error
	writes      int
}

func (s *committedMessageLostReplyProvider) SendMessage(ctx context.Context, message *store.Message) error {
	if err := s.Store.SendMessage(ctx, message); err != nil {
		return err
	}
	s.writes++
	if s.writes == 1 {
		if err := s.afterCommit(); err != nil {
			return err
		}
		return store.ErrOutcomeUnknown
	}
	return nil
}

func TestTaskDataCommittedMessageLostReplyDoesNotReauthorize(t *testing.T) {
	for name, wrap := range map[string]func(error) error{
		"wrapped handler error": func(err error) error { return fmt.Errorf("message handler: %w", err) },
		"joined cleanup error":  func(err error) error { return errors.Join(err, store.ErrTaskDataCleanupChanged) },
	} {
		t.Run(name, func(t *testing.T) {
			db, err := sqlite.NewDB(":memory:")
			require.NoError(t, err)
			t.Cleanup(func() { _ = db.Close() })
			backend := sqlite.NewStore(db, ":memory:")
			cleanup := testStorageClient(backend)
			provider := &committedMessageLostReplyProvider{Store: backend, afterCommit: func() error {
				return cleanup.DeleteResult(t.Context(), "tenant", "unrelated-task")
			}}
			appStorage, err := storageclient.NewEmbedded(provider)
			require.NoError(t, err)
			authorizations := 0
			app := fiber.New()
			app.Post("/namespaces/:namespace/messages", func(c fiber.Ctx) error {
				if err := withInternalTaskData(c, appStorage, "", func(context.Context) error {
					authorizations++
					return nil
				}, func(ctx context.Context) error {
					err := appStorage.SendMessage(ctx, &store.Message{
						Namespace: "tenant", FromTask: "sender", ToTask: "reader", ParentTask: "parent", Content: "one message",
					})
					if err != nil {
						return wrap(err)
					}
					return nil
				}); err != nil {
					return err
				}
				return c.SendStatus(http.StatusNoContent)
			})
			response, err := app.Test(httptest.NewRequest(http.MethodPost, "/namespaces/tenant/messages", nil))
			require.NoError(t, err)
			defer response.Body.Close() //nolint:errcheck
			require.Equal(t, http.StatusConflict, response.StatusCode)
			require.Empty(t, response.Header.Get(fiber.HeaderRetryAfter))
			require.Equal(t, 1, authorizations)
			require.Equal(t, 1, provider.writes)
			messages, err := backend.GetMessages(t.Context(), "tenant", "reader", "parent", false)
			require.NoError(t, err)
			require.Len(t, messages, 1, "an unreadable committed write must not become a fresh insert")
		})
	}
}
