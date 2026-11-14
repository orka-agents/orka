// Copyright (c) 2026. MIT License - see LICENSE file for details.

package api

import (
	"fmt"
	"strings"

	"github.com/gofiber/fiber/v3"
	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// UpdateAgentSpecRequest replaces a complete spec only on the revision the
// caller observed. Unlike the legacy PUT, none of these fields is optional.
type UpdateAgentSpecRequest struct {
	ExpectedUID        string                  `json:"expectedUID"`
	ExpectedGeneration int64                   `json:"expectedGeneration"`
	Spec               *corev1alpha1.AgentSpec `json:"spec"`
}

// UpdateAgentSpec conditionally replaces an Agent's spec without adopting a
// concurrent spec edit or a same-named replacement during a patch retry.
func (h *Handlers) UpdateAgentSpec(c fiber.Ctx) error {
	namespace, err := h.resolveNamespace(c, c.Query(toolNamespaceArg, ""))
	if err != nil {
		return err
	}
	if err := h.authorizeContextTokenAction(c, "updateAgent", h.contextTokenAuthorization.AgentWriteScopes); err != nil {
		return err
	}
	// Keep the legacy handler's authorization-before-parsing order.
	var req UpdateAgentSpecRequest
	if err := c.Bind().JSON(&req); err != nil {
		return fiber.NewError(fiber.StatusBadRequest, fmt.Sprintf("invalid request body: %v", err))
	}
	if strings.TrimSpace(req.ExpectedUID) == "" || req.ExpectedGeneration <= 0 || req.Spec == nil {
		return fiber.NewError(fiber.StatusBadRequest, "expectedUID, positive expectedGeneration and spec are required")
	}
	if err := validateAgentSpec(*req.Spec); err != nil {
		return err
	}
	return h.updateAgentSpec(c, namespace, *req.Spec, &req)
}
