/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package scout

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	// DefaultMaxProposals is the default per-run proposal cap (N).
	DefaultMaxProposals = 3
	// MaxProposalsLimit is the largest cap a caller may configure.
	MaxProposalsLimit = 10

	MaxSources              = 50
	MaxSourceIDBytes        = 64
	MaxSourceItemsSeen      = 100_000
	MaxDropReasons          = 16
	MaxDroppedCount         = 100_000
	MaxWatchlistSuggestions = 5
)

// SourceKind names one watchlist source family.
type SourceKind string

const (
	SourceKindGitHub  SourceKind = "github"
	SourceKindTracker SourceKind = "tracker"
	SourceKindFeed    SourceKind = "feed"
	SourceKindSignals SourceKind = "signals"
)

// SourceStatus says whether a source was read in full.
type SourceStatus string

const (
	SourceStatusOK          SourceStatus = "ok"
	SourceStatusUnreachable SourceStatus = "unreachable"
	SourceStatusRateLimited SourceStatus = "rate-limited"
	SourceStatusSkipped     SourceStatus = "skipped"
	SourceStatusError       SourceStatus = "error"
)

// DropReason classifies an item the scout or a verifier did not publish.
type DropReason string

const (
	DropOutOfScope         DropReason = "out-of-scope"
	DropDuplicate          DropReason = "duplicate"
	DropPreviouslyRejected DropReason = "previously-rejected"
	DropBelowRank          DropReason = "below-rank"
	DropUnverifiedLink     DropReason = "unverified-link"
	DropUnverifiedPath     DropReason = "unverified-path"
	DropInvalidSchema      DropReason = "invalid-schema"
	DropCheckUnavailable   DropReason = "check-unavailable"
)

var dropReasons = map[DropReason]struct{}{
	DropOutOfScope: {}, DropDuplicate: {}, DropPreviouslyRejected: {}, DropBelowRank: {},
	DropUnverifiedLink: {}, DropUnverifiedPath: {}, DropInvalidSchema: {}, DropCheckUnavailable: {},
}

// Digest is the orka.scout.digest.v1 result of one scout run.
type Digest struct {
	SchemaVersion        string                `json:"schemaVersion"`
	Run                  RunMetadata           `json:"run"`
	Sources              []SourceReport        `json:"sources"`
	Proposals            []Proposal            `json:"proposals"`
	Dropped              []Dropped             `json:"dropped"`
	WatchlistSuggestions []WatchlistSuggestion `json:"watchlistSuggestions"`
}

// RunMetadata identifies the run and the window of source activity it read.
type RunMetadata struct {
	// WindowStart and WindowEnd bound the source activity considered, as
	// RFC 3339 timestamps.
	WindowStart string `json:"windowStart"`
	WindowEnd   string `json:"windowEnd"`
	// OrkaCommit is the pinned commit of the read-only Orka checkout every
	// proposal cites.
	OrkaCommit string `json:"orkaCommit"`
	// Partial is true when any source was not read in full.
	Partial bool `json:"partial"`
}

// SourceReport records the outcome of reading one watchlist source.
type SourceReport struct {
	ID        string       `json:"id"`
	Kind      SourceKind   `json:"kind"`
	Status    SourceStatus `json:"status"`
	ItemsSeen int          `json:"itemsSeen"`
	Detail    string       `json:"detail,omitempty"`
}

// Dropped counts items that were not published, by reason.
type Dropped struct {
	Reason DropReason `json:"reason"`
	Count  int        `json:"count"`
}

// WatchlistSuggestion proposes a watchlist addition for a person to review.
// The scout cannot edit the watchlist itself.
type WatchlistSuggestion struct {
	URL    string `json:"url"`
	Reason string `json:"reason"`
}

var sourceIDPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Limits bounds a digest. The caller's cap is authoritative; a digest never
// declares its own.
type Limits struct {
	// MaxProposals is N. Zero selects DefaultMaxProposals.
	MaxProposals int
}

func (l Limits) maxProposals() (int, error) {
	if l.MaxProposals == 0 {
		return DefaultMaxProposals, nil
	}
	if l.MaxProposals < 1 || l.MaxProposals > MaxProposalsLimit {
		return 0, fmt.Errorf("max proposals must be between 1 and %d", MaxProposalsLimit)
	}
	return l.MaxProposals, nil
}

