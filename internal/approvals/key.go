/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package approvals

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/orka-agents/orka/internal/events"
)

const (
	maxApprovalTargetArgsPreviewBytes = 8 * 1024
	maxApprovalTargetTextChars        = 1024
	maxApprovalNumberCanonicalChars   = 8 * 1024
)

// ApprovalTarget is the stable, sanitized contract for a human approval request.
// It deliberately stores only a digest of target arguments, not raw side-effect
// payloads, so approval events can be persisted without leaking secrets.
type ApprovalTarget struct {
	ApprovalID        string          `json:"approvalID"`
	TaskUID           string          `json:"taskUID,omitempty"`
	TargetTool        string          `json:"targetTool"`
	TargetArgsDigest  string          `json:"targetArgsDigest"`
	TargetSpecDigest  string          `json:"targetSpecDigest,omitempty"`
	TargetArgsPreview json.RawMessage `json:"targetArgsPreview,omitempty"`
	Action            string          `json:"action"`
	RiskSummary       string          `json:"riskSummary,omitempty"`
	Severity          string          `json:"severity,omitempty"`
}

// ResolvedApproval is the compact controller-to-worker approval decision
// payload injected into resumed worker Pods.
type ResolvedApproval struct {
	ID                string          `json:"id"`
	TaskUID           string          `json:"taskUID,omitempty"`
	TargetTool        string          `json:"targetTool,omitempty"`
	TargetArgsDigest  string          `json:"targetArgsDigest,omitempty"`
	TargetSpecDigest  string          `json:"targetSpecDigest,omitempty"`
	TargetArgsPreview json.RawMessage `json:"targetArgsPreview,omitempty"`
	Status            string          `json:"status"`
	Actor             string          `json:"actor,omitempty"`
	DecisionTime      string          `json:"decisionTime,omitempty"`
	Reason            string          `json:"reason,omitempty"`
	Action            string          `json:"action,omitempty"`
	RiskSummary       string          `json:"riskSummary,omitempty"`
	Severity          string          `json:"severity,omitempty"`
}

