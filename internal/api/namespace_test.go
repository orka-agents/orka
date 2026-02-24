/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

func TestResolveNamespace(t *testing.T) {
	tests := []struct {
		name             string
		explicit         string
		watchNamespace   string
		enforceIsolation bool
		userInfo         *UserInfo // nil means no auth context
		wantNS           string
		wantStatus       int // 0 means expect success
	}{
		{
			name:           "explicit matches watchNamespace",
			explicit:       "prod",
			watchNamespace: "prod",
			wantNS:         "prod",
		},
		{
			name:             "explicit mismatches watchNamespace with enforceIsolation",
			explicit:         "other",
			watchNamespace:   "prod",
			enforceIsolation: true,
			wantStatus:       http.StatusForbidden,
		},
		{
			name:           "explicit mismatches watchNamespace without enforceIsolation",
			explicit:       "other",
			watchNamespace: "prod",
			wantStatus:     http.StatusForbidden,
		},
		{
			name:           "no explicit, watchNamespace set",
			explicit:       "",
			watchNamespace: "prod",
			wantNS:         "prod",
		},
		{
			name:     "no explicit, no watchNamespace, SA namespace from context",
			explicit: "",
			userInfo: &UserInfo{
				Username:  "system:serviceaccount:my-ns:worker",
				Namespace: "my-ns",
			},
			wantNS: "my-ns",
		},
		{
			name:     "no explicit, no watchNamespace, no SA",
			explicit: "",
			wantNS:   "default",
		},
		{
			name:     "empty string explicit treated as no explicit",
			explicit: "",
			userInfo: &UserInfo{
				Username:  "system:serviceaccount:sa-ns:worker",
				Namespace: "sa-ns",
			},
			wantNS: "sa-ns",
		},
		{
			name:           "watchNamespace empty, no enforcement, uses fallback chain",
			explicit:       "",
			watchNamespace: "",
			wantNS:         "default",
		},
		{
			name:             "enforceIsolation allows matching namespace",
			explicit:         "my-ns",
			enforceIsolation: true,
			userInfo: &UserInfo{
				Username:  "system:serviceaccount:my-ns:worker",
				Namespace: "my-ns",
			},
			wantNS: "my-ns",
		},
		{
			name:             "enforceIsolation rejects mismatching namespace",
			explicit:         "other-ns",
			enforceIsolation: true,
			userInfo: &UserInfo{
				Username:  "system:serviceaccount:my-ns:worker",
				Namespace: "my-ns",
			},
			wantStatus: http.StatusForbidden,
		},
		{
			name:             "enforceIsolation with no userInfo allows any namespace",
			explicit:         "any-ns",
			enforceIsolation: true,
			wantNS:           "any-ns",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			app := fiber.New()

			var resultNS string
			var resultErr error

			app.Get("/test", func(c fiber.Ctx) error {
				if tt.userInfo != nil {
					c.Locals(UserInfoContextKey, tt.userInfo)
				}
				resultNS, resultErr = ResolveNamespace(c, tt.explicit, tt.watchNamespace, tt.enforceIsolation)
				return c.SendString("OK")
			})

			req := httptest.NewRequest(http.MethodGet, "/test", nil)
			_, err := app.Test(req)
			if err != nil {
				t.Fatalf("Test request failed: %v", err)
			}

			if tt.wantStatus != 0 {
				if resultErr == nil {
					t.Fatalf("expected error with status %d, got nil", tt.wantStatus)
				}
				var fiberErr *fiber.Error
				if ok := errors.As(resultErr, &fiberErr); !ok {
					t.Fatalf("expected *fiber.Error, got %T: %v", resultErr, resultErr)
				} else if fiberErr.Code != tt.wantStatus {
					t.Errorf("error status = %d, want %d", fiberErr.Code, tt.wantStatus)
				}
				return
			}

			if resultErr != nil {
				t.Fatalf("unexpected error: %v", resultErr)
			}
			if resultNS != tt.wantNS {
				t.Errorf("namespace = %q, want %q", resultNS, tt.wantNS)
			}
		})
	}
}
