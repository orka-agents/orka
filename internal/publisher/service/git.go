package service

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/orka-agents/orka/internal/publisher"
	"github.com/orka-agents/orka/internal/safesymlink"
)

const (
	gitConfigGlobalNull = "GIT_CONFIG_GLOBAL=/dev/null"
	gitBatchHeaderLimit = 256
	gitBatchWaitDelay   = time.Second
	langC               = "LANG=C"
	gitConfigNoSystem   = "GIT_CONFIG_NOSYSTEM=1"
	lcAllC              = "LC_ALL=C"
)

var gitHardening = []string{
	"credential.helper=", "credential.interactive=never", "core.askPass=", "core.hooksPath=/dev/null",
	"core.attributesFile=/dev/null", "core.excludesFile=/dev/null", "core.fsmonitor=false", "core.untrackedCache=false",
	"commit.gpgSign=false", "tag.gpgSign=false", "protocol.allow=never", "protocol.file.allow=always",
	"protocol.https.allow=always", "protocol.ssh.allow=never", "protocol.ext.allow=never", "submodule.recurse=false",
	"fetch.recurseSubmodules=false", "push.recurseSubmodules=no", "filter.lfs.required=false", "filter.lfs.clean=",
	"filter.lfs.smudge=", "filter.lfs.process=", "fetch.fsckObjects=true", "transfer.fsckObjects=true",
	"receive.fsckObjects=true", "gc.auto=0", "maintenance.auto=false", "http.followRedirects=false",
}

type gitRunner struct {
	binary           string
	tempRoot         string
	maxCommandOutput int64
	gitExecPath      string
	trustedPath      string
	proxyEnvironment publisher.ProxyEnvironment
}

type gitSandbox struct {
	root     string
	home     string
	xdg      string
	template string
}

type gitResult struct {
	stdout []byte
	stderr []byte
}

type gitBlobBatch struct {
	ctx              context.Context
	command          *exec.Cmd
	stdin            io.WriteCloser
	stdoutPipe       io.ReadCloser
	stdout           *bufio.Reader
	stderr           *limitedBuffer
	remainingBytes   int64
	remainingObjects int
	finished         bool
	done             chan struct{}
	doneOnce         sync.Once
}

func newGitRunner(
	binary, tempRoot string,
	maxCommandOutput int64,
	proxyEnvironment publisher.ProxyEnvironment,
) (*gitRunner, error) {
	trustedPath := filepath.Dir(binary) + string(os.PathListSeparator) + "/usr/local/bin:/usr/bin:/bin"
	command := exec.CommandContext(context.Background(), binary, "--exec-path")
	configureCommand(command)
	command.Env = []string{
		"HOME=/dev/null", "XDG_CONFIG_HOME=/dev/null", gitConfigGlobalNull, gitConfigNoSystem,
		lcAllC, langC, "TZ=UTC", "PATH=" + trustedPath,
	}
	output := &limitedBuffer{limit: 64 << 10}
	command.Stdout = output
	command.Stderr = &limitedBuffer{limit: 64 << 10}
	if err := command.Run(); err != nil || output.truncated {
		return nil, fmt.Errorf("resolve Git exec path")
	}
	execPath := strings.TrimSpace(output.String())
	if !filepath.IsAbs(execPath) || filepath.Clean(execPath) != execPath {
		return nil, fmt.Errorf("git exec path is invalid")
	}
	return &gitRunner{
		binary: binary, tempRoot: tempRoot, maxCommandOutput: maxCommandOutput,
		gitExecPath: execPath, trustedPath: trustedPath, proxyEnvironment: proxyEnvironment,
	}, nil
}

func (r *gitRunner) sandbox(prefix string) (*gitSandbox, error) {
	root, err := os.MkdirTemp(r.tempRoot, "orka-workspace-publisher-"+prefix+"-")
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(root, 0o700); err != nil {
		_ = os.RemoveAll(root)
		return nil, err
	}
	box := &gitSandbox{root: root, home: filepath.Join(root, "home"), xdg: filepath.Join(root, "xdg"), template: filepath.Join(root, "template")}
	for _, directory := range []string{box.home, box.xdg, box.template} {
		if err := os.Mkdir(directory, 0o700); err != nil {
			_ = os.RemoveAll(root)
			return nil, err
		}
	}
	return box, nil
}

