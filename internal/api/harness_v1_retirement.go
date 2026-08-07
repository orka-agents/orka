/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"errors"
	"strings"

	"github.com/gofiber/fiber/v3"
)

func (s *Server) handleHarnessV1Retirement(c fiber.Ctx) error {
	if s == nil || s.config.HarnessV1Retirement == nil {
		return fiber.NewError(fiber.StatusNotImplemented, "harness v1 retirement is unavailable")
	}
	userInfo := GetUserInfo(c)
	wantUsername := strings.TrimSpace(s.config.HarnessV1RetirementUsername)
	if userInfo == nil || userInfo.AuthType != AuthTypeTokenReview || wantUsername == "" ||
		strings.TrimSpace(userInfo.Username) != wantUsername {
		return fiber.NewError(fiber.StatusForbidden, "caller is not the harness v1 retirement hook")
	}
	if err := s.config.HarnessV1Retirement.Retire(c.Context()); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return fiber.NewError(fiber.StatusGatewayTimeout, "harness v1 retirement timed out")
		}
		log.Error(err, "harness v1 controller retirement failed")
		return fiber.NewError(fiber.StatusServiceUnavailable, "harness v1 retirement failed")
	}
	return c.SendStatus(fiber.StatusNoContent)
}
