//go:build linux

package patchverification

import (
	"context"
	"os"
	"path/filepath"
)

// PrepareWorkerSource is called only by the isolated general worker. launcher
// installs process rlimits before exec; the Pod separately enforces cgroup and
// tmpfs limits. Neither candidate code nor a worktree checkout is executed.
func PrepareWorkerSource(ctx context.Context, input PreparationInput, root, launcher string) (*PreparationReport, error) {
	if input.Manifest.Application != nil || launcher != "/worker" || root != "/workspace" {
		return nil, PreparationFailure(PreparationInputDecode, ErrEvidence)
	}
	return prepareWorkerSourceWithLauncher(ctx, input, root, []string{launcher, "--validation-git"})
}

func prepareWorkerSourceWithLauncher(ctx context.Context, input PreparationInput, root string, launcher []string) (*PreparationReport, error) {
	if err := ValidatePreparationInput(input); err != nil {
		return nil, PreparationFailure(PreparationInputDecode, err)
	}
	if input.TaskUID == "" {
		return nil, PreparationFailure(PreparationInputDecode, ErrEvidence)
	}
	info, err := os.Lstat(root)
	if err != nil {
		return nil, PreparationFailure(PreparationWorkspace, err)
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return nil, PreparationFailure(PreparationWorkspace, ErrEvidence)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, PreparationFailure(PreparationWorkspace, err)
	}
	if len(entries) != 0 {
		return nil, PreparationFailure(PreparationWorkspace, ErrEvidence)
	}
	format := sourceSHA1
	if len(input.Manifest.Sources.Original.Commit) == 64 {
		format = sourceSHA256
	}
	git, err := sourceNewGitLauncher(ctx, root, format, launcher)
	if err != nil {
		return nil, PreparationFailure(PreparationGitInit, err)
	}
	defer func() { _ = os.RemoveAll(git.repository) }()
	fetchOptions := []string{
		"-c", "protocol.https.allow=always",
		"-c", "http.followRedirects=false", "-c", "http.proxy=", "-c", "http.extraHeader=",
		"-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true",
		"-c", "fetch.unpackLimit=1", "-c", "pack.threads=1",
		"fetch", "--quiet", "--no-tags", "--depth=1",
	}
	if len(input.Manifest.Sources.Paths) == 0 {
		args := append(append([]string{}, fetchOptions...), "--no-write-fetch-head", "--no-recurse-submodules",
			"--", input.Manifest.Sources.Repository, input.Manifest.Sources.Original.Commit)
		if _, err := git.run(ctx, nil, 4096, args...); err != nil {
			return nil, PreparationFailure(PreparationGitFetch, err)
		}
		tree, err := git.importCommit(ctx, filepath.Join(git.repository, "objects"), input.Manifest.Sources.Original.Commit)
		if err != nil {
			return nil, PreparationFailure(PreparationGitImport, err)
		}
		if tree != input.Manifest.Sources.Original.Tree {
			return nil, PreparationFailure(PreparationGitImport, ErrIntegrity)
		}
		return prepareWorkerArchives(ctx, git, input, tree)
	}
	args := append(append([]string{}, fetchOptions...), "--filter=blob:none", "--no-write-fetch-head", "--no-recurse-submodules",
		"--", input.Manifest.Sources.Repository, input.Manifest.Sources.Original.Commit)
	if _, err := git.run(ctx, nil, 4096, args...); err != nil {
		return nil, PreparationFailure(PreparationGitFetch, err)
	}
	objectsDir := filepath.Join(git.repository, "objects")
	rootTree, scopedEntries, err := git.resolveScopedCommitEntries(ctx, objectsDir, input.Manifest.Sources.Original.Commit, input.Manifest.Sources.Paths)
	if err != nil {
		return nil, PreparationFailure(PreparationGitImport, err)
	}
	if rootTree != input.Manifest.Sources.Original.Tree {
		return nil, PreparationFailure(PreparationGitImport, ErrIntegrity)
	}
	seenBlobs := make(map[string]bool, len(scopedEntries))
	blobArgs := append(append([]string{}, fetchOptions...), "--no-write-fetch-head", "--no-recurse-submodules",
		"--", input.Manifest.Sources.Repository)
	for _, entry := range scopedEntries {
		if !seenBlobs[entry.identity] {
			seenBlobs[entry.identity] = true
			blobArgs = append(blobArgs, entry.identity)
		}
	}
	if _, err := git.run(ctx, nil, 4096, blobArgs...); err != nil {
		return nil, PreparationFailure(PreparationGitFetch, err)
	}
	scopedTree, err := git.importScopedBlobs(ctx, objectsDir, scopedEntries, len(rootTree))
	if err != nil {
		return nil, PreparationFailure(PreparationGitImport, err)
	}
	return prepareWorkerArchives(ctx, git, input, scopedTree)
}

func prepareWorkerArchives(ctx context.Context, git *sourceGit, input PreparationInput, tree string) (*PreparationReport, error) {
	digest, err := PreparationInputDigest(input)
	if err != nil {
		return nil, PreparationFailure(PreparationInputDecode, err)
	}
	report := &PreparationReport{Version: PreparationProtocolVersion, TaskUID: input.TaskUID,
		InputDigest: digest, Manifest: input.Manifest, Provenance: make(map[string][]byte)}
	archiveLimit := MaxPublicSourceArchiveBytes
	original, err := git.archiveLimit(ctx, tree, archiveLimit)
	if err != nil {
		return nil, PreparationFailure(PreparationOriginalArchive, err)
	}
	report.Manifest.Sources.Original.ArchiveDigest = Digest(original)
	report.Provenance[Digest(original)] = original
	for _, file := range input.Manifest.Files {
		report.Provenance[file.Digest] = file.Content
	}
	if input.Manifest.Action == VerifyPatch {
		patchedTree, err := git.applyPatch(ctx, tree, input.Patch, len(original))
		if err != nil {
			return nil, PreparationFailure(PreparationPatchApply, err)
		}
		archive, err := git.archiveLimit(ctx, patchedTree, archiveLimit)
		if err != nil {
			return nil, PreparationFailure(PreparationPatchedArchive, err)
		}
		diff, err := git.run(ctx, nil, MaxPreparationInputBytes, "diff", "--no-ext-diff", "--no-textconv",
			"--binary", "--full-index", "--no-renames", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", tree, patchedTree, "--")
		if err != nil {
			return nil, PreparationFailure(PreparationDiff, err)
		}
		report.Manifest.Sources.Patched = SourceIdentity{Tree: patchedTree, ArchiveDigest: Digest(archive)}
		report.Manifest.Sources.PatchDigest, report.Manifest.Sources.DiffDigest = Digest(input.Patch), Digest(diff)
		report.Provenance[Digest(archive)], report.Provenance[Digest(input.Patch)], report.Provenance[Digest(diff)] = archive, input.Patch, diff
	}
	if err := ValidatePreparationReport(input, *report); err != nil {
		return nil, PreparationFailure(PreparationReportValidate, err)
	}
	return report, nil
}
