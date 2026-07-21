package main

import (
	"encoding/json"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/llm"
	"github.com/orka-agents/orka/internal/workerenv"
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
	return normalized == "validate-analysis" || strings.HasPrefix(normalized, "validate-analysis-") ||
		normalized == "submit-analysis" || strings.HasPrefix(normalized, "submit-analysis-")
}

func isFlatAnalysisSubmissionTool(name string) bool {
	normalized := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(name)), "_", "-")
	return normalized == "submit-analysis" || strings.HasPrefix(normalized, "submit-analysis-")
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
	if isFlatAnalysisSubmissionTool(toolName) {
		if err := json.Unmarshal(args, &call.Analysis); err != nil {
			return "", true, fmt.Errorf("parse submit_analysis arguments: %w", err)
		}
	} else if err := json.Unmarshal(args, &call); err != nil {
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

func analysisToolNames(
	tools []llm.Tool,
	customTools map[string]*corev1alpha1.Tool,
) (validation, timeline map[string]bool, finalization []llm.Tool) {
	validation = map[string]bool{}
	timeline = map[string]bool{}
	for _, tool := range tools {
		actual := resolvedCustomToolName(tool.Name, customTools)
		switch {
		case isAnalysisValidationTool(actual):
			validation[tool.Name] = true
			finalization = append(finalization, tool)
		case isTimelineVerificationTool(actual):
			timeline[tool.Name] = true
			finalization = append(finalization, tool)
		}
	}
	return validation, timeline, finalization
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
	toolsFreeFinalPrompt = "You have reached your tool-use budget. Do NOT call any more tools. " +
		"Using the information already gathered, provide the final answer in the format requested by the Task."
	emptyFinalPrompt = "Your last response was empty. Do NOT call any tools. " +
		"Provide the final answer now in the format requested by the Task, using the information already gathered."
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
	validationToolNames      map[string]bool
	timelineToolNames        map[string]bool
	finalizationTools        []llm.Tool
	customTools              map[string]*corev1alpha1.Tool
	submissionToolName       string
	validationFailures       map[string]int
	validationFinalRetries   int
	transientCritiqueRetries int
	timelineVerified         bool
	transientCritiqueEnabled bool
	investigationToolCalls   int
	toolResults              map[string]string
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

func agentLoopMaxIterations(coordination workerenv.CoordinationEnv, validationRequired bool) int {
	maxIterations := 10
	if coordination.Enabled {
		maxIterations = 50
	}
	if coordination.AutonomousMode {
		maxIterations = 100
	}
	if validationRequired {
		maxIterations = analysisLoopMaxIterations
	}
	return maxIterations
}

func newAnalysisLoopGuard(
	tools []llm.Tool,
	customTools map[string]*corev1alpha1.Tool,
) *analysisLoopGuard {
	validation, timeline, finalization := analysisToolNames(tools, customTools)
	submissionToolName := "validate_analysis"
	for _, tool := range tools {
		if validation[tool.Name] {
			submissionToolName = tool.Name
			break
		}
	}
	return &analysisLoopGuard{
		validationRequired:       len(validation) > 0,
		validationToolNames:      validation,
		timelineToolNames:        timeline,
		transientCritiqueEnabled: len(timeline) > 0,
		finalizationTools:        finalization,
		customTools:              customTools,
		submissionToolName:       submissionToolName,
		validationFailures:       map[string]int{},
		toolResults:              map[string]string{},
	}
}

func (g *analysisLoopGuard) isValidationTool(name string) bool {
	return g.validationToolNames[name]
}

func (g *analysisLoopGuard) isTimelineTool(name string) bool {
	return g.timelineToolNames[name]
}

func (g *analysisLoopGuard) isFinalizationTool(name string) bool {
	return g.isValidationTool(name) || g.isTimelineTool(name)
}

func (g *analysisLoopGuard) prepareRequest(
	req *llm.CompletionRequest,
	messages []llm.Message,
	iteration, maxIterations int,
) {
	if g.timelineVerified {
		req.Tools = g.withoutTimelineTools(req.Tools)
	}
	budgetReached := iteration >= maxIterations-analysisValidationFocusRounds ||
		g.investigationToolCalls >= analysisMaxInvestigationToolCalls
	if g.validationRequired && budgetReached {
		req.Tools = g.activeFinalizationTools()
		req.Messages = appendPrompt(messages, g.modelPrompt(validationFocusPrompt))
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
				llm.Message{Role: "user", Content: g.modelPrompt(validationRequiredPrompt)},
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
	if !g.transientCritiqueEnabled {
		return finalResponseDecision{result: content}
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
		if g.isTimelineTool(toolName) {
			g.timelineVerified = true
		}
		actualToolName := resolvedCustomToolName(toolName, g.customTools)
		final, validationCall, validationErr := validatedAnalysisResult(actualToolName, args, result)
		if validationCall {
			inspection.execErr = validationErr
			inspection.final = final
		}
	}
	if inspection.execErr == nil || !g.isValidationTool(toolName) {
		return inspection
	}
	failure := normalizedValidationFailure(inspection.execErr)
	g.validationFailures[failure]++
	inspection.repair = toolName + " failed: " + failure +
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
		return "", g.modelPrompt(transientValidationPrompt)
	}
	return final, repair
}

func (g *analysisLoopGuard) withoutTimelineTools(tools []llm.Tool) []llm.Tool {
	out := make([]llm.Tool, 0, len(tools))
	for _, tool := range tools {
		if !g.isTimelineTool(tool.Name) {
			out = append(out, tool)
		}
	}
	return out
}

func (g *analysisLoopGuard) completedToolCallResult(name string) (string, bool) {
	if g.timelineVerified && g.isTimelineTool(name) {
		return "timeline verification already completed; call " + g.submissionToolName + " now", true
	}
	return "", false
}

func (g *analysisLoopGuard) activeFinalizationTools() []llm.Tool {
	if !g.timelineVerified {
		return g.finalizationTools
	}
	out := make([]llm.Tool, 0, len(g.finalizationTools))
	for _, tool := range g.finalizationTools {
		if !g.isTimelineTool(tool.Name) {
			out = append(out, tool)
		}
	}
	return out
}

func (g *analysisLoopGuard) modelPrompt(prompt string) string {
	return strings.ReplaceAll(prompt, "validate_analysis", g.submissionToolName)
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
