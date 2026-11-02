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

// Verifier re-checks proposal evidence before anything is published. Every
// field is required; there is no way to skip a check.
type Verifier struct {
	// PinnedCommit is the trusted commit of the scout's read-only checkout,
	// such as the scout Task's status.delivery.startingSHA. A digest that
	// claims a different commit is rejected as a whole.
	PinnedCommit string
	Paths        PathChecker
	Links        LinkChecker
}

// Verify returns a copy of digest that keeps only proposals whose every Orka
// path exists at the digest commit and whose every source link resolves.
// Removed proposals are counted under dropped by reason and never returned.
// The input digest must already be valid; Verify validates it again so a
// caller cannot verify unvalidated data by mistake.
func (v Verifier) Verify(ctx context.Context, digest *Digest, limits Limits) (*Digest, error) {
	if v.Paths == nil || v.Links == nil {
		return nil, errors.New("verifier requires both a path checker and a link checker")
	}
	if !commitPattern.MatchString(v.PinnedCommit) {
		return nil, errors.New("verifier requires a full lowercase hexadecimal pinned commit")
	}
	if err := digest.Validate(limits); err != nil {
		return nil, fmt.Errorf("invalid digest: %w", err)
	}
	if digest.Run.OrkaCommit != v.PinnedCommit {
		return nil, fmt.Errorf("digest commit %s does not match pinned commit %s", digest.Run.OrkaCommit, v.PinnedCommit)
	}
	verified := *digest
	verified.Proposals = make([]Proposal, 0, len(digest.Proposals))
	verified.Dropped = append([]Dropped{}, digest.Dropped...)
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
	return &verified, nil
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
