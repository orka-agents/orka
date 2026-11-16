/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package memoryconsolidation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/redact"
	"github.com/orka-agents/orka/internal/store"
)

// Validate decides which submitted operations a run keeps. The model's output
// is untrusted: every operation is checked against what the run read, and
// one that fails is dropped with its reason while the others still count.
//
// An operation is kept only when:
//   - its type is known and its targets fit it: none for add, two or more for
//     merge, one for supersede, one or more for retire;
//   - every target is a memory the run read, at the generation it read, and
//     no earlier kept operation targets it; when current is set, it is also
//     still an active memory at that generation, so an operation on a memory
//     that changed since the run read it is dropped rather than proposed
//     stale;
//   - every source is a Session the run read, and a cited message is one of
//     that Session's messages the run read; an add cites at least one;
//   - add, merge and supersede carry content of at most MaxContentChars;
//   - the run keeps no more than maxOperations operations, and removes no
//     more than a quarter of the memories it read, at least one. A merge of
//     k memories removes k-1, a supersede none and a retire k: a merge keeps
//     what it combines, so it costs only the duplicates it removes.
func Validate(submission Submission, input Input, current map[string]uint64, maxOperations int) ([]Operation, []Rejection, []Conflict) {
	generations := make(map[string]uint64, len(input.Memories))
	for _, memory := range input.Memories {
		generations[memory.ID] = memory.Generation
	}
	messages := make(map[string]map[string]bool, len(input.Sessions))
	for _, session := range input.Sessions {
		ids := make(map[string]bool, len(session.Messages))
		for _, message := range session.Messages {
			if message.ID != "" {
				ids[message.ID] = true
			}
		}
		messages[session.Name] = ids
	}
	removalBudget := RemovalBudget(len(input.Memories))
	removed := 0
	targeted := map[string]bool{}

	var kept []Operation
	var rejected []Rejection
	reject := func(index int, op Operation, format string, args ...any) {
		if len(rejected) < MaxRejected {
			rejected = append(rejected, Rejection{Index: index, Type: truncateRunes(op.Type, 40),
				Title: truncateRunes(op.Title, MaxTitleChars), Reason: truncateRunes(fmt.Sprintf(format, args...), MaxReasonChars)})
		}
	}
	operations := submission.Operations
	if len(operations) > maxSubmittedItems {
		operations = operations[:maxSubmittedItems]
	}
	for index, op := range operations {
		op.Type = strings.ToLower(strings.TrimSpace(op.Type))
		op.Title = strings.Join(strings.Fields(op.Title), " ")
		op.Content = strings.TrimSpace(op.Content)
		op.Reason = truncateRunes(strings.TrimSpace(op.Reason), MaxReasonChars)
		if len(kept) >= maxOperations {
			reject(index, op, "the run keeps at most %d operations", maxOperations)
			continue
		}
		wantTargets, ok := targetRule(op.Type)
		if !ok {
			reject(index, op, "unknown operation type %q", op.Type)
			continue
		}
		if reason := wantTargets(len(op.Targets)); reason != "" {
			reject(index, op, "%s", reason)
			continue
		}
		if op.Type != OperationRetire {
			if op.Content == "" {
				reject(index, op, "a %s operation needs the content of the memory it creates", op.Type)
				continue
			}
			if len([]rune(op.Content)) > MaxContentChars {
				reject(index, op, "content is longer than %d characters", MaxContentChars)
				continue
			}
		} else {
			op.Content = ""
		}
		if reason := checkTargets(op.Targets, generations, current, targeted); reason != "" {
			reject(index, op, "%s", reason)
			continue
		}
		if reason := checkSources(op, messages); reason != "" {
			reject(index, op, "%s", reason)
			continue
		}
		if removed+removes(op) > removalBudget {
			reject(index, op, "the run removes at most %d of the %d memories it read", removalBudget, len(input.Memories))
			continue
		}
		if op.Title == "" {
			op.Title = defaultTitle(op)
		}
		op.Title = truncateRunes(op.Title, MaxTitleChars)
		op.Tags = normalizeTags(op.Tags)
		for _, target := range op.Targets {
			targeted[target.MemoryID] = true
		}
		removed += removes(op)
		kept = append(kept, op)
	}
	if len(submission.Operations) > maxSubmittedItems {
		rejected = append(rejected, Rejection{Index: maxSubmittedItems,
			Reason: fmt.Sprintf("%d operations past the first %d were not read", len(submission.Operations)-maxSubmittedItems, maxSubmittedItems)})
	}
	return kept, rejected, validConflicts(submission.Conflicts, generations)
}

