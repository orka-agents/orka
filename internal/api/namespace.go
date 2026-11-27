/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"fmt"

	"github.com/gofiber/fiber/v3"
)

const resolvedNamespaceLocalKey = "resolvedNamespace"

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

	if enforceIsolation {
		ui := GetUserInfo(c)
		if ui != nil && ui.Namespace == "" {
			log.Info("namespace access denied: namespace-less identity",
				"authType", ui.AuthType,
				"requestedNamespace", ns,
				"ip", c.IP(),
			)
			return "", fiber.NewError(fiber.StatusForbidden, "namespace-bound identity required")
		}
		if ui != nil && ns != ui.Namespace {
			log.Info("namespace access denied: isolation violation",
				"userNamespace", ui.Namespace,
				"requestedNamespace", ns,
				"ip", c.IP(),
			)
			return "", fiber.NewError(fiber.StatusForbidden,
				fmt.Sprintf("namespace %q not allowed, restricted to %q", ns, ui.Namespace))
		}
	}

	c.Locals(resolvedNamespaceLocalKey, ns)
	return ns, nil
}
