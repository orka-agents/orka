package main

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/orka-agents/orka/internal/llm"
)

type validatedAnalysisCall struct {
	Analysis validatedAnalysis `json:"analysis"`
}

type validatedAnalysis struct {
	Summary         string   `json:"summary"`
	IsTransient     *bool    `json:"is_transient"`
	RootCause       string   `json:"root_cause"`
	Severity        string   `json:"severity"`
	SuggestedFix    string   `json:"suggested_fix"`
	RelevantFiles   []string `json:"relevant_files"`
	GCSBytes        *int     `json:"gcs_bytes,omitempty"`
	ValidationToken string   `json:"validation_token,omitempty"`
}

type validationToolResult struct {
	GCSBytes        *int   `json:"gcs_bytes"`
	ValidationToken string `json:"validation_token"`
}

func isAnalysisValidationTool(name string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	return normalized == "validate-analysis" || strings.HasPrefix(normalized, "validate-analysis-")
}

func isTimelineVerificationTool(name string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	return normalized == "verify-timeline" || strings.HasPrefix(normalized, "verify-timeline-")
}

func validatedAnalysisResult(toolName string, args json.RawMessage, result string) (string, bool, error) {
	if !isAnalysisValidationTool(toolName) {
		return "", false, nil
	}
	var call validatedAnalysisCall
	if err := json.Unmarshal(args, &call); err != nil {
		return "", true, fmt.Errorf("parse validate_analysis arguments: %w", err)
	}
	if err := validateAnalysisShape(call.Analysis); err != nil {
		return "", true, err
	}
	var validation validationToolResult
	if err := json.Unmarshal([]byte(result), &validation); err != nil {
		return "", true, fmt.Errorf("parse validate_analysis result: %w", err)
	}
	if validation.GCSBytes == nil || *validation.GCSBytes < 0 {
		return "", true, fmt.Errorf("validate_analysis result has no gcs_bytes")
	}
	if strings.TrimSpace(validation.ValidationToken) == "" {
		return "", true, fmt.Errorf("validate_analysis result has no validation_token")
	}
	call.Analysis.GCSBytes = validation.GCSBytes
	call.Analysis.ValidationToken = validation.ValidationToken
	final, err := json.Marshal(call.Analysis)
	if err != nil {
		return "", true, fmt.Errorf("marshal validated analysis: %w", err)
	}
	return string(final), true, nil
}

func validateAnalysisShape(a validatedAnalysis) error {
	if strings.TrimSpace(a.Summary) == "" {
		return fmt.Errorf("analysis.summary is required")
	}
	if strings.TrimSpace(a.RootCause) == "" {
		return fmt.Errorf("analysis.root_cause is required")
	}
	switch strings.ToLower(strings.TrimSpace(a.Severity)) {
	case "critical", "high", "medium", "low":
	default:
		return fmt.Errorf("analysis.severity %q is invalid", a.Severity)
	}
	if a.IsTransient == nil {
		return fmt.Errorf("analysis.is_transient is required")
	}
	if strings.TrimSpace(a.SuggestedFix) == "" {
		return fmt.Errorf("analysis.suggested_fix is required")
	}
	if a.RelevantFiles == nil {
		return fmt.Errorf("analysis.relevant_files is required")
	}
	return nil
}

func analysisFinalizationTools(tools []llm.Tool) []llm.Tool {
	out := make([]llm.Tool, 0, 2)
	for _, tool := range tools {
		if isAnalysisValidationTool(tool.Name) || isTimelineVerificationTool(tool.Name) {
			out = append(out, tool)
		}
	}
	return out
}

func hasAnalysisValidationTool(tools []llm.Tool) bool {
	for _, tool := range tools {
		if isAnalysisValidationTool(tool.Name) {
			return true
		}
	}
	return false
}

func normalizedValidationFailure(err error) string {
	if err == nil {
		return ""
	}
	return strings.Join(strings.Fields(err.Error()), " ")
}

