package api

import (
	"errors"

	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/store"
)

const serviceUnavailableRetryAfterSeconds = "1"

func temporaryStorageHTTPError(err error) *fiber.Error {
	if errors.Is(err, store.ErrOutcomeUnknown) {
		return fiber.NewError(fiber.StatusConflict, "storage write outcome is unknown; do not retry this request")
	}
	if !store.IsTemporaryStorageError(err) {
		return nil
	}
	return fiber.NewError(fiber.StatusServiceUnavailable, "storage is temporarily unavailable; retry the request")
}
