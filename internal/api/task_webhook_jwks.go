/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"context"
	"time"

	"github.com/gofiber/fiber/v3"
	"github.com/orka-agents/orka/internal/taskwebhook"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
)

// TaskWebhookJWKSPath publishes the public keys that verify Task webhook
// deliveries. It is unauthenticated: the set holds only public keys.
const TaskWebhookJWKSPath = "/.well-known/orka/task-webhook-jwks.json"

// taskWebhookJWKS serves the current and previous Task webhook verification
// keys. Without a configured key source it serves an empty set.
func (s *Server) taskWebhookJWKS(c fiber.Ctx) error {
	set := taskwebhook.JWKS{Keys: []taskwebhook.JWK{}}
	if s.config.TaskWebhookKeys != nil {
		ctx, cancel := context.WithTimeout(c.Context(), 5*time.Second)
		defer cancel()
		keys, err := s.config.TaskWebhookKeys.PublishedKeys(ctx)
		if err != nil {
			logf.FromContext(ctx).Error(err, "failed to load Task webhook verification keys")
			c.Set(fiber.HeaderCacheControl, "no-store")
			return c.Status(fiber.StatusServiceUnavailable).JSON(fiber.Map{
				apiFieldError: fiber.Map{"code": fiber.StatusServiceUnavailable, apiFieldMessage: "webhook verification keys are unavailable"},
			})
		}
		set = keys.JWKS()
	}
	c.Set(fiber.HeaderCacheControl, "public, max-age=60")
	c.Set(fiber.HeaderContentType, "application/jwk-set+json")
	return c.JSON(set, "application/jwk-set+json")
}
