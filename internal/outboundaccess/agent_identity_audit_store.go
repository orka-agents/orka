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

type AgentIdentityAuditFilter struct {
	Namespace string
	TaskUID   string
	AgentName string
	Outcome   string
	Limit     int
}

type AgentIdentityAuditSink interface {
	Record(record AgentIdentityAuditRecord)
}

type AgentIdentityAuditReader interface {
	List(filter AgentIdentityAuditFilter) []AgentIdentityAuditRecord
}

type AgentIdentityAuditBuffer struct {
	mu      sync.RWMutex
	maxSize int
	records []AgentIdentityAuditRecord
}

func NewAgentIdentityAuditBuffer(maxSize int) *AgentIdentityAuditBuffer {
	if maxSize <= 0 {
		maxSize = defaultAgentIdentityAuditRingSize
	}
	return &AgentIdentityAuditBuffer{
		maxSize: maxSize,
		records: make([]AgentIdentityAuditRecord, 0, min(maxSize, 64)),
	}
}

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

func (b *AgentIdentityAuditBuffer) List(filter AgentIdentityAuditFilter) []AgentIdentityAuditRecord {
	if b == nil {
		return []AgentIdentityAuditRecord{}
	}
	limit := filter.Limit
	if limit == 0 {
		limit = 100
	}
	ns := strings.TrimSpace(filter.Namespace)
	taskUID := strings.TrimSpace(filter.TaskUID)
	agentName := strings.TrimSpace(filter.AgentName)
	outcome := strings.TrimSpace(filter.Outcome)

	b.mu.RLock()
	defer b.mu.RUnlock()
	capHint := len(b.records)
	if limit > 0 {
		capHint = min(limit, len(b.records))
	}
	out := make([]AgentIdentityAuditRecord, 0, capHint)
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
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return slices.Clip(out)
}
