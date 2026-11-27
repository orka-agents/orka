package publisher

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

type RebaseBranchRequest struct {
	OperationID       string     `json:"operationId"`
	Target            Repository `json:"target"`
	TargetRef         string     `json:"targetRef"`
	BaseRef           string     `json:"baseRef"`
	BaseOID           string     `json:"baseOid"`
	HeadOID           string     `json:"headOid"`
	CommitAuthorName  string     `json:"commitAuthorName,omitempty"`
	CommitAuthorEmail string     `json:"commitAuthorEmail,omitempty"`
	CommitTimestamp   time.Time  `json:"commitTimestamp,omitempty"`
	SignOff           bool       `json:"signOff,omitempty"`
	SigningKey        []byte     `json:"-"`
}

type RebaseBranchResult struct {
	OperationID  string `json:"operationId"`
	PreviousOID  string `json:"previousOid"`
	BaseOID      string `json:"baseOid"`
	NewHeadOID   string `json:"newHeadOid"`
	RebasedCount int    `json:"rebasedCount"`
	NoChange     bool   `json:"noChange,omitempty"`
}

const maxRebaseCommits = 64

//nolint:gocyclo // Clean-room rebase validates every boundary, commit, signature, and CAS step explicitly.
func (p *Publisher) RebaseBranch(ctx context.Context, request RebaseBranchRequest) (RebaseBranchResult, error) {
	if err := validateRebaseBranchRequest(request); err != nil {
		return RebaseBranchResult{}, err
	}
	committerName, committerEmail, err := ValidateCommitIdentity(request.CommitAuthorName, request.CommitAuthorEmail)
	if err != nil {
		return RebaseBranchResult{}, err
	}
	var signerKey []byte
	if len(request.SigningKey) > 0 {
		if _, _, err := ParseSSHSigningKey(request.SigningKey); err != nil {
			return RebaseBranchResult{}, err
		}
		signerKey = request.SigningKey
	}

	box, err := p.newSandbox("rebase")
	if err != nil {
		return RebaseBranchResult{}, err
	}
	defer box.Close() //nolint:errcheck

	repositoryPath := filepath.Join(box.root, "rebase.git")
	if err := p.initBare(ctx, box, repositoryPath, request.HeadOID); err != nil {
		return RebaseBranchResult{}, fmt.Errorf("initialize bare rebase repository: %w", err)
	}

	observed, err := p.observeRef(ctx, box, request.Target, request.TargetRef)
	if err != nil {
		return RebaseBranchResult{}, err
	}
	if observed.Absent || observed.OID != request.HeadOID {
		return RebaseBranchResult{}, operationError(ErrBranchMoved, "preflight rebase branch", fmt.Sprintf("expected %s, observed %s", request.HeadOID, formatRemoteRef(observed)), nil)
	}

	if _, err := p.runGit(ctx, box, box.root, nil, nil,
		"--git-dir="+repositoryPath, "fetch", "--no-tags", "--no-recurse-submodules", "--",
		request.Target.URL, request.BaseRef+":refs/orka/base", request.TargetRef+":refs/orka/head"); err != nil {
		return RebaseBranchResult{}, fmt.Errorf("fetch base and head for rebase: %w", err)
	}
	fetchedBase, err := p.revParse(ctx, box, repositoryPath, "refs/orka/base^{commit}")
	if err != nil || fetchedBase != request.BaseOID {
		return RebaseBranchResult{}, operationError(ErrSourceMoved, "verify rebase base commit", fmt.Sprintf("expected %s, fetched %s", request.BaseOID, fetchedBase), err)
	}
	fetchedHead, err := p.revParse(ctx, box, repositoryPath, "refs/orka/head^{commit}")
	if err != nil || fetchedHead != request.HeadOID {
		return RebaseBranchResult{}, operationError(ErrBranchMoved, "verify rebase head commit", fmt.Sprintf("expected %s, fetched %s", request.HeadOID, fetchedHead), err)
	}

	if ancestor, ancErr := p.isAncestor(ctx, box, repositoryPath, request.BaseOID, request.HeadOID); ancErr == nil && ancestor {
		return RebaseBranchResult{
			OperationID: request.OperationID,
			PreviousOID: request.HeadOID,
			BaseOID:     request.BaseOID,
			NewHeadOID:  request.HeadOID,
			NoChange:    true,
		}, nil
	}

	revList, err := p.runGit(ctx, box, box.root, nil, nil,
		"--git-dir="+repositoryPath, "rev-list", "--max-count="+strconv.Itoa(maxRebaseCommits+1), "--reverse", "--parents", request.BaseOID+".."+request.HeadOID)
	if err != nil {
		return RebaseBranchResult{}, fmt.Errorf("enumerate commits to rebase: %w", err)
	}
	lines := strings.Split(strings.TrimSpace(revList.stdout), "\n")
	if len(lines) == 1 && lines[0] == "" {
		lines = nil
	}
	if len(lines) > maxRebaseCommits {
		return RebaseBranchResult{}, operationError(ErrRebaseRefused, "rebase branch", fmt.Sprintf("PR has %d commits to rebase (max %d)", len(lines), maxRebaseCommits), nil)
	}

	commitTime := request.CommitTimestamp.UTC().Truncate(time.Second)
	if commitTime.IsZero() {
		commitTime = time.Now().UTC().Truncate(time.Second)
	}

	currentParent := request.BaseOID
	rebasedCount := 0
	for _, line := range lines {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return RebaseBranchResult{}, operationError(ErrRebaseRefused, "rebase branch", "automated rebase requires linear non-merge commits", nil)
		}
		origOID, origParent := fields[0], fields[1]
		if err := validateObjectID("rebase commit", origOID); err != nil {
			return RebaseBranchResult{}, err
		}

		merged, mergeErr := p.runGit(ctx, box, box.root, nil, nil,
			"--git-dir="+repositoryPath, "merge-tree", "--write-tree", "--merge-base="+origParent, currentParent, origOID)
		if mergeErr != nil {
			return RebaseBranchResult{}, operationError(ErrRebaseRefused, "rebase branch", fmt.Sprintf("merge conflict while rebasing commit %s", origOID[:min(12, len(origOID))]), mergeErr)
		}
		treeLine := strings.TrimSpace(strings.SplitN(merged.stdout, "\n", 2)[0])
		if err := validateObjectID("rebased tree", treeLine); err != nil {
			return RebaseBranchResult{}, operationError(ErrRebaseRefused, "rebase branch", "rebase produced invalid tree", err)
		}

		meta, err := p.runGit(ctx, box, box.root, nil, nil,
			"--git-dir="+repositoryPath, "show", "-s", "--format=%an%x00%ae%x00%ad%x00%B", "--date=raw", origOID)
		if err != nil {
			return RebaseBranchResult{}, fmt.Errorf("read commit %s metadata: %w", origOID, err)
		}
		parts := strings.SplitN(meta.stdout, "\x00", 4)
		if len(parts) != 4 {
			return RebaseBranchResult{}, operationError(ErrPreparedArtifactCorrupt, "rebase branch", "malformed commit metadata", nil)
		}
		origAuthorName, origAuthorEmail, origAuthorDate, origMessage := parts[0], parts[1], strings.TrimSpace(parts[2]), strings.TrimRight(parts[3], "\n")+"\n"
		if request.SignOff {
			signOffLine := fmt.Sprintf("Signed-off-by: %s <%s>", committerName, committerEmail)
			if !hasTrailingLine(origMessage, signOffLine) {
				origMessage += "\n" + signOffLine + "\n"
			}
		}

		var newOID string
		committerDate := gitCommitObjectDate(commitTime)
		if len(signerKey) > 0 {
			signer, _, signErr := ParseSSHSigningKey(signerKey)
			if signErr != nil {
				return RebaseBranchResult{}, signErr
			}
			unsignedPayload := buildRebasedUnsignedCommitObject(treeLine, currentParent, origAuthorName, origAuthorEmail, origAuthorDate, committerName, committerEmail, committerDate, origMessage)
			armoredSig, signErr := SignGitCommitPayload(signer, unsignedPayload)
			if signErr != nil {
				return RebaseBranchResult{}, fmt.Errorf("sign rebased commit: %w", signErr)
			}
			if verifyErr := VerifyGitCommitSignature(signer.PublicKey(), unsignedPayload, armoredSig); verifyErr != nil {
				return RebaseBranchResult{}, operationError(ErrPreparedArtifactCorrupt, "verify rebased commit signature", "", verifyErr)
			}
			signedPayload := buildRebasedSignedCommitObject(treeLine, currentParent, origAuthorName, origAuthorEmail, origAuthorDate, committerName, committerEmail, committerDate, armoredSig, origMessage)
			hashed, hashErr := p.runGit(ctx, box, box.root, nil, signedPayload, "--git-dir="+repositoryPath, "hash-object", "-t", "commit", "-w", "--stdin")
			if hashErr != nil {
				return RebaseBranchResult{}, fmt.Errorf("write signed rebased commit: %w", hashErr)
			}
			newOID = strings.TrimSpace(hashed.stdout)
		} else {
			env := map[string]string{
				"GIT_AUTHOR_NAME":     origAuthorName,
				"GIT_AUTHOR_EMAIL":    origAuthorEmail,
				"GIT_AUTHOR_DATE":     origAuthorDate,
				"GIT_COMMITTER_NAME":  committerName,
				"GIT_COMMITTER_EMAIL": committerEmail,
				"GIT_COMMITTER_DATE":  "@" + strconv.FormatInt(commitTime.Unix(), 10) + " +0000",
			}
			committed, commitErr := p.runGit(ctx, box, box.root, env, []byte(origMessage),
				"--git-dir="+repositoryPath, "commit-tree", treeLine, "-p", currentParent, "-F", "-")
			if commitErr != nil {
				return RebaseBranchResult{}, fmt.Errorf("create rebased commit: %w", commitErr)
			}
			newOID = strings.TrimSpace(committed.stdout)
		}
		if err := validateObjectID("rebased commit", newOID); err != nil {
			return RebaseBranchResult{}, err
		}
		currentParent = newOID
		rebasedCount++
	}

	if err := p.fsckCommit(ctx, box, repositoryPath, currentParent); err != nil {
		return RebaseBranchResult{}, operationError(ErrPreparedArtifactCorrupt, "verify rebased commits", "strict fsck failed", err)
	}
	if _, err := p.runGit(ctx, box, box.root, nil, nil,
		"--git-dir="+repositoryPath, "update-ref", "refs/orka/rebased", currentParent); err != nil {
		return RebaseBranchResult{}, fmt.Errorf("record rebased ref: %w", err)
	}

	lease := "--force-with-lease=" + request.TargetRef + ":" + request.HeadOID
	refspec := "refs/orka/rebased:" + request.TargetRef
	mutationCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.publishTimeout)
	defer cancel()
	if _, pushErr := p.runGit(mutationCtx, box, box.root, nil, nil,
		"--git-dir="+repositoryPath, "push", "--porcelain", "--no-verify", "--no-recurse-submodules",
		lease, "--", request.Target.URL, refspec); pushErr != nil {
		if postObserved, obsErr := p.observeRef(mutationCtx, box, request.Target, request.TargetRef); obsErr == nil && !postObserved.Absent && postObserved.OID == currentParent {
			return RebaseBranchResult{
				OperationID:  request.OperationID,
				PreviousOID:  request.HeadOID,
				BaseOID:      request.BaseOID,
				NewHeadOID:   currentParent,
				RebasedCount: rebasedCount,
			}, nil
		}
		return RebaseBranchResult{}, operationError(ErrPublicationUnknown, "push rebased branch", "remote push outcome is unknown or rejected", pushErr)
	}

	return RebaseBranchResult{
		OperationID:  request.OperationID,
		PreviousOID:  request.HeadOID,
		BaseOID:      request.BaseOID,
		NewHeadOID:   currentParent,
		RebasedCount: rebasedCount,
	}, nil
}

