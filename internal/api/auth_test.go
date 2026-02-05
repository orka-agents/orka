/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
	authenticationv1 "k8s.io/api/authentication/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func setupTestApp(c client.Client) *fiber.App {
	app := fiber.New()
	app.Use(NewAuthMiddleware(c))
	app.Get("/test", func(ctx fiber.Ctx) error {
		return ctx.SendString("OK")
	})
	return app
}

func TestNewAuthMiddleware_MissingAuthHeader(t *testing.T) {
	scheme := runtime.NewScheme()
	authenticationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	app := setupTestApp(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestNewAuthMiddleware_InvalidFormat(t *testing.T) {
	scheme := runtime.NewScheme()
	authenticationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	app := setupTestApp(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Basic dXNlcjpwYXNz") // Not Bearer

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestNewAuthMiddleware_EmptyToken(t *testing.T) {
	scheme := runtime.NewScheme()
	authenticationv1.AddToScheme(scheme)
	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()

	app := setupTestApp(fakeClient)

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("Authorization", "Bearer ") // Empty token

	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusUnauthorized)
	}
}

func TestGetUserInfo_ValidContext(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(ctx fiber.Ctx) error {
		// Set user info in context
		userInfo := &UserInfo{
			Username: "test-user",
			UID:      "uid-123",
			Groups:   []string{"group1", "group2"},
		}
		ctx.Locals(UserInfoContextKey, userInfo)

		// Get user info
		retrieved := GetUserInfo(ctx)
		if retrieved == nil {
			return fiber.NewError(fiber.StatusInternalServerError, "user info is nil")
		}
		if retrieved.Username != "test-user" {
			return fiber.NewError(fiber.StatusInternalServerError, "username mismatch")
		}
		if retrieved.UID != "uid-123" {
			return fiber.NewError(fiber.StatusInternalServerError, "UID mismatch")
		}
		if len(retrieved.Groups) != 2 {
			return fiber.NewError(fiber.StatusInternalServerError, "groups mismatch")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestGetUserInfo_NilContext(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(ctx fiber.Ctx) error {
		// Don't set user info - should return nil
		retrieved := GetUserInfo(ctx)
		if retrieved != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "expected nil user info")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestGetUserInfo_WrongType(t *testing.T) {
	app := fiber.New()
	app.Get("/test", func(ctx fiber.Ctx) error {
		// Set wrong type in context
		ctx.Locals(UserInfoContextKey, "not a UserInfo")

		// Should return nil for wrong type
		retrieved := GetUserInfo(ctx)
		if retrieved != nil {
			return fiber.NewError(fiber.StatusInternalServerError, "expected nil for wrong type")
		}
		return ctx.SendString("OK")
	})

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	resp, err := app.Test(req)
	if err != nil {
		t.Fatalf("Test request failed: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		t.Errorf("StatusCode = %d, want %d", resp.StatusCode, http.StatusOK)
	}
}

func TestUserInfo_Fields(t *testing.T) {
	userInfo := &UserInfo{
		Username: "system:serviceaccount:default:my-sa",
		UID:      "abc-123",
		Groups:   []string{"system:serviceaccounts", "system:serviceaccounts:default"},
	}

	if userInfo.Username != "system:serviceaccount:default:my-sa" {
		t.Errorf("Username = %s, want system:serviceaccount:default:my-sa", userInfo.Username)
	}
	if userInfo.UID != "abc-123" {
		t.Errorf("UID = %s, want abc-123", userInfo.UID)
	}
	if len(userInfo.Groups) != 2 {
		t.Errorf("Groups len = %d, want 2", len(userInfo.Groups))
	}
}

func TestConstants(t *testing.T) {
	if AuthHeader != "Authorization" {
		t.Errorf("AuthHeader = %s, want Authorization", AuthHeader)
	}
	if BearerPrefix != "Bearer " {
		t.Errorf("BearerPrefix = %s, want 'Bearer '", BearerPrefix)
	}
	if UserInfoContextKey != "userInfo" {
		t.Errorf("UserInfoContextKey = %s, want userInfo", UserInfoContextKey)
	}
}