// Validate checks the digest structure, every proposal, and the partial-run
// report. Exceeding the proposal cap rejects the whole digest: a scout that
// ignores its cap is not following its instructions, so nothing it produced
// is trusted.
func (d *Digest) Validate(limits Limits) error {
	if d == nil {
		return errors.New("digest is required")
	}
	maxProposals, err := limits.maxProposals()
	if err != nil {
		return err
	}
	if d.SchemaVersion != DigestSchemaVersion {
		return fmt.Errorf("schemaVersion must be %q", DigestSchemaVersion)
	}
	if err := d.Run.validate(); err != nil {
		return fmt.Errorf("run: %w", err)
	}
	if err := d.validateSources(); err != nil {
		return err
	}
	if d.Proposals == nil {
		return errors.New("proposals is required; use an empty list when nothing qualified")
	}
	if len(d.Proposals) > maxProposals {
		return fmt.Errorf("proposals has %d entries, cap is %d", len(d.Proposals), maxProposals)
	}
	titles := make(map[string]struct{}, len(d.Proposals))
	for index := range d.Proposals {
		proposal := &d.Proposals[index]
		if err := proposal.Validate(); err != nil {
			return fmt.Errorf("proposals[%d]: %w", index, err)
		}
		if proposal.Evidence.Commit != d.Run.OrkaCommit {
			return fmt.Errorf("proposals[%d]: evidence.commit does not match run.orkaCommit", index)
		}
		if _, duplicate := titles[proposal.Title]; duplicate {
			return fmt.Errorf("proposals[%d]: title repeats an earlier proposal", index)
		}
		titles[proposal.Title] = struct{}{}
	}
	if err := validateDropped(d.Dropped); err != nil {
		return err
	}
	if d.WatchlistSuggestions == nil {
		return errors.New("watchlistSuggestions is required; use an empty list when there are none")
	}
	if len(d.WatchlistSuggestions) > MaxWatchlistSuggestions {
		return fmt.Errorf("watchlistSuggestions has %d entries, maximum is %d", len(d.WatchlistSuggestions), MaxWatchlistSuggestions)
	}
	for index, suggestion := range d.WatchlistSuggestions {
		if err := ValidateSourceURL(suggestion.URL); err != nil {
			return fmt.Errorf("watchlistSuggestions[%d].url: %w", index, err)
		}
		if err := checkText(fmt.Sprintf("watchlistSuggestions[%d].reason", index), suggestion.Reason, MaxReasonRunes, false); err != nil {
			return err
		}
	}
	return nil
}

func (r *RunMetadata) validate() error {
	start, err := time.Parse(time.RFC3339, r.WindowStart)
	if err != nil {
		return errors.New("windowStart must be an RFC 3339 timestamp")
	}
	end, err := time.Parse(time.RFC3339, r.WindowEnd)
	if err != nil {
		return errors.New("windowEnd must be an RFC 3339 timestamp")
	}
	if !end.After(start) {
		return errors.New("windowEnd must be after windowStart")
	}
	if !commitPattern.MatchString(r.OrkaCommit) {
		return errors.New("orkaCommit must be a full lowercase hexadecimal object name")
	}
	return nil
}

// validateSources requires a report for every source the run attempted, and
// requires run.partial to agree with it, so a run cannot claim completeness
// while a source failed, or hide the fact that it read nothing.
func (d *Digest) validateSources() error {
	if len(d.Sources) == 0 {
		return errors.New("sources must report every watchlist source the run attempted")
	}
	if len(d.Sources) > MaxSources {
		return fmt.Errorf("sources has %d entries, maximum is %d", len(d.Sources), MaxSources)
	}
	partial := false
	seen := make(map[string]struct{}, len(d.Sources))
	for index, source := range d.Sources {
		if len(source.ID) > MaxSourceIDBytes || !sourceIDPattern.MatchString(source.ID) {
			return fmt.Errorf("sources[%d].id must be a lowercase identifier of at most %d bytes", index, MaxSourceIDBytes)
		}
		if _, duplicate := seen[source.ID]; duplicate {
			return fmt.Errorf("sources[%d].id %q is listed twice", index, source.ID)
		}
		seen[source.ID] = struct{}{}
		switch source.Kind {
		case SourceKindGitHub, SourceKindTracker, SourceKindFeed, SourceKindSignals:
		default:
			return fmt.Errorf("sources[%d].kind is not a known source kind", index)
		}
		switch source.Status {
		case SourceStatusOK:
		case SourceStatusUnreachable, SourceStatusRateLimited, SourceStatusSkipped, SourceStatusError:
			partial = true
			if source.Detail == "" {
				return fmt.Errorf("sources[%d].detail must explain a %q status", index, source.Status)
			}
		default:
			return fmt.Errorf("sources[%d].status is not a known status", index)
		}
		if source.ItemsSeen < 0 || source.ItemsSeen > MaxSourceItemsSeen {
			return fmt.Errorf("sources[%d].itemsSeen is out of range", index)
		}
		if source.Detail != "" {
			if err := checkText(fmt.Sprintf("sources[%d].detail", index), source.Detail, MaxReasonRunes, false); err != nil {
				return err
			}
		}
	}
	if partial != d.Run.Partial {
		return errors.New("run.partial must be true exactly when a source status is not ok")
	}
	return nil
}

func validateDropped(dropped []Dropped) error {
	if dropped == nil {
		return errors.New("dropped is required; use an empty list when nothing was dropped")
	}
	if len(dropped) > MaxDropReasons {
		return fmt.Errorf("dropped has %d entries, maximum is %d", len(dropped), MaxDropReasons)
	}
	seen := make(map[DropReason]struct{}, len(dropped))
	for index, entry := range dropped {
		if _, known := dropReasons[entry.Reason]; !known {
			return fmt.Errorf("dropped[%d].reason is not a known reason", index)
		}
		if _, duplicate := seen[entry.Reason]; duplicate {
			return fmt.Errorf("dropped[%d].reason %q is listed twice", index, entry.Reason)
		}
		seen[entry.Reason] = struct{}{}
		if entry.Count < 1 || entry.Count > MaxDroppedCount {
			return fmt.Errorf("dropped[%d].count is out of range", index)
		}
	}
	return nil
}

// DroppedCount returns the count recorded for reason.
func (d *Digest) DroppedCount(reason DropReason) int {
	for _, entry := range d.Dropped {
		if entry.Reason == reason {
			return entry.Count
		}
	}
	return 0
}

func (d *Digest) addDropped(reason DropReason) {
	for index := range d.Dropped {
		if d.Dropped[index].Reason == reason {
			d.Dropped[index].Count++
			return
		}
	}
	d.Dropped = append(d.Dropped, Dropped{Reason: reason, Count: 1})
}
