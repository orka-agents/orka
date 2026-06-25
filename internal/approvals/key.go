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
	"strings"

	"github.com/sozercan/orka/internal/events"
)

const (
	maxApprovalTargetArgsPreviewBytes = 8 * 1024
	maxApprovalTargetTextChars        = 1024
)

// ApprovalTarget is the stable, sanitized contract for a human approval request.
// It deliberately stores only a digest of target arguments, not raw side-effect
// payloads, so approval events can be persisted without leaking secrets.
type ApprovalTarget struct {
	ApprovalID        string          `json:"approvalID"`
	TaskUID           string          `json:"taskUID,omitempty"`
	TargetTool        string          `json:"targetTool"`
	TargetArgsDigest  string          `json:"targetArgsDigest"`
	TargetArgsPreview json.RawMessage `json:"targetArgsPreview,omitempty"`
	Action            string          `json:"action"`
	RiskSummary       string          `json:"riskSummary,omitempty"`
	Severity          string          `json:"severity,omitempty"`
}

// ResolvedApproval is the compact controller-to-worker approval decision
// payload injected into resumed worker Pods.
type ResolvedApproval struct {
	ID               string `json:"id"`
	TaskUID          string `json:"taskUID,omitempty"`
	TargetTool       string `json:"targetTool,omitempty"`
	TargetArgsDigest string `json:"targetArgsDigest,omitempty"`
	Status           string `json:"status"`
	Actor            string `json:"actor,omitempty"`
	DecisionTime     string `json:"decisionTime,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Action           string `json:"action,omitempty"`
	RiskSummary      string `json:"riskSummary,omitempty"`
	Severity         string `json:"severity,omitempty"`
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
func ApprovalID(namespace, taskName, taskUID, targetTool, targetArgsDigest string) string {
	parts := []string{
		strings.TrimSpace(namespace),
		strings.TrimSpace(taskName),
		strings.TrimSpace(taskUID),
		strings.TrimSpace(targetTool),
		strings.TrimSpace(targetArgsDigest),
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
	if strings.TrimSpace(action) == "" {
		action = fmt.Sprintf("Execute %s", targetTool)
	}
	action = boundApprovalTargetText(action)
	riskSummary = boundApprovalTargetText(riskSummary)
	severity = boundApprovalTargetText(severity)
	return ApprovalTarget{
		ApprovalID:        ApprovalID(namespace, taskName, taskUID, targetTool, digest),
		TaskUID:           strings.TrimSpace(taskUID),
		TargetTool:        targetTool,
		TargetArgsDigest:  digest,
		TargetArgsPreview: preview,
		Action:            action,
		RiskSummary:       riskSummary,
		Severity:          severity,
	}, nil
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
	canonical, err := json.Marshal(value)
	if err != nil {
		return nil, fmt.Errorf("canonicalize target arguments: %w", err)
	}
	return canonical, nil
}
