package publisher

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/orka-agents/orka/internal/workspacedelta"
)

type treeEntry struct {
	kind workspacedelta.EntryKind
	mode string
	oid  string
}

//nolint:gocyclo // Prepare keeps the ordered durable security boundary explicit.
func (p *Publisher) Prepare(ctx context.Context, request PrepareRequest) (PreparedPublication, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err := validatePrepareRequest(request); err != nil {
		return PreparedPublication{}, err
	}
	requestDigest, err := prepareRequestDigest(request)
	if err != nil {
		return PreparedPublication{}, err
	}
	publicationDirectory, err := p.ensurePublicationDirectory(request.PublicationID, request.PublicationGeneration)
	if err != nil {
		return PreparedPublication{}, err
	}
	metadataPath := filepath.Join(publicationDirectory, preparedMetadataName)
	if _, statErr := os.Stat(metadataPath); statErr == nil {
		prepared, loadErr := p.loadPrepared(request.PublicationID)
		if loadErr != nil {
			return PreparedPublication{}, loadErr
		}
		if prepared.RequestDigest != requestDigest {
			return PreparedPublication{}, operationError(ErrIdempotencyConflict, "prepare publication", "publication ID already has a different prepared request digest", nil)
		}
		prepared.OperationID = request.OperationID
		return prepared, nil
	} else if !os.IsNotExist(statErr) {
		return PreparedPublication{}, statErr
	}
	delta, err := parseDeltaArtifact(request.DeltaArtifact, request.DeltaArtifactDigest, p.maxDeltaBytes)
	if err != nil {
		return PreparedPublication{}, err
	}
	delta, err = delta.withRelativeRoot(request.RelativeRoot)
	if err != nil {
		return PreparedPublication{}, err
	}
	if err := ctx.Err(); err != nil {
		return PreparedPublication{}, err
	}
	box, err := p.newSandbox("prepare")
	if err != nil {
		return PreparedPublication{}, err
	}
	defer box.Close() //nolint:errcheck
	repositoryPath := filepath.Join(box.root, "source.git")
	if err := p.initBare(ctx, box, repositoryPath, request.BaselineOID); err != nil {
		return PreparedPublication{}, fmt.Errorf("initialize fresh source clone: %w", err)
	}
	observed, err := p.observeSource(ctx, box, request.Source, request.SourceRef)
	if err != nil {
		return PreparedPublication{}, operationError(ErrSourceMoved, "observe source ref", "remote baseline could not be established", err)
	}
	if observed.Absent || observed.OID != request.BaselineOID {
		return PreparedPublication{}, operationError(ErrSourceMoved, "observe source ref", fmt.Sprintf("expected %s, observed %s", request.BaselineOID, formatRemoteRef(observed)), nil)
	}
	// Publication bundles are restored into an empty verifier, so they must
	// contain the baseline's complete parent history rather than a shallow
	// boundary that exists only in this temporary repository.
	if _, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "fetch", "--no-tags", "--no-recurse-submodules", "--", request.Source.URL, request.SourceRef+":refs/orka/source"); err != nil {
		return PreparedPublication{}, operationError(ErrSourceMoved, "fetch exact source baseline", "", err)
	}
	fetched, err := p.revParse(ctx, box, repositoryPath, "refs/orka/source^{commit}")
	if err != nil || fetched != request.BaselineOID {
		return PreparedPublication{}, operationError(ErrSourceMoved, "verify fetched source baseline", fmt.Sprintf("expected %s, fetched %s", request.BaselineOID, fetched), err)
	}
	if err := p.fsckCommit(ctx, box, repositoryPath, request.BaselineOID); err != nil {
		return PreparedPublication{}, operationError(ErrPreparedArtifactCorrupt, "verify fetched source objects", "strict fsck failed", err)
	}
	baselineTreeOID, err := p.revParse(ctx, box, repositoryPath, request.BaselineOID+"^{tree}")
	if err != nil {
		return PreparedPublication{}, fmt.Errorf("resolve baseline tree: %w", err)
	}
	baseline, err := p.readTree(ctx, box, repositoryPath, request.BaselineOID)
	if err != nil {
		return PreparedPublication{}, err
	}
	if err := validateDeltaAgainstBaseline(delta, baseline); err != nil {
		return PreparedPublication{}, err
	}
	indexPath := filepath.Join(box.root, "publication.index")
	indexEnv := map[string]string{"GIT_INDEX_FILE": indexPath}
	if _, err := p.runGit(ctx, box, box.root, indexEnv, nil, "--git-dir="+repositoryPath, "read-tree", request.BaselineOID+"^{tree}"); err != nil {
		return PreparedPublication{}, fmt.Errorf("seed clean publication index: %w", err)
	}
	if err := p.applyDelta(ctx, box, repositoryPath, indexEnv, delta, baseline); err != nil {
		return PreparedPublication{}, err
	}
	written, err := p.runGit(ctx, box, box.root, indexEnv, nil, "--git-dir="+repositoryPath, "write-tree")
	if err != nil {
		return PreparedPublication{}, fmt.Errorf("write deterministic publication tree: %w", err)
	}
	treeOID := strings.TrimSpace(written.stdout)
	if err := validateObjectID("prepared tree", treeOID); err != nil {
		return PreparedPublication{}, err
	}
	if treeOID == baselineTreeOID {
		return PreparedPublication{}, operationError(ErrUnsupportedDelta, "prepare publication", "delta does not change the Git tree (for example, it only changes empty directories)", nil)
	}
	commitEnv := map[string]string{
		"GIT_AUTHOR_NAME":     CommitAuthorName,
		"GIT_AUTHOR_EMAIL":    CommitAuthorEmail,
		"GIT_AUTHOR_DATE":     gitDate(request.CommitTimestamp),
		"GIT_COMMITTER_NAME":  CommitAuthorName,
		"GIT_COMMITTER_EMAIL": CommitAuthorEmail,
		"GIT_COMMITTER_DATE":  gitDate(request.CommitTimestamp),
	}
	committed, err := p.runGit(ctx, box, box.root, commitEnv, []byte(request.CommitMessage), "--git-dir="+repositoryPath, "commit-tree", treeOID, "-p", request.BaselineOID, "-F", "-")
	if err != nil {
		return PreparedPublication{}, fmt.Errorf("create deterministic Orka commit: %w", err)
	}
	commitOID := strings.TrimSpace(committed.stdout)
	if err := validateObjectID("prepared commit", commitOID); err != nil {
		return PreparedPublication{}, err
	}
	if err := p.verifyPreparedCommit(ctx, box, repositoryPath, commitOID, treeOID, request); err != nil {
		return PreparedPublication{}, err
	}
	bundleRef := "refs/orka/publications/" + strings.TrimPrefix(digestBytes([]byte(request.PublicationID)), DigestPrefix)
	if _, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "update-ref", bundleRef, commitOID, strings.Repeat("0", len(commitOID))); err != nil {
		return PreparedPublication{}, fmt.Errorf("create immutable bundle ref: %w", err)
	}
	temporaryBundle := filepath.Join(box.root, bundleFileName)
	if _, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "bundle", "create", "--version=3", temporaryBundle, bundleRef); err != nil {
		return PreparedPublication{}, fmt.Errorf("create durable Git bundle: %w", err)
	}
	bundleBytes, err := readBoundedFile(temporaryBundle, p.maxBundleBytes)
	if err != nil {
		return PreparedPublication{}, operationError(ErrPreparedArtifactCorrupt, "read prepared Git bundle", "", err)
	}
	if err := p.verifyBundle(ctx, box, temporaryBundle, commitOID, bundleRef); err != nil {
		return PreparedPublication{}, err
	}
	bundlePath := filepath.Join(publicationDirectory, bundleFileName)
	if err := writeDurableFile(bundlePath, bundleBytes, 0o600); err != nil {
		return PreparedPublication{}, fmt.Errorf("persist durable Git bundle: %w", err)
	}
	prepared := PreparedPublication{
		PublicationID: request.PublicationID, PublicationGeneration: request.PublicationGeneration,
		OperationID: request.OperationID, RequestDigest: requestDigest,
		Source: request.Source, SourceRef: request.SourceRef, Target: request.Target, TargetRef: request.TargetRef,
		BranchClaimGeneration: request.BranchClaimGeneration, BaselineOID: request.BaselineOID,
		RemoteBefore: request.RemoteBefore, DeltaArtifactDigest: request.DeltaArtifactDigest,
		RelativeRoot:   request.RelativeRoot,
		ManifestDigest: delta.manifestDigest, TreeOID: treeOID, CommitOID: commitOID,
		BundleDigest: digestBytes(bundleBytes), BundleSize: int64(len(bundleBytes)), BundleRef: bundleRef, BundlePath: bundlePath,
		CommitMessage: request.CommitMessage, CommitTimestamp: request.CommitTimestamp,
	}
	if err := writeCanonicalDurable(metadataPath, prepared); err != nil {
		return PreparedPublication{}, fmt.Errorf("persist prepared publication receipt: %w", err)
	}
	return prepared, nil
}

