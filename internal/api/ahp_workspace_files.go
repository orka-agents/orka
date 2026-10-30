/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/orka-agents/orka/internal/ahphost"
)

const (
	// ahpFileReadMax bounds a file read from a workspace.
	ahpFileReadMax = 1 << 20
	// ahpListMax bounds a directory listing, and ahpWatchFilesMax the files a
	// watch follows.
	ahpListMax       = 5000
	ahpWatchFilesMax = 5000
	ahpWatchInterval = 5 * time.Second
	ahpEntryFile     = "file"
	ahpEntryDir      = "directory"
)

// errAHPOutputTooLarge reports output past a read's bound.
var errAHPOutputTooLarge = errors.New("the output is too large")

// ahpWorkspacePath turns a file URI into a clean path inside the workspace.
func ahpWorkspacePath(uri string) (string, bool) {
	raw, ok := ahpURIPath(uri)
	if !ok {
		return "", false
	}
	clean := path.Clean(raw)
	if clean != ahpWorkspaceRoot && !strings.HasPrefix(clean, ahpWorkspaceRoot+"/") {
		return "", false
	}
	return clean, true
}

// output runs a program in the workspace and returns its standard output, up
// to limit bytes.
func (w *ahpWorkspaces) output(ctx context.Context, pod *corev1.Pod, limit int, argv ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, ahpWorkspaceExecTimeout)
	defer cancel()
	out := &ahpLimitedBuffer{limit: limit}
	if err := w.streamOutput(ctx, pod, argv, out); err != nil {
		if out.over {
			return nil, errAHPOutputTooLarge
		}
		return nil, err
	}
	if out.over {
		return nil, errAHPOutputTooLarge
	}
	return out.buf.Bytes(), nil
}

// ahpInWorkspace resolves $1 with every symlink followed into $p, and stops
// the script as not found unless $p is still under the workspace. A path that
// is inside the workspace by its text alone can still be a link out of it,
// such as one a cloned repository carries to the workspace's git token.
var ahpInWorkspace = ahpInWorkspaceUnder(ahpWorkspaceRoot)

func ahpInWorkspaceUnder(root string) string {
	return `p=$(readlink -f "$1") || exit 3; case "$p" in ` + root + `|` + root + `/*) ;; *) exit 3 ;; esac; `
}

// readFile reads a regular file from the workspace.
func (w *ahpWorkspaces) readFile(ctx context.Context, pod *corev1.Pod, file string) ([]byte, error) {
	return w.output(ctx, pod, ahpFileReadMax, ahpShell, "-c", ahpInWorkspace+`[ -f "$p" ] || exit 3; exec cat -- "$p"`, "sh", file)
}

// list lists a directory in the workspace, directories marked with a slash.
func (w *ahpWorkspaces) list(ctx context.Context, pod *corev1.Pod, dir string) ([]ahptypes.DirectoryEntry, error) {
	raw, err := w.output(ctx, pod, 1<<20, ahpShell, "-c", ahpInWorkspace+`[ -d "$p" ] || exit 3; cd "$p" && ls -1Ap`, "sh", dir)
	if err != nil {
		return nil, err
	}
	var entries []ahptypes.DirectoryEntry
	for line := range strings.SplitSeq(strings.TrimRight(string(raw), "\n"), "\n") {
		if line == "" || len(entries) >= ahpListMax {
			continue
		}
		if name, ok := strings.CutSuffix(line, "/"); ok {
			entries = append(entries, ahptypes.DirectoryEntry{Name: name, Type: ahpEntryDir})
		} else {
			entries = append(entries, ahptypes.DirectoryEntry{Name: line, Type: ahpEntryFile})
		}
	}
	return entries, nil
}

// stamps reports each file's modification time and size under root, below
// .git directories excluded, at most ahpWatchFilesMax of them.
func (w *ahpWorkspaces) stamps(ctx context.Context, pod *corev1.Pod, root string, recursive bool) (map[string]string, error) {
	depth := "1"
	if recursive {
		depth = "64"
	}
	raw, err := w.output(ctx, pod, 4<<20, ahpShell, "-c",
		ahpInWorkspace+`[ -e "$p" ] || exit 3; find "$1" -maxdepth "$2" -name .git -prune -o -type f -print | head -n "$3" | while IFS= read -r f; do stat -c '%s/%y/%n' -- "$f"; done`,
		"sh", root, depth, strconv.Itoa(ahpWatchFilesMax))
	if err != nil {
		return nil, err
	}
	return ahpParseStamps(raw), nil
}

