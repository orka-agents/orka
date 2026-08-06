/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/types"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const agentExecutionResolutionDigestDomain = "agent-execution-resolution-ref/v1"

type agentExecutionResolutionDigestInput struct {
	SchemaVersion         int32                                         `json:"schemaVersion"`
	AdjudicationNamespace string                                        `json:"adjudicationNamespace"`
	AdjudicationName      string                                        `json:"adjudicationName"`
	AdjudicationUID       types.UID                                     `json:"adjudicationUID"`
	Action                corev1alpha1.AgentExecutionAdjudicationAction `json:"action"`
	OperationDigest       string                                        `json:"operationDigest"`
	AppliedAt             time.Time                                     `json:"appliedAt"`
}

// CanonicalAgentExecutionResolutionRefDigest returns the shared digest used by
// both the adjudication controller and the fail-closed admission boundary.
func CanonicalAgentExecutionResolutionRefDigest(
	namespace string,
	ref *corev1alpha1.AgentExecutionResolutionRef,
) (string, error) {
	if ref == nil {
		return "", errors.New("AgentExecutionResolutionRef is required")
	}
	appliedAt := ref.AppliedAt.Rfc3339Copy()
	canonical, err := harnessv2.CanonicalValue(agentExecutionResolutionDigestInput{
		SchemaVersion:         1,
		AdjudicationNamespace: namespace,
		AdjudicationName:      ref.AdjudicationName,
		AdjudicationUID:       ref.AdjudicationUID,
		Action:                ref.Action,
		OperationDigest:       ref.OperationDigest,
		AppliedAt:             appliedAt.UTC(),
	})
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("orka.acp." + agentExecutionResolutionDigestDomain + "\x00"))
	_, _ = hash.Write(canonical)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
