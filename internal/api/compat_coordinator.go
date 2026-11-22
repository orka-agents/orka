/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"github.com/gofiber/fiber/v3"
	toolspkg "github.com/orka-agents/orka/internal/tools"

	"github.com/orka-agents/orka/internal/llm"
)

type compatCoordinatorSetup struct {
	Namespace           string
	ToolUseAction       string
	AuthorizationConfig ContextTokenAuthorizationConfig
	ConnectorsEnabled   bool
}

func prepareCompatCoordinatorTools(
	c fiber.Ctx,
	req *llm.CompletionRequest,
	setup compatCoordinatorSetup,
) (bool, error) {
	if c.Get("X-Orka-Tools") == "disabled" {
		return false, nil
	}
	req.Tools = nil
	injectOrkaTools(req)
	if !setup.ConnectorsEnabled {
		req.Tools = filterCompletionToolsExcluding(req.Tools, toolspkg.ListConnectionsToolName)
	}
	req.Tools = filterCompletionToolsForContextToken(c, setup.AuthorizationConfig, req.Tools)
	if err := authorizeContextTokenToolUse(c, setup.AuthorizationConfig, setup.ToolUseAction, completionToolNames(req.Tools)); err != nil {
		return false, err
	}
	req.SystemPrompt = coordinatorSystemPrompt(setup.Namespace) + "\n\n" + req.SystemPrompt
	req.Messages = stripClientToolMessages(req.Messages)
	return true, nil
}
