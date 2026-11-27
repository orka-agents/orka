package storageclient

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
)

const maxMemoryProposalApplyAttempts = 5

func (c *Client) ApplyMemoryProposal(ctx context.Context, apply store.MemoryProposalApply) (*store.Memory, error) {
	namespace := strings.TrimSpace(apply.Namespace)
	id := strings.TrimSpace(apply.ID)
	if namespace == "" || id == "" {
		return nil, store.ValidationErrorf("namespace and id are required")
	}
	appliedBy := redact.SensitiveText(strings.TrimSpace(apply.AppliedBy))
	var lastErr error
	for range maxMemoryProposalApplyAttempts {
		target, err := c.ResolveMemoryProposalApplication(ctx, namespace, id)
		if err != nil {
			return nil, err
		}
		proposal, err := c.GetMemoryProposal(ctx, namespace, id)
		if err != nil {
			return nil, err
		}
		application := store.MemoryProposalApplication{Target: target, AppliedBy: appliedBy, AppliedAt: time.Now().UTC()}
		if target.AppliedMemoryID == "" && store.IsMemoryConsolidationProposalType(proposal.Type) && apply.AuthorizeMemoryChange != nil {
			for _, changed := range proposal.Targets {
				if err := apply.AuthorizeMemoryChange(ctx, target.Namespace, changed.MemoryID); err != nil {
					return nil, err
				}
			}
		}
		if target.MemoryID != "" {
			if apply.AuthorizeExistingMemory != nil {
				if err := apply.AuthorizeExistingMemory(ctx, target.Namespace, target.MemoryID); err != nil {
					return nil, err
				}
			}
		} else {
			application.Memory = memoryFromProposal(proposal, application.AppliedAt)
		}
		memory, err := c.CommitMemoryProposalApplication(ctx, application)
		if errors.Is(err, store.ErrPreconditionFailed) {
			lastErr = err
			continue
		}
		if err != nil {
			return nil, err
		}
		if target.MemoryID != "" && (memory == nil || memory.ID != target.MemoryID) {
			return nil, fmt.Errorf("storage provider returned a memory other than the resolved target %q", target.MemoryID)
		}
		return memory, nil
	}
	return nil, lastErr
}

func memoryFromProposal(proposal *store.MemoryProposal, now time.Time) *store.Memory {
	return &store.Memory{
		Namespace:        proposal.Namespace,
		AgentName:        proposal.AgentName,
		TaskName:         proposal.TaskName,
		Source:           store.MemorySourceProposal,
		SourceProposalID: proposal.ID,
		Content:          redact.SensitiveText(proposal.Content),
		Tags:             tagsFromProposalDescription(proposal.Description),
		CreatedAt:        now,
		UpdatedAt:        now,
	}
}

func tagsFromProposalDescription(description string) []string {
	for line := range strings.SplitSeq(description, "\n") {
		key, value, ok := strings.Cut(line, ":")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "tags") {
			continue
		}
		seen := map[string]struct{}{}
		tags := []string{}
		for tag := range strings.SplitSeq(value, ",") {
			tag = strings.ToLower(strings.TrimSpace(tag))
			if _, duplicate := seen[tag]; tag == "" || duplicate {
				continue
			}
			seen[tag] = struct{}{}
			tags = append(tags, tag)
		}
		return tags
	}
	return nil
}
