/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	fakeDefaultPollInterval = 5 * time.Millisecond
	fakeDefaultArtifactMode = 0o644
)

// ExecHandler lets tests provide custom command execution behavior.
type ExecHandler func(context.Context, ExecRequest) (ExecResult, error)

// FakeOption configures a FakeExecutor.
type FakeOption func(*FakeExecutor)

// WithAutoReady controls whether newly claimed fake workspaces immediately
// enter PhaseReady. The default is true.
func WithAutoReady(autoReady bool) FakeOption {
	return func(f *FakeExecutor) {
		f.autoReady = autoReady
	}
}

// WithReadyPollInterval controls how often WaitReady observes workspace state.
func WithReadyPollInterval(interval time.Duration) FakeOption {
	return func(f *FakeExecutor) {
		if interval > 0 {
			f.readyPollInterval = interval
		}
	}
}

// WithNow controls the timestamp source used by the fake executor.
func WithNow(now func() time.Time) FakeOption {
	return func(f *FakeExecutor) {
		if now != nil {
			f.now = now
		}
	}
}

// FakeExecutor is an in-memory WorkspaceExecutor for unit tests.
type FakeExecutor struct {
	mu                sync.Mutex
	now               func() time.Time
	readyPollInterval time.Duration
	autoReady         bool
	nextID            int
	workspaces        map[string]*fakeWorkspace
	reuseIndex        map[string]string
	execHandler       ExecHandler
	execScripts       []fakeExecScript
}

type fakeWorkspace struct {
	ref         WorkspaceRef
	template    TemplateRef
	reuseKey    string
	phase       Phase
	retained    bool
	message     string
	createdAt   time.Time
	readyAt     time.Time
	releasedAt  time.Time
	deletedAt   time.Time
	labels      map[string]string
	annotations map[string]string
	artifacts   map[string]DownloadedArtifact
}

type fakeExecScript struct {
	result ExecResult
	err    error
	delay  time.Duration
}

// NewFakeExecutor returns an in-memory executor suitable for deterministic unit tests.
func NewFakeExecutor(opts ...FakeOption) *FakeExecutor {
	f := &FakeExecutor{
		now:               time.Now,
		readyPollInterval: fakeDefaultPollInterval,
		autoReady:         true,
		workspaces:        make(map[string]*fakeWorkspace),
		reuseIndex:        make(map[string]string),
	}
	for _, opt := range opts {
		opt(f)
	}
	return f
}

var _ WorkspaceExecutor = (*FakeExecutor)(nil)

// SetExecHandler sets a custom command handler. It replaces queued exec scripts.
func (f *FakeExecutor) SetExecHandler(handler ExecHandler) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execHandler = handler
	f.execScripts = nil
}

// EnqueueExecResult queues the next command result returned by Exec.
func (f *FakeExecutor) EnqueueExecResult(result ExecResult, err error) {
	f.EnqueueExecDelay(0, result, err)
}

// EnqueueExecDelay queues the next command result after a fake delay that honors ctx cancellation.
func (f *FakeExecutor) EnqueueExecDelay(delay time.Duration, result ExecResult, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execHandler = nil
	f.execScripts = append(f.execScripts, fakeExecScript{result: result, err: err, delay: delay})
}

// MarkReady moves a pending workspace to PhaseReady.
func (f *FakeExecutor) MarkReady(ref WorkspaceRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ws, err := f.findWorkspaceLocked("mark ready", ref)
	if err != nil {
		return err
	}
	if ws.phase == PhaseDeleted {
		return NewError("mark ready", ErrorKindNotFound, "workspace is deleted", false, nil)
	}
	if ws.phase == PhaseFailed {
		return NewError("mark ready", ErrorKindFailedPrecondition, "workspace has failed", false, nil)
	}
	ws.phase = PhaseReady
	ws.readyAt = f.now()
	ws.message = "workspace ready"
	return nil
}

// Fail marks a workspace as failed.
func (f *FakeExecutor) Fail(ref WorkspaceRef, message string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	ws, err := f.findWorkspaceLocked("fail", ref)
	if err != nil {
		return err
	}
	ws.phase = PhaseFailed
	ws.message = message
	return nil
}

