package storageclient

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
)

// maxMemoryProposalApplyAttempts bounds how often an application resolves a
// target again after the provider reports that it changed.
const maxMemoryProposalApplyAttempts = 5

// ApplyMemoryProposal applies an accepted memory proposal in three steps, so
// no provider transaction is open while Orka authorizes:
//
//  1. The provider resolves the memory the application would return, by ID
//     only.
//  2. Orka authorizes that exact memory through apply.AuthorizeExistingMemory
//     (when set). A new memory is covered by the caller's own create
//     permission; Orka builds it from the stored proposal and redacts it.
//  3. The provider commits only while the proposal still resolves to the same
//     target. When it changed, for example because a concurrent application
//     created the memory, the loop resolves and authorizes again.
//
// A caller therefore never receives an existing memory it was not authorized
// to read.
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
		application := store.MemoryProposalApplication{Target: target, AppliedBy: appliedBy, AppliedAt: time.Now().UTC()}
		if target.MemoryID != "" {
			if apply.AuthorizeExistingMemory != nil {
				if err := apply.AuthorizeExistingMemory(ctx, target.Namespace, target.MemoryID); err != nil {
					return nil, err
				}
			}
		} else {
			proposal, err := c.GetMemoryProposal(ctx, namespace, id)
			if err != nil {
				return nil, err
			}
			application.Memory = memoryFromProposal(proposal, application.AppliedAt)
		}
		memory, err := c.CommitMemoryProposalApplication(ctx, application)
		if !errors.Is(err, store.ErrPreconditionFailed) {
			return memory, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// memoryFromProposal builds the memory an accepted proposal creates. The
// storage client redacted the proposal when it was created; the copy applies
// the current rule again, so a proposal stored under an older rule cannot
// become a memory unredacted.
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

// tagsFromProposalDescription reads memory tags from the first "Tags:" line of
// a proposal description, lower-cased, trimmed and without duplicates.
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
