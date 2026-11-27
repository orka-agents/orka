/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package memoryconsolidation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/store"
)

type Source interface {
	ListMemoryConsolidationSessions(ctx context.Context, namespace string, after, through time.Time, limit int) ([]store.SessionMetadata, int, error)
	LoadTranscript(ctx context.Context, namespace, name string, maxMessages int) ([]store.SessionMessage, error)
	ListMemories(ctx context.Context, filter store.MemoryFilter) ([]store.Memory, error)
	ListMemoryProposals(ctx context.Context, filter store.MemoryProposalFilter) ([]store.MemoryProposal, error)
}

const (
	sessionTypeTask      = "task"
	maxTranscriptRecords = 5 * MaxMessagesPerSession
)

func CollectInputs(ctx context.Context, src Source, namespace string, start, end time.Time) (Input, error) {
	sessions, eligible, err := src.ListMemoryConsolidationSessions(ctx, namespace, start, end, MaxInputSessions)
	if err != nil {
		return Input{}, fmt.Errorf("list changed sessions: %w", err)
	}
	input := Input{EligibleSessions: eligible, Memories: []InputMemory{}, Sessions: []InputSession{}}
	memories, err := src.ListMemories(ctx, store.MemoryFilter{Namespace: namespace, Limit: MaxInputMemories})
	if err != nil {
		return Input{}, fmt.Errorf("list memories: %w", err)
	}
	if len(memories) >= MaxInputMemories {
		input.Truncated = true
		memories = memories[:MaxInputMemories]
	}
	if eligible > len(sessions) {
		input.Truncated = true
	}
	budget := MaxMemoryInputChars
	for _, memory := range memories {
		if memoryCut(memory) {
			input.Truncated = true
		}
		read := InputMemory{ID: memory.ID, Generation: memory.Generation, Content: truncateRunes(memory.Content, MaxMemoryChars),
			Tags: boundedTags(memory.Tags), RecalledCount: memory.RecalledCount, LastRecalledAt: memory.LastRecalledAt,
			UpdatedAt: memory.UpdatedAt.UTC()}
		if budget -= encodedSize(read); budget < 0 {
			input.Truncated = true
			break
		}
		input.Memories = append(input.Memories, read)
	}
	if err := collectProposals(ctx, src, namespace, &input); err != nil {
		return Input{}, err
	}
	budget = MaxTranscriptInputChars
	for _, session := range sessions {
		if budget <= 0 {
			input.Truncated = true
			break
		}
		records, err := src.LoadTranscript(ctx, namespace, session.Name, maxTranscriptRecords)
		if err != nil {
			return Input{}, fmt.Errorf("load transcript of session %q: %w", session.Name, err)
		}
		if len(records) >= maxTranscriptRecords {
			input.Truncated = true
		}
		var messages []InputMessage
		for _, message := range records {
			role := strings.ToLower(strings.TrimSpace(message.Role))
			content := strings.TrimSpace(message.Content)
			if (role != "user" && role != "assistant") || content == "" || message.ToolCalls != nil || message.ToolCallID != "" {
				continue
			}
			if utf8.RuneCountInString(content) > MaxMessageChars {
				input.Truncated = true
			}
			messages = append(messages, InputMessage{ID: message.ID, Role: role, Content: truncateRunes(content, MaxMessageChars)})
		}
		if len(messages) > MaxMessagesPerSession {
			input.Truncated = true
			messages = messages[len(messages)-MaxMessagesPerSession:]
		}
		read := InputSession{Name: session.Name, Type: sessionType(session.SessionType), UpdatedAt: session.UpdatedAt.UTC()}
		remaining := budget - encodedSize(read)
		kept := len(messages)
		for kept > 0 {
			size := encodedSize(messages[kept-1])
			if remaining-size < 0 {
				input.Truncated = true
				break
			}
			remaining -= size
			kept--
		}
		read.Messages = messages[kept:]
		if len(read.Messages) > 0 {
			input.Sessions = append(input.Sessions, read)
			budget = remaining
		}
		if kept > 0 {
			budget = 0
		}
	}
	return input, nil
}

func encodedSize(value any) int {
	data, err := encodeData(value)
	if err != nil {
		return MaxInputChars
	}
	return utf8.RuneCount(data) + 1
}