// ahpParseStamps reads stat's "size/mtime/name" lines into each file's stamp.
// The modification time is stat's %y, with fractional seconds where the
// filesystem keeps them, so a file rewritten at the same size within a
// second still changes its stamp. Neither the size nor the time holds a
// slash, and the name, last, keeps its own.
func ahpParseStamps(raw []byte) map[string]string {
	stamps := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 64<<10), 64<<10)
	for scanner.Scan() {
		size, rest, ok := strings.Cut(scanner.Text(), "/")
		if !ok {
			continue
		}
		mtime, name, ok := strings.Cut(rest, "/")
		if !ok || name == "" {
			continue
		}
		stamps[name] = size + " " + mtime
	}
	return stamps
}

// ahpLimitedBuffer keeps at most limit bytes and notes when more arrived.
type ahpLimitedBuffer struct {
	buf   bytes.Buffer
	limit int
	over  bool
}

func (b *ahpLimitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.buf.Len(); len(p) > room {
		b.buf.Write(p[:max(room, 0)])
		b.over = true
		return 0, errAHPOutputTooLarge
	}
	b.buf.Write(p)
	return len(p), nil
}

// workspaceFor returns the user's running workspace when they may use it.
// Reads never start a workspace.
func (b *ahpBackend) workspaceFor(ctx context.Context, principal ahphost.Principal) (*corev1.Pod, bool) {
	if !b.workspaces.enabled() {
		return nil, false
	}
	return b.workspaces.running(ctx, principal)
}

// readWorkspaceFile serves a file from the user's running workspace. running
// is false when no workspace runs or the URI names nothing in one.
func (b *ahpBackend) readWorkspaceFile(ctx context.Context, principal ahphost.Principal, uri string) (ahptypes.ResourceReadResult, bool, error) {
	file, ok := ahpWorkspacePath(uri)
	if !ok {
		return ahptypes.ResourceReadResult{}, false, nil
	}
	pod, ok := b.workspaceFor(ctx, principal)
	if !ok {
		return ahptypes.ResourceReadResult{}, false, nil
	}
	data, err := b.workspaces.readFile(ctx, pod, file)
	if errors.Is(err, errAHPOutputTooLarge) {
		return ahptypes.ResourceReadResult{}, true, &ahphost.Rejection{Reason: fmt.Sprintf("the file is larger than %d bytes", ahpFileReadMax)}
	}
	if err != nil {
		return ahptypes.ResourceReadResult{}, true, ahphost.ErrResourceNotFound
	}
	if utf8.Valid(data) {
		return ahptypes.ResourceReadResult{Data: string(data), Encoding: ahptypes.ContentEncodingUtf8}, true, nil
	}
	return ahptypes.ResourceReadResult{Data: base64.StdEncoding.EncodeToString(data), Encoding: ahptypes.ContentEncodingBase64}, true, nil
}

// ListResource lists a directory in the user's workspace.
func (b *ahpBackend) ListResource(ctx context.Context, principal ahphost.Principal, uri string) ([]ahptypes.DirectoryEntry, error) {
	dir, ok := ahpWorkspacePath(uri)
	if !ok {
		return nil, ahphost.ErrResourceNotFound
	}
	pod, ok := b.workspaceFor(ctx, principal)
	if !ok {
		return nil, ahphost.ErrResourceNotFound
	}
	entries, err := b.workspaces.list(ctx, pod, dir)
	if err != nil {
		return nil, ahphost.ErrResourceNotFound
	}
	return entries, nil
}

// WatchResources polls the user's workspace for changes under a file or
// directory, filtered by the watch's include and exclude patterns.
func (b *ahpBackend) WatchResources(ctx context.Context, principal ahphost.Principal, params ahptypes.CreateResourceWatchParams, changed func([]ahptypes.ResourceChange) error) error {
	root, ok := ahpWorkspacePath(params.Uri)
	if !ok || !b.workspaces.enabled() {
		return ahphost.ErrResourceNotFound
	}
	includes, excludes := ahpWatchPatterns(params.Includes), ahpWatchPatterns(params.Excludes)
	// Every poll matches each file against every pattern, so they are bounded.
	patterns := slices.Concat(includes, excludes)
	if len(patterns) > ahpWatchPatternsMax || slices.ContainsFunc(patterns, func(p string) bool { return len(p) > ahpWatchPatternMax }) {
		return &ahphost.Rejection{Reason: fmt.Sprintf("a watch takes at most %d patterns of up to %d bytes", ahpWatchPatternsMax, ahpWatchPatternMax)}
	}
	recursive := params.Recursive != nil && *params.Recursive
	matches := func(file string) bool {
		rel := strings.TrimPrefix(strings.TrimPrefix(file, root), "/")
		if len(includes) > 0 && !ahpMatchAny(includes, rel) {
			return false
		}
		return !ahpMatchAny(excludes, rel)
	}
	poll := func() map[string]string {
		pod, ok := b.workspaces.running(ctx, principal)
		if !ok {
			return nil
		}
		stamps, err := b.workspaces.stamps(ctx, pod, root, recursive)
		if err != nil {
			return nil
		}
		for file := range stamps {
			if !matches(file) {
				delete(stamps, file)
			}
		}
		return stamps
	}
	last := poll()
	_ = changed(nil)
	ticker := time.NewTicker(ahpWatchInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
		current := poll()
		if current == nil {
			// The workspace is not running; nothing can change.
			continue
		}
		var changes []ahptypes.ResourceChange
		for file, stamp := range current {
			previous, existed := last[file]
			switch {
			case !existed:
				changes = append(changes, ahptypes.ResourceChange{Uri: ahpDirectoryURI(file), Type: ahptypes.ResourceChangeTypeAdded})
			case previous != stamp:
				changes = append(changes, ahptypes.ResourceChange{Uri: ahpDirectoryURI(file), Type: ahptypes.ResourceChangeTypeUpdated})
			}
		}
		for file := range last {
			if _, still := current[file]; !still {
				changes = append(changes, ahptypes.ResourceChange{Uri: ahpDirectoryURI(file), Type: ahptypes.ResourceChangeTypeDeleted})
			}
		}
		// Changes that could not be recorded are compared again next time.
		if changed(changes) == nil {
			last = current
		}
	}
}

