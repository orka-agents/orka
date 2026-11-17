/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package memoryconsolidation

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/store"
)

// Source is the least-privilege store dependency a run's inputs come from.
type Source interface {
	ListMemoryConsolidationSessions(ctx context.Context, namespace string, after, through time.Time, limit int) ([]store.SessionMetadata, int, error)
	LoadTranscript(ctx context.Context, namespace, name string, maxMessages int) ([]store.SessionMessage, error)
	ListMemories(ctx context.Context, filter store.MemoryFilter) ([]store.Memory, error)
	ListMemoryProposals(ctx context.Context, filter store.MemoryProposalFilter) ([]store.MemoryProposal, error)
}

const (
	// sessionTypeTask is the type of a Task's Session, the store's default.
	sessionTypeTask = "task"
	// maxTranscriptRecords is how many of a Session's newest records a run
	// loads to find its newest MaxMessagesPerSession user and assistant
	// messages among tool calls and results.
	maxTranscriptRecords = 5 * MaxMessagesPerSession
)

// CollectInputs reads what a run in namespace sees for the window (start,
// end]: every active memory, and the user and assistant messages of the
// chat and task Sessions that changed in the window, most recently changed
// first. Gateway Sessions are never read. A Session with a turn still
// running is read too: its transcript holds only finished turns, and
// releasing its lock does not change it again, so skipping it could miss it
// for good. Tool messages are dropped, so memory an agent recalled cannot
// return as new evidence. Each memory, tag and message is cut to its bound,
// and memories and transcripts each stop growing at their own budget. The
// namespace's open memory proposals are read too, so the run does not
// propose again what already awaits review.
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
	// A list at the limit may have left older memories out; the store does
	// not say how many more there are.
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
		// The budget keeps a Session's newest turns, which the run is meant
		// to learn from; the model still reads them in order. The Session's
		// own fields count once it keeps any turn.
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
			// Older Sessions would only fit in part, after a newer one was cut.
			budget = 0
		}
	}
	return input, nil
}

// encodedSize is how many characters value takes in the run's data, as
// UserMessage encodes it, plus one for the comma that separates it from the
// next item. Counting each item as encoded charges the budgets for escapes
// and for every field name, ID and timestamp, not only for the text.
func encodedSize(value any) int {
	data, err := encodeData(value)
	if err != nil {
		// Input values always encode; count an impossible failure as full.
		return MaxInputChars
	}
	return utf8.RuneCount(data) + 1
}

// ProposalLister lists a namespace's memory proposals.
type ProposalLister interface {
	ListMemoryProposals(ctx context.Context, filter store.MemoryProposalFilter) ([]store.MemoryProposal, error)
}

// OpenProposals reads the namespace's open memory proposals as CollectInputs
// does, and reports whether it reached a limit, so some were left out. A
// completion uses it to see proposals created while the run ran.
func OpenProposals(ctx context.Context, src ProposalLister, namespace string) ([]InputProposal, bool, error) {
	var input Input
	if err := collectProposals(ctx, src, namespace, &input); err != nil {
		return nil, false, err
	}
	return input.Proposals, input.Truncated, nil
}

// collectProposals reads the namespace's open memory proposals, the newest
// first across both statuses: pending ones, and accepted ones not yet
// applied. Proposals of other kinds, such as skills, are not memory changes
// and are never listed.
func collectProposals(ctx context.Context, src ProposalLister, namespace string, input *Input) error {
	var open []store.MemoryProposal
	// Each kind and status is listed on its own, so proposals of other
	// kinds, such as skills, cannot crowd memory changes out of a list.
	for _, status := range []string{store.MemoryProposalStatusPending, store.MemoryProposalStatusAccepted} {
		for _, proposalType := range memoryChangeProposalTypes {
			proposals, err := src.ListMemoryProposals(ctx, store.MemoryProposalFilter{Namespace: namespace, Type: proposalType,
				Status: status, Limit: MaxInputProposals})
			if err != nil {
				return fmt.Errorf("list %s %s memory proposals: %w", status, proposalType, err)
			}
			// A list at the limit may have left older proposals out. Applying
			// a proposal sets it to applied, so an accepted list holds no
			// applied ones; a provider that keeps them accepted, with an
			// AppliedMemoryID, can fill a list with them, and the full list
			// then marks the input truncated, as any bound reached does.
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
	// Merge the lists newest first, so the shared limits keep the newest.
	slices.SortStableFunc(open, func(a, b store.MemoryProposal) int { return b.CreatedAt.Compare(a.CreatedAt) })
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

// memoryChangeProposalTypes are the proposal types that change memory: a
// new memory, or a merge, supersede or retire.
var memoryChangeProposalTypes = []string{store.MemoryProposalTypeMemory, store.MemoryProposalTypeMerge,
	store.MemoryProposalTypeSupersede, store.MemoryProposalTypeRetire}

// boundedTags returns at most MaxTags tags of a memory, each cut to
// MaxTagChars: memories have no limit of their own.
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

// truncateRunes returns at most limit runes of value, marking a cut.
func truncateRunes(value string, limit int) string {
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit]) + "…"
}

// memoryCut reports whether a run reads less of a memory than it holds: its
// content or one of its tags is longer than a run reads, or it has more tags.
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
