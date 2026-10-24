package api

import (
	"github.com/gofiber/fiber/v3"

	"github.com/orka-agents/orka/internal/store"
)

// serviceUnavailableRetryAfterSeconds is the retry hint every 503 answer
// carries unless its handler set its own.
const serviceUnavailableRetryAfterSeconds = "1"

// temporaryStorageHTTPError answers a storage write that was not accepted for
// now, or whose outcome is unknown, with 503 so the caller retries: it is
// neither a success nor a definite failure. It returns nil for any other
// error.
func temporaryStorageHTTPError(err error) *fiber.Error {
	if !store.IsTemporaryStorageError(err) {
		return nil
	}
	return fiber.NewError(fiber.StatusServiceUnavailable, "storage is temporarily unavailable; retry the request")
}