func validatePrepareRequest(request PrepareRequest) error {
	if err := validateIdentifier("publication ID", request.PublicationID); err != nil {
		return err
	}
	if request.PublicationGeneration < 1 || request.BranchClaimGeneration < 1 {
		return invalid("publication generation", "publication and branch claim generations must be at least 1")
	}
	if err := validateIdentifier("operation ID", request.OperationID); err != nil {
		return err
	}
	if err := validateRepository(request.Source); err != nil {
		return err
	}
	if err := validateRepository(request.Target); err != nil {
		return err
	}
	if err := validateSourceRef(request.SourceRef); err != nil {
		return err
	}
	if err := validateBranchRef(request.TargetRef); err != nil {
		return err
	}
	if err := validateObjectID("baseline", request.BaselineOID); err != nil {
		return err
	}
	if err := validateSourceRefBaseline(request.SourceRef, request.BaselineOID); err != nil {
		return err
	}
	if err := validateRemoteRef("remote before", request.RemoteBefore); err != nil {
		return err
	}
	if request.RemoteBefore.OID != "" && len(request.RemoteBefore.OID) != len(request.BaselineOID) {
		return invalid("remote before", "object format differs from source baseline")
	}
	if err := validateDigest("workspace delta digest", request.DeltaArtifactDigest); err != nil {
		return err
	}
	if len(request.DeltaArtifact) == 0 {
		return invalid("workspace delta", "artifact must not be empty")
	}
	if request.RelativeRoot != "" && request.RelativeRoot != "." {
		if len(request.RelativeRoot) > maxRelativeRootBytes {
			return invalid("workspace relative root", "must be at most %d bytes", maxRelativeRootBytes)
		}
		if err := validateWorkspacePath(request.RelativeRoot); err != nil {
			return invalid("workspace relative root", "must be a canonical relative path")
		}
	}
	if request.CommitMessage == "" || len(request.CommitMessage) > 4096 || !utf8.ValidString(request.CommitMessage) ||
		strings.ContainsRune(request.CommitMessage, 0) || strings.Contains(request.CommitMessage, "\r") || !strings.HasSuffix(request.CommitMessage, "\n") {
		return invalid("commit message", "must be non-empty UTF-8, at most 4096 bytes, contain no NUL/CR, and end with newline")
	}
	if request.CommitTimestamp.Location() != time.UTC || request.CommitTimestamp.Nanosecond() != 0 || request.CommitTimestamp.IsZero() {
		return invalid("commit timestamp", "must be a non-zero whole-second UTC timestamp")
	}
	return nil
}