// RemovalBudget is how many of memories active memories a run may remove:
// a quarter, and at least one.
func RemovalBudget(memories int) int {
	return max(1, memories/4)
}

// removes is how many active memories an operation removes.
func removes(op Operation) int {
	switch op.Type {
	case OperationMerge:
		return len(op.Targets) - 1
	case OperationRetire:
		return len(op.Targets)
	default:
		return 0
	}
}

// targetRule returns the check of an operation type's target count.
func targetRule(operationType string) (func(int) string, bool) {
	switch operationType {
	case OperationAdd:
		return func(n int) string {
			if n != 0 {
				return "an add operation targets no memories"
			}
			return ""
		}, true
	case OperationMerge:
		return func(n int) string {
			if n < 2 || n > store.MaxMemoryProposalTargets {
				return fmt.Sprintf("a merge operation targets 2 to %d memories", store.MaxMemoryProposalTargets)
			}
			return ""
		}, true
	case OperationSupersede:
		return func(n int) string {
			if n != 1 {
				return "a supersede operation targets exactly one memory"
			}
			return ""
		}, true
	case OperationRetire:
		return func(n int) string {
			if n < 1 || n > store.MaxMemoryProposalTargets {
				return fmt.Sprintf("a retire operation targets 1 to %d memories", store.MaxMemoryProposalTargets)
			}
			return ""
		}, true
	default:
		return nil, false
	}
}

func checkTargets(targets []store.MemoryProposalTarget, generations, current map[string]uint64, targeted map[string]bool) string {
	seen := map[string]bool{}
	for _, target := range targets {
		generation, read := generations[target.MemoryID]
		now, active := current[target.MemoryID]
		switch {
		case !read:
			return fmt.Sprintf("target memory %q is not one the run read", target.MemoryID)
		case generation != target.Generation:
			return fmt.Sprintf("target memory %q is at generation %d, not %d", target.MemoryID, generation, target.Generation)
		case current != nil && !active:
			return fmt.Sprintf("target memory %q is no longer active", target.MemoryID)
		case current != nil && now != generation:
			return fmt.Sprintf("target memory %q changed since the run read it", target.MemoryID)
		case seen[target.MemoryID]:
			return fmt.Sprintf("target memory %q is named twice", target.MemoryID)
		case targeted[target.MemoryID]:
			return fmt.Sprintf("target memory %q is already changed by an earlier operation", target.MemoryID)
		}
		seen[target.MemoryID] = true
	}
	return ""
}

func checkSources(op Operation, messages map[string]map[string]bool) string {
	if len(op.Sources) > store.MaxMemoryProposalSources {
		return fmt.Sprintf("an operation cites at most %d sources", store.MaxMemoryProposalSources)
	}
	if op.Type == OperationAdd && len(op.Sources) == 0 {
		return "an add operation cites the Sessions it learned from"
	}
	for _, source := range op.Sources {
		ids, read := messages[source.SessionName]
		if !read {
			return fmt.Sprintf("source Session %q is not one the run read", source.SessionName)
		}
		if source.MessageID != "" && !ids[source.MessageID] {
			return fmt.Sprintf("source message %q is not one the run read in Session %q", source.MessageID, source.SessionName)
		}
	}
	return ""
}

func validConflicts(conflicts []Conflict, generations map[string]uint64) []Conflict {
	var kept []Conflict
	for _, conflict := range conflicts {
		if len(kept) >= MaxConflicts {
			break
		}
		var ids []string
		for _, id := range conflict.MemoryIDs {
			if _, read := generations[id]; read && len(ids) < store.MaxMemoryProposalTargets {
				ids = append(ids, id)
			}
		}
		note := truncateRunes(strings.TrimSpace(conflict.Note), MaxReasonChars)
		if len(ids) > 0 && note != "" {
			kept = append(kept, Conflict{MemoryIDs: ids, Note: note})
		}
	}
	return kept
}

func defaultTitle(op Operation) string {
	if op.Content != "" {
		return strings.Join(strings.Fields(strings.SplitN(op.Content, "\n", 2)[0]), " ")
	}
	return fmt.Sprintf("Retire %d memories", len(op.Targets))
}

func normalizeTags(tags []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, tag := range tags {
		tag = strings.ToLower(strings.TrimSpace(tag))
		if tag == "" || seen[tag] || strings.ContainsAny(tag, ",\n") || len([]rune(tag)) > MaxTagChars {
			continue
		}
		seen[tag] = true
		out = append(out, tag)
		if len(out) == MaxTags {
			break
		}
	}
	return out
}

// proposalTypes maps operations to the proposal types that apply them.
var proposalTypes = map[string]string{
	OperationAdd:       store.MemoryProposalTypeMemory,
	OperationMerge:     store.MemoryProposalTypeMerge,
	OperationSupersede: store.MemoryProposalTypeSupersede,
	OperationRetire:    store.MemoryProposalTypeRetire,
}