// Claim creates a new fake workspace or reuses an existing one by claim name or reuse key.
func (f *FakeExecutor) Claim(ctx context.Context, req ClaimRequest) (*ClaimResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, contextError("claim", err)
	}
	if strings.TrimSpace(req.Namespace) == "" {
		return nil, NewError("claim", ErrorKindInvalidArgument, "namespace is required", false, nil)
	}
	if strings.TrimSpace(req.Template.Name) == "" {
		return nil, NewError("claim", ErrorKindInvalidArgument, "template name is required", false, nil)
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	if req.ReuseKey != "" {
		if existingKey := f.reuseIndex[reuseIndexKey(req.Namespace, req.Template, req.ReuseKey)]; existingKey != "" {
			if ws := f.reusableWorkspaceLocked(existingKey); ws != nil {
				return f.reusedClaimResultLocked(ws), nil
			}
		}
	}

	if req.ClaimName != "" {
		key := workspaceKey(WorkspaceRef{Namespace: req.Namespace, ClaimName: req.ClaimName})
		if ws := f.reusableWorkspaceLocked(key); ws != nil {
			return f.reusedClaimResultLocked(ws), nil
		}
	}

	claimName := req.ClaimName
	if claimName == "" {
		f.nextID++
		claimName = fmt.Sprintf("workspace-%d", f.nextID)
	}

	now := f.now()
	ref := WorkspaceRef{
		Namespace:   req.Namespace,
		ClaimName:   claimName,
		SandboxName: claimName + "-sandbox",
		ID:          fmt.Sprintf("fake-%s-%s", req.Namespace, claimName),
	}
	ws := &fakeWorkspace{
		ref:         ref,
		template:    req.Template,
		reuseKey:    req.ReuseKey,
		phase:       PhasePending,
		message:     "workspace claimed",
		createdAt:   now,
		labels:      copyStringMap(req.Labels),
		annotations: copyStringMap(req.Annotations),
		artifacts:   make(map[string]DownloadedArtifact),
	}
	if f.autoReady {
		ws.phase = PhaseReady
		ws.readyAt = now
		ws.message = "workspace ready"
	}

	key := workspaceKey(ref)
	f.workspaces[key] = ws
	if req.ReuseKey != "" {
		f.reuseIndex[reuseIndexKey(req.Namespace, req.Template, req.ReuseKey)] = key
	}

	return &ClaimResult{
		Ref:       ws.ref,
		Template:  ws.template,
		ReuseKey:  ws.reuseKey,
		Created:   true,
		Phase:     ws.phase,
		Message:   ws.message,
		ClaimedAt: now,
	}, nil
}

// WaitReady waits until the fake workspace is marked ready or the context times out.
func (f *FakeExecutor) WaitReady(ctx context.Context, req WaitReadyRequest) (*ReadyResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()

	for {
		f.mu.Lock()
		ws, err := f.findWorkspaceLocked("wait ready", req.Ref)
		if err != nil {
			f.mu.Unlock()
			return nil, err
		}
		snapshot := f.readySnapshotLocked(ws)
		f.mu.Unlock()

		switch snapshot.phase {
		case PhaseReady:
			return &ReadyResult{Ref: snapshot.ref, Phase: snapshot.phase, Message: snapshot.message, ReadyAt: snapshot.readyAt}, nil
		case PhaseDeleted:
			return nil, NewError("wait ready", ErrorKindNotFound, "workspace is deleted", false, nil)
		case PhaseFailed:
			return nil, NewError("wait ready", ErrorKindFailedPrecondition, snapshot.message, false, nil)
		}

		if err := sleepContext(ctx, f.readyPollInterval); err != nil {
			return nil, contextError("wait ready", err)
		}
	}
}

