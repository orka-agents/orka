/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package scout

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

const (
	// DefaultRunWindow is the source window a scout run reads: the 8 days
	// that end when the run starts.
	DefaultRunWindow = 8 * 24 * time.Hour
	// RunWindowTolerance is how far a digest's reported window may sit from
	// the trusted one. The scout has no trusted clock of its own, so it may
	// only know the date.
	RunWindowTolerance = 24 * time.Hour
)

// PathChecker reports whether a repository path exists at a commit.
// Implementations must return an error, not false, when they cannot decide,
// for example because the commit is not present locally.
type PathChecker interface {
	PathExists(ctx context.Context, commit, repoPath string) (bool, error)
}

// LinkChecker reports whether an outside URL currently resolves. As with
// PathChecker, an inconclusive check is an error rather than false.
type LinkChecker interface {
	LinkResolves(ctx context.Context, rawURL string) (bool, error)
}

// IssueChecker reports whether an Orka issue or pull request number exists.
// As with PathChecker, an inconclusive check is an error rather than false.
type IssueChecker interface {
	IssueExists(ctx context.Context, number int) (bool, error)
}

// Verifier re-checks proposal evidence before anything is published. Every
// field except RunWindow is required; there is no way to skip a check.
type Verifier struct {
	// PinnedCommit is the trusted commit of the scout's read-only checkout,
	// such as the scout Task's status.delivery.startingSHA. A digest that
	// claims a different commit is rejected as a whole.
	PinnedCommit string
	// RunStart is the trusted time the run started, such as the scout child
	// Task's creationTimestamp. A digest whose window is more than
	// RunWindowTolerance away from [RunStart-RunWindow, RunStart] is rejected
	// as a whole, because the scout cannot read a trusted clock and a guessed
	// window would misstate what was read.
	RunStart time.Time
	// RunWindow is the window length the watchlist sets. Zero selects
	// DefaultRunWindow.
	RunWindow time.Duration
	Paths     PathChecker
	Links     LinkChecker
	Issues    IssueChecker
}

// Verify returns a copy of digest that keeps only proposals whose every Orka
// path exists at the digest commit, whose every cited Orka issue exists and
// whose every source link resolves, and only watchlist suggestions whose link
// resolves. Removed proposals are counted under
// dropped by reason and never returned. The input digest must already be
// valid; Verify validates it again so a caller cannot verify unvalidated data
// by mistake, and validates the result before returning it.
func (v Verifier) Verify(ctx context.Context, digest *Digest, limits Limits) (*Digest, error) {
	if v.Paths == nil || v.Links == nil || v.Issues == nil {
		return nil, errors.New("verifier requires a path checker, a link checker and an issue checker")
	}
	if !commitPattern.MatchString(v.PinnedCommit) {
		return nil, errors.New("verifier requires a full lowercase hexadecimal pinned commit")
	}
	if v.RunStart.IsZero() {
		return nil, errors.New("verifier requires the trusted run start time")
	}
	if v.RunWindow < 0 {
		return nil, errors.New("verifier run window must not be negative")
	}
	if err := digest.Validate(limits); err != nil {
		return nil, fmt.Errorf("invalid digest: %w", err)
	}
	if digest.Run.OrkaCommit != v.PinnedCommit {
		return nil, fmt.Errorf("digest commit %s does not match pinned commit %s", digest.Run.OrkaCommit, v.PinnedCommit)
	}
	if err := v.checkWindow(digest.Run); err != nil {
		return nil, err
	}
	verified := *digest
	verified.Proposals = make([]Proposal, 0, len(digest.Proposals))
	verified.Dropped = append([]Dropped{}, digest.Dropped...)
	// Suggestions are advisory, so one whose link cannot be confirmed is
	// removed rather than failing the digest; a person never sees it.
	verified.WatchlistSuggestions = make([]WatchlistSuggestion, 0, len(digest.WatchlistSuggestions))
	for _, suggestion := range digest.WatchlistSuggestions {
		resolves, err := v.Links.LinkResolves(ctx, suggestion.URL)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, ctxErr
		}
		if err == nil && resolves {
			verified.WatchlistSuggestions = append(verified.WatchlistSuggestions, suggestion)
		}
	}
	for _, proposal := range digest.Proposals {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		reason, err := v.check(ctx, &proposal)
		if err != nil {
			return nil, err
		}
		if reason != "" {
			verified.addDropped(reason)
			continue
		}
		verified.Proposals = append(verified.Proposals, proposal)
	}
	if err := verified.Validate(limits); err != nil {
		return nil, fmt.Errorf("verified digest: %w", err)
	}
	return &verified, nil
}

