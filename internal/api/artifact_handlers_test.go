/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	corev1alpha1 "github.com/sozercan/orka/api/v1alpha1"
	"github.com/sozercan/orka/internal/store"
	"github.com/sozercan/orka/internal/store/sqlite"
)

// setupTestHandlersWithArtifactStore creates Handlers backed by an in-memory
// SQLite store that implements ArtifactStore, plus any runtime objects for the
// fake k8s client.
func setupTestHandlersWithArtifactStore(objs ...runtime.Object) (*Handlers, *fiber.App, *sqlite.Store) {
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(objs...).Build()
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	handlers := NewHandlers(HandlersConfig{Client: fakeClient, SessionStore: ss, ResultStore: ss, ArtifactStore: ss})

	app := fiber.New()
	return handlers, app, ss
}

// ---------- Internal: UploadArtifact ----------

func TestUploadArtifact(t *testing.T) {
	h, app, ss := setupTestInternalHandlers()
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	t.Run("success", func(t *testing.T) {
		body := []byte("hello artifact")
		req := httptest.NewRequest(http.MethodPost,
			"/internal/v1/artifacts/default/my-task/output.txt",
			bytes.NewReader(body))
		req.Header.Set("Content-Type", "text/plain")

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)

		// Verify data was stored
		data, ct, err := ss.GetArtifact(context.Background(), "default", "my-task", "output.txt")
		require.NoError(t, err)
		require.Equal(t, body, data)
		require.Equal(t, "text/plain", ct)
	})

	t.Run("default content type", func(t *testing.T) {
		body := []byte("binary data")
		req := httptest.NewRequest(http.MethodPost,
			"/internal/v1/artifacts/default/my-task/data.bin",
			bytes.NewReader(body))
		// No Content-Type header set

		resp, err := app.Test(req)
		require.NoError(t, err)
		require.Equal(t, http.StatusCreated, resp.StatusCode)

		_, ct, err := ss.GetArtifact(context.Background(), "default", "my-task", "data.bin")
		require.NoError(t, err)
		require.Equal(t, "application/octet-stream", ct)
	})
}

func TestUploadArtifactTooLarge(t *testing.T) {
	h, _, _ := setupTestInternalHandlers()
	// Use a custom app with a large enough body limit so the handler's own
	// size check is exercised rather than Fiber's built-in limit.
	app := fiber.New(fiber.Config{BodyLimit: 20 << 20})
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username: "system:serviceaccount:default:worker",
		})
		return c.Next()
	})
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	body := make([]byte, 10<<20+1) // 10MB + 1 byte
	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/my-task/big.bin",
		bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/octet-stream")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusRequestEntityTooLarge, resp.StatusCode)
}

func TestUploadArtifactEmptyBody(t *testing.T) {
	h, app, _ := setupTestInternalHandlers()
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/my-task/empty.txt",
		bytes.NewReader([]byte{}))
	req.Header.Set("Content-Type", "text/plain")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusBadRequest, resp.StatusCode)
}

func TestUploadArtifactMissingParams(t *testing.T) {
	h, _, _ := setupTestInternalHandlers()

	tests := []struct {
		name string
		path string
	}{
		{"missing filename", "/internal/v1/artifacts/:namespace/:taskName"},
		{"missing taskName", "/internal/v1/artifacts/:namespace"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			app := fiber.New()
			app.Use(func(c fiber.Ctx) error {
				c.Locals(UserInfoContextKey, &UserInfo{
					Username: "system:serviceaccount:default:worker",
				})
				return c.Next()
			})
			app.Post(tc.path, h.UploadArtifact)

			url := strings.ReplaceAll(tc.path, ":namespace", "default")
			url = strings.ReplaceAll(url, ":taskName", "my-task")
			req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader([]byte("data")))
			req.Header.Set("Content-Type", "text/plain")

			resp, err := app.Test(req)
			require.NoError(t, err)
			require.Equal(t, http.StatusBadRequest, resp.StatusCode)
		})
	}
}

func TestUploadArtifactStoreNotEnabled(t *testing.T) {
	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	h := NewInternalHandlers(ss, ss, ss, ss, nil) // nil artifact store

	app := fiber.New()
	app.Use(func(c fiber.Ctx) error {
		c.Locals(UserInfoContextKey, &UserInfo{
			Username: "system:serviceaccount:default:worker",
		})
		return c.Next()
	})
	app.Post("/internal/v1/artifacts/:namespace/:taskName/:filename", h.UploadArtifact)

	req := httptest.NewRequest(http.MethodPost,
		"/internal/v1/artifacts/default/my-task/file.txt",
		bytes.NewReader([]byte("data")))
	req.Header.Set("Content-Type", "text/plain")

	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotImplemented, resp.StatusCode)
}