func prepareRequestDigest(request PrepareRequest) (string, error) {
	return digestCanonical(struct {
		Domain                string     `json:"domain"`
		PublicationID         string     `json:"publicationId"`
		PublicationGeneration int64      `json:"publicationGeneration"`
		Source                Repository `json:"source"`
		SourceRef             string     `json:"sourceRef"`
		Target                Repository `json:"target"`
		TargetRef             string     `json:"targetRef"`
		BranchClaimGeneration int64      `json:"branchClaimGeneration"`
		BaselineOID           string     `json:"baselineOid"`
		RemoteBefore          RemoteRef  `json:"remoteBefore"`
		DeltaArtifactDigest   string     `json:"deltaArtifactDigest"`
		RelativeRoot          string     `json:"relativeRoot,omitempty"`
		CommitAuthorName      string     `json:"commitAuthorName"`
		CommitAuthorEmail     string     `json:"commitAuthorEmail"`
		CommitMessage         string     `json:"commitMessage"`
		CommitTimestamp       time.Time  `json:"commitTimestamp"`
	}{
		Domain: "orka.publisher.prepare.v1", PublicationID: request.PublicationID,
		PublicationGeneration: request.PublicationGeneration, Source: request.Source, SourceRef: request.SourceRef,
		Target: request.Target, TargetRef: request.TargetRef, BranchClaimGeneration: request.BranchClaimGeneration,
		BaselineOID: request.BaselineOID, RemoteBefore: request.RemoteBefore,
		DeltaArtifactDigest: request.DeltaArtifactDigest, RelativeRoot: request.RelativeRoot, CommitAuthorName: CommitAuthorName,
		CommitAuthorEmail: CommitAuthorEmail, CommitMessage: request.CommitMessage, CommitTimestamp: request.CommitTimestamp,
	})
}

