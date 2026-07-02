/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gofiber/fiber/v3"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

func TestTaskAccessLoadReadableMapsTaskNotFound(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/read/:id", func(c fiber.Ctx) error {
		_, err := handlers.taskAccess().loadReadable(c, "testRead", "default", c.Params("id"))
		if err != nil {
			return err
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/read/missing", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

func TestTaskAccessLoadReadableReturnsLoadedTask(t *testing.T) {
	task := &corev1alpha1.Task{ObjectMeta: metav1.ObjectMeta{Name: "task-a", Namespace: "default"}}
	handlers, app := setupTestHandlersWithObjects(task)
	app.Get("/read/:id", func(c fiber.Ctx) error {
		loaded, err := handlers.taskAccess().loadReadable(c, "testRead", "default", c.Params("id"))
		if err != nil {
			return err
		}
		return c.JSON(fiber.Map{"name": loaded.Name, "namespace": loaded.Namespace})
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/read/task-a", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
}

func TestTaskAccessContextTokenOnlyLoadSkipsNonContextTokenCallers(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/maybe/:id", func(c fiber.Ctx) error {
		loaded, err := handlers.taskAccess().loadReadableForContextToken(c, "testRead", "default", c.Params("id"))
		if err != nil {
			return err
		}
		if loaded != nil {
			return c.SendStatus(fiber.StatusConflict)
		}
		return c.SendStatus(fiber.StatusNoContent)
	})

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/maybe/missing", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("status = %d, want 204", resp.StatusCode)
	}
}

func TestTaskAccessPreservesTraceStoreMissingBeforeTaskLoad(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Get("/tasks/:id/trace", handlers.GetTaskTrace)

	resp, err := app.Test(httptest.NewRequest(http.MethodGet, "/tasks/missing/trace", nil))
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 before task load", resp.StatusCode)
	}
}

func TestTaskAccessPreservesForkStoreMissingBeforeBodyAndTaskLoad(t *testing.T) {
	handlers, app := setupTestHandlers()
	app.Post("/tasks/:id/fork", handlers.ForkTask)

	req := httptest.NewRequest(http.MethodPost, "/tasks/missing/fork", strings.NewReader("{"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("app.Test: %v", err)
	}
	if resp.StatusCode != http.StatusNotImplemented {
		t.Fatalf("status = %d, want 501 before body parse/task load", resp.StatusCode)
	}
}
