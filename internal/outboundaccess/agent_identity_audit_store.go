/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package outboundaccess

import (
	"slices"
	"strings"
	"sync"
	"time"
)

const defaultAgentIdentityAuditRingSize = 1000

// AgentIdentityAuditRecord is the redacted, safe-to-expose record of one
// identity-backed outbound call (issued or refused). It carries the same
// schema as the structured agent-identity-audit log line and never contains
// tokens, headers, bodies, full URLs, or raw error text.
type AgentIdentityAuditRecord struct {
	Timestamp        time.Time `json:"timestamp"`
	Event            string    `json:"event"`
	Outcome          string    `json:"outcome"`
	Reason           string    `json:"reason,omitempty"`
	Mode             string    `json:"mode"`
	PolicyNamespace  string    `json:"policyNamespace"`
	PolicyName       string    `json:"policyName"`
	PolicyUID        string    `json:"policyUID,omitempty"`
	PolicyGeneration int64     `json:"policyGeneration,omitempty"`
	Tool             string    `json:"tool"`
	ToolMethod       string    `json:"toolMethod,omitempty"`
	ArgumentsDigest  string    `json:"argumentsDigest"`
	TaskUID          string    `json:"taskUID,omitempty"`
	AgentNamespace   string    `json:"agentNamespace,omitempty"`
	AgentName        string    `json:"agentName,omitempty"`
	AgentUID         string    `json:"agentUID,omitempty"`
	AgentGeneration  int64     `json:"agentGeneration,omitempty"`
	IdentityIssuer   string    `json:"identityIssuer,omitempty"`
	IdentityID       string    `json:"identityID,omitempty"`
	IssuerInstance   string    `json:"issuerInstance,omitempty"`
	Requester        string    `json:"requester,omitempty"`
}

// AgentIdentityAuditFilter selects matching AgentIdentityAuditRecords.
type AgentIdentityAuditFilter struct {
	Namespace string
	TaskUID   string
	AgentName string
	Outcome   string
	Limit     int
}

// AgentIdentityAuditSink receives redacted identity-use records for querying.
type AgentIdentityAuditSink interface {
	Record(record AgentIdentityAuditRecord)
}

// AgentIdentityAuditReader queries redacted identity-use records.
type AgentIdentityAuditReader interface {
	List(filter AgentIdentityAuditFilter) []AgentIdentityAuditRecord
}

// AgentIdentityAuditBuffer is a bounded in-memory ring buffer of redacted
// identity-use records, safe for concurrent writes and queries.
type AgentIdentityAuditBuffer struct {
	mu      sync.RWMutex
	maxSize int
	records []AgentIdentityAuditRecord
}

// NewAgentIdentityAuditBuffer creates a ring buffer holding up to maxSize
// records (defaulting to defaultAgentIdentityAuditRingSize when <= 0).
func NewAgentIdentityAuditBuffer(maxSize int) *AgentIdentityAuditBuffer {
	if maxSize <= 0 {
		maxSize = defaultAgentIdentityAuditRingSize
	}
	return &AgentIdentityAuditBuffer{
		maxSize: maxSize,
		records: make([]AgentIdentityAuditRecord, 0, min(maxSize, 64)),
	}
}

// Record appends one redacted identity-use record, evicting the oldest entry
// when the buffer is full.
func (b *AgentIdentityAuditBuffer) Record(record AgentIdentityAuditRecord) {
	if b == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.records) >= b.maxSize {
		copy(b.records, b.records[1:])
		b.records[len(b.records)-1] = record
		return
	}
	b.records = append(b.records, record)
}

// List returns matching records ordered newest-first.
func (b *AgentIdentityAuditBuffer) List(filter AgentIdentityAuditFilter) []AgentIdentityAuditRecord {
	if b == nil {
		return []AgentIdentityAuditRecord{}
	}
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	ns := strings.TrimSpace(filter.Namespace)
	taskUID := strings.TrimSpace(filter.TaskUID)
	agentName := strings.TrimSpace(filter.AgentName)
	outcome := strings.TrimSpace(filter.Outcome)

	b.mu.RLock()
	defer b.mu.RUnlock()
	out := make([]AgentIdentityAuditRecord, 0, min(limit, len(b.records)))
	for _, rec := range slices.Backward(b.records) {

		if ns != "" && rec.PolicyNamespace != ns && rec.AgentNamespace != ns {
			continue
		}
		if taskUID != "" && rec.TaskUID != taskUID {
			continue
		}
		if agentName != "" && rec.AgentName != agentName {
			continue
		}
		if outcome != "" && rec.Outcome != outcome {
			continue
		}
		out = append(out, rec)
		if len(out) >= limit {
			break
		}
	}
	return slices.Clip(out)
}
