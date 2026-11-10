package evals

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// CandidateSchemaVersion is the schema a proposer's answer must use.
const CandidateSchemaVersion = "orka.improvement.candidate.v1"

// Bounds on a proposer's answer.
const (
	MaxCandidateHypothesisRunes   = 500
	MaxCandidateSystemPromptBytes = 16 * 1024
	maxCandidateAnswerBytes       = 64 * 1024
)

// Candidate is a proposer's answer: one hypothesis and a patch to the
// promotable fields of the stable Agent. v1 promotes only the inline system
// prompt and the model name under the same provider.
type Candidate struct {
	SchemaVersion string     `json:"schemaVersion"`
	Hypothesis    string     `json:"hypothesis"`
	AgentPatch    AgentPatch `json:"agentPatch"`
}

// AgentPatch is the change a candidate makes. A field left empty keeps the
// stable Agent's value.
type AgentPatch struct {
	SystemPrompt string `json:"systemPrompt,omitempty"`
	Model        string `json:"model,omitempty"`
}

// CandidateLimits are the choices a proposer may make.
type CandidateLimits struct {
	// Models are the model names a candidate may switch to. Empty allows
	// only prompt changes.
	Models []string
	// CurrentSystemPrompt and CurrentModel are the stable Agent's values; a
	// candidate must change at least one of them.
	CurrentSystemPrompt string
	CurrentModel        string
}

// ParseCandidate reads a proposer's answer. The answer is model output, so
// it is decoded strictly: the first JSON object in it must be the whole
// candidate, with no unknown fields, and every field is bounded.
func ParseCandidate(raw []byte, limits CandidateLimits) (*Candidate, error) {
	if len(raw) > maxCandidateAnswerBytes {
		return nil, fmt.Errorf("the answer is %d bytes; at most %d", len(raw), maxCandidateAnswerBytes)
	}
	start := bytes.IndexByte(raw, '{')
	if start < 0 {
		return nil, errors.New("the answer holds no JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw[start:]))
	decoder.DisallowUnknownFields()
	var candidate Candidate
	if err := decoder.Decode(&candidate); err != nil {
		return nil, fmt.Errorf("decode the candidate: %w", err)
	}
	if candidate.SchemaVersion != CandidateSchemaVersion {
		return nil, fmt.Errorf("schemaVersion must be %q", CandidateSchemaVersion)
	}
	candidate.Hypothesis = strings.TrimSpace(candidate.Hypothesis)
	if err := checkCandidateText("hypothesis", candidate.Hypothesis, false); err != nil {
		return nil, err
	}
	if count := utf8.RuneCountInString(candidate.Hypothesis); count > MaxCandidateHypothesisRunes {
		return nil, fmt.Errorf("hypothesis has %d characters; at most %d", count, MaxCandidateHypothesisRunes)
	}
	patch := &candidate.AgentPatch
	patch.Model = strings.TrimSpace(patch.Model)
	if patch.SystemPrompt != "" {
		if strings.TrimSpace(patch.SystemPrompt) == "" {
			return nil, errors.New("agentPatch.systemPrompt is blank")
		}
		if len(patch.SystemPrompt) > MaxCandidateSystemPromptBytes {
			return nil, fmt.Errorf("agentPatch.systemPrompt is %d bytes; at most %d", len(patch.SystemPrompt), MaxCandidateSystemPromptBytes)
		}
		if err := checkCandidateText("agentPatch.systemPrompt", patch.SystemPrompt, true); err != nil {
			return nil, err
		}
	}
	if patch.Model != "" && !slices.Contains(limits.Models, patch.Model) {
		if len(limits.Models) == 0 {
			return nil, errors.New("agentPatch.model is not allowed: this controller permits prompt changes only")
		}
		return nil, fmt.Errorf("agentPatch.model %q is not one of the allowed models %s", patch.Model, strings.Join(limits.Models, ", "))
	}
	promptChanged := patch.SystemPrompt != "" && patch.SystemPrompt != limits.CurrentSystemPrompt
	modelChanged := patch.Model != "" && patch.Model != limits.CurrentModel
	if !promptChanged && !modelChanged {
		return nil, errors.New("the candidate changes neither the system prompt nor the model")
	}
	if !promptChanged {
		patch.SystemPrompt = ""
	}
	if !modelChanged {
		patch.Model = ""
	}
	return &candidate, nil
}

// checkCandidateText rejects control and formatting characters, which could
// hide content from the person who approves the candidate.
func checkCandidateText(name, value string, multiline bool) error {
	if value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	for _, r := range value {
		switch {
		case (r == '\n' || r == '\t') && multiline:
		case r == utf8.RuneError:
			return fmt.Errorf("%s contains a replacement character", name)
		case unicode.IsControl(r), unicode.Is(unicode.Cf, r):
			return fmt.Errorf("%s contains a control or formatting character U+%04X", name, r)
		}
	}
	return nil
}
