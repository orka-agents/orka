/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package memoryconsolidation implements the memory consolidation runs of
// ADR 0051: the bounded inputs a run reads, the prompt and result schema the
// model sees, the validation that decides which of its operations Orka
// reports or proposes, and the report the run leaves.
package memoryconsolidation

import (
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

// Bounds on a run. They keep a run's input, its proposals and its report
// small enough for one model call and one review.
const (
	DefaultMinNewSessions = 5
	DefaultMaxOperations  = 10
	MaxOperationsLimit    = 20
	DefaultLookback       = 7 * 24 * time.Hour
	MaxLookback           = 30 * 24 * time.Hour

	// MaxInputMemories is the most memories a run reads.
	MaxInputMemories = 200
	// MaxInputSessions is the most Sessions a run reads: the most recently
	// changed of those eligible.
	MaxInputSessions = 50
	// MaxMessagesPerSession is how many of each Session's newest messages a
	// run reads, before dropping all but user and assistant text.
	MaxMessagesPerSession = 40
	MaxMessageChars       = 2000
	// MaxMemoryChars bounds each memory a run reads; memories have no limit
	// of their own.
	MaxMemoryChars = 2000
	// MaxMemoryInputChars and MaxTranscriptInputChars bound the two parts of
	// a run's input separately, so memories never crowd out the transcripts
	// a run learns from. Together they stay well inside a model's context
	// even at one token per character; a model that still cannot take the
	// input fails the run, which leaves its Sessions to the next one.
	MaxMemoryInputChars     = 60_000
	MaxTranscriptInputChars = 60_000
	MaxInputChars           = MaxMemoryInputChars + MaxTranscriptInputChars + MaxProposalInputChars

	// MaxInputProposals is the most open memory proposals a run reads, the
	// newest first, so it does not propose again what already awaits review.
	MaxInputProposals = 50
	// MaxProposalInputChars bounds the open proposals a run reads, apart
	// from its memories and transcripts.
	MaxProposalInputChars = 20_000

	MaxContentChars   = 1200
	MaxTitleChars     = 120
	MaxReasonChars    = 500
	MaxSummaryChars   = 2000
	MaxTags           = 10
	MaxTagChars       = 40
	MaxConflicts      = 20
	MaxRejected       = 50
	maxSubmittedItems = 100
)

// Wire names of the operations a run submits.
const (
	OperationAdd       = "add"
	OperationMerge     = "merge"
	OperationSupersede = "supersede"
	OperationRetire    = "retire"
)

// Run statuses, as the store records them.
const (
	StatusRunning   = store.MemoryConsolidationRunning
	StatusCompleted = store.MemoryConsolidationCompleted
	StatusSkipped   = store.MemoryConsolidationSkipped
)

// Settings are a run's effective configuration.
type Settings struct {
	Mode           corev1alpha1.MemoryConsolidationMode `json:"mode"`
	MinNewSessions int                                  `json:"minNewSessions"`
	MaxOperations  int                                  `json:"maxOperations"`
	Lookback       time.Duration                        `json:"lookback"`
}

// SettingsFor returns the effective settings of a Task's run, applying the
// defaults and clamping every value to its bound.
func SettingsFor(spec *corev1alpha1.MemoryConsolidationSpec) Settings {
	settings := Settings{
		Mode:           corev1alpha1.MemoryConsolidationDryRun,
		MinNewSessions: DefaultMinNewSessions,
		MaxOperations:  DefaultMaxOperations,
		Lookback:       DefaultLookback,
	}
	if spec == nil {
		return settings
	}
	if spec.Mode == corev1alpha1.MemoryConsolidationPropose {
		settings.Mode = corev1alpha1.MemoryConsolidationPropose
	}
	if spec.MinNewSessions != nil {
		settings.MinNewSessions = clamp(int(*spec.MinNewSessions), 1, MaxInputSessions)
	}
	if spec.MaxOperations != nil {
		settings.MaxOperations = clamp(int(*spec.MaxOperations), 1, MaxOperationsLimit)
	}
	if spec.Lookback != nil && spec.Lookback.Duration > 0 {
		settings.Lookback = min(max(spec.Lookback.Duration, time.Hour), MaxLookback)
	}
	return settings
}

// WindowCommitGrace is how long before a run begins its window ends. A
// Session change is stamped with its write's issue time, and a retried write
// can commit long after; a provider refuses a write more than MaxOperationAge
// after it was issued, allowing MaxClockSkew, so no change stamped before the
// end commits after the run reads.
const WindowCommitGrace = storagev1.MaxOperationAge + storagev1.MaxClockSkew

// Window returns the Sessions a run reads: those changed after start and no
// later than end. A namespace's first run in a mode starts the lookback
// before now. A later run starts at the mode's cursor: where the latest
// completed run ended, or, before any completed, where the first run
// started, so a skipped run's Sessions are read again. A cursor is never
// followed further back than MaxLookback.
//
// The window ends on a whole second, WindowCommitGrace before now. The store
// stamps some Session changes with whole-second times, and a change is
// stamped before its transaction commits, so a change stamped at or before
// the end has committed before the run reads, and any later one falls in the
// next window rather than between the two.
func Window(cursor *time.Time, now time.Time, lookback time.Duration) (start, end time.Time) {
	end = now.UTC().Truncate(time.Second).Add(-WindowCommitGrace)
	if cursor == nil {
		return end.Add(-lookback), end
	}
	start = cursor.UTC()
	if earliest := end.Add(-MaxLookback); start.Before(earliest) {
		start = earliest
	}
	if start.After(end) {
		start = end
	}
	return start, end
}

// Input is what a run reads.
type Input struct {
	Memories []InputMemory  `json:"memories"`
	Sessions []InputSession `json:"sessions"`
	// Proposals are the namespace's open memory proposals: pending, or
	// accepted and not yet applied.
	Proposals []InputProposal `json:"proposals,omitempty"`
	// EligibleSessions counts every Session that changed in the window,
	// including those beyond MaxInputSessions.
	EligibleSessions int `json:"eligibleSessions"`
	// Truncated reports that a limit was reached, so some memories, Sessions
	// messages or open proposals may have been left out: the character
	// budgets, the 200 memories, the 50 Sessions, a Session's message limits,
	// or the 50 open proposals.
	Truncated bool `json:"truncated,omitempty"`
	// CutFrom is the cursor a window was cut from when it lay further back
	// than MaxLookback: Sessions changed between it and the window's start
	// were not read.
	CutFrom *time.Time `json:"cutFrom,omitempty"`
}

// InputMemory is one active memory as a run sees it.
type InputMemory struct {
	ID             string     `json:"id"`
	Generation     uint64     `json:"generation"`
	Content        string     `json:"content"`
	Tags           []string   `json:"tags,omitempty"`
	RecalledCount  int        `json:"recalledCount"`
	LastRecalledAt *time.Time `json:"lastRecalledAt,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

// InputProposal is one open memory proposal as a run sees it: a change that
// already awaits review.
type InputProposal struct {
	ID      string                       `json:"id"`
	Type    string                       `json:"type"`
	Status  string                       `json:"status"`
	Title   string                       `json:"title,omitempty"`
	Content string                       `json:"content,omitempty"`
	Targets []store.MemoryProposalTarget `json:"targets,omitempty"`
}

// InputSession is one Session's recent user and assistant messages.
type InputSession struct {
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	UpdatedAt time.Time      `json:"updatedAt"`
	Messages  []InputMessage `json:"messages"`
}

// InputMessage is one transcript message, bounded.
type InputMessage struct {
	ID      string `json:"id,omitempty"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

// Submission is the structured result a run's model submits.
type Submission struct {
	Operations []Operation `json:"operations"`
	Conflicts  []Conflict  `json:"conflicts,omitempty"`
	Summary    string      `json:"summary,omitempty"`
}

// Operation is one proposed change to the namespace's memory.
type Operation struct {
	Type    string                       `json:"type"`
	Title   string                       `json:"title,omitempty"`
	Content string                       `json:"content,omitempty"`
	Tags    []string                     `json:"tags,omitempty"`
	Reason  string                       `json:"reason,omitempty"`
	Targets []store.MemoryProposalTarget `json:"targets,omitempty"`
	Sources []store.MemoryProposalSource `json:"sources,omitempty"`
	// Covers names open proposals the run read that this operation makes
	// redundant, so the reviewer can reject or archive them in its favor.
	Covers []string `json:"covers,omitempty"`
}

// Conflict is a contradiction between memories that the run could not settle.
type Conflict struct {
	MemoryIDs []string `json:"memoryIds"`
	Note      string   `json:"note"`
}

// Rejection records an operation Orka dropped and why.
type Rejection struct {
	Index  int    `json:"index"`
	Type   string `json:"type,omitempty"`
	Title  string `json:"title,omitempty"`
	Reason string `json:"reason"`
}

// Report is the outcome of a run. It becomes the Task result.
type Report struct {
	Mode             corev1alpha1.MemoryConsolidationMode `json:"mode"`
	Status           string                               `json:"status"`
	WindowStart      time.Time                            `json:"windowStart"`
	WindowEnd        time.Time                            `json:"windowEnd"`
	Memories         int                                  `json:"memories"`
	EligibleSessions int                                  `json:"eligibleSessions"`
	ReadSessions     int                                  `json:"readSessions"`
	InputTruncated   bool                                 `json:"inputTruncated,omitempty"`
	// WindowCutFrom is the cursor the window was cut from; see Input.CutFrom.
	WindowCutFrom *time.Time  `json:"windowCutFrom,omitempty"`
	SkippedReason string      `json:"skippedReason,omitempty"`
	Operations    []Operation `json:"operations"`
	ProposalIDs   []string    `json:"proposalIds,omitempty"`
	Rejected      []Rejection `json:"rejected,omitempty"`
	Conflicts     []Conflict  `json:"conflicts,omitempty"`
	Summary       string      `json:"summary,omitempty"`
	// Abridged reports that detail was dropped to fit the report's size
	// limit; see Report.Encode.
	Abridged bool `json:"abridged,omitempty"`
	// Structural reports that the report holds only its structure; see
	// Report.Structure. The full report is served to callers who may read
	// the run's inputs.
	Structural bool `json:"structural,omitempty"`
}

// BeginResponse is the internal API's answer to a run that begins. Input is
// set only while the run is running; a finished run returns its report.
type BeginResponse struct {
	Status   string   `json:"status"`
	Settings Settings `json:"settings"`
	Input    *Input   `json:"input,omitempty"`
	Report   *Report  `json:"report,omitempty"`
}

func clamp(value, low, high int) int {
	return min(max(value, low), high)
}
