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
	"strings"
	"time"

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
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	record := AgentIdentityAuditRecord{
		Timestamp:        now,
		Event:            "agent_identity_use",
		Mode:             mode,
		PolicyNamespace:  policy.Namespace,
		PolicyName:       policy.Name,
		PolicyUID:        string(policy.UID),
		PolicyGeneration: policy.Generation,
		Tool:             req.Tool.Name,
		ToolMethod:       req.Tool.Method,
		ArgumentsDigest:  argumentsDigest(req.Tool.Name, req.Arguments),
		TaskUID:          auditTaskUID(req),
	}
	values := []any{
		"event", record.Event,
		"policyNamespace", record.PolicyNamespace, "policyName", record.PolicyName,
		"policyUID", record.PolicyUID, "policyGeneration", record.PolicyGeneration,
		"mode", mode, "tool", record.Tool, "toolMethod", record.ToolMethod,
		"argumentsDigest", record.ArgumentsDigest,
		"taskUID", record.TaskUID,
	}
	if binding := req.AgentIdentity; binding != nil {
		record.AgentNamespace = binding.AgentNamespace
		record.AgentName = binding.AgentName
		record.AgentUID = binding.AgentUID
		record.AgentGeneration = binding.AgentGeneration
		record.IdentityIssuer = binding.Identity.Issuer
		record.IdentityID = binding.Identity.ID
		record.IssuerInstance = auditIssuerInstance(binding.Identity.Instance)
		values = append(values,
			"agentNamespace", record.AgentNamespace, "agentName", record.AgentName,
			"agentUID", record.AgentUID, "agentGeneration", record.AgentGeneration,
			"identityIssuer", record.IdentityIssuer, "identityID", record.IdentityID,
			"issuerInstance", record.IssuerInstance,
		)
	}
	if mode == corev1alpha1.OutboundAgentIdentityModeDelegated {
		record.Requester = r.requesterPseudonym(req.Requester)
		values = append(values, "requester", record.Requester)
	}
	if err != nil {
		record.Outcome = "refused"
		record.Reason = agentIdentityRefusalReason(err)
		if r.AuditSink != nil {
			r.AuditSink.Record(record)
		}
		logger.Info("agent identity call refused", append(values, "outcome", record.Outcome, "reason", record.Reason)...)
		return
	}
	record.Outcome = "issued"
	if r.AuditSink != nil {
		r.AuditSink.Record(record)
	}
	logger.Info("agent identity call issued", append(values, "outcome", record.Outcome)...)
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

// auditIssuerInstance names the issuer instance without the authority URL a
// non-default authority adds after "@": records carry no full URLs, so the
// authority appears as a stable digest that still tells two apart.
func auditIssuerInstance(instance string) string {
	issuer, authority, ok := strings.Cut(instance, "@")
	if !ok {
		return instance
	}
	sum := sha256.Sum256([]byte(authority))
	return issuer + "@sha256:" + hex.EncodeToString(sum[:8])
}

func argumentsDigest(tool string, arguments []byte) string {
	sum := sha256.Sum256(append([]byte(tool+"\x00"), arguments...))
	return "sha256:" + hex.EncodeToString(sum[:])
}