// ahpWatchPatternsMax and ahpWatchPatternMax bound a watch's include and
// exclude patterns: their number and each one's length in bytes.
const (
	ahpWatchPatternsMax = 64
	ahpWatchPatternMax  = 256
)

// ahpWatchPatterns reads a watch's { items } pattern list.
func ahpWatchPatterns(raw *json.RawMessage) []string {
	if raw == nil {
		return nil
	}
	var wrapped struct {
		Items []string `json:"items"`
	}
	if json.Unmarshal(*raw, &wrapped) != nil {
		return nil
	}
	return wrapped.Items
}

// ahpMatchAny matches a relative path against glob patterns. A pattern
// starting with **/ also matches at any depth, and a bare path matches the
// path and anything under it.
func ahpMatchAny(patterns []string, rel string) bool {
	for _, pattern := range patterns {
		pattern = strings.TrimPrefix(pattern, "./")
		if pattern == rel || strings.HasPrefix(rel, strings.TrimSuffix(pattern, "/")+"/") {
			return true
		}
		if ok, _ := path.Match(pattern, rel); ok {
			return true
		}
		if tail, ok := strings.CutPrefix(pattern, "**/"); ok {
			for suffix := rel; suffix != ""; {
				if ok, _ := path.Match(tail, suffix); ok {
					return true
				}
				_, next, found := strings.Cut(suffix, "/")
				if !found {
					break
				}
				suffix = next
			}
		}
	}
	return false
}

const (
	// ahpChangesMax bounds the files a branch's changeset lists, and
	// ahpChangeContentMax the content shown for one side of a file.
	ahpChangesMax       = 300
	ahpChangeContentMax = 1 << 20
	// ahpChangesContentTotal bounds the content read for one changeset, well
	// within the read's own bound, which also carries a header per object.
	ahpChangesContentTotal = 48 << 20
	ahpChangeTooLarge      = "This file is too large to show.\n"
	ahpChangeBinary        = "This file is binary.\n"
	ahpGitProgram          = "git"
	// ahpShell runs the workspace's commands.
	ahpShell = "/bin/sh"
)

// branchChanges reads every file a worktree's branch changed since it forked
// from base, with both sides' contents, keyed by full path.
// ahpBranchForkScript prints where a worktree's branch left its base, then the
// files changed since, NUL-separated. With no base, as for a repository off
// github.com whose default branch was not looked up, it uses the default
// branch the clone found on the remote.
const ahpBranchForkScript = `set -e
cd "$1"
upstream="origin/$2"
[ -n "$2" ] || upstream=$(git symbolic-ref --quiet --short refs/remotes/origin/HEAD)
fork=$(git merge-base HEAD "$upstream")
printf '%s\0' "$fork"
git diff --no-renames --name-status -z "$fork" HEAD`