// checkWindow compares the reported window with the trusted one.
func (v Verifier) checkWindow(run RunMetadata) error {
	window := v.RunWindow
	if window == 0 {
		window = DefaultRunWindow
	}
	// Validate already parsed both timestamps.
	start, _ := time.Parse(time.RFC3339, run.WindowStart)
	end, _ := time.Parse(time.RFC3339, run.WindowEnd)
	if !within(end, v.RunStart) || !within(start, v.RunStart.Add(-window)) {
		return fmt.Errorf("digest window %s to %s does not match the run window ending at %s",
			run.WindowStart, run.WindowEnd, v.RunStart.UTC().Format(time.RFC3339))
	}
	return nil
}

func within(got, want time.Time) bool {
	delta := got.Sub(want)
	return delta >= -RunWindowTolerance && delta <= RunWindowTolerance
}

func (v Verifier) check(ctx context.Context, proposal *Proposal) (DropReason, error) {
	for _, repoPath := range proposal.Evidence.OrkaPaths {
		exists, err := v.Paths.PathExists(ctx, proposal.Evidence.Commit, repoPath)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if err != nil {
			return DropCheckUnavailable, nil
		}
		if !exists {
			return DropUnverifiedPath, nil
		}
	}
	// Cited issues and the duplicate search's related issues are both shown
	// to reviewers, so both must exist.
	issues := make([]int, 0, len(proposal.Evidence.OrkaIssues)+len(proposal.Related))
	issues = append(issues, proposal.Evidence.OrkaIssues...)
	for _, related := range proposal.Related {
		issues = append(issues, related.Number)
	}
	for _, number := range issues {
		exists, err := v.Issues.IssueExists(ctx, number)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if err != nil {
			return DropCheckUnavailable, nil
		}
		if !exists {
			return DropUnverifiedIssue, nil
		}
	}
	for _, source := range proposal.Evidence.Sources {
		resolves, err := v.Links.LinkResolves(ctx, source.URL)
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if err != nil {
			return DropCheckUnavailable, nil
		}
		if !resolves {
			return DropUnverifiedLink, nil
		}
	}
	return "", nil
}

// GitCheckout checks paths against the object database of a local clone, so
// the answer is about the pinned commit, not whatever is checked out.
type GitCheckout struct {
	// Dir is the clone's working directory or bare repository.
	Dir string
	// Git is the git executable. Empty selects "git" from PATH.
	Git string
}

// PathExists runs `git cat-file -t <commit>:<path>`. A missing path is false;
// a missing commit, a non-repository or any other git failure is an error.
func (g GitCheckout) PathExists(ctx context.Context, commit, repoPath string) (bool, error) {
	if !commitPattern.MatchString(commit) {
		return false, errors.New("commit must be a full lowercase hexadecimal object name")
	}
	if err := ValidateOrkaPath(repoPath); err != nil {
		return false, err
	}
	if strings.TrimSpace(g.Dir) == "" {
		return false, errors.New("checkout directory is required")
	}
	if _, _, err := g.run(ctx, "cat-file", "-e", commit+"^{commit}"); err != nil {
		return false, fmt.Errorf("commit %s is not available in the checkout: %w", commit, err)
	}
	objectType, stderr, err := g.run(ctx, "cat-file", "-t", commit+":"+repoPath)
	if err != nil {
		// The commit exists, so only git's own "path not in commit" answer
		// means false; anything else is inconclusive.
		if strings.Contains(stderr, "does not exist in '") || strings.Contains(stderr, "exists on disk, but not in '") {
			return false, nil
		}
		return false, err
	}
	switch objectType {
	case "blob", "tree", "commit":
		return true, nil
	default:
		return false, fmt.Errorf("unexpected object type %q", objectType)
	}
}

func (g GitCheckout) run(ctx context.Context, args ...string) (stdout, stderr string, err error) {
	binary := g.Git
	if binary == "" {
		binary = "git"
	}
	full := append([]string{"-C", g.Dir, "--no-replace-objects"}, args...)
	command := exec.CommandContext(ctx, binary, full...)
	command.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "LC_ALL=C"}
	var out, errOut bytes.Buffer
	command.Stdout = &out
	command.Stderr = &errOut
	if runErr := command.Run(); runErr != nil {
		stderr = strings.TrimSpace(errOut.String())
		return "", stderr, fmt.Errorf("git %s: %w: %s", args[0], runErr, stderr)
	}
	return strings.TrimSpace(out.String()), "", nil
}