func validatedAnalysisIsTransient(result string) bool {
	var analysis validatedAnalysis
	return json.Unmarshal([]byte(result), &analysis) == nil && analysis.IsTransient != nil && *analysis.IsTransient
}

const (
	analysisLoopMaxIterations     = 25
	analysisValidationFocusRounds = 5
	maxValidationFailures         = 3
	maxValidationFinalRetries     = 2
	maxTransientCritiqueRetries   = 2
	validationRequiredPrompt      = "The final answer cannot be accepted until validate_analysis succeeds. " +
		"Call validate_analysis now with the complete final analysis and every supporting evidence token. " +
		"Do not return prose or final JSON before validation succeeds."
	validationFocusPrompt = "You have reached the investigation budget. Stop reading artifacts. " +
		"If is_transient is true, call verify_timeline first unless it already succeeded. " +
		"Then call validate_analysis exactly once with the complete final analysis and all supporting evidence tokens."
	toolsFreeFinalPrompt = "You have reached your investigation budget. Do NOT call any more tools. " +
		"Using ONLY the evidence you have already gathered, output your FINAL answer now as the required JSON object " +
		"and nothing else."
	emptyFinalPrompt = "Your last response was empty. Do NOT call any tools. " +
		"Output your FINAL answer now as the required JSON object and nothing else, " +
		"using the evidence you have already gathered."
	transientCritiquePrompt = "You set is_transient=true but never called verify_timeline " +
		"to confirm the failing operation. " +
		"A bare context deadline or transient-signature match is not proof. Call verify_timeline now. " +
		"If you cannot prove a known transient class, set is_transient=false, then re-emit the final JSON."
	transientValidationPrompt = "The validated analysis sets is_transient=true " +
		"without a successful verify_timeline call. " +
		"Call verify_timeline for the specific failing operation, then call validate_analysis again."
	invalidAnalysisPrompt = "The final analysis JSON is malformed. Correct the JSON syntax and required field types, " +
		"then return the complete final JSON object."
)

type analysisLoopGuard struct {
	validationRequired       bool
	finalizationTools        []llm.Tool
	validationFailures       map[string]int
	validationFinalRetries   int
	transientCritiqueRetries int
	timelineVerified         bool
}

type finalResponseDecision struct {
	result   string
	messages []llm.Message
	retry    bool
	err      error
}

type validationInspection struct {
	execErr error
	final   string
	repair  string
	fatal   error
}

func newAnalysisLoopGuard(tools []llm.Tool) *analysisLoopGuard {
	return &analysisLoopGuard{
		validationRequired: hasAnalysisValidationTool(tools),
		finalizationTools:  analysisFinalizationTools(tools),
		validationFailures: map[string]int{},
	}
}

func (g *analysisLoopGuard) prepareRequest(
	req *llm.CompletionRequest,
	messages []llm.Message,
	iteration, maxIterations int,
) {
	if g.validationRequired && iteration >= maxIterations-analysisValidationFocusRounds {
		req.Tools = g.finalizationTools
		req.Messages = appendPrompt(messages, validationFocusPrompt)
		return
	}
	if !g.validationRequired && iteration >= maxIterations-2 {
		req.Tools = nil
		req.Messages = appendPrompt(messages, toolsFreeFinalPrompt)
	}
}

