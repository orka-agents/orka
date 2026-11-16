/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package memoryconsolidation

import (
	"context"
	"fmt"
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
// and memories and transcripts each stop growing at their own budget.
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
		size := utf8.RuneCountInString(read.Content)
		for _, tag := range read.Tags {
			size += utf8.RuneCountInString(tag)
		}
		if budget -= size; budget < 0 {
			input.Truncated = true
			break
		}
		input.Memories = append(input.Memories, read)
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
		// to learn from; the model still reads them in order.
		kept := len(messages)
		for kept > 0 {
			size := utf8.RuneCountInString(messages[kept-1].Content)
			if budget-size < 0 {
				input.Truncated = true
				budget = 0
				break
			}
			budget -= size
			kept--
		}
		read.Messages = messages[kept:]
		if len(read.Messages) > 0 {
			input.Sessions = append(input.Sessions, read)
		}
	}
	return input, nil
}

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