func (b *gitSandbox) close() { _ = os.RemoveAll(b.root) }

func (r *gitRunner) command(ctx context.Context, box *gitSandbox, directory string, args ...string) *exec.Cmd {
	fullArgs := make([]string, 0, len(gitHardening)*2+len(args))
	for _, setting := range gitHardening {
		fullArgs = append(fullArgs, "-c", setting)
	}
	fullArgs = append(fullArgs, args...)
	command := exec.CommandContext(ctx, r.binary, fullArgs...)
	configureCommand(command)
	command.Dir = directory
	command.Env = []string{
		"HOME=" + box.home, "XDG_CONFIG_HOME=" + box.xdg, gitConfigGlobalNull, gitConfigNoSystem,
		"GIT_TEMPLATE_DIR=" + box.template, "GIT_NO_REPLACE_OBJECTS=1", "GIT_EXEC_PATH=" + r.gitExecPath,
		"GIT_TERMINAL_PROMPT=0", "GIT_ASKPASS=/bin/false", "SSH_ASKPASS=/bin/false", "GIT_ALLOW_PROTOCOL=file:https",
		"GIT_PROTOCOL_FROM_USER=0", "GIT_OPTIONAL_LOCKS=0", lcAllC, langC, "TZ=UTC", "PATH=" + r.trustedPath,
	}
	command.Env = append(command.Env, r.proxyEnvironment.Variables()...)
	return command
}

func (r *gitRunner) run(ctx context.Context, box *gitSandbox, directory string, outputLimit int64, args ...string) (gitResult, error) {
	command := r.command(ctx, box, directory, args...)
	if outputLimit <= 0 {
		outputLimit = r.maxCommandOutput
	}
	stdout := &limitedBuffer{limit: outputLimit}
	stderr := &limitedBuffer{limit: r.maxCommandOutput}
	command.Stdout = stdout
	command.Stderr = stderr
	err := command.Run()
	result := gitResult{stdout: stdout.Bytes(), stderr: stderr.Bytes()}
	if stdout.truncated || stderr.truncated {
		return result, apiError(ErrSCMTransport, "scm_output_limit", "Git output exceeded the configured limit", 502, false, err)
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return result, apiError(ErrSCMTransport, "scm_failure", "Git operation failed", 502, true, err)
		}
		return result, apiError(ErrSCMTransport, "scm_unavailable", "Git operation could not be started", 503, true, err)
	}
	return result, nil
}

func (r *gitRunner) startBlobBatch(
	ctx context.Context,
	box *gitSandbox,
	repositoryPath string,
	maxBytes int64,
	maxObjects int,
) (*gitBlobBatch, error) {
	command := r.command(ctx, box, box.root, "--git-dir="+repositoryPath, "cat-file", "--batch")
	command.WaitDelay = gitBatchWaitDelay
	stdin, err := command.StdinPipe()
	if err != nil {
		return nil, apiError(ErrSCMTransport, "scm_unavailable", "Git batch input could not be opened", 503, true, err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, apiError(ErrSCMTransport, "scm_unavailable", "Git batch output could not be opened", 503, true, err)
	}
	stderr := &limitedBuffer{limit: r.maxCommandOutput}
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, apiError(ErrSCMTransport, "scm_unavailable", "Git batch operation could not be started", 503, true, err)
	}
	batch := &gitBlobBatch{
		ctx: ctx, command: command, stdin: stdin, stdout: bufio.NewReaderSize(stdout, gitBatchHeaderLimit), stderr: stderr,
		stdoutPipe: stdout, remainingBytes: maxBytes, remainingObjects: maxObjects, done: make(chan struct{}),
	}
	go batch.cancelOnContextDone()
	return batch, nil
}

