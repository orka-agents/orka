package api

import (
	"errors"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/store"
)

// serviceUnavailableRetryAfterSeconds is the retry hint every 503 answer
// carries unless its handler set its own.
const serviceUnavailableRetryAfterSeconds = "1"

// temporaryStorageHTTPError distinguishes safe storage retries from unknown
// outcomes. HTTP retries do not preserve a storage operation ID, so an unknown
// outcome must not advertise a new request as safe to repeat.
func temporaryStorageHTTPError(err error) *fiber.Error {
	if errors.Is(err, store.ErrOutcomeUnknown) {
		return fiber.NewError(fiber.StatusConflict, "storage write outcome is unknown; do not retry this request")
	}
	if !store.IsTemporaryStorageError(err) {
		return nil
	}
	return fiber.NewError(fiber.StatusServiceUnavailable, "storage is temporarily unavailable; retry the request")
}