// ---------- Public: ListTaskArtifacts ----------

func TestListTaskArtifacts(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "my-task", Namespace: "default"},
	}
	h, app, ss := setupTestHandlersWithArtifactStore(task)
	app.Get("/api/v1/tasks/:id/artifacts", h.ListTaskArtifacts)

	// Pre-populate artifacts
	ctx := context.Background()
	require.NoError(t, ss.SaveArtifact(ctx, "default", "my-task", "output.txt", "text/plain", []byte("hello")))
	require.NoError(t, ss.SaveArtifact(ctx, "default", "my-task", "report.json", "application/json", []byte(`{"ok":true}`)))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/my-task/artifacts", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Artifacts []store.ArtifactMetadata `json:"artifacts"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Len(t, result.Artifacts, 2)

	names := map[string]bool{}
	for _, a := range result.Artifacts {
		names[a.Filename] = true
	}
	require.True(t, names["output.txt"])
	require.True(t, names["report.json"])
}

func TestListTaskArtifactsEmpty(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "empty-task", Namespace: "default"},
	}
	h, app, _ := setupTestHandlersWithArtifactStore(task)
	app.Get("/api/v1/tasks/:id/artifacts", h.ListTaskArtifacts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/empty-task/artifacts", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var result struct {
		Artifacts []store.ArtifactMetadata `json:"artifacts"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&result))
	require.Empty(t, result.Artifacts)
}

func TestListTaskArtifactsTaskNotFound(t *testing.T) {
	h, app, _ := setupTestHandlersWithArtifactStore() // no task objects
	app.Get("/api/v1/tasks/:id/artifacts", h.ListTaskArtifacts)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/nonexistent/artifacts", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

// ---------- Public: DownloadTaskArtifact ----------

func TestDownloadTaskArtifact(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "my-task", Namespace: "default"},
	}
	h, app, ss := setupTestHandlersWithArtifactStore(task)
	app.Get("/api/v1/tasks/:id/artifacts/:filename", h.DownloadTaskArtifact)

	ctx := context.Background()
	data := []byte("artifact content here")
	require.NoError(t, ss.SaveArtifact(ctx, "default", "my-task", "output.txt", "text/plain", data))

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/my-task/artifacts/output.txt", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Equal(t, "text/plain", resp.Header.Get("Content-Type"))
	require.Contains(t, resp.Header.Get("Content-Disposition"), `filename="output.txt"`)

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	require.Equal(t, data, body)
}

func TestDownloadTaskArtifactNotFound(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "my-task", Namespace: "default"},
	}
	h, app, _ := setupTestHandlersWithArtifactStore(task)
	app.Get("/api/v1/tasks/:id/artifacts/:filename", h.DownloadTaskArtifact)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/my-task/artifacts/nonexistent.txt", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDownloadTaskArtifactTaskNotFound(t *testing.T) {
	h, app, _ := setupTestHandlersWithArtifactStore() // no task objects
	app.Get("/api/v1/tasks/:id/artifacts/:filename", h.DownloadTaskArtifact)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/nonexistent/artifacts/file.txt", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}

func TestDownloadTaskArtifactStoreNotConfigured(t *testing.T) {
	task := &corev1alpha1.Task{
		ObjectMeta: metav1.ObjectMeta{Name: "my-task", Namespace: "default"},
	}
	scheme := runtime.NewScheme()
	_ = corev1alpha1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithRuntimeObjects(task).Build()

	db, _ := sqlite.NewDB(":memory:")
	ss := sqlite.NewStore(db, ":memory:")
	h := NewHandlers(HandlersConfig{Client: fakeClient, SessionStore: ss, ResultStore: ss}) // nil artifact store

	app := fiber.New()
	app.Get("/api/v1/tasks/:id/artifacts/:filename", h.DownloadTaskArtifact)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/tasks/my-task/artifacts/file.txt", nil)
	resp, err := app.Test(req)
	require.NoError(t, err)
	require.Equal(t, http.StatusNotFound, resp.StatusCode)
}