// TargetArgsDigest returns a sha256 digest of canonical JSON arguments. Empty
// arguments canonicalize to an empty object, matching LLM tool-call semantics.
func TargetArgsDigest(args json.RawMessage) (string, error) {
	canonical, err := canonicalJSON(args)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

// ApprovalID returns a deterministic approval/idempotency key for a concrete
// side-effect target within a task.
func ApprovalID(namespace, taskName, taskUID, targetTool, targetArgsDigest string, targetSpecDigests ...string) string {
	parts := []string{
		strings.TrimSpace(namespace),
		strings.TrimSpace(taskName),
		strings.TrimSpace(taskUID),
		strings.TrimSpace(targetTool),
		strings.TrimSpace(targetArgsDigest),
	}
	if len(targetSpecDigests) > 0 {
		if targetSpecDigest := strings.TrimSpace(targetSpecDigests[0]); targetSpecDigest != "" {
			parts = append(parts, targetSpecDigest)
		}
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

// NewApprovalTarget builds a sanitized approval target for the given tool call.
func NewApprovalTarget(
	namespace string,
	taskName string,
	taskUID string,
	targetTool string,
	args json.RawMessage,
	action string,
	riskSummary string,
	severity string,
	targetSpecDigests ...string,
) (ApprovalTarget, error) {
	targetTool = strings.TrimSpace(targetTool)
	if targetTool == "" {
		return ApprovalTarget{}, fmt.Errorf("target tool is required")
	}
	digest, err := TargetArgsDigest(args)
	if err != nil {
		return ApprovalTarget{}, err
	}
	preview, _, err := events.SanitizeExecutionEventJSON(args)
	if err != nil {
		return ApprovalTarget{}, err
	}
	preview, err = boundApprovalTargetArgsPreview(preview)
	if err != nil {
		return ApprovalTarget{}, err
	}
	targetSpecDigest := ""
	if len(targetSpecDigests) > 0 {
		targetSpecDigest = strings.TrimSpace(targetSpecDigests[0])
	}
	if strings.TrimSpace(action) == "" {
		action = fmt.Sprintf("Execute %s", targetTool)
	}
	action = boundApprovalTargetText(action)
	riskSummary = boundApprovalTargetText(riskSummary)
	severity = boundApprovalTargetText(severity)
	return ApprovalTarget{
		ApprovalID:        ApprovalID(namespace, taskName, taskUID, targetTool, digest, targetSpecDigest),
		TaskUID:           strings.TrimSpace(taskUID),
		TargetTool:        targetTool,
		TargetArgsDigest:  digest,
		TargetSpecDigest:  targetSpecDigest,
		TargetArgsPreview: preview,
		Action:            action,
		RiskSummary:       riskSummary,
		Severity:          severity,
	}, nil
}

// TargetSpecDigest returns a sha256 digest of a sanitized tool specification.
func TargetSpecDigest(spec any) (string, error) {
	if spec == nil {
		return "", nil
	}
	raw, err := json.Marshal(spec)
	if err != nil {
		return "", fmt.Errorf("marshal target spec: %w", err)
	}
	canonical, err := canonicalJSON(raw)
	if err != nil {
		return "", fmt.Errorf("canonicalize target spec: %w", err)
	}
	sum := sha256.Sum256(canonical)
	return hex.EncodeToString(sum[:]), nil
}

func boundApprovalTargetText(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	bounded, _, _ := events.RedactAndTruncateExecutionEventText(value, maxApprovalTargetTextChars)
	return bounded
}

func boundApprovalTargetArgsPreview(preview json.RawMessage) (json.RawMessage, error) {
	if len(preview) <= maxApprovalTargetArgsPreviewBytes {
		return preview, nil
	}
	bounded, err := json.Marshal(map[string]any{
		"truncated": true,
		"preview":   "[truncated sanitized approval target arguments]",
	})
	if err != nil {
		return nil, fmt.Errorf("marshal truncated approval target args preview: %w", err)
	}
	return json.RawMessage(bounded), nil
}

type canonicalJSONNumber string

func (n canonicalJSONNumber) MarshalJSON() ([]byte, error) {
	return []byte(n), nil
}

func normalizeCanonicalJSONNumbers(value any) (any, error) {
	switch typed := value.(type) {
	case json.Number:
		normalized, err := normalizeJSONNumberString(typed.String())
		if err != nil {
			return nil, err
		}
		return canonicalJSONNumber(normalized), nil
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			normalized, err := normalizeCanonicalJSONNumbers(typed[i])
			if err != nil {
				return nil, err
			}
			out[i] = normalized
		}
		return out, nil
	case map[string]any:
		out := make(map[string]any, len(typed))
		for key, item := range typed {
			normalized, err := normalizeCanonicalJSONNumbers(item)
			if err != nil {
				return nil, err
			}
			out[key] = normalized
		}
		return out, nil
	default:
		return value, nil
	}
}

func normalizeJSONNumberString(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("parse target arguments: empty JSON number")
	}
	sign := ""
	if strings.HasPrefix(raw, "-") {
		sign = "-"
		raw = strings.TrimPrefix(raw, "-")
	}
	exponent := 0
	if expIndex := strings.IndexAny(raw, "eE"); expIndex >= 0 {
		parsed, err := strconv.Atoi(raw[expIndex+1:])
		if err != nil {
			return "", fmt.Errorf("parse target arguments: invalid JSON number exponent %q", raw)
		}
		if parsed > maxApprovalNumberCanonicalChars || parsed < -maxApprovalNumberCanonicalChars {
			return "", fmt.Errorf("parse target arguments: JSON number exponent exceeds safe bound")
		}
		exponent = parsed
		raw = raw[:expIndex]
	}
	intPart, fracPart, _ := strings.Cut(raw, ".")
	if intPart == "" {
		return "", fmt.Errorf("parse target arguments: invalid JSON number %q", raw)
	}
	digitsRaw := intPart + fracPart
	leadingZeros := len(digitsRaw) - len(strings.TrimLeft(digitsRaw, "0"))
	digits := strings.TrimLeft(digitsRaw, "0")
	if digits == "" {
		return "0", nil
	}
	if len(digits) > maxApprovalNumberCanonicalChars {
		return "", fmt.Errorf("parse target arguments: JSON number exceeds safe canonical length")
	}
	decimalPos := len(intPart) + exponent - leadingZeros
	var out string
	switch {
	case decimalPos <= 0:
		zeros := -decimalPos
		if zeros+len(digits)+2 > maxApprovalNumberCanonicalChars {
			return "", fmt.Errorf("parse target arguments: JSON number exceeds safe canonical length")
		}
		out = "0." + strings.Repeat("0", zeros) + digits
	case decimalPos >= len(digits):
		zeros := decimalPos - len(digits)
		if len(digits)+zeros > maxApprovalNumberCanonicalChars {
			return "", fmt.Errorf("parse target arguments: JSON number exceeds safe canonical length")
		}
		out = digits + strings.Repeat("0", zeros)
	default:
		out = digits[:decimalPos] + "." + digits[decimalPos:]
	}
	if dot := strings.IndexByte(out, '.'); dot >= 0 {
		intPart = out[:dot]
		fracPart = strings.TrimRight(out[dot+1:], "0")
		if fracPart == "" {
			out = intPart
		} else {
			out = intPart + "." + fracPart
		}
	}
	if strings.HasPrefix(out, "0.") {
		return sign + out, nil
	}
	out = strings.TrimLeft(out, "0")
	if out == "" {
		out = "0"
	}
	return sign + out, nil
}

func canonicalJSON(raw json.RawMessage) ([]byte, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		trimmed = []byte(`{}`)
	}
	var value any
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	if err := dec.Decode(&value); err != nil {
		return nil, fmt.Errorf("parse target arguments: %w", err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("parse target arguments: trailing data")
	}
	normalized, err := normalizeCanonicalJSONNumbers(value)
	if err != nil {
		return nil, err
	}
	canonical, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("canonicalize target arguments: %w", err)
	}
	return canonical, nil
}