// Exec executes a queued or custom fake command against a ready workspace.
func (f *FakeExecutor) Exec(ctx context.Context, req ExecRequest) (*ExecResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, contextError("exec", err)
	}
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		return nil, NewError("exec", ErrorKindInvalidArgument, "command is required", false, nil)
	}

	f.mu.Lock()
	if _, err := f.findReadyWorkspaceLocked("exec", req.Ref); err != nil {
		f.mu.Unlock()
		return nil, err
	}
	handler := f.execHandler
	var script *fakeExecScript
	if handler == nil && len(f.execScripts) > 0 {
		next := f.execScripts[0]
		f.execScripts = f.execScripts[1:]
		script = &next
	}
	f.mu.Unlock()

	startedAt := f.now()
	var result ExecResult
	var err error
	switch {
	case handler != nil:
		result, err = handler(ctx, req)
	case script != nil:
		if script.delay > 0 {
			if err := sleepContext(ctx, script.delay); err != nil {
				return nil, contextError("exec", err)
			}
		}
		result, err = script.result, script.err
	default:
		result = ExecResult{ExitCode: 0}
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, contextError("exec", ctxErr)
		}
		return nil, normalizeError("exec", err)
	}

	finishedAt := f.now()
	result.Ref = coalesceRef(result.Ref, req.Ref)
	if len(result.Command) == 0 {
		result.Command = append([]string(nil), req.Command...)
	}
	if result.StartedAt.IsZero() {
		result.StartedAt = startedAt
	}
	if result.FinishedAt.IsZero() {
		result.FinishedAt = finishedAt
	}
	if req.MaxOutputBytes > 0 {
		result.Stdout, result.StdoutTruncated = truncateBytes(result.Stdout, req.MaxOutputBytes)
		result.Stderr, result.StderrTruncated = truncateBytes(result.Stderr, req.MaxOutputBytes)
	}
	if result.ExitCode != 0 {
		return &result, NewError("exec", ErrorKindCommandFailed, fmt.Sprintf("command exited with code %d", result.ExitCode), false, nil)
	}
	return &result, nil
}

// Upload stores fake artifacts in a workspace.
func (f *FakeExecutor) Upload(ctx context.Context, req UploadRequest) (*UploadResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, contextError("upload", err)
	}
	if len(req.Artifacts) == 0 {
		return nil, NewError("upload", ErrorKindInvalidArgument, "at least one artifact is required", false, nil)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	ws, err := f.findUsableWorkspaceLocked("upload", req.Ref)
	if err != nil {
		return nil, err
	}

	uploaded := make([]Artifact, 0, len(req.Artifacts))
	for _, artifact := range req.Artifacts {
		artifactPath, err := cleanArtifactPath(artifact.Path)
		if err != nil {
			return nil, NewError("upload", ErrorKindInvalidArgument, err.Error(), false, err)
		}
		data := append([]byte(nil), artifact.Data...)
		mode := artifact.Mode
		if mode == 0 {
			mode = fakeDefaultArtifactMode
		}
		modTime := artifact.ModTime
		if modTime.IsZero() {
			modTime = f.now()
		}
		meta := Artifact{
			Path:    artifactPath,
			Size:    int64(len(data)),
			Digest:  digest(data),
			Mode:    mode,
			ModTime: modTime,
		}
		ws.artifacts[artifactPath] = DownloadedArtifact{Artifact: meta, Data: data}
		uploaded = append(uploaded, meta)
	}
	return &UploadResult{Ref: ws.ref, Artifacts: uploaded}, nil
}