func (b *gitBlobBatch) cancelOnContextDone() {
	select {
	case <-b.ctx.Done():
		_ = b.stdin.Close()
		_ = b.stdoutPipe.Close()
		if b.command.Cancel != nil {
			_ = b.command.Cancel()
		} else if b.command.Process != nil {
			_ = b.command.Process.Kill()
		}
	case <-b.done:
	}
}

func (b *gitBlobBatch) stopCancellationWatcher() {
	if b.done != nil {
		b.doneOnce.Do(func() { close(b.done) })
	}
}

func (b *gitBlobBatch) classifyPrematureExit(message string, readErr error) error {
	if b.finished || b.command == nil {
		return apiError(ErrSCMTransport, "source_corrupt", message, 502, false, readErr)
	}
	b.finished = true
	b.stopCancellationWatcher()
	_ = b.stdin.Close()
	if b.stdoutPipe != nil {
		_ = b.stdoutPipe.Close()
	}
	waitErr := b.command.Wait()
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if b.stderr.truncated {
		return apiError(ErrSCMTransport, "scm_output_limit", "Git output exceeded the configured limit", 502, false, waitErr)
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return apiError(ErrSCMTransport, "scm_failure", "Git batch operation failed", 502, true, waitErr)
		}
		return apiError(ErrSCMTransport, "scm_unavailable", "Git batch operation could not complete", 503, true, waitErr)
	}
	return apiError(ErrSCMTransport, "source_corrupt", message, 502, false, readErr)
}

func (b *gitBlobBatch) readBlob(entry workspaceEntry, limit int64) ([]byte, error) {
	if err := b.ctx.Err(); err != nil {
		return nil, err
	}
	if b.finished {
		return nil, apiError(ErrSCMTransport, "scm_failure", "Git batch operation is already closed", 502, true, nil)
	}
	if entry.Size < 0 || entry.Size > limit || entry.Size > b.remainingBytes || b.remainingObjects < 1 {
		return nil, apiError(ErrSCMTransport, "scm_output_limit", "Git output exceeded the configured limit", 502, false, nil)
	}
	if entry.Size < 0 || entry.Size > math.MaxInt {
		return nil, apiError(ErrSCMTransport, "scm_output_limit", "Git output exceeded the configured limit", 502, false, nil)
	}
	blobSize := int(entry.Size)
	if int64(blobSize) != entry.Size {
		return nil, apiError(ErrSCMTransport, "scm_output_limit", "Git output exceeded the configured limit", 502, false, nil)
	}
	if _, err := io.WriteString(b.stdin, entry.OID+"\n"); err != nil {
		if contextErr := b.ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, apiError(ErrSCMTransport, "scm_failure", "Git batch operation failed", 502, true, err)
	}
	header, err := b.stdout.ReadSlice('\n')
	if err != nil {
		if contextErr := b.ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, apiError(ErrSCMTransport, "source_corrupt", "Git batch response header exceeded the configured limit", 502, false, err)
		}
		return nil, b.classifyPrematureExit("Git batch response ended before the object header", err)
	}
	if len(header) < 2 || len(header) > gitBatchHeaderLimit {
		return nil, apiError(ErrSCMTransport, "source_corrupt", "Git batch response header is malformed", 502, false, nil)
	}
	fields := strings.Fields(string(header[:len(header)-1]))
	if len(fields) != 3 || fields[0] != entry.OID || fields[1] != "blob" {
		return nil, apiError(ErrSCMTransport, "source_corrupt", "Git batch response did not match the requested blob", 502, false, nil)
	}
	size, err := strconv.ParseInt(fields[2], 10, 64)
	if err != nil || size != entry.Size {
		return nil, apiError(ErrSCMTransport, "source_corrupt", "Git blob size changed during workspace preparation", 502, false, err)
	}
	blob := make([]byte, blobSize)
	if _, err := io.ReadFull(b.stdout, blob); err != nil {
		if contextErr := b.ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, b.classifyPrematureExit("Git batch response ended before the object content", err)
	}
	separator, err := b.stdout.ReadByte()
	if err != nil {
		if contextErr := b.ctx.Err(); contextErr != nil {
			return nil, contextErr
		}
		return nil, b.classifyPrematureExit("Git batch response ended before the object separator", err)
	}
	if separator != '\n' {
		return nil, apiError(ErrSCMTransport, "source_corrupt", "Git batch response object framing is malformed", 502, false, nil)
	}
	b.remainingBytes -= entry.Size
	b.remainingObjects--
	return blob, nil
}

