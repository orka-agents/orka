/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package memoryconsolidation

import (
	"time"

	corev1alpha1 "github.com/orka-agents/orka/api/v1alpha1"
	"github.com/orka-agents/orka/internal/store"
	storagev1 "github.com/orka-agents/orka/pkg/storage/v1"
)

const (
	DefaultMinNewSessions = 5
	DefaultMaxOperations  = 10
	MaxOperationsLimit    = 20
	DefaultLookback       = 7 * 24 * time.Hour
	MaxLookback           = 30 * 24 * time.Hour

	MaxInputMemories        = 200
	MaxInputSessions        = 50
	MaxMessagesPerSession   = 40
	MaxMessageChars         = 2000
	MaxMemoryChars          = 2000
	MaxMemoryInputChars     = 60_000
	MaxTranscriptInputChars = 60_000
	MaxInputChars           = MaxMemoryInputChars + MaxTranscriptInputChars + MaxProposalInputChars

	MaxInputProposals     = 50
	MaxProposalInputChars = 20_000
	MaxCoveredIDChars     = 128

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

const (
	OperationAdd       = "add"
	OperationMerge     = "merge"
	OperationSupersede = "supersede"
	OperationRetire    = "retire"
)

const (
	StatusRunning   = store.MemoryConsolidationRunning
	StatusCompleted = store.MemoryConsolidationCompleted
	StatusSkipped   = store.MemoryConsolidationSkipped
)

type Settings struct {
	Mode           corev1alpha1.MemoryConsolidationMode `json:"mode"`
	MinNewSessions int                                  `json:"minNewSessions"`
	MaxOperations  int                                  `json:"maxOperations"`
	Lookback       time.Duration                        `json:"lookback"`
}

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

const WindowCommitGrace = storagev1.MaxOperationAge + storagev1.MaxClockSkew

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

type Input struct {
	Memories         []InputMemory   `json:"memories"`
	Sessions         []InputSession  `json:"sessions"`
	Proposals        []InputProposal `json:"proposals,omitempty"`
	LaterProposals   []InputProposal `json:"-"`
	ClosedProposals  map[string]bool `json:"-"`
	EligibleSessions int             `json:"eligibleSessions"`
	Truncated        bool            `json:"truncated,omitempty"`
	CutFrom          *time.Time      `json:"cutFrom,omitempty"`
}

type InputMemory struct {
	ID             string     `json:"id"`
	Generation     uint64     `json:"generation"`
	Content        string     `json:"content"`
	Tags           []string   `json:"tags,omitempty"`
	RecalledCount  int        `json:"recalledCount"`
	LastRecalledAt *time.Time `json:"lastRecalledAt,omitempty"`
	UpdatedAt      time.Time  `json:"updatedAt"`
}

type InputProposal struct {
	ID      string                       `json:"id"`
	Type    string                       `json:"type"`
	Status  string                       `json:"status"`
	Title   string                       `json:"title,omitempty"`
	Content string                       `json:"content,omitempty"`
	Targets []store.MemoryProposalTarget `json:"targets,omitempty"`
}

type InputSession struct {
	Name      string         `json:"name"`
	Type      string         `json:"type"`
	UpdatedAt time.Time      `json:"updatedAt"`
	Messages  []InputMessage `json:"messages"`
}

type InputMessage struct {
	ID      string `json:"id,omitempty"`
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Submission struct {
	Operations []Operation `json:"operations"`
	Conflicts  []Conflict  `json:"conflicts,omitempty"`
	Summary    string      `json:"summary,omitempty"`
}

type Operation struct {
	Type    string                       `json:"type"`
	Title   string                       `json:"title,omitempty"`
	Content string                       `json:"content,omitempty"`
	Tags    []string                     `json:"tags,omitempty"`
	Reason  string                       `json:"reason,omitempty"`
	Targets []store.MemoryProposalTarget `json:"targets,omitempty"`
	Sources []store.MemoryProposalSource `json:"sources,omitempty"`
	Covers  []string                     `json:"covers,omitempty"`
}

type Conflict struct {
	MemoryIDs []string `json:"memoryIds"`
	Note      string   `json:"note"`
}

type Rejection struct {
	Index  int    `json:"index"`
	Type   string `json:"type,omitempty"`
	Title  string `json:"title,omitempty"`
	Reason string `json:"reason"`
}

type Report struct {
	Mode             corev1alpha1.MemoryConsolidationMode `json:"mode"`
	Status           string                               `json:"status"`
	WindowStart      time.Time                            `json:"windowStart"`
	WindowEnd        time.Time                            `json:"windowEnd"`
	Memories         int                                  `json:"memories"`
	EligibleSessions int                                  `json:"eligibleSessions"`
	ReadSessions     int                                  `json:"readSessions"`
	InputTruncated   bool                                 `json:"inputTruncated,omitempty"`
	WindowCutFrom    *time.Time                           `json:"windowCutFrom,omitempty"`
	SkippedReason    string                               `json:"skippedReason,omitempty"`
	Operations       []Operation                          `json:"operations"`
	ProposalIDs      []string                             `json:"proposalIds,omitempty"`
	Rejected         []Rejection                          `json:"rejected,omitempty"`
	Conflicts        []Conflict                           `json:"conflicts,omitempty"`
	Summary          string                               `json:"summary,omitempty"`
	Abridged         bool                                 `json:"abridged,omitempty"`
	Structural       bool                                 `json:"structural,omitempty"`
}

type BeginResponse struct {
	Status   string   `json:"status"`
	Settings Settings `json:"settings"`
	Input    *Input   `json:"input,omitempty"`
	Report   *Report  `json:"report,omitempty"`
}

func clamp(value, low, high int) int {
	return min(max(value, low), high)
}
