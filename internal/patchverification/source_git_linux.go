//go:build linux

package patchverification

import (
	"archive/tar"
	"bytes"
	"compress/zlib"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	sourceMaxTreeBytes   = 2 << 20
	sourceMaxObjectBytes = 128 << 20
	sourceGitTimeout     = 15 * time.Second
	sourceGitTree        = "tree"
	sourceGitBlob        = "blob"
	sourceSHA1           = "sha1"
	sourceSHA256         = "sha256"
)

type sourceBuffer struct {
	buffer   bytes.Buffer
	limit    int
	overflow bool
	cancel   context.CancelFunc
}

func (output *sourceBuffer) Len() int {
	return output.buffer.Len()
}

func (output *sourceBuffer) Bytes() []byte {
	return output.buffer.Bytes()
}

func (output *sourceBuffer) Write(content []byte) (int, error) {
	if len(content) > output.limit-output.Len() {
		output.overflow = true
		if output.cancel != nil {
			output.cancel()
		}
		return 0, errSourceLimit
	}
	return output.buffer.Write(content)
}

type sourceGit struct {
	binary           string
	launcher         []string
	root             string
	repository       string
	objects          string
	identities       map[string]string
	bytes            int
	operationTimeout time.Duration
}

func sourceNewGitLauncher(ctx context.Context, root, format string, launcher []string) (*sourceGit, error) {
	if len(launcher) == 0 || launcher[0] == "" {
		return nil, errSourceGit
	}
	gitBinary := ""
	for _, candidate := range []string{"/usr/bin/git", "/bin/git"} {
		if info, err := os.Stat(candidate); err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0111 != 0 {
			gitBinary = candidate
			break
		}
	}
	if gitBinary == "" || (format != sourceSHA1 && format != sourceSHA256) {
		return nil, errSourceGit
	}
	git := &sourceGit{binary: gitBinary, launcher: launcher, root: root, repository: filepath.Join(root, "git"), identities: make(map[string]string)}
	if _, err := git.run(ctx, nil, 4096, "init", "--quiet", "--bare", "--template=", "--object-format="+format, git.repository); err != nil {
		return nil, err
	}
	return git, nil
}

func (git *sourceGit) run(ctx context.Context, input []byte, limit int, arguments ...string) ([]byte, error) {
	if len(git.launcher) == 0 || git.launcher[0] == "" {
		return nil, errSourceGit
	}
	timeout := sourceGitTimeout
	if git.operationTimeout != 0 {
		timeout = git.operationTimeout
	}
	operation, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	options := make([]string, 0, 24+len(arguments))
	options = append(options,
		"-c", "core.hooksPath=/dev/null", "-c", "core.attributesFile=/dev/null",
		"-c", "credential.helper=", "-c", "protocol.allow=never", "-c", "protocol.file.allow=never",
		"-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "gc.auto=0",
		"-c", "maintenance.auto=false", "--git-dir="+git.repository,
	)
	launcher := git.launcher
	commandArgs := append(append(append([]string{}, launcher[1:]...), git.binary), options...)
	command := exec.CommandContext(operation, launcher[0], append(commandArgs, arguments...)...)
	command.Dir = git.root
	command.Env = []string{
		"PATH=/usr/bin:/bin", "HOME=" + git.root, "XDG_CONFIG_HOME=" + git.root,
		"LANG=C", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_ATTR_NOSYSTEM=1", "GIT_NO_REPLACE_OBJECTS=1",
		"GIT_NO_LAZY_FETCH=1", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0",
		"GIT_PROTOCOL_FROM_USER=0", "GIT_LITERAL_PATHSPECS=1", "GIT_INDEX_FILE=" + filepath.Join(git.repository, "index"),
		"GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false",
		"TMPDIR=" + git.root,
	}
	if len(git.launcher) > 1 && git.launcher[0] == ApplicationWorkerPath && git.launcher[1] == "git" {
		for _, name := range []string{"ORKA_APPLICATION_RUNTIME_PROFILE", "ORKA_APPLICATION_NODE_NAME", "ORKA_APPLICATION_TARGET_CLUSTER_UID"} {
			if value := os.Getenv(name); value != "" {
				command.Env = append(command.Env, name+"="+value)
			}
		}
	}
	if git.objects != "" {
		command.Env = append(command.Env, "GIT_OBJECT_DIRECTORY="+git.objects)
	}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
	output := &sourceBuffer{limit: limit, cancel: cancel}
	command.Stdin, command.Stdout, command.Stderr = bytes.NewReader(input), output, io.Discard
	err := command.Run()
	if output.overflow {
		return nil, errSourceLimit
	}
	if operation.Err() != nil {
		return nil, operation.Err()
	}
	if err != nil {
		return nil, preparationCommandError(err)
	}
	return output.Bytes(), nil
}