func hasTrailingLine(message, want string) bool {
	trimmed := strings.TrimRight(message, "\n")
	paragraphs := strings.Split(trimmed, "\n\n")
	if len(paragraphs) == 0 {
		return false
	}
	return slices.Contains(strings.Split(paragraphs[len(paragraphs)-1], "\n"), want)
}

func validateRebaseBranchRequest(request RebaseBranchRequest) error {
	if err := validateIdentifier("operation ID", request.OperationID); err != nil {
		return err
	}
	if err := validateRepository(request.Target); err != nil {
		return err
	}
	if err := validateBranchRef(request.TargetRef); err != nil {
		return err
	}
	if err := validateBranchRef(request.BaseRef); err != nil {
		return err
	}
	if err := validateObjectID("base commit", request.BaseOID); err != nil {
		return err
	}
	if err := validateObjectID("head commit", request.HeadOID); err != nil {
		return err
	}
	return nil
}

func buildRebasedUnsignedCommitObject(treeOID, parentOID, authorName, authorEmail, authorDate, committerName, committerEmail, committerDate, message string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", treeOID)
	fmt.Fprintf(&b, "parent %s\n", parentOID)
	fmt.Fprintf(&b, "author %s <%s> %s\n", authorName, authorEmail, authorDate)
	fmt.Fprintf(&b, "committer %s <%s> %s\n", committerName, committerEmail, committerDate)
	b.WriteByte('\n')
	b.WriteString(message)
	return []byte(b.String())
}

func buildRebasedSignedCommitObject(treeOID, parentOID, authorName, authorEmail, authorDate, committerName, committerEmail, committerDate, armoredSig, message string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "tree %s\n", treeOID)
	fmt.Fprintf(&b, "parent %s\n", parentOID)
	fmt.Fprintf(&b, "author %s <%s> %s\n", authorName, authorEmail, authorDate)
	fmt.Fprintf(&b, "committer %s <%s> %s\n", committerName, committerEmail, committerDate)
	b.WriteString("gpgsig ")
	sigLines := strings.Split(armoredSig, "\n")
	for i, line := range sigLines {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(line)
		b.WriteByte('\n')
	}
	b.WriteByte('\n')
	b.WriteString(message)
	return []byte(b.String())
}
