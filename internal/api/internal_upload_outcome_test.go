package api

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/storageclient"
	"github.com/orka-agents/orka/internal/storageclient/storagefault"
	"github.com/orka-agents/orka/internal/store"
	"github.com/orka-agents/orka/internal/store/sqlite"
)

// uploadOutcomeApp serves the worker result and artifact upload routes with
// the server's error handler over a storage client whose writes the returned
// injector controls.
func uploadOutcomeApp(t *testing.T) (*fiber.App, *storagefault.Injector, *storageclient.Client) {
	t.Helper()
	db, err := sqlite.NewDB(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	stores, injector, err := storagefault.NewEmbeddedClient(sqlite.NewStore(db, ":memory:"))
	require.NoError(t, err)
	h, _, _ := setupTestInternalHandlers()
	h.resultStore = stores
	h.artifactStore = stores
	app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
	user := internalCallerAuthWorkerUser("my-task-pod", "my-task-pod-uid")
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, user)
		ctx, cancel := context.WithTimeout(c.Context(), 10*time.Second)
		defer cancel()
		c.SetContext(ctx)
		return c.Next()
	})
	app.Post("/internal/v1/results/:namespace/:taskName", h.SubmitResult)
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)
	return app, injector, stores
}

func postUpload(t *testing.T, app *fiber.App, path, body string) (*http.Response, string) {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
	request.Header.Set(fiber.HeaderContentType, "text/plain")
	resp, err := app.Test(request, fiber.TestConfig{Timeout: 30 * time.Second, FailOnTimeout: true})
	require.NoError(t, err)
	data, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	return resp, string(data)
}

// A result or artifact upload whose write committed but whose outcome the
// storage client could not read answers 409 without a retry hint: a new HTTP
// request carries a new operation ID and a later order stamp, so it would not
// replay the committed write but run again.
func TestInternalUploadsAnswerUnknownOutcomeWithoutRetryHint(t *testing.T) {
	for _, upload := range []struct {
		name, method, path string
		stored             func(*testing.T, *storageclient.Client) string
	}{
		{
			name: "result", method: "SaveResult", path: "/internal/v1/results/default/my-task",
			stored: func(t *testing.T, stores *storageclient.Client) string {
				data, err := stores.GetResult(t.Context(), "default", "my-task")
				require.NoError(t, err)
				return string(data)
			},
		},
		{
			name: "artifact", method: "SaveArtifact", path: "/internal/v1/artifacts/default/my-task/out.txt",
			stored: func(t *testing.T, stores *storageclient.Client) string {
				data, _, err := stores.GetArtifact(t.Context(), store.ArtifactKey{
					Owner: store.ArtifactOwner{Namespace: "default", TaskName: "my-task"}, Filename: "out.txt",
				})
				require.NoError(t, err)
				return string(data)
			},
		},
	} {
		t.Run(upload.name, func(t *testing.T) {
			app, injector, stores := uploadOutcomeApp(t)
			write := storagefault.Match{Methods: []string{upload.method}}
			// The first attempt commits and loses its response; every later
			// attempt, including the client's replay after it resolves the
			// operation as committed, is refused, so the outcome cannot be read.
			refused := injector.Inject(storagefault.Fault{
				Match: write, Action: storagefault.Fail(fmt.Errorf("%w: refused by the test", store.ErrConflict)),
			})
			lost := injector.Inject(storagefault.Fault{Match: write, Action: storagefault.LoseResponse(), Times: 1})

			resp, body := postUpload(t, app, upload.path, "uploaded")
			require.Equal(t, http.StatusConflict, resp.StatusCode, body)
			require.Contains(t, body, "storage write outcome is unknown; do not retry this request")
			require.Empty(t, resp.Header.Get(fiber.HeaderRetryAfter))
			require.Equal(t, 1, lost.Hits())
			require.Positive(t, refused.Hits())

			refused.Remove()
			require.Equal(t, "uploaded", upload.stored(t, stores), "the write committed once")
		})
	}
}