func (b *gitBlobBatch) finish() error {
	if b.finished {
		return nil
	}
	b.finished = true
	b.stopCancellationWatcher()
	closeErr := b.stdin.Close()
	waitErr := b.command.Wait()
	_ = b.stdoutPipe.Close()
	if err := b.ctx.Err(); err != nil {
		return err
	}
	if b.stderr.truncated {
		return apiError(ErrSCMTransport, "scm_output_limit", "Git output exceeded the configured limit", 502, false, waitErr)
	}
	if waitErr != nil {
		var exitErr *exec.ExitError
		if errors.As(waitErr, &exitErr) {
			return apiError(ErrSCMTransport, "scm_failure", "Git batch operation failed", 502, true, waitErr)
		}
		return apiError(ErrSCMTransport, "scm_unavailable", "Git batch operation could not complete", 503, true, waitErr)
	}
	if closeErr != nil {
		return apiError(ErrSCMTransport, "scm_failure", "Git batch input could not be closed", 502, true, closeErr)
	}
	return nil
}

func (b *gitBlobBatch) abort() {
	if b == nil || b.finished {
		return
	}
	b.finished = true
	b.stopCancellationWatcher()
	_ = b.stdin.Close()
	_ = b.stdoutPipe.Close()
	if b.command.Cancel != nil {
		_ = b.command.Cancel()
	} else if b.command.Process != nil {
		_ = b.command.Process.Kill()
	}
	_ = b.command.Wait()
}

func (r *gitRunner) resolveSource(ctx context.Context, repositoryURL, ref string) (string, string, error) {
	if strings.TrimSpace(ref) == "" {
		box, err := r.sandbox("resolve-default")
		if err != nil {
			return "", "", err
		}
		defer box.close()
		return r.observeDefaultRef(ctx, box, repositoryURL)
	}
	if validateObjectID(ref) == nil {
		box, _, _, err := r.prepareExactRepository(ctx, repositoryURL, ref, ref)
		if box != nil {
			defer box.close()
		}
		if err != nil {
			return "", "", err
		}
		return ref, ref, nil
	}
	box, err := r.sandbox("resolve")
	if err != nil {
		return "", "", err
	}
	defer box.close()
	if isBareWorkspaceSourceRef(ref) {
		return r.observeBareRef(ctx, box, repositoryURL, ref)
	}
	oid, err := r.observeRef(ctx, box, repositoryURL, ref)
	if err != nil {
		return "", "", err
	}
	return ref, oid, nil
}

