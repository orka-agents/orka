/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package store

import (
	"crypto/sha256"
	"encoding/hex"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

const agentExecutionPolicyDigestDomain = "agent-execution-policy"

// CanonicalAgentExecutionPolicyDigest returns the shared digest used by v1
// candidate resolution and the fail-closed binding admission boundary.
func CanonicalAgentExecutionPolicyDigest(spec corev1alpha1.AgentExecutionPolicySpec) (string, error) {
	canonical, err := harnessv2.CanonicalValue(spec)
	if err != nil {
		return "", err
	}
	hash := sha256.New()
	_, _ = hash.Write([]byte("orka.acp." + agentExecutionPolicyDigestDomain + "\x00"))
	_, _ = hash.Write(canonical)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil)), nil
}