func (p *Publisher) readTree(ctx context.Context, box *sandbox, repositoryPath, revision string) (map[string]treeEntry, error) {
	result, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "ls-tree", "-r", "-t", "-z", "--full-tree", revision+"^{tree}")
	if err != nil {
		return nil, fmt.Errorf("inspect trusted baseline tree: %w", err)
	}
	entries := make(map[string]treeEntry)
	for record := range bytes.SplitSeq([]byte(result.stdout), []byte{0}) {
		if len(record) == 0 {
			continue
		}
		parts := bytes.SplitN(record, []byte{'\t'}, 2)
		if len(parts) != 2 {
			return nil, operationError(ErrPreparedArtifactCorrupt, "inspect trusted baseline tree", "malformed ls-tree record", nil)
		}
		metadata := strings.Fields(string(parts[0]))
		entryPath := string(parts[1])
		if len(metadata) != 3 {
			return nil, operationError(ErrPreparedArtifactCorrupt, "inspect trusted baseline tree", "malformed ls-tree metadata", nil)
		}
		if err := validateWorkspacePath(entryPath); err != nil {
			return nil, operationError(ErrUnsafeDelta, "validate trusted baseline tree", entryPath, err)
		}
		var kind workspacedelta.EntryKind
		switch metadata[0] {
		case "040000":
			kind = workspacedelta.EntryDirectory
		case "100644", "100755":
			kind = workspacedelta.EntryFile
		case "120000":
			kind = workspacedelta.EntrySymlink
		case "160000":
			return nil, operationError(ErrUnsafeDelta, "validate trusted baseline tree", fmt.Sprintf("submodule gitlink %q is forbidden", entryPath), nil)
		default:
			return nil, operationError(ErrUnsafeDelta, "validate trusted baseline tree", fmt.Sprintf("unsupported mode %s for %q", metadata[0], entryPath), nil)
		}
		entries[entryPath] = treeEntry{kind: kind, mode: metadata[0], oid: metadata[2]}
	}
	return entries, nil
}