func (git *sourceGit) object(ctx context.Context, kind, identity string, limit int) ([]byte, error) {
	if !sourceValidCommit(identity) {
		return nil, errSourceIntegrity
	}
	content, err := git.run(ctx, nil, 64, "cat-file", "-s", identity)
	if err != nil {
		return nil, err
	}
	size, err := strconv.ParseInt(strings.TrimSuffix(string(content), "\n"), 10, 64)
	if err != nil || size < 0 || size > int64(limit) {
		return nil, errSourceLimit
	}
	content, err = git.run(ctx, nil, int(size), "cat-file", kind, identity)
	if err != nil {
		return nil, err
	}
	if int64(len(content)) != size {
		return nil, errSourceIntegrity
	}
	return content, nil
}

func (git *sourceGit) copyObject(ctx context.Context, reader *sourceGit, kind, identity string, limit int) error {
	if previous, found := git.identities[identity]; found {
		if previous != kind {
			return errSourceIntegrity
		}
		return nil
	}
	content, err := reader.object(ctx, kind, identity, min(limit, sourceMaxObjectBytes-git.bytes))
	if err != nil {
		return err
	}
	if kind == sourceGitTree {
		if err := sourceValidateRawTree(content, len(identity)/2); err != nil {
			return err
		}
	}
	actual, err := git.run(ctx, content, 128, "hash-object", "-w", "--no-filters", "-t", kind, "--stdin")
	if err != nil {
		return err
	}
	if string(actual) != identity+"\n" {
		return errSourceIntegrity
	}
	git.identities[identity] = kind
	git.bytes += len(content)
	return nil
}

func (git *sourceGit) copyPartialTreeObject(ctx context.Context, reader *sourceGit, identity string, limit int) error {
	if previous, found := git.identities[identity]; found {
		if previous != sourceGitTree {
			return errSourceIntegrity
		}
		return nil
	}
	content, err := reader.object(ctx, sourceGitTree, identity, min(limit, sourceMaxObjectBytes-git.bytes))
	if err != nil {
		return err
	}
	if err := sourceValidateRawTree(content, len(identity)/2); err != nil {
		return err
	}
	actual, err := git.run(ctx, content, 128, "hash-object", "-w", "--literally", "--no-filters", "-t", sourceGitTree, "--stdin")
	if err != nil {
		return err
	}
	if string(actual) != identity+"\n" {
		return errSourceIntegrity
	}
	git.identities[identity] = sourceGitTree
	git.bytes += len(content)
	return nil
}

func sourceValidateRawTree(content []byte, identityBytes int) error {
	previous := ""
	seen := make(map[string]bool)
	for len(content) != 0 {
		space, nul := bytes.IndexByte(content, ' '), bytes.IndexByte(content, 0)
		if space < 1 || nul <= space || len(content)-nul-1 < identityBytes || len(seen) >= sourceMaxTreeEntries {
			return errSourceIntegrity
		}
		mode, name := string(content[:space]), string(content[space+1:nul])
		if !sourceValidPath(name) || path.Base(name) != name || seen[name] {
			return errSourcePath
		}
		seen[name] = true
		order := name
		switch mode {
		case "40000":
			order += "/"
		case "100644", "100755":
		default:
			return errSourcePath
		}
		if previous != "" && previous >= order {
			return errSourceIntegrity
		}
		previous = order
		content = content[nul+1+identityBytes:]
	}
	return nil
}

