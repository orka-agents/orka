/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"net/http/httptest"
	"testing"

	"github.com/gofiber/fiber/v3"
)

type recordingHarnessV1RetirementService struct {
	calls int
}

func (s *recordingHarnessV1RetirementService) Retire(context.Context) error {
	s.calls++
	return nil
}

func TestHarnessV1RetirementRequiresExactTokenReviewIdentity(t *testing.T) {
	t.Parallel()
	const username = "system:serviceaccount:orka-test:orka-harness-v1-drain"
	tests := []struct {
		name       string
		user       *UserInfo
		wantStatus int
		wantCalls  int
	}{
		{
			name:       "exact hook",
			user:       &UserInfo{Username: username, AuthType: AuthTypeTokenReview},
			wantStatus: fiber.StatusNoContent,
			wantCalls:  1,
		},
		{
			name:       "wrong service account",
			user:       &UserInfo{Username: "system:serviceaccount:orka-test:other", AuthType: AuthTypeTokenReview},
			wantStatus: fiber.StatusForbidden,
		},
		{
			name:       "oidc identity",
			user:       &UserInfo{Username: username, AuthType: AuthTypeOIDC},
			wantStatus: fiber.StatusForbidden,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			retirement := &recordingHarnessV1RetirementService{}
			server := &Server{config: ServerConfig{
				HarnessV1Retirement: retirement, HarnessV1RetirementUsername: username,
			}}
			app := fiber.New(fiber.Config{ErrorHandler: customErrorHandler})
			app.Post(harnessV1RetirementPath, func(c fiber.Ctx) error {
				c.Locals(UserInfoContextKey, tt.user)
				return server.handleHarnessV1Retirement(c)
			})
			request := httptest.NewRequest("POST", harnessV1RetirementPath, nil)
			response, err := app.Test(request)
			if err != nil {
				t.Fatalf("request: %v", err)
			}
			defer func() { _ = response.Body.Close() }()
			if response.StatusCode != tt.wantStatus {
				t.Fatalf("status = %d, want %d", response.StatusCode, tt.wantStatus)
			}
			if retirement.calls != tt.wantCalls {
				t.Fatalf("retirement calls = %d, want %d", retirement.calls, tt.wantCalls)
			}
		})
	}
}