func (r *gitRunner) observeDefaultRef(ctx context.Context, box *gitSandbox, repositoryURL string) (string, string, error) {
	result, err := r.run(ctx, box, box.root, r.maxCommandOutput, "ls-remote", "--symref", "--", repositoryURL, "HEAD")
	if err != nil {
		return "", "", err
	}
	trimmed := strings.TrimSpace(string(result.stdout))
	if trimmed == "" {
		return "", "", apiError(ErrSCMTransport, "source_ref_absent", "repository default source ref is absent", 409, false, nil)
	}
	var resolvedRef, oid string
	for line := range strings.SplitSeq(trimmed, "\n") {
		fields := strings.Fields(line)
		switch {
		case len(fields) == 3 && fields[0] == "ref:" && fields[2] == "HEAD":
			if resolvedRef != "" || validateBranchRef(fields[1]) != nil {
				return "", "", apiError(ErrSCMTransport, "source_ref_invalid", "default source ref observation was malformed", 502, false, nil)
			}
			resolvedRef = fields[1]
		case len(fields) == 2 && fields[1] == "HEAD" && validateObjectID(fields[0]) == nil:
			if oid != "" {
				return "", "", apiError(ErrSCMTransport, "source_ref_ambiguous", "default source ref observation was ambiguous", 502, false, nil)
			}
			oid = fields[0]
		default:
			return "", "", apiError(ErrSCMTransport, "source_ref_invalid", "default source ref observation was malformed", 502, false, nil)
		}
	}
	if oid == "" {
		return "", "", apiError(ErrSCMTransport, "source_ref_absent", "repository default source ref is absent", 409, false, nil)
	}
	if resolvedRef == "" {
		// Some transports advertise only the HEAD object. Freezing the exact OID
		// is still safe and avoids guessing a branch name.
		return oid, oid, nil
	}
	return resolvedRef, oid, nil
}

func (r *gitRunner) observeBareRef(ctx context.Context, box *gitSandbox, repositoryURL, ref string) (string, string, error) {
	branchRef := "refs/heads/" + ref
	tagRef := "refs/tags/" + ref
	peeledTagRef := tagRef + "^{}"
	result, err := r.run(ctx, box, box.root, r.maxCommandOutput, "ls-remote", "--", repositoryURL, branchRef, tagRef, peeledTagRef)
	if err != nil {
		return "", "", err
	}
	trimmed := strings.TrimSpace(string(result.stdout))
	if trimmed == "" {
		return "", "", apiError(ErrSCMTransport, "source_ref_absent", "source ref is absent", 409, false, nil)
	}
	observed := make(map[string]string, 3)
	for line := range strings.SplitSeq(trimmed, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || validateObjectID(fields[0]) != nil {
			return "", "", apiError(ErrSCMTransport, "source_ref_invalid", "source ref observation was malformed", 502, false, nil)
		}
		switch fields[1] {
		case branchRef, tagRef, peeledTagRef:
		default:
			return "", "", apiError(ErrSCMTransport, "source_ref_invalid", "source ref observation was malformed", 502, false, nil)
		}
		if observed[fields[1]] != "" {
			return "", "", apiError(ErrSCMTransport, "source_ref_ambiguous", "source ref observation was ambiguous", 502, false, nil)
		}
		observed[fields[1]] = fields[0]
	}
	branchOID := observed[branchRef]
	tagOID := observed[tagRef]
	peeledTagOID := observed[peeledTagRef]
	if peeledTagOID != "" && tagOID == "" {
		return "", "", apiError(ErrSCMTransport, "source_ref_invalid", "source ref observation was malformed", 502, false, nil)
	}
	if branchOID != "" && tagOID != "" {
		return "", "", apiError(ErrSCMTransport, "source_ref_ambiguous", "bare source ref matches both a branch and a tag", 409, false, nil)
	}
	if branchOID != "" {
		return branchRef, branchOID, nil
	}
	if tagOID != "" {
		if peeledTagOID != "" {
			tagOID = peeledTagOID
		}
		return tagRef, tagOID, nil
	}
	return "", "", apiError(ErrSCMTransport, "source_ref_invalid", "source ref observation was malformed", 502, false, nil)
}