// ProposalID is the deterministic ID of a run's index-th proposal, so a
// retried completion cannot create a second copy.
func ProposalID(taskUID string, index int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "memory-consolidation/%s/%d", taskUID, index))
	return "mprop-" + hex.EncodeToString(sum[:16])
}

// Proposals returns one pending proposal for each kept operation, attributed
// to the run's Task. A proposal's tags ride on a Tags: line in its
// description, as every memory proposal's do.
func Proposals(kept []Operation, namespace, taskName, taskUID, agentName string) []store.MemoryProposal {
	proposals := make([]store.MemoryProposal, 0, len(kept))
	for index, op := range kept {
		description := op.Reason
		if len(op.Tags) > 0 {
			tagLine := "Tags: " + strings.Join(op.Tags, ", ")
			description = strings.TrimSpace(description + "\n\n" + tagLine)
		}
		proposals = append(proposals, store.MemoryProposal{
			ID: ProposalID(taskUID, index), Namespace: namespace, TaskName: taskName, AgentName: agentName,
			Type: proposalTypes[op.Type], Title: op.Title, Description: description, Content: op.Content,
			Targets: append([]store.MemoryProposalTarget(nil), op.Targets...),
			Sources: append([]store.MemoryProposalSource(nil), op.Sources...),
		})
	}
	return proposals
}

// NewReport returns the report of a run over input with the kept and
// rejected operations. In Propose mode it names the proposals created.
func NewReport(settings Settings, status string, input Input, kept []Operation, rejected []Rejection, conflicts []Conflict,
	summary string, proposals []store.MemoryProposal) Report {
	report := Report{
		Mode: settings.Mode, Status: status, Memories: len(input.Memories), EligibleSessions: input.EligibleSessions,
		ReadSessions: len(input.Sessions), InputTruncated: input.Truncated, Operations: kept, Rejected: rejected, Conflicts: conflicts,
		Summary: truncateRunes(strings.TrimSpace(summary), MaxSummaryChars),
	}
	if report.Operations == nil {
		report.Operations = []Operation{}
	}
	if settings.Mode == corev1alpha1.MemoryConsolidationPropose {
		for _, proposal := range proposals {
			report.ProposalIDs = append(report.ProposalIDs, proposal.ID)
		}
	}
	return report
}

// Redact applies Orka's sensitive-text redaction to every free-text field of
// the report, which keeps model output and becomes the Task result.
func (r *Report) Redact() {
	for i := range r.Operations {
		op := &r.Operations[i]
		op.Title, op.Content, op.Reason = redact.SensitiveText(op.Title), redact.SensitiveText(op.Content), redact.SensitiveText(op.Reason)
		for j := range op.Tags {
			op.Tags[j] = redact.SensitiveText(op.Tags[j])
		}
	}
	for i := range r.Rejected {
		rejection := &r.Rejected[i]
		rejection.Type = redact.SensitiveText(rejection.Type)
		rejection.Title = redact.SensitiveText(rejection.Title)
		rejection.Reason = redact.SensitiveText(rejection.Reason)
	}
	for i := range r.Conflicts {
		r.Conflicts[i].Note = redact.SensitiveText(r.Conflicts[i].Note)
	}
	r.Summary = redact.SensitiveText(r.Summary)
}

// Encode redacts the report and encodes it in at most limit bytes. Every
// field is bounded, but text that is mostly multibyte or escaped can still
// encode larger than a run's report may be. Encode then drops, in order, the
// rejected operations, the conflicts, the summary, and the kept operations'
// text, and marks the report Abridged: the kept operations' proposals keep
// their full text, so the report loses nothing that a review needs.
func (r *Report) Encode(limit int) ([]byte, error) {
	r.Redact()
	steps := []func(){
		func() { r.Rejected = nil },
		func() { r.Conflicts = nil },
		func() { r.Summary = "" },
		func() {
			for i := range r.Operations {
				r.Operations[i].Title, r.Operations[i].Content, r.Operations[i].Reason, r.Operations[i].Tags = "", "", "", nil
			}
		},
	}
	for _, step := range steps {
		encoded, err := json.Marshal(r)
		if err != nil || len(encoded) <= limit {
			return encoded, err
		}
		step()
		r.Abridged = true
	}
	encoded, err := json.Marshal(r)
	if err != nil {
		return nil, err
	}
	if len(encoded) > limit {
		return nil, fmt.Errorf("memory consolidation report is %d bytes, more than %d", len(encoded), limit)
	}
	return encoded, nil
}
