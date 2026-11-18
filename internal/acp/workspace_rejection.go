package acp

// Public runtime session creation messages for a workspace a built-in runtime
// refuses to run. The supervisor returns exactly these strings and the
// controller surfaces them verbatim in Task status, so they must stay fixed and
// never include workspace contents.
const (
	WorkspaceValidationFailedMessage            = "runtime session failed during workspace validation"
	OpenCodeProjectConfigurationRejectedMessage = WorkspaceValidationFailedMessage +
		": the OpenCode runtime refuses workspaces containing .opencode, opencode.json, or opencode.jsonc"
)