func (r *gitRunner) observeRef(ctx context.Context, box *gitSandbox, repositoryURL, ref string) (string, error) {
	args := []string{"ls-remote", "--refs", "--", repositoryURL, ref}
	peeledRef := ""
	if strings.HasPrefix(ref, "refs/tags/") {
		peeledRef = ref + "^{}"
		args = []string{"ls-remote", "--", repositoryURL, ref, peeledRef}
	}
	result, err := r.run(ctx, box, box.root, r.maxCommandOutput, args...)
	if err != nil {
		return "", err
	}
	trimmed := strings.TrimSpace(string(result.stdout))
	if trimmed == "" {
		return "", apiError(ErrSCMTransport, "source_ref_absent", "source ref is absent", 409, false, nil)
	}
	var directOID, peeledOID string
	for line := range strings.SplitSeq(trimmed, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 || validateObjectID(fields[0]) != nil {
			return "", apiError(ErrSCMTransport, "source_ref_invalid", "source ref observation was malformed", 502, false, nil)
		}
		switch fields[1] {
		case ref:
			if directOID != "" {
				return "", apiError(ErrSCMTransport, "source_ref_ambiguous", "source ref observation was ambiguous", 502, false, nil)
			}
			directOID = fields[0]
		case peeledRef:
			if peeledRef == "" || peeledOID != "" {
				return "", apiError(ErrSCMTransport, "source_ref_ambiguous", "source ref observation was ambiguous", 502, false, nil)
			}
			peeledOID = fields[0]
		default:
			return "", apiError(ErrSCMTransport, "source_ref_invalid", "source ref observation was malformed", 502, false, nil)
		}
	}
	if directOID == "" {
		return "", apiError(ErrSCMTransport, "source_ref_invalid", "source ref observation was malformed", 502, false, nil)
	}
	if peeledOID != "" {
		return peeledOID, nil
	}
	return directOID, nil
}

func (r *gitRunner) prepareExactRepository(ctx context.Context, repositoryURL, ref, baselineOID string) (*gitSandbox, string, string, error) {
	box, err := r.sandbox("workspace")
	if err != nil {
		return nil, "", "", apiError(ErrSCMTransport, "sandbox_unavailable", "workspace sandbox could not be created", 503, true, err)
	}
	cleanup := true
	defer func() {
		if cleanup {
			box.close()
		}
	}()
	if validateObjectID(ref) == nil {
		if ref != baselineOID {
			return nil, "", "", apiError(ErrSCMTransport, "source_moved", "exact source commit does not match the persisted baseline", 409, false, nil)
		}
	} else {
		observed, err := r.observeRef(ctx, box, repositoryURL, ref)
		if err != nil {
			return nil, "", "", err
		}
		if observed != baselineOID {
			return nil, "", "", apiError(ErrSCMTransport, "source_moved", "source ref moved from the persisted baseline", 409, false, nil)
		}
	}
	repositoryPath := filepath.Join(box.root, "source.git")
	format := "sha1"
	if len(baselineOID) == 64 {
		format = "sha256"
	}
	if _, err := r.run(ctx, box, box.root, r.maxCommandOutput, "init", "--bare", "--object-format="+format, "--", repositoryPath); err != nil {
		return nil, "", "", err
	}
	if _, err := r.run(ctx, box, box.root, r.maxCommandOutput, "--git-dir="+repositoryPath, "fetch", "--no-tags", "--no-recurse-submodules", "--depth=1", "--", repositoryURL, ref+":refs/orka/source"); err != nil {
		return nil, "", "", err
	}
	resolved, err := r.run(ctx, box, box.root, 256, "--git-dir="+repositoryPath, "rev-parse", "--verify", "refs/orka/source^{commit}")
	if err != nil {
		return nil, "", "", err
	}
	if strings.TrimSpace(string(resolved.stdout)) != baselineOID {
		return nil, "", "", apiError(ErrSCMTransport, "source_moved", "fetched source ref did not match the persisted baseline", 409, false, nil)
	}
	if _, err := r.run(ctx, box, box.root, r.maxCommandOutput, "--git-dir="+repositoryPath, "fsck", "--strict", "--no-reflogs", "--no-progress", baselineOID); err != nil {
		return nil, "", "", apiError(ErrSCMTransport, "source_corrupt", "fetched source objects failed strict validation", 502, false, err)
	}
	tree, err := r.run(ctx, box, box.root, 256, "--git-dir="+repositoryPath, "rev-parse", "--verify", baselineOID+"^{tree}")
	if err != nil {
		return nil, "", "", err
	}
	treeOID := strings.TrimSpace(string(tree.stdout))
	if err := validateObjectID(treeOID); err != nil {
		return nil, "", "", err
	}
	cleanup = false
	return box, repositoryPath, treeOID, nil
}

