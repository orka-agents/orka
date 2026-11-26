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

const agentIdentityAuditLogName = "agent-identity-audit"

var agentIdentityAuditOutput io.Writer = os.Stderr

var defaultAgentIdentityAuditLog = funcr.NewJSON(func(record string) {
	_, _ = fmt.Fprintln(agentIdentityAuditOutput, record)
}, funcr.Options{LogTimestamp: true})

const (
	agentIdentityRefusalPolicy     = "policy"
	agentIdentityRefusalBinding    = "binding"
	agentIdentityRefusalDelegation = "delegation"
	agentIdentityRefusalIssuer     = "issuer"
	agentIdentityRefusalCanceled   = "canceled"
)

type agentIdentityRefusal struct {
	reason string
	err    error
}

func (e *agentIdentityRefusal) Error() string { return e.err.Error() }
func (e *agentIdentityRefusal) Unwrap() error { return e.err }

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

func auditTaskUID(req ResolveRequest) string {
	if req.TaskUID != "" || req.AgentIdentity == nil {
		return req.TaskUID
	}
	return req.AgentIdentity.TaskUID
}

func (r *KubernetesResolver) requesterPseudonym(requester *corev1alpha1.RequestedBy) string {
	if requester == nil || len(r.AuditPseudonymKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, r.AuditPseudonymKey)
	_, _ = mac.Write([]byte(requester.Issuer + "\x00" + requester.Subject))
	return "hmac-sha256:" + hex.EncodeToString(mac.Sum(nil))
}

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