type sourceTreeEntry struct {
	name     string
	kind     string
	identity string
	mode     int64
}

func (git *sourceGit) treeEntries(ctx context.Context, tree string) ([]sourceTreeEntry, error) {
	content, err := git.run(ctx, nil, sourceMaxTreeBytes, "ls-tree", "-z", "--full-tree", tree)
	if err != nil {
		return nil, err
	}
	entries := make([]sourceTreeEntry, 0)
	seen := make(map[string]bool)
	for len(content) != 0 {
		end := bytes.IndexByte(content, 0)
		if end < 0 || len(entries) >= sourceMaxTreeEntries {
			return nil, errSourceLimit
		}
		header, name, found := strings.Cut(string(content[:end]), "\t")
		fields := strings.Split(header, " ")
		if !found || len(fields) != 3 || !sourceValidPath(name) || seen[name] || !sourceValidCommit(fields[2]) || len(fields[2]) != len(tree) {
			return nil, errSourcePath
		}
		seen[name] = true
		entry := sourceTreeEntry{name: name, kind: fields[1], identity: fields[2]}
		switch {
		case fields[0] == "040000" && entry.kind == sourceGitTree:
			entry.mode = 0755
		case fields[0] == "100644" && entry.kind == sourceGitBlob:
			entry.mode = 0644
		case fields[0] == "100755" && entry.kind == sourceGitBlob:
			entry.mode = 0755
		default:
			return nil, errSourcePath
		}
		entries = append(entries, entry)
		content = content[end+1:]
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].name < entries[right].name })
	return entries, nil
}

func (git *sourceGit) walkTree(ctx context.Context, reader *sourceGit, tree string) ([]sourceTreeEntry, error) {
	pending := []sourceTreeEntry{{identity: tree}}
	entries := make([]sourceTreeEntry, 0)
	for len(pending) != 0 {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		current := pending[len(pending)-1]
		pending = pending[:len(pending)-1]
		if err := git.copyObject(ctx, reader, sourceGitTree, current.identity, sourceMaxTreeBytes); err != nil {
			return nil, err
		}
		children, err := git.treeEntries(ctx, current.identity)
		if err != nil {
			return nil, err
		}
		if len(children) > sourceMaxTreeEntries-len(entries) {
			return nil, errSourceLimit
		}
		for _, entry := range children {
			if current.name != "" {
				entry.name = current.name + "/" + entry.name
			}
			if !sourceValidPath(entry.name) {
				return nil, errSourcePath
			}
			entries = append(entries, entry)
			if entry.kind == sourceGitTree {
				pending = append(pending, entry)
			}
		}
	}
	sort.Slice(entries, func(left, right int) bool { return entries[left].name < entries[right].name })
	return entries, nil
}

func (git *sourceGit) importCommit(ctx context.Context, objectDirectory, commit string) (string, error) {
	if !sourceValidCommit(commit) {
		return "", errSourceInput
	}
	reader := *git
	reader.objects = objectDirectory
	kind, err := reader.run(ctx, nil, 64, "cat-file", "-t", commit)
	if err != nil {
		return "", err
	}
	if string(kind) != "commit\n" {
		return "", errSourceIntegrity
	}
	if err := git.copyObject(ctx, &reader, "commit", commit, sourceMaxManifestBytes); err != nil {
		return "", err
	}
	content, err := git.object(ctx, "commit", commit, sourceMaxManifestBytes)
	if err != nil {
		return "", err
	}
	header, _, found := bytes.Cut(content, []byte{'\n'})
	tree, hasPrefix := strings.CutPrefix(string(header), "tree ")
	if !found || !hasPrefix || !sourceValidCommit(tree) || len(tree) != len(commit) {
		return "", errSourceIntegrity
	}
	if err := git.copyObject(ctx, &reader, sourceGitTree, tree, sourceMaxTreeBytes); err != nil {
		return "", err
	}
	identity, err := git.run(ctx, nil, 128, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return "", err
	}
	if string(identity) != tree+"\n" {
		return "", errSourceIntegrity
	}
	entries, err := git.walkTree(ctx, &reader, tree)
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		limit := sourceMaxArchiveBytes
		if entry.kind == sourceGitTree {
			limit = sourceMaxTreeBytes
		}
		if err := git.copyObject(ctx, &reader, entry.kind, entry.identity, limit); err != nil {
			return "", err
		}
	}
	if _, err := git.run(ctx, nil, 0, "read-tree", tree); err != nil {
		return "", err
	}
	written, err := git.run(ctx, nil, 128, "write-tree")
	if err != nil {
		return "", err
	}
	if string(written) != tree+"\n" {
		return "", errSourceIntegrity
	}
	return tree, nil
}