func (r *gitRunner) listTree(
	ctx context.Context,
	box *gitSandbox,
	batch *gitBlobBatch,
	repositoryPath, baselineOID string,
	limits WorkspaceLimits,
) ([]workspaceEntry, error) {
	metadataLimit := int64(limits.MaxEntries) * int64(limits.MaxPathBytes+160)
	metadataLimit = min(metadataLimit, limits.MaxArtifactBytes)
	result, err := r.run(ctx, box, box.root, metadataLimit, "--git-dir="+repositoryPath, "ls-tree", "-lrz", "--full-tree", "-r", baselineOID)
	if err != nil {
		return nil, err
	}
	records := bytes.Split(result.stdout, []byte{0})
	if len(records) > 0 && len(records[len(records)-1]) == 0 {
		records = records[:len(records)-1]
	}
	if len(records) > limits.MaxEntries {
		return nil, invalidRequest("repository tree exceeds the entry limit", nil)
	}
	entries := make([]workspaceEntry, 0, len(records))
	paths := make(map[string]struct{}, len(records))
	links := make(map[string]string)
	var expanded int64
	for _, record := range records {
		metadata, rawPath, found := bytes.Cut(record, []byte{'\t'})
		fields := strings.Fields(string(metadata))
		if !found || len(fields) != 4 || fields[1] != "blob" {
			return nil, invalidRequest("repository contains a submodule or unsupported Git object", nil)
		}
		mode := fields[0]
		if mode != "100644" && mode != "100755" && mode != "120000" {
			return nil, invalidRequest("repository contains an unsupported file mode", nil)
		}
		oid := fields[2]
		if err := validateObjectID(oid); err != nil {
			return nil, err
		}
		filePath := string(rawPath)
		if err := validateWorkspacePath(filePath, limits.MaxPathBytes); err != nil {
			return nil, err
		}
		size, err := strconv.ParseInt(fields[3], 10, 64)
		maxEntryBytes := limits.MaxFileBytes
		if mode == "120000" {
			maxEntryBytes = int64(limits.MaxPathBytes)
		}
		if err != nil || size < 0 || size > maxEntryBytes || size > limits.MaxExpandedBytes-expanded {
			return nil, invalidRequest("repository entry exceeds workspace limits", err)
		}
		entry := workspaceEntry{Path: filePath, Mode: mode, OID: oid, Size: size}
		if mode == "120000" {
			blob, err := batch.readBlob(entry, int64(limits.MaxPathBytes))
			if err != nil {
				return nil, err
			}
			entry.Target = string(blob)
			if _, err := safesymlink.Resolve(filePath, entry.Target, limits.MaxPathBytes, limits.MaxPathBytes); err != nil {
				return nil, invalidRequest("repository contains an unsafe symlink", err)
			}
			links[filePath] = entry.Target
		}
		expanded += size
		paths[filePath] = struct{}{}
		entries = append(entries, entry)
	}
	if err := safesymlink.ValidateGraph(paths, links, limits.MaxPathBytes, limits.MaxPathBytes); err != nil {
		return nil, invalidRequest("repository contains an unsafe symlink graph", err)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	return entries, nil
}

type limitedBuffer struct {
	buffer    bytes.Buffer
	limit     int64
	truncated bool
}

func (b *limitedBuffer) Write(value []byte) (int, error) {
	original := len(value)
	remaining := b.limit - int64(b.buffer.Len())
	if remaining > 0 {
		if int64(len(value)) > remaining {
			value = value[:remaining]
			b.truncated = true
		}
		_, _ = b.buffer.Write(value)
	} else if len(value) > 0 {
		b.truncated = true
	}
	return original, nil
}

func (b *limitedBuffer) Bytes() []byte  { return append([]byte(nil), b.buffer.Bytes()...) }
func (b *limitedBuffer) String() string { return b.buffer.String() }
