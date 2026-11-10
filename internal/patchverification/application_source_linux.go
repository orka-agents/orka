//go:build linux

package patchverification

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const applicationTreeListingLimit = 40 << 20

// The application path batches immutable Git object reads. Reusing the legacy
// per-blob subprocess importer for a complete vendor tree would turn a bounded
// source operation into tens of thousands of process launches.
func PrepareApplicationSource(ctx context.Context, input ApplicationPreparationInput, root, launcher string) (*ApplicationPreparationReport, error) {
	if input.Manifest.Application == nil || root != ApplicationWorkPath || launcher != ApplicationWorkerPath ||
		input.TaskUID == "" || ValidatePreparationInput(input) != nil {
		return nil, ErrEvidence
	}
	if err := applicationEmptyDirectory(root); err != nil {
		return nil, err
	}
	sourceDirectory, err := applicationCreateSourceDirectory(ApplicationSourceVolumePath)
	if err != nil {
		return nil, err
	}
	format := sourceSHA1
	if len(input.Manifest.Sources.Original.Commit) == 64 {
		format = sourceSHA256
	}
	git, err := sourceNewGitLauncher(ctx, root, format, []string{launcher, "git"})
	if err != nil {
		return nil, err
	}
	git.operationTimeout = 2 * time.Minute
	defer func() { _ = os.RemoveAll(git.repository) }()
	_, err = git.run(ctx, nil, 4096,
		"-c", "protocol.https.allow=always", "-c", "http.followRedirects=false", "-c", "http.proxy=", "-c", "http.extraHeader=",
		"-c", "fetch.fsckObjects=true", "-c", "transfer.fsckObjects=true", "-c", "fetch.unpackLimit=1", "-c", "pack.threads=1",
		"fetch", "--quiet", "--no-tags", "--depth=1", "--no-write-fetch-head", "--no-recurse-submodules",
		"--", input.Manifest.Sources.Repository, input.Manifest.Sources.Original.Commit)
	if err != nil {
		return nil, err
	}
	commit, err := git.object(ctx, "commit", input.Manifest.Sources.Original.Commit, sourceMaxManifestBytes)
	if err != nil || applicationObjectDigest("commit", commit, len(input.Manifest.Sources.Original.Commit)) != input.Manifest.Sources.Original.Commit {
		return nil, ErrIntegrity
	}
	line, _, ok := bytes.Cut(commit, []byte{'\n'})
	tree, prefix := strings.CutPrefix(string(line), "tree ")
	if !ok || !prefix || tree != input.Manifest.Sources.Original.Tree {
		return nil, ErrBinding
	}
	inputDigest, err := PreparationInputDigest(input)
	if err != nil {
		return nil, err
	}
	report := &ApplicationPreparationReport{Version: ApplicationProtocolVersion, TaskUID: input.TaskUID,
		InputDigest: inputDigest, Manifest: input.Manifest, Provenance: map[string][]byte{}}
	original, err := applicationArchive(ctx, git, tree, path.Join(sourceDirectory, ApplicationOriginalSourceName))
	if err != nil {
		return nil, err
	}
	report.Manifest.Sources.Original.ArchiveDigest = original.ArchiveDigest
	report.Manifest.ApplicationSourceMetadata = &ApplicationSourceMetadata{Version: ApplicationProtocolVersion, Original: original}
	if input.Manifest.Action == VerifyPatch {
		patchedTree, err := applicationApplyPatch(ctx, git, tree, input.Patch)
		if err != nil {
			return nil, err
		}
		patched, err := applicationArchive(ctx, git, patchedTree, path.Join(sourceDirectory, ApplicationPatchedSourceName))
		if err != nil {
			return nil, err
		}
		diff, err := git.run(ctx, nil, MaxPreparationInputBytes, "diff", "--no-ext-diff", "--no-textconv",
			"--binary", "--full-index", "--no-renames", "--no-color", "--src-prefix=a/", "--dst-prefix=b/", tree, patchedTree, "--")
		if err != nil {
			return nil, err
		}
		report.Manifest.Sources.Patched = SourceIdentity{Tree: patchedTree, ArchiveDigest: patched.ArchiveDigest}
		report.Manifest.Sources.PatchDigest, report.Manifest.Sources.DiffDigest = Digest(input.Patch), Digest(diff)
		report.Manifest.ApplicationSourceMetadata.Patched = &patched
		report.Provenance[Digest(input.Patch)], report.Provenance[Digest(diff)] = input.Patch, diff
	}
	metadata, err := json.Marshal(report.Manifest.ApplicationSourceMetadata)
	if err != nil {
		return nil, ErrEvidence
	}
	report.Provenance[Digest(metadata)] = metadata
	if err := ValidateApplicationPreparationReport(input, *report); err != nil {
		return nil, err
	}
	return report, nil
}

func applicationEmptyDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrEvidence
	}
	entries, err := os.ReadDir(directory)
	if err != nil || len(entries) != 0 {
		return ErrEvidence
	}
	return nil
}