func (git *sourceGit) resolveScopedCommitEntries(ctx context.Context, objectDirectory, commit string, paths []string) (string, []sourceTreeEntry, error) {
	if !sourceValidCommit(commit) || validateScopedSourcePaths(paths) != nil || len(paths) == 0 {
		return "", nil, errSourceInput
	}
	reader := *git
	reader.objects = objectDirectory
	kind, err := reader.run(ctx, nil, 64, "cat-file", "-t", commit)
	if err != nil {
		return "", nil, err
	}
	if string(kind) != "commit\n" {
		return "", nil, errSourceIntegrity
	}
	if err := git.copyObject(ctx, &reader, "commit", commit, sourceMaxManifestBytes); err != nil {
		return "", nil, err
	}
	content, err := git.object(ctx, "commit", commit, sourceMaxManifestBytes)
	if err != nil {
		return "", nil, err
	}
	header, _, found := bytes.Cut(content, []byte{'\n'})
	tree, hasPrefix := strings.CutPrefix(string(header), "tree ")
	if !found || !hasPrefix || !sourceValidCommit(tree) || len(tree) != len(commit) {
		return "", nil, errSourceIntegrity
	}
	if err := git.copyPartialTreeObject(ctx, &reader, tree, sourceMaxTreeBytes); err != nil {
		return "", nil, err
	}
	identity, err := git.run(ctx, nil, 128, "rev-parse", "--verify", commit+"^{tree}")
	if err != nil {
		return "", nil, err
	}
	if string(identity) != tree+"\n" {
		return "", nil, errSourceIntegrity
	}
	resolved := make([]sourceTreeEntry, 0, len(paths))
	for _, targetPath := range paths {
		currentTree := tree
		parts := strings.Split(targetPath, "/")
		for index, part := range parts {
			if err := git.copyPartialTreeObject(ctx, &reader, currentTree, sourceMaxTreeBytes); err != nil {
				return "", nil, err
			}
			children, err := git.treeEntries(ctx, currentTree)
			if err != nil {
				return "", nil, err
			}
			var matched *sourceTreeEntry
			for i := range children {
				if children[i].name == part {
					matched = &children[i]
					break
				}
			}
			if matched == nil {
				return "", nil, errSourcePath
			}
			if index < len(parts)-1 {
				if matched.kind != sourceGitTree {
					return "", nil, errSourcePath
				}
				currentTree = matched.identity
				continue
			}
			if matched.kind != sourceGitBlob {
				return "", nil, errSourcePath
			}
			resolved = append(resolved, sourceTreeEntry{
				name: targetPath, kind: sourceGitBlob, identity: matched.identity, mode: matched.mode,
			})
		}
	}
	return tree, resolved, nil
}

func (git *sourceGit) importScopedBlobs(ctx context.Context, objectDirectory string, entries []sourceTreeEntry, identityLen int) (string, error) {
	if len(entries) == 0 {
		return "", errSourceInput
	}
	reader := *git
	reader.objects = objectDirectory
	if _, err := git.run(ctx, nil, 0, "read-tree", "--empty"); err != nil {
		return "", err
	}
	for _, entry := range entries {
		if err := git.copyObject(ctx, &reader, sourceGitBlob, entry.identity, sourceMaxArchiveBytes); err != nil {
			return "", err
		}
		mode := "100644"
		if entry.mode == 0755 {
			mode = "100755"
		}
		cacheInfo := mode + "," + entry.identity + "," + entry.name
		if _, err := git.run(ctx, nil, 0, "update-index", "--add", "--cacheinfo", cacheInfo); err != nil {
			return "", err
		}
	}
	written, err := git.run(ctx, nil, 128, "write-tree")
	if err != nil {
		return "", err
	}
	scopedTree := strings.TrimSuffix(string(written), "\n")
	if !sourceValidCommit(scopedTree) || len(scopedTree) != identityLen {
		return "", errSourceIntegrity
	}
	return scopedTree, nil
}