type ProposalLister interface {
	ListMemoryProposals(ctx context.Context, filter store.MemoryProposalFilter) ([]store.MemoryProposal, error)
}

func OpenProposals(ctx context.Context, src ProposalLister, namespace string) ([]InputProposal, bool, error) {
	var input Input
	if err := collectProposals(ctx, src, namespace, &input); err != nil {
		return nil, false, err
	}
	return input.Proposals, input.Truncated, nil
}

type ProposalReader interface {
	GetMemoryProposal(ctx context.Context, namespace, id string) (*store.MemoryProposal, error)
}

func ClosedProposals(ctx context.Context, src ProposalReader, namespace string, read []InputProposal) (map[string]bool, error) {
	closed := map[string]bool{}
	for _, proposal := range read {
		current, err := src.GetMemoryProposal(ctx, namespace, proposal.ID)
		if errors.Is(err, store.ErrNotFound) {
			closed[proposal.ID] = true
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read memory proposal %q: %w", proposal.ID, err)
		}
		open := (current.Status == store.MemoryProposalStatusPending || current.Status == store.MemoryProposalStatusAccepted) &&
			current.AppliedMemoryID == ""
		if !open {
			closed[proposal.ID] = true
		}
	}
	return closed, nil
}

func collectProposals(ctx context.Context, src ProposalLister, namespace string, input *Input) error {
	var open []store.MemoryProposal
	for _, status := range []string{store.MemoryProposalStatusPending, store.MemoryProposalStatusAccepted} {
		for _, proposalType := range memoryChangeProposalTypes {
			proposals, err := src.ListMemoryProposals(ctx, store.MemoryProposalFilter{Namespace: namespace, Type: proposalType,
				Status: status, Limit: MaxInputProposals})
			if err != nil {
				return fmt.Errorf("list %s %s memory proposals: %w", status, proposalType, err)
			}
			if len(proposals) >= MaxInputProposals {
				input.Truncated = true
			}
			for _, proposal := range proposals {
				if proposal.Type == proposalType && proposal.AppliedMemoryID == "" && proposal.Status == status {
					open = append(open, proposal)
				}
			}
		}
	}
	slices.SortStableFunc(open, func(a, b store.MemoryProposal) int { return b.CreatedAt.Compare(a.CreatedAt) })
	seen := make(map[string]bool, len(open))
	open = slices.DeleteFunc(open, func(proposal store.MemoryProposal) bool {
		duplicate := seen[proposal.ID]
		seen[proposal.ID] = true
		return duplicate
	})
	budget := MaxProposalInputChars
	for _, proposal := range open {
		if len(input.Proposals) >= MaxInputProposals {
			input.Truncated = true
			return nil
		}
		title := strings.Join(strings.Fields(proposal.Title), " ")
		if utf8.RuneCountInString(proposal.Content) > MaxMemoryChars || utf8.RuneCountInString(title) > MaxTitleChars {
			input.Truncated = true
		}
		read := InputProposal{ID: proposal.ID, Type: proposal.Type, Status: proposal.Status,
			Title:   truncateRunes(title, MaxTitleChars),
			Content: truncateRunes(proposal.Content, MaxMemoryChars),
			Targets: append([]store.MemoryProposalTarget(nil), proposal.Targets...)}
		if budget -= encodedSize(read); budget < 0 {
			input.Truncated = true
			return nil
		}
		input.Proposals = append(input.Proposals, read)
	}
	return nil
}

var memoryChangeProposalTypes = []string{store.MemoryProposalTypeMemory, store.MemoryProposalTypeMerge,
	store.MemoryProposalTypeSupersede, store.MemoryProposalTypeRetire}

func boundedTags(tags []string) []string {
	if len(tags) > MaxTags {
		tags = tags[:MaxTags]
	}
	out := make([]string, 0, len(tags))
	for _, tag := range tags {
		out = append(out, truncateRunes(tag, MaxTagChars))
	}
	return out
}

func sessionType(value string) string {
	if strings.TrimSpace(value) == "" {
		return sessionTypeTask
	}
	return value
}

func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}

func memoryCut(memory store.Memory) bool {
	if utf8.RuneCountInString(memory.Content) > MaxMemoryChars || len(memory.Tags) > MaxTags {
		return true
	}
	for _, tag := range memory.Tags {
		if utf8.RuneCountInString(tag) > MaxTagChars {
			return true
		}
	}
	return false
}