// ValidateApplicationVolumeRoot accepts only a fresh volume, allowing the real
// ext4 recovery directory but never following it or adopting other content.
func ValidateApplicationVolumeRoot(volume string) error {
	info, err := os.Lstat(volume)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return ErrEvidence
	}
	entries, err := os.ReadDir(volume)
	if err != nil {
		return ErrEvidence
	}
	// Fresh ext4 volumes may contain their filesystem recovery directory. It
	// is not source content; no other existing entry or archive tree is adopted.
	for _, entry := range entries {
		if entry.Name() != "lost+found" || !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return ErrEvidence
		}
	}
	return nil
}

func applicationCreateSourceDirectory(volume string) (string, error) {
	if err := ValidateApplicationVolumeRoot(volume); err != nil {
		return "", err
	}
	directory := path.Join(volume, ApplicationSourceDirectory)
	if err := os.Mkdir(directory, 0700); err != nil {
		return "", ErrEvidence
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0700 {
		return "", ErrEvidence
	}
	owner, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(owner.Uid) != os.Geteuid() || int(owner.Gid) != os.Getegid() {
		return "", ErrEvidence
	}
	if err := applicationEmptyDirectory(directory); err != nil {
		return "", err
	}
	return directory, nil
}

func applicationApplyPatch(ctx context.Context, git *sourceGit, tree string, patch []byte) (string, error) {
	if err := applicationPatchBounds(patch); err != nil {
		return "", err
	}
	for _, args := range [][]string{
		{"read-tree", tree}, {applicationGitApply, "--cached", "--check", "--whitespace=nowarn", "-"},
		{applicationGitApply, "--cached", "--whitespace=nowarn", "-"},
	} {
		var stdin []byte
		if args[0] == applicationGitApply {
			stdin = patch
		}
		if _, err := git.run(ctx, stdin, 0, args...); err != nil {
			return "", err
		}
	}
	content, err := git.run(ctx, nil, 128, "write-tree")
	result := strings.TrimSpace(string(content))
	if err != nil || !gitIdentityPattern.MatchString(result) || len(result) != len(tree) {
		return "", ErrIntegrity
	}
	return result, nil
}

func applicationPatchBounds(patch []byte) error {
	if len(patch) == 0 || len(patch) > MaxPreparationPatchBytes || !sourceCleanText(string(patch)) ||
		bytes.Contains(patch, []byte("GIT binary patch")) {
		return errors.New("application patches require bounded text diffs; binary expansion is unsupported")
	}
	sections := bytes.Count(patch, []byte("diff --git "))
	if sections == 0 || sections > 128 {
		return ErrLimit
	}
	return nil
}

func applicationTreeEntries(content []byte) ([]sourceTreeEntry, error) {
	if len(content) > applicationTreeListingLimit {
		return nil, ErrLimit
	}
	entries := make([]sourceTreeEntry, 0)
	seen := map[string]bool{}
	for len(content) != 0 {
		line, remaining, ok := bytes.Cut(content, []byte{0})
		if !ok || len(entries) >= MaxApplicationSourceEntries {
			return nil, ErrLimit
		}
		content = remaining
		header, name, ok := strings.Cut(string(line), "\t")
		fields := strings.Fields(header)
		if !ok || len(fields) != 3 || !sourceValidPath(name) || seen[name] ||
			!gitIdentityPattern.MatchString(fields[2]) {
			return nil, ErrEvidence
		}
		entry := sourceTreeEntry{name: name, kind: fields[1], identity: fields[2]}
		switch {
		case fields[0] == "040000" && entry.kind == sourceGitTree:
			entry.mode = 0755
		case fields[0] == "100644" && entry.kind == sourceGitBlob:
			entry.mode = 0644
		case fields[0] == "100755" && entry.kind == sourceGitBlob:
			entry.mode = 0755
		default:
			return nil, ErrEvidence
		}
		seen[name] = true
		entries = append(entries, entry)
	}
	return entries, nil
}

func applicationVerifyEntries(entries []sourceTreeEntry, root string) error {
	children := map[string][]sourceTreeEntry{".": {}}
	directories := map[string]string{".": root}
	for _, entry := range entries {
		children[path.Dir(entry.name)] = append(children[path.Dir(entry.name)], entry)
		if entry.kind == sourceGitTree {
			directories[entry.name] = entry.identity
		}
	}
	for parent := range children {
		if _, ok := directories[parent]; !ok {
			return ErrIntegrity
		}
	}
	for name, identity := range directories {
		list := children[name]
		slices.SortFunc(list, func(a, b sourceTreeEntry) int {
			first, second := path.Base(a.name), path.Base(b.name)
			if a.kind == sourceGitTree {
				first += "/"
			}
			if b.kind == sourceGitTree {
				second += "/"
			}
			return strings.Compare(first, second)
		})
		content := []byte{}
		for _, entry := range list {
			mode := "100644"
			if entry.kind == sourceGitTree {
				mode = "40000"
			} else if entry.mode == 0755 {
				mode = "100755"
			}
			digest, err := hex.DecodeString(entry.identity)
			if err != nil || len(entry.identity) != len(root) {
				return ErrIntegrity
			}
			content = append(content, []byte(mode+" "+path.Base(entry.name)+"\x00")...)
			content = append(content, digest...)
		}
		if applicationObjectDigest("tree", content, len(root)) != identity {
			return ErrIntegrity
		}
	}
	return nil
}

func applicationBatchBlobs(ctx context.Context, git *sourceGit, entries []sourceTreeEntry) (map[string][]byte, error) {
	ids := []string{}
	seen := map[string]bool{}
	for _, entry := range entries {
		if entry.kind == sourceGitBlob && !seen[entry.identity] {
			ids, seen[entry.identity] = append(ids, entry.identity), true
		}
	}
	if len(ids) == 0 {
		return nil, ErrEvidence
	}
	content, err := git.run(ctx, []byte(strings.Join(ids, "\n")+"\n"), MaxApplicationSourceContentBytes+8<<20, "cat-file", "--batch")
	if err != nil {
		return nil, err
	}
	blobs := make(map[string][]byte, len(ids))
	total := int64(0)
	for _, id := range ids {
		header, rest, ok := bytes.Cut(content, []byte{'\n'})
		fields := strings.Fields(string(header))
		if !ok || len(fields) != 3 || fields[0] != id || fields[1] != "blob" {
			return nil, ErrIntegrity
		}
		size, err := strconv.ParseInt(fields[2], 10, 64)
		if err != nil || size < 0 || size > MaxApplicationSourceContentBytes-total ||
			size >= int64(len(rest)) || rest[size] != '\n' {
			return nil, ErrLimit
		}
		blob := rest[:size]
		if applicationObjectDigest("blob", blob, len(id)) != id {
			return nil, ErrIntegrity
		}
		blobs[id], content, total = blob, rest[size+1:], total+size
	}
	if len(content) != 0 || ctx.Err() != nil {
		return nil, ErrIntegrity
	}
	return blobs, nil
}

type applicationArchiveWriter struct {
	writer io.Writer
	count  int64
}

func (writer *applicationArchiveWriter) Write(content []byte) (int, error) {
	if int64(len(content)) > MaxApplicationSourceBytes-writer.count {
		return 0, ErrLimit
	}
	n, err := writer.writer.Write(content)
	writer.count += int64(n)
	return n, err
}

func applicationArchive(ctx context.Context, git *sourceGit, tree, filename string) (ApplicationArchiveMetadata, error) {
	listing, err := git.run(ctx, nil, applicationTreeListingLimit, "ls-tree", "-r", "-t", "-z", "--full-tree", tree)
	if err != nil {
		return ApplicationArchiveMetadata{}, err
	}
	entries, err := applicationTreeEntries(listing)
	if err != nil || applicationVerifyEntries(entries, tree) != nil {
		return ApplicationArchiveMetadata{}, ErrIntegrity
	}
	blobs, err := applicationBatchBlobs(ctx, git, entries)
	if err != nil {
		return ApplicationArchiveMetadata{}, err
	}
	file, err := os.OpenFile(filename, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0444)
	if err != nil {
		return ApplicationArchiveMetadata{}, ErrIntegrity
	}
	defer func() { _ = file.Close() }()
	hasher := sha256.New()
	output := &applicationArchiveWriter{writer: io.MultiWriter(file, hasher)}
	writer := tar.NewWriter(output)
	metadata := ApplicationArchiveMetadata{Entries: len(entries)}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return ApplicationArchiveMetadata{}, err
		}
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		var content []byte
		if entry.kind == sourceGitBlob {
			content = blobs[entry.identity]
			if int64(len(content)) > MaxApplicationSourceContentBytes-metadata.ContentBytes {
				return ApplicationArchiveMetadata{}, ErrLimit
			}
			metadata.ContentBytes += int64(len(content))
			header.Size, header.Typeflag = int64(len(content)), tar.TypeReg
		}
		if err := writer.WriteHeader(header); err != nil {
			return ApplicationArchiveMetadata{}, err
		}
		if _, err := writer.Write(content); err != nil {
			return ApplicationArchiveMetadata{}, err
		}
	}
	if err := writer.Close(); err != nil {
		return ApplicationArchiveMetadata{}, err
	}
	if file.Sync() != nil {
		return ApplicationArchiveMetadata{}, ErrIntegrity
	}
	metadata.ArchiveBytes = output.count
	metadata.ArchiveDigest = fmt.Sprintf("sha256:%x", hasher.Sum(nil))
	if !validApplicationArchiveMetadata(metadata) {
		return ApplicationArchiveMetadata{}, ErrLimit
	}
	return metadata, nil
}