func (git *sourceGit) archive(ctx context.Context, tree string) ([]byte, error) {
	return git.archiveLimit(ctx, tree, sourceMaxArchiveBytes)
}

func (git *sourceGit) archiveLimit(ctx context.Context, tree string, limit int) ([]byte, error) {
	entries, err := git.walkTree(ctx, git, tree)
	if err != nil {
		return nil, err
	}
	output := &sourceBuffer{limit: limit}
	writer := tar.NewWriter(output)
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		header := &tar.Header{Name: entry.name, Mode: entry.mode, Typeflag: tar.TypeDir, ModTime: time.Unix(0, 0), Format: tar.FormatPAX}
		var content []byte
		if entry.kind == sourceGitBlob {
			content, err = git.object(ctx, sourceGitBlob, entry.identity, limit-output.Len())
			if err != nil {
				return nil, err
			}
			header.Size, header.Typeflag = int64(len(content)), tar.TypeReg
		}
		if err := writer.WriteHeader(header); err != nil {
			return nil, errSourceLimit
		}
		if _, err := writer.Write(content); err != nil {
			return nil, errSourceLimit
		}
	}
	if err := writer.Close(); err != nil {
		return nil, errSourceLimit
	}
	return output.Bytes(), nil
}

func (git *sourceGit) applyPatch(ctx context.Context, originalTree string, patch []byte, originalArchiveBytes int) (string, error) {
	if err := sourcePatchBounds(ctx, patch, originalArchiveBytes); err != nil {
		return "", PreparationFailure(PreparationPatchValidate, err)
	}
	if _, err := git.run(ctx, nil, 0, "read-tree", originalTree); err != nil {
		return "", err
	}
	if _, err := git.run(ctx, patch, 0, "apply", "--cached", "--check", "--whitespace=nowarn", "-"); err != nil {
		return "", PreparationFailure(PreparationPatchValidate, err)
	}
	if _, err := git.run(ctx, patch, 0, "apply", "--cached", "--whitespace=nowarn", "-"); err != nil {
		return "", err
	}
	content, err := git.run(ctx, nil, 128, "write-tree")
	if err != nil {
		return "", err
	}
	tree := strings.TrimSuffix(string(content), "\n")
	if !sourceValidCommit(tree) || len(tree) != len(originalTree) {
		return "", errSourceIntegrity
	}
	return tree, nil
}

