package outboundaccess

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"strings"

	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// agentIdentityAuditLogName names the logger that carries identity use
// records, so a log pipeline can route and retain them on their own.
const agentIdentityAuditLogName = "agent-identity-audit"

// auditAgentIdentityUse writes one record for an identity-backed call, issued
// or refused (ADR 0039). Records never carry tokens, request bodies, or full
// URLs. In Delegated mode the requester appears only as a keyed pseudonym.
func (r *KubernetesResolver) auditAgentIdentityUse(policy *corev1alpha1.OutboundAccessPolicy, req ResolveRequest, mode string, err error) {
	logger := r.AuditLog
	if logger.GetSink() == nil {
		logger = ctrllog.Log.WithName(agentIdentityAuditLogName)
	}
	values := []any{
		"event", "agent_identity_use",
		"policyNamespace", policy.Namespace, "policyName", policy.Name,
		"policyUID", string(policy.UID), "policyGeneration", policy.Generation,
		"mode", mode, "tool", req.Tool.Name, "toolMethod", req.Tool.Method,
		"argumentsDigest", argumentsDigest(req.Tool.Name, req.Arguments),
	}
	if binding := req.AgentIdentity; binding != nil {
		values = append(values,
			"agentNamespace", binding.AgentNamespace, "agentName", binding.AgentName,
			"agentUID", binding.AgentUID, "agentGeneration", binding.AgentGeneration,
			"identityIssuer", binding.Identity.Issuer, "identityID", binding.Identity.ID,
			"issuerInstance", binding.Identity.Instance, "taskUID", binding.TaskUID,
		)
	}
	if mode == corev1alpha1.OutboundAgentIdentityModeDelegated {
		values = append(values, "requester", r.requesterPseudonym(req.Requester))
	}
	if err != nil {
		logger.Info("agent identity call refused", append(values, "outcome", "refused", "reason", err.Error())...)
		return
	}
	logger.Info("agent identity call issued", append(values, "outcome", "issued")...)
}

// requesterPseudonym joins records per person without naming them. Without
// a key it records nothing about the requester.
func (r *KubernetesResolver) requesterPseudonym(requester *corev1alpha1.RequestedBy) string {
	if requester == nil || len(r.AuditPseudonymKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, r.AuditPseudonymKey)
	_, _ = mac.Write([]byte(strings.TrimSpace(requester.Issuer) + "\x00" + strings.TrimSpace(requester.Subject)))
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))
}

func argumentsDigest(tool string, arguments []byte) string {
	sum := sha256.Sum256(append([]byte(tool+"\x00"), arguments...))
	return "sha256:" + hex.EncodeToString(sum[:])
}
