package outboundaccess

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/go-logr/logr/funcr"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
)

// agentIdentityAuditLogName names the logger that carries identity use
// records, so a log pipeline can route and retain them on their own.
const agentIdentityAuditLogName = "agent-identity-audit"

// agentIdentityAuditOutput receives records from a resolver without an
// AuditLog, such as an AI worker's, which configures no logger of its own.
var agentIdentityAuditOutput io.Writer = os.Stderr

// defaultAgentIdentityAuditLog writes one JSON line per record. Like the
// controller's audit sink, no log level filters it and it is never sampled.
var defaultAgentIdentityAuditLog = funcr.NewJSON(func(record string) {
	_, _ = fmt.Fprintln(agentIdentityAuditOutput, record)
}, funcr.Options{LogTimestamp: true})

// Refusal reasons in identity use records. A record carries only one of
// these, never the error text: errors can quote call arguments, issuer
// responses, or other values an audit reader must not see.
const (
	// agentIdentityRefusalPolicy: the policy, its references, or the Tool's
	// target failed a check before an identity was considered.
	agentIdentityRefusalPolicy = "policy"
	// agentIdentityRefusalBinding: the Task's identity binding, the bound
	// Agent, or the Tool is not eligible for the identity.
	agentIdentityRefusalBinding = "binding"
	// agentIdentityRefusalDelegation: the Agent may no longer act for the
	// requester, or the requester's credential or arguments were refused.
	agentIdentityRefusalDelegation = "delegation"
	// agentIdentityRefusalIssuer: the identity issuer refused or returned
	// no credential.
	agentIdentityRefusalIssuer = "issuer"
	// agentIdentityRefusalCanceled: the call was canceled or timed out.
	agentIdentityRefusalCanceled = "canceled"
)

// agentIdentityRefusal tags a refused identity-backed call with its audit
// reason. It keeps the error's text and chain for callers.
type agentIdentityRefusal struct {
	reason string
	err    error
}

func (e *agentIdentityRefusal) Error() string { return e.err.Error() }
func (e *agentIdentityRefusal) Unwrap() error { return e.err }

// refuseAgentIdentity tags err with reason unless it already has one.
func refuseAgentIdentity(reason string, err error) error {
	if _, tagged := errors.AsType[*agentIdentityRefusal](err); err == nil || tagged {
		return err
	}
	return &agentIdentityRefusal{reason: reason, err: err}
}

func agentIdentityRefusalReason(err error) string {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return agentIdentityRefusalCanceled
	}
	if tagged, ok := errors.AsType[*agentIdentityRefusal](err); ok {
		return tagged.reason
	}
	return agentIdentityRefusalPolicy
}

// auditAgentIdentityUse writes one record for an identity-backed call, issued
// or refused (ADR 0039); calls under other adapters leave none. Resolve defers
// it once the policy is read, so it sees the call's final error. Records never
// carry tokens, request bodies, full URLs, or error text. In Delegated mode
// the requester appears only as a keyed pseudonym.
func (r *KubernetesResolver) auditAgentIdentityUse(policy *corev1alpha1.OutboundAccessPolicy, call *ResolveRequest, result *error) {
	if policy.Spec.AgentIdentity == nil {
		return
	}
	req, err := *call, *result
	logger := r.AuditLog
	if logger.GetSink() == nil {
		logger = defaultAgentIdentityAuditLog
	}
	logger = logger.WithName(agentIdentityAuditLogName)
	mode := AgentIdentityMode(policy.Spec.AgentIdentity)
	values := []any{
		"event", "agent_identity_use",
		"policyNamespace", policy.Namespace, "policyName", policy.Name,
		"policyUID", string(policy.UID), "policyGeneration", policy.Generation,
		"mode", mode, "tool", req.Tool.Name, "toolMethod", req.Tool.Method,
		"argumentsDigest", argumentsDigest(req.Tool.Name, req.Arguments),
		"taskUID", auditTaskUID(req),
	}
	if binding := req.AgentIdentity; binding != nil {
		values = append(values,
			"agentNamespace", binding.AgentNamespace, "agentName", binding.AgentName,
			"agentUID", binding.AgentUID, "agentGeneration", binding.AgentGeneration,
			"identityIssuer", binding.Identity.Issuer, "identityID", binding.Identity.ID,
			"issuerInstance", binding.Identity.Instance,
		)
	}
	if mode == corev1alpha1.OutboundAgentIdentityModeDelegated {
		values = append(values, "requester", r.requesterPseudonym(req.Requester))
	}
	if err != nil {
		logger.Info("agent identity call refused", append(values, "outcome", "refused", "reason", agentIdentityRefusalReason(err))...)
		return
	}
	logger.Info("agent identity call issued", append(values, "outcome", "issued")...)
}

// auditTaskUID names the calling Task whether or not it has an identity
// binding; both come from the same authenticated Task.
func auditTaskUID(req ResolveRequest) string {
	if req.TaskUID != "" || req.AgentIdentity == nil {
		return req.TaskUID
	}
	return req.AgentIdentity.TaskUID
}

// requesterPseudonym joins records per person without naming them. Without
// a key it records nothing about the requester. Issuer and subject are
// opaque claims and are keyed exactly as verified, like
// connectors.SubjectDigest, so distinct subjects never share a pseudonym.
func (r *KubernetesResolver) requesterPseudonym(requester *corev1alpha1.RequestedBy) string {
	if requester == nil || len(r.AuditPseudonymKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, r.AuditPseudonymKey)
	_, _ = mac.Write([]byte(requester.Issuer + "\x00" + requester.Subject))
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))
}

func argumentsDigest(tool string, arguments []byte) string {
	sum := sha256.Sum256(append([]byte(tool+"\x00"), arguments...))
	return "sha256:" + hex.EncodeToString(sum[:])
}