func sourcePatchBounds(ctx context.Context, patch []byte, originalArchiveBytes int) error {
	if len(patch) > sourceMaxObjectBytes || originalArchiveBytes < 0 || originalArchiveBytes > sourceMaxArchiveBytes {
		return errSourceLimit
	}
	imageBytes, sections := originalArchiveBytes+len(patch), 0
	if imageBytes > sourceMaxObjectBytes {
		return errSourceLimit
	}
	for len(patch) != 0 {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, remaining, _ := bytes.Cut(patch, []byte{'\n'})
		line, patch = bytes.TrimSuffix(line, []byte{'\r'}), remaining
		if bytes.HasPrefix(line, []byte("diff ")) || bytes.HasPrefix(line, []byte("--- ")) {
			sections++
		}
		if !bytes.Equal(line, []byte("GIT binary patch")) {
			continue
		}
		hunks := 0
		for len(patch) != 0 {
			header, remaining, _ := bytes.Cut(patch, []byte{'\n'})
			header = bytes.TrimSuffix(header, []byte{'\r'})
			kind, length, found := strings.Cut(string(header), " ")
			if !found || (kind != "literal" && kind != "delta") {
				break
			}
			size, err := strconv.ParseUint(length, 10, 32)
			if err != nil || size > sourceMaxArchiveBytes || hunks >= 2 {
				return errSourceLimit
			}
			expanded, remaining, err := sourceExpandPatchHunk(ctx, remaining, int(size))
			if err != nil {
				return err
			}
			patch = remaining
			if len(expanded) > sourceMaxObjectBytes-imageBytes {
				return errSourceLimit
			}
			imageBytes += len(expanded)
			if kind == "delta" {
				_, delta, err := sourceDeltaSize(expanded)
				if err != nil {
					return err
				}
				target, _, err := sourceDeltaSize(delta)
				if err != nil {
					return err
				}
				if target > sourceMaxObjectBytes-imageBytes {
					return errSourceLimit
				}
				imageBytes += target
			}
			hunks++
		}
		if hunks == 0 {
			return errSourceIntegrity
		}
	}
	if sections > 2*sourceMaxTreeEntries || max(sections, 1) > sourceMaxObjectBytes/max(imageBytes, 1) {
		return errSourceLimit
	}
	return nil
}

func sourceExpandPatchHunk(ctx context.Context, patch []byte, size int) ([]byte, []byte, error) {
	compressed := &sourceBuffer{limit: sourceMaxArchiveBytes}
	for len(patch) != 0 {
		line, remaining, _ := bytes.Cut(patch, []byte{'\n'})
		line, patch = bytes.TrimSuffix(line, []byte{'\r'}), remaining
		if len(line) == 0 {
			break
		}
		decoded, err := sourceDecodeGit85(line)
		if err != nil {
			return nil, nil, err
		}
		if _, err := compressed.Write(decoded); err != nil {
			return nil, nil, errSourceLimit
		}
	}
	stream, err := zlib.NewReader(bytes.NewReader(compressed.Bytes()))
	if err != nil {
		return nil, nil, errSourceIntegrity
	}
	expanded, readErr := io.ReadAll(io.LimitReader(sourceContextReader{ctx: ctx, reader: stream}, int64(size)+1))
	closeErr := stream.Close()
	if ctx.Err() != nil {
		return nil, nil, ctx.Err()
	}
	if readErr != nil || closeErr != nil || len(expanded) != size {
		return nil, nil, errSourceIntegrity
	}
	return expanded, patch, nil
}

func sourceDecodeGit85(line []byte) ([]byte, error) {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz!#$%&()*+-;<=>?@^_`{|}~"
	if len(line) == 0 {
		return nil, errSourceIntegrity
	}
	size := 0
	switch {
	case line[0] >= 'A' && line[0] <= 'Z':
		size = int(line[0]-'A') + 1
	case line[0] >= 'a' && line[0] <= 'z':
		size = int(line[0]-'a') + 27
	default:
		return nil, errSourceIntegrity
	}
	groups := (size + 3) / 4
	if len(line) != 1+groups*5 {
		return nil, errSourceIntegrity
	}
	decoded := make([]byte, groups*4)
	for group := range groups {
		value := uint64(0)
		for _, character := range line[1+group*5 : 1+(group+1)*5] {
			digit := strings.IndexByte(alphabet, character)
			if digit < 0 {
				return nil, errSourceIntegrity
			}
			value = value*85 + uint64(digit)
		}
		if value > 1<<32-1 {
			return nil, errSourceIntegrity
		}
		binary.BigEndian.PutUint32(decoded[group*4:], uint32(value))
	}
	return decoded[:size], nil
}

func sourceDeltaSize(content []byte) (int, []byte, error) {
	size := uint64(0)
	for shift := uint(0); len(content) != 0 && shift < 64; shift += 7 {
		value := content[0]
		content = content[1:]
		size |= uint64(value&0x7f) << shift
		if size > sourceMaxArchiveBytes || shift >= 63 {
			return 0, nil, errSourceLimit
		}
		if value&0x80 == 0 {
			return int(size), content, nil
		}
	}
	return 0, nil, errSourceIntegrity
}
