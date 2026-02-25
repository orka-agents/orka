/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"fmt"

	"github.com/gofiber/fiber/v3"
)

// ResolveNamespace determines the effective namespace for a request.
// When watchNamespace is set: it's the only allowed namespace (explicit mismatches are rejected with 403).
// Otherwise: explicit param > SA namespace from token > "default".
// If enforceIsolation is true, the resolved namespace must match the user's SA namespace.
func ResolveNamespace(c fiber.Ctx, explicit string, watchNamespace string, enforceIsolation bool) (string, error) {
	var ns string
	if watchNamespace != "" {
		if explicit != "" && explicit != watchNamespace {
			log.Info("namespace access denied: watchNamespace mismatch",
				"requestedNamespace", explicit,
				"allowedNamespace", watchNamespace,
				"ip", c.IP(),
			)
			return "", fiber.NewError(fiber.StatusForbidden, "namespace not allowed")
		}
		ns = watchNamespace
	} else if explicit != "" {
		ns = explicit
	} else {
		ns = GetEffectiveNamespace(c, "")
	}

	// Enforce namespace isolation: user can only access their SA namespace
	if enforceIsolation {
		ui := GetUserInfo(c)
		if ui != nil && ui.Namespace != "" && ns != ui.Namespace {
			log.Info("namespace access denied: isolation violation",
				"userNamespace", ui.Namespace,
				"requestedNamespace", ns,
				"ip", c.IP(),
			)
			return "", fiber.NewError(fiber.StatusForbidden,
				fmt.Sprintf("namespace %q not allowed, restricted to %q", ns, ui.Namespace))
		}
	}

	return ns, nil
}