func (w *ahpWorkspaces) branchChanges(ctx context.Context, pod *corev1.Pod, worktree, base string) (map[string]ahpGitChange, error) {
	raw, err := w.output(ctx, pod, 1<<20, ahpShell, "-c", ahpBranchForkScript, "sh", worktree, base)
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(string(raw), "\x00"), "\x00")
	if len(fields) == 0 || fields[0] == "" {
		return nil, errors.New("no fork point")
	}
	fork := fields[0]
	type entry struct{ status, file string }
	var entries []entry
	for i := 1; i+1 < len(fields) && len(entries) < ahpChangesMax; i += 2 {
		// git cat-file reads one object name per line, so a file whose name
		// holds a line break cannot be read safely; leave it out.
		if strings.ContainsAny(fields[i+1], "\n\r") {
			continue
		}
		entries = append(entries, entry{status: fields[i], file: fields[i+1]})
	}
	var objects []string
	for _, e := range entries {
		if e.status != "A" {
			objects = append(objects, fork+":"+e.file)
		}
		if e.status != "D" {
			objects = append(objects, "HEAD:"+e.file)
		}
	}
	contents, err := w.blobs(ctx, pod, worktree, objects)
	if err != nil {
		return nil, err
	}
	changed := map[string]ahpGitChange{}
	for _, e := range entries {
		var change ahpGitChange
		if e.status != "A" {
			change.Before = contents[fork+":"+e.file]
		}
		if e.status != "D" {
			change.After = contents["HEAD:"+e.file]
		}
		changed[path.Join(worktree, e.file)] = change
	}
	return changed, nil
}

// blobs reads objects with git cat-file. Large or binary contents are
// replaced by a note; a missing object has no entry.
func (w *ahpWorkspaces) blobs(ctx context.Context, pod *corev1.Pod, worktree string, objects []string) (map[string]*string, error) {
	contents := map[string]*string{}
	if len(objects) == 0 {
		return contents, nil
	}
	sizes, err := w.blobSizes(ctx, pod, worktree, objects)
	if err != nil {
		return nil, err
	}
	small, large := ahpSelectBlobs(objects, sizes)
	for _, object := range large {
		note := ahpChangeTooLarge
		contents[object] = &note
	}
	if len(small) == 0 {
		return contents, nil
	}
	ctx, cancel := context.WithTimeout(ctx, ahpWorkspaceExecTimeout)
	defer cancel()
	out := &ahpLimitedBuffer{limit: 64 << 20}
	stdin := strings.NewReader(strings.Join(small, "\n") + "\n")
	if err := w.exec(ctx, pod, []string{ahpGitProgram, "-C", worktree, "cat-file", "--batch"},
		remotecommand.StreamOptions{Stdin: stdin, Stdout: out, Stderr: io.Discard}); err != nil {
		return nil, err
	}
	if out.over {
		return nil, errAHPOutputTooLarge
	}
	reader := bufio.NewReader(&out.buf)
	for _, object := range small {
		header, err := reader.ReadString('\n')
		if err != nil {
			return nil, err
		}
		parts := strings.Fields(header)
		if len(parts) != 3 {
			continue
		}
		size, err := strconv.Atoi(parts[2])
		if err != nil {
			return nil, err
		}
		if size < 0 || size > ahpChangeContentMax {
			// Each object was checked to be small; anything else means the
			// answers do not match the questions.
			return nil, errors.New("git returned an object larger than it reported")
		}
		data := make([]byte, size+1)
		if _, err := io.ReadFull(reader, data); err != nil {
			return nil, err
		}
		text := string(data[:size])
		if !utf8.ValidString(text) || strings.ContainsRune(text, 0) {
			text = ahpChangeBinary
		}
		contents[object] = &text
	}
	return contents, nil
}

// ahpSelectBlobs splits the objects that are blobs into those read in full
// and those shown as too large: alone, or past what one read returns in all.
func ahpSelectBlobs(objects []string, sizes map[string]int) (small, large []string) {
	budget := ahpChangesContentTotal
	for _, object := range objects {
		size, ok := sizes[object]
		switch {
		case !ok:
		case size > ahpChangeContentMax || size > budget:
			large = append(large, object)
		default:
			budget -= size
			small = append(small, object)
		}
	}
	return small, large
}

// blobSizes returns the size of each object that is a blob.
func (w *ahpWorkspaces) blobSizes(ctx context.Context, pod *corev1.Pod, worktree string, objects []string) (map[string]int, error) {
	ctx, cancel := context.WithTimeout(ctx, ahpWorkspaceExecTimeout)
	defer cancel()
	out := &ahpLimitedBuffer{limit: 1 << 20}
	stdin := strings.NewReader(strings.Join(objects, "\n") + "\n")
	if err := w.exec(ctx, pod, []string{ahpGitProgram, "-C", worktree, "cat-file", "--batch-check"},
		remotecommand.StreamOptions{Stdin: stdin, Stdout: out, Stderr: io.Discard}); err != nil {
		return nil, err
	}
	sizes := map[string]int{}
	lines := strings.Split(strings.TrimSuffix(out.buf.String(), "\n"), "\n")
	if out.over || len(lines) != len(objects) {
		return nil, errors.New("git answered a different number of objects than it was asked")
	}
	for i, object := range objects {
		parts := strings.Fields(lines[i])
		if len(parts) == 3 && parts[1] == "blob" {
			if size, err := strconv.Atoi(parts[2]); err == nil {
				sizes[object] = size
			}
		}
	}
	return sizes, nil
}
