package acp

import (
	"encoding/json"
	"fmt"
	"regexp"
	"unicode/utf8"
)

// MaxOpenCodeSystemPromptEncodedBytes leaves room for the model, permissions,
// private workspace paths, and proxy settings in OPENCODE_CONFIG_CONTENT.
const MaxOpenCodeSystemPromptEncodedBytes = 32 << 10

// MaxOpenCodeConfigEnvironmentBytes stays below Linux's per-environment-string
// limit. The supervisor checks the complete encoded configuration as well.
const MaxOpenCodeConfigEnvironmentBytes = 96 << 10

var openCodePromptSubstitution = regexp.MustCompile(`(?i)\{\s*(?:env|file)\s*:`)

// ValidateOpenCodeSystemPrompt admits bounded literal prompts only. OpenCode
// expands configuration substitutions before JSON parsing; Agent text must not
// turn into environment/file reads or expose the session proxy capability.
func ValidateOpenCodeSystemPrompt(prompt string) error {
	if !utf8.ValidString(prompt) {
		return fmt.Errorf("OpenCode system prompt must be valid UTF-8")
	}
	if openCodePromptSubstitution.MatchString(prompt) {
		return fmt.Errorf("OpenCode system prompt must not contain configuration substitutions")
	}
	encoded, err := json.Marshal(prompt)
	if err != nil {
		return fmt.Errorf("OpenCode system prompt cannot be encoded")
	}
	if len(encoded) > MaxOpenCodeSystemPromptEncodedBytes {
		return fmt.Errorf("OpenCode system prompt exceeds the supported encoded limit of %d bytes", MaxOpenCodeSystemPromptEncodedBytes)
	}
	return nil
}