func (g *analysisLoopGuard) handleFinalResponse(
	content string,
	iteration, maxIterations int,
	messages []llm.Message,
) finalResponseDecision {
	if g.validationRequired {
		g.validationFinalRetries++
		if g.validationFinalRetries > maxValidationFinalRetries {
			return finalResponseDecision{err: fmt.Errorf("analysis ended without successful validate_analysis")}
		}
		return finalResponseDecision{
			messages: append(messages,
				llm.Message{Role: "assistant", Content: content},
				llm.Message{Role: "user", Content: validationRequiredPrompt},
			),
			retry: true,
		}
	}
	if strings.TrimSpace(content) == "" && iteration < maxIterations-1 {
		return finalResponseDecision{
			messages: append(messages,
				llm.Message{Role: "assistant", Content: content},
				llm.Message{Role: "user", Content: emptyFinalPrompt},
			),
			retry: true,
		}
	}
	transient, analysisLike, parseErr := analysisTransientState(content)
	if analysisLike && parseErr != nil {
		if g.transientCritiqueRetries >= maxTransientCritiqueRetries || iteration >= maxIterations-1 {
			return finalResponseDecision{err: parseErr}
		}
		g.transientCritiqueRetries++
		return finalResponseDecision{
			messages: append(messages,
				llm.Message{Role: "assistant", Content: content},
				llm.Message{Role: "user", Content: invalidAnalysisPrompt},
			),
			retry: true,
		}
	}
	if transient && !g.timelineVerified &&
		g.transientCritiqueRetries < maxTransientCritiqueRetries && iteration < maxIterations-1 {
		g.transientCritiqueRetries++
		return finalResponseDecision{
			messages: append(messages,
				llm.Message{Role: "assistant", Content: content},
				llm.Message{Role: "user", Content: transientCritiquePrompt},
			),
			retry: true,
		}
	}
	return finalResponseDecision{result: content}
}

func (g *analysisLoopGuard) inspectTool(
	toolName string,
	args json.RawMessage,
	result string,
	execErr error,
) validationInspection {
	inspection := validationInspection{execErr: execErr}
	if inspection.execErr == nil {
		if isTimelineVerificationTool(toolName) {
			g.timelineVerified = true
		}
		final, validationCall, validationErr := validatedAnalysisResult(toolName, args, result)
		if validationCall {
			inspection.execErr = validationErr
			inspection.final = final
		}
	}
	if inspection.execErr == nil || !isAnalysisValidationTool(toolName) {
		return inspection
	}
	failure := normalizedValidationFailure(inspection.execErr)
	g.validationFailures[failure]++
	inspection.repair = "validate_analysis failed: " + failure +
		". Correct the exact missing or invalid field and retry with changed arguments. Do not repeat the same call."
	if g.validationFailures[failure] >= maxValidationFailures {
		inspection.fatal = fmt.Errorf(
			"validate_analysis repeated the same failure %d times: %s",
			g.validationFailures[failure],
			failure,
		)
	}
	return inspection
}

func (g *analysisLoopGuard) finishValidatedResult(final, repair string) (string, string) {
	if final == "" {
		return "", repair
	}
	if validatedAnalysisIsTransient(final) && !g.timelineVerified {
		return "", transientValidationPrompt
	}
	return final, repair
}

func appendPrompt(messages []llm.Message, prompt string) []llm.Message {
	return append(append([]llm.Message{}, messages...), llm.Message{Role: "user", Content: prompt})
}

func analysisTransientState(content string) (transient, analysisLike bool, err error) {
	candidate, found := lastJSONObject(content)
	if !found {
		if strings.Contains(content, "is_transient") {
			return false, true, fmt.Errorf("final analysis has no valid JSON object")
		}
		return false, false, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(candidate, &fields); err != nil {
		return false, true, fmt.Errorf("parse final analysis JSON: %w", err)
	}
	raw, ok := fields["is_transient"]
	if !ok {
		return false, false, nil
	}
	if err := json.Unmarshal(raw, &transient); err != nil {
		return false, true, fmt.Errorf("parse final analysis is_transient: %w", err)
	}
	return transient, true, nil
}

func lastJSONObject(content string) ([]byte, bool) {
	var best []byte
	depth, start := 0, -1
	inString, escaped := false, false
	for i := 0; i < len(content); i++ {
		ch := content[i]
		if inString {
			switch {
			case escaped:
				escaped = false
			case ch == '\\':
				escaped = true
			case ch == '"':
				inString = false
			}
			continue
		}
		switch ch {
		case '"':
			if depth > 0 {
				inString = true
			}
		case '{':
			if depth == 0 {
				start = i
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && start >= 0 {
				candidate := []byte(content[start : i+1])
				if json.Valid(candidate) {
					best = append(best[:0], candidate...)
				}
			}
		}
	}
	return best, best != nil
}