func validateDeltaAgainstBaseline(delta parsedDelta, baseline map[string]treeEntry) error {
	deletions := make(map[string]workspacedelta.Deletion, len(delta.deletions))
	for _, deletion := range delta.deletions {
		before, ok := baseline[deletion.Path]
		if !ok || before.kind != deletion.Kind {
			return operationError(ErrUnsafeDelta, "validate delta deletion", fmt.Sprintf("%q does not match trusted baseline", deletion.Path), nil)
		}
		deletions[deletion.Path] = deletion
	}
	for _, change := range delta.manifest.Entries {
		before, exists := baseline[change.Path]
		switch change.Operation {
		case workspacedelta.ChangeAdded:
			if _, deleted := deletions[change.Path]; deleted {
				return operationError(ErrUnsafeDelta, "validate delta addition", fmt.Sprintf("%q is both added and deleted", change.Path), nil)
			}
			if exists {
				return operationError(ErrUnsafeDelta, "validate delta addition", fmt.Sprintf("%q already exists in trusted baseline", change.Path), nil)
			}
		case workspacedelta.ChangeModified:
			if _, deleted := deletions[change.Path]; deleted {
				return operationError(ErrUnsafeDelta, "validate delta modification", fmt.Sprintf("%q is both modified and deleted", change.Path), nil)
			}
			if !exists || before.kind != change.Kind {
				return operationError(ErrUnsafeDelta, "validate delta modification", fmt.Sprintf("%q does not match trusted baseline kind", change.Path), nil)
			}
		case workspacedelta.ChangeReplaced:
			if !exists || before.kind == change.Kind {
				return operationError(ErrUnsafeDelta, "validate delta replacement", fmt.Sprintf("%q is not a type replacement", change.Path), nil)
			}
			if _, ok := deletions[change.Path]; !ok {
				return operationError(ErrUnsafeDelta, "validate delta replacement", fmt.Sprintf("%q lacks matching deletion metadata", change.Path), nil)
			}
		}
	}
	for deletionPath, deletion := range deletions {
		if deletion.Kind != workspacedelta.EntryDirectory {
			continue
		}
		prefix := deletionPath + "/"
		for entryPath, entry := range baseline {
			if entry.kind == workspacedelta.EntryDirectory || !strings.HasPrefix(entryPath, prefix) {
				continue
			}
			if _, ok := deletions[entryPath]; !ok {
				return operationError(ErrUnsafeDelta, "validate directory deletion", fmt.Sprintf("%q omits descendant %q", deletionPath, entryPath), nil)
			}
		}
	}
	return nil
}

func (p *Publisher) applyDelta(ctx context.Context, box *sandbox, repositoryPath string, indexEnv map[string]string, delta parsedDelta, baseline map[string]treeEntry) error {
	for _, deletion := range delta.deletions {
		if deletion.Kind == workspacedelta.EntryDirectory {
			continue
		}
		input := []byte("0 " + strings.Repeat("0", len(baseline[deletion.Path].oid)) + "\t" + deletion.Path + "\n")
		if _, err := p.runGit(ctx, box, box.root, indexEnv, input, "--git-dir="+repositoryPath, "update-index", "--index-info"); err != nil {
			return fmt.Errorf("remove %q from clean publication index: %w", deletion.Path, err)
		}
	}
	for _, change := range delta.manifest.Entries {
		var mode string
		var content []byte
		switch change.Kind {
		case workspacedelta.EntryDirectory:
			continue
		case workspacedelta.EntryFile:
			content = delta.files[change.Path]
			mode = "100644"
			if change.Mode&0o111 != 0 {
				mode = "100755"
			}
		case workspacedelta.EntrySymlink:
			content = []byte(change.Target)
			mode = "120000"
		}
		hashed, err := p.runGit(ctx, box, box.root, nil, content, "--git-dir="+repositoryPath, "hash-object", "-w", "--stdin")
		if err != nil {
			return fmt.Errorf("hash clean payload for %q: %w", change.Path, err)
		}
		oid := strings.TrimSpace(hashed.stdout)
		if err := validateObjectID("payload object", oid); err != nil {
			return err
		}
		input := []byte(mode + " " + oid + "\t" + change.Path + "\n")
		if _, err := p.runGit(ctx, box, box.root, indexEnv, input, "--git-dir="+repositoryPath, "update-index", "--index-info"); err != nil {
			return fmt.Errorf("stage clean payload for %q: %w", change.Path, err)
		}
	}
	return nil
}

