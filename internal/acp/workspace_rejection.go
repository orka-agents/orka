package acp

const (
	WorkspaceValidationFailedMessage            = "runtime session failed during workspace validation"
	OpenCodeProjectConfigurationRejectedMessage = WorkspaceValidationFailedMessage +
		": the OpenCode runtime refuses workspaces containing .opencode, opencode.json, or opencode.jsonc"
)