// Download reads fake artifacts from a workspace. Empty Paths downloads all artifacts.
func (f *FakeExecutor) Download(ctx context.Context, req DownloadRequest) (*DownloadResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, contextError("download", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	ws, err := f.findUsableWorkspaceLocked("download", req.Ref)
	if err != nil {
		return nil, err
	}

	paths := append([]string(nil), req.Paths...)
	if len(paths) == 0 {
		paths = make([]string, 0, len(ws.artifacts))
		for artifactPath := range ws.artifacts {
			paths = append(paths, artifactPath)
		}
		sort.Strings(paths)
	}

	downloaded := make([]DownloadedArtifact, 0, len(paths))
	for _, requestedPath := range paths {
		artifactPath, err := cleanArtifactPath(requestedPath)
		if err != nil {
			return nil, NewError("download", ErrorKindInvalidArgument, err.Error(), false, err)
		}
		artifact, ok := ws.artifacts[artifactPath]
		if !ok {
			return nil, NewError("download", ErrorKindNotFound, fmt.Sprintf("artifact %q not found", artifactPath), false, nil)
		}
		copied := artifact
		copied.Data = append([]byte(nil), artifact.Data...)
		downloaded = append(downloaded, copied)
	}
	return &DownloadResult{Ref: ws.ref, Artifacts: downloaded}, nil
}

// Release marks a workspace released or retained.
func (f *FakeExecutor) Release(ctx context.Context, req ReleaseRequest) (*ReleaseResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, contextError("release", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	ws, err := f.findWorkspaceLocked("release", req.Ref)
	if err != nil {
		return nil, err
	}
	if ws.phase == PhaseDeleted {
		return nil, NewError("release", ErrorKindNotFound, "workspace is deleted", false, nil)
	}

	ws.releasedAt = f.now()
	if req.Retain {
		ws.phase = PhaseRetained
		ws.retained = true
		ws.message = releaseMessage(req.Reason, "workspace retained")
		return &ReleaseResult{Ref: ws.ref, Retained: true, Phase: ws.phase, Message: ws.message}, nil
	}

	ws.phase = PhaseReleased
	ws.retained = false
	ws.message = releaseMessage(req.Reason, "workspace released")
	f.removeReuseIndexLocked(ws)
	return &ReleaseResult{Ref: ws.ref, Released: true, Phase: ws.phase, Message: ws.message}, nil
}

// Delete marks a workspace deleted and removes it from reuse indexes.
func (f *FakeExecutor) Delete(ctx context.Context, req DeleteRequest) (*DeleteResult, error) {
	ctx, cancel := contextWithTimeout(ctx, req.Timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, contextError("delete", err)
	}

	f.mu.Lock()
	defer f.mu.Unlock()
	ws, err := f.findWorkspaceLocked("delete", req.Ref)
	if err != nil {
		return nil, err
	}
	if ws.phase == PhaseDeleted {
		return &DeleteResult{Ref: ws.ref, Deleted: false, Phase: ws.phase, Message: ws.message}, nil
	}
	ws.phase = PhaseDeleted
	ws.retained = false
	ws.deletedAt = f.now()
	ws.message = releaseMessage(req.Reason, "workspace deleted")
	f.removeReuseIndexLocked(ws)
	return &DeleteResult{Ref: ws.ref, Deleted: true, Phase: ws.phase, Message: ws.message}, nil
}

// Describe returns a copy of the fake workspace snapshot.
func (f *FakeExecutor) Describe(ctx context.Context, req DescribeRequest) (*Description, error) {
	if err := ctx.Err(); err != nil {
		return nil, contextError("describe", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	ws, err := f.findWorkspaceLocked("describe", req.Ref)
	if err != nil {
		return nil, err
	}
	return f.descriptionLocked(ws), nil
}

func (f *FakeExecutor) reusedClaimResultLocked(ws *fakeWorkspace) *ClaimResult {
	ws.phase = PhaseReady
	ws.retained = false
	ws.message = "workspace reused"
	if ws.readyAt.IsZero() {
		ws.readyAt = f.now()
	}
	return &ClaimResult{
		Ref:       ws.ref,
		Template:  ws.template,
		ReuseKey:  ws.reuseKey,
		Reused:    true,
		Phase:     ws.phase,
		Message:   ws.message,
		ClaimedAt: f.now(),
	}
}

func (f *FakeExecutor) reusableWorkspaceLocked(key string) *fakeWorkspace {
	ws := f.workspaces[key]
	if ws == nil {
		return nil
	}
	switch ws.phase {
	case PhaseDeleted, PhaseFailed, PhaseReleased:
		return nil
	default:
		return ws
	}
}

func (f *FakeExecutor) findWorkspaceLocked(op string, ref WorkspaceRef) (*fakeWorkspace, error) {
	if ref.IsZero() {
		return nil, NewError(op, ErrorKindInvalidArgument, "workspace reference is required", false, nil)
	}
	if key := workspaceKey(ref); key != "" {
		if ws := f.workspaces[key]; ws != nil {
			return ws, nil
		}
	}
	if ref.ID != "" || ref.SandboxName != "" {
		for _, ws := range f.workspaces {
			if ref.ID != "" && ws.ref.ID == ref.ID {
				return ws, nil
			}
			if ref.SandboxName != "" && ws.ref.SandboxName == ref.SandboxName && (ref.Namespace == "" || ref.Namespace == ws.ref.Namespace) {
				return ws, nil
			}
		}
	}
	return nil, NewError(op, ErrorKindNotFound, "workspace not found", false, nil)
}

func (f *FakeExecutor) findReadyWorkspaceLocked(op string, ref WorkspaceRef) (*fakeWorkspace, error) {
	ws, err := f.findWorkspaceLocked(op, ref)
	if err != nil {
		return nil, err
	}
	if ws.phase == PhaseDeleted {
		return nil, NewError(op, ErrorKindNotFound, "workspace is deleted", false, nil)
	}
	if ws.phase != PhaseReady {
		return nil, NewError(op, ErrorKindFailedPrecondition, fmt.Sprintf("workspace phase is %s", ws.phase), false, nil)
	}
	return ws, nil
}

func (f *FakeExecutor) findUsableWorkspaceLocked(op string, ref WorkspaceRef) (*fakeWorkspace, error) {
	ws, err := f.findWorkspaceLocked(op, ref)
	if err != nil {
		return nil, err
	}
	switch ws.phase {
	case PhaseDeleted:
		return nil, NewError(op, ErrorKindNotFound, "workspace is deleted", false, nil)
	case PhaseFailed:
		return nil, NewError(op, ErrorKindFailedPrecondition, "workspace has failed", false, nil)
	}
	return ws, nil
}

type readySnapshot struct {
	ref     WorkspaceRef
	phase   Phase
	message string
	readyAt time.Time
}

func (f *FakeExecutor) readySnapshotLocked(ws *fakeWorkspace) readySnapshot {
	return readySnapshot{ref: ws.ref, phase: ws.phase, message: ws.message, readyAt: ws.readyAt}
}

func (f *FakeExecutor) descriptionLocked(ws *fakeWorkspace) *Description {
	artifacts := make([]Artifact, 0, len(ws.artifacts))
	for _, artifact := range ws.artifacts {
		artifacts = append(artifacts, artifact.Artifact)
	}
	sort.Slice(artifacts, func(i, j int) bool { return artifacts[i].Path < artifacts[j].Path })
	return &Description{
		Ref:         ws.ref,
		Template:    ws.template,
		ReuseKey:    ws.reuseKey,
		Phase:       ws.phase,
		Retained:    ws.retained,
		Message:     ws.message,
		CreatedAt:   ws.createdAt,
		ReadyAt:     ws.readyAt,
		ReleasedAt:  ws.releasedAt,
		DeletedAt:   ws.deletedAt,
		Labels:      copyStringMap(ws.labels),
		Annotations: copyStringMap(ws.annotations),
		Artifacts:   artifacts,
	}
}

func (f *FakeExecutor) removeReuseIndexLocked(ws *fakeWorkspace) {
	if ws.reuseKey == "" {
		return
	}
	key := reuseIndexKey(ws.ref.Namespace, ws.template, ws.reuseKey)
	if f.reuseIndex[key] == workspaceKey(ws.ref) {
		delete(f.reuseIndex, key)
	}
}

func contextWithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func normalizeError(op string, err error) error {
	var workspaceErr *Error
	if errors.As(err, &workspaceErr) {
		return workspaceErr
	}
	return NewError(op, ErrorKindUnknown, "operation failed", false, err)
}

func coalesceRef(resultRef, requestRef WorkspaceRef) WorkspaceRef {
	if !resultRef.IsZero() {
		return resultRef
	}
	return requestRef
}

func workspaceKey(ref WorkspaceRef) string {
	if ref.Namespace == "" || ref.ClaimName == "" {
		return ""
	}
	return ref.Namespace + "/" + ref.ClaimName
}

func reuseIndexKey(namespace string, template TemplateRef, reuseKey string) string {
	return namespace + "/" + template.Namespace + "/" + template.Name + "/" + reuseKey
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func cleanArtifactPath(artifactPath string) (string, error) {
	artifactPath = strings.TrimSpace(artifactPath)
	if artifactPath == "" {
		return "", fmt.Errorf("artifact path is required")
	}
	artifactPath = path.Clean("/" + artifactPath)
	artifactPath = strings.TrimPrefix(artifactPath, "/")
	if artifactPath == "." || artifactPath == "" {
		return "", fmt.Errorf("artifact path is required")
	}
	return artifactPath, nil
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func truncateBytes(value string, maxBytes int64) (string, bool) {
	if maxBytes < 0 || int64(len(value)) <= maxBytes {
		return value, false
	}
	return value[:int(maxBytes)], true
}

func releaseMessage(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return fallback + ": " + strings.TrimSpace(reason)
}