func (p *Publisher) verifyPreparedCommit(ctx context.Context, box *sandbox, repositoryPath, commitOID, treeOID string, request PrepareRequest) error {
	resolvedTree, err := p.revParse(ctx, box, repositoryPath, commitOID+"^{tree}")
	if err != nil || resolvedTree != treeOID {
		return operationError(ErrPreparedArtifactCorrupt, "verify prepared commit tree", fmt.Sprintf("expected %s, resolved %s", treeOID, resolvedTree), err)
	}
	parents, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "show", "-s", "--format=%P", commitOID)
	if err != nil || strings.TrimSpace(parents.stdout) != request.BaselineOID {
		return operationError(ErrPreparedArtifactCorrupt, "verify prepared commit parent", fmt.Sprintf("expected %s, resolved %s", request.BaselineOID, strings.TrimSpace(parents.stdout)), err)
	}
	identity, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "show", "-s", "--format=%an%x00%ae%x00%cn%x00%ce%x00%at%x00%ct%x00%B", commitOID)
	if err != nil {
		return operationError(ErrPreparedArtifactCorrupt, "verify prepared commit identity", "", err)
	}
	parts := strings.SplitN(identity.stdout, "\x00", 7)
	if len(parts) != 7 || parts[0] != CommitAuthorName || parts[1] != CommitAuthorEmail || parts[2] != CommitAuthorName ||
		parts[3] != CommitAuthorEmail || parts[4] != strconv.FormatInt(request.CommitTimestamp.Unix(), 10) ||
		parts[5] != strconv.FormatInt(request.CommitTimestamp.Unix(), 10) || strings.TrimSuffix(parts[6], "\n") != request.CommitMessage {
		return operationError(ErrPreparedArtifactCorrupt, "verify prepared commit identity", fmt.Sprintf("commit metadata differs from immutable request: %#v", parts), nil)
	}
	return nil
}

func (p *Publisher) verifyBundle(ctx context.Context, box *sandbox, bundlePath, commitOID, bundleRef string) error {
	verifyRepository := filepath.Join(box.root, "verify.git")
	if err := p.initBare(ctx, box, verifyRepository, commitOID); err != nil {
		return err
	}
	if _, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+verifyRepository, "bundle", "verify", bundlePath); err != nil {
		return operationError(ErrPreparedArtifactCorrupt, "verify prepared Git bundle", "bundle verification failed", err)
	}
	if _, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+verifyRepository, "fetch", "--no-tags", "--no-recurse-submodules", "--", bundlePath, bundleRef+":refs/orka/verified"); err != nil {
		return operationError(ErrPreparedArtifactCorrupt, "verify prepared Git bundle", "bundle import failed", err)
	}
	resolved, err := p.revParse(ctx, box, verifyRepository, "refs/orka/verified^{commit}")
	if err != nil || resolved != commitOID {
		return operationError(ErrPreparedArtifactCorrupt, "verify prepared Git bundle", fmt.Sprintf("expected %s, resolved %s", commitOID, resolved), err)
	}
	return nil
}

func (p *Publisher) fsckCommit(ctx context.Context, box *sandbox, repositoryPath, commitOID string) error {
	_, err := p.runGit(ctx, box, box.root, nil, nil, "--git-dir="+repositoryPath, "fsck", "--strict", "--no-reflogs", "--no-progress", commitOID)
	return err
}

func gitDate(value time.Time) string {
	return "@" + strconv.FormatInt(value.Unix(), 10) + " +0000"
}

func formatRemoteRef(value RemoteRef) string {
	if value.Absent {
		return "Absent"
	}
	return value.OID
}
