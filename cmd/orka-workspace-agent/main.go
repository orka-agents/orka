/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	defaultListenAddr            = ":80"
	defaultCommandTimeout        = 30 * time.Minute
	completedExecutionRetention  = 15 * time.Minute
	defaultMaxOutputBytes        = 1 << 20
	defaultMaxRequestBytes       = 256 << 20
	defaultHandoffTokenFile      = "/app/orka-workspace-handoff-token"
	defaultHandoffTokenUpload    = "orka-workspace-handoff-token"
	envListenAddr                = "ORKA_WORKSPACE_AGENT_LISTEN_ADDR"
	envHandoffToken              = "ORKA_WORKSPACE_HANDOFF_TOKEN"
	envHandoffTokenFile          = "ORKA_WORKSPACE_HANDOFF_TOKEN_FILE"
	envDefaultCommandTimeoutSecs = "ORKA_WORKSPACE_AGENT_DEFAULT_COMMAND_TIMEOUT_SECONDS"
	envDefaultMaxOutputBytes     = "ORKA_WORKSPACE_AGENT_MAX_OUTPUT_BYTES"
	envMaxRequestBytes           = "ORKA_WORKSPACE_AGENT_MAX_REQUEST_BYTES"
)

var allowedRoots = []string{"/app", "/workspace", "/home/worker", "/tmp"}

func main() {
	if err := run(); err != nil {
		slog.Error("workspace agent failed", "err", err)
		os.Exit(1)
	}
}

func run() error {
	addr := strings.TrimSpace(os.Getenv(envListenAddr))
	if addr == "" {
		addr = defaultListenAddr
	}
	server := newWorkspaceAgentServer()
	slog.Info("starting workspace agent", "addr", addr)
	return http.ListenAndServe(addr, server.routes())
}

type workspaceAgentServer struct {
	defaultCommandTimeout time.Duration
	defaultMaxOutputBytes int64
	maxRequestBytes       int64

	mu         sync.Mutex
	executions map[string]execResponse
}

func newWorkspaceAgentServer() *workspaceAgentServer {
	return &workspaceAgentServer{
		defaultCommandTimeout: durationEnvSeconds(envDefaultCommandTimeoutSecs, defaultCommandTimeout),
		defaultMaxOutputBytes: int64Env(envDefaultMaxOutputBytes, defaultMaxOutputBytes),
		maxRequestBytes:       int64Env(envMaxRequestBytes, defaultMaxRequestBytes),
		executions:            make(map[string]execResponse),
	}
}

func (s *workspaceAgentServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("/v1/exec", s.requireAuth(s.handleExec))
	mux.HandleFunc("/v1/exec/", s.requireAuth(s.handleExecStatus))
	mux.HandleFunc("/v1/files", s.requireAuth(s.handleFiles))
	mux.HandleFunc("/v1/files/download", s.requireAuth(s.handleDownload))
	mux.HandleFunc("/v1/scrub", s.requireAuth(s.handleScrub))
	return mux
}

func (s *workspaceAgentServer) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, err := handoffToken()
		if err != nil {
			allowBootstrap, handled := s.allowHandoffBootstrap(w, r)
			if allowBootstrap {
				next(w, r)
				return
			}
			if handled {
				return
			}
			http.Error(w, "handoff credential unavailable", http.StatusServiceUnavailable)
			return
		}
		if !validHandoffBearer(r.Header.Get("Authorization"), token) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (s *workspaceAgentServer) allowHandoffBootstrap(w http.ResponseWriter, r *http.Request) (bool, bool) {
	if r.Method != http.MethodPut || r.URL.Path != "/v1/files" {
		return false, false
	}
	data, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "invalid handoff bootstrap request", http.StatusBadRequest)
		return false, true
	}

	var req uploadRequest
	if err := json.Unmarshal(data, &req); err != nil || len(req.Files) != 1 {
		http.Error(w, "invalid handoff bootstrap request", http.StatusBadRequest)
		return false, true
	}
	path := filepath.Clean(req.Files[0].Path)
	requestedPath, err := normalizeAgentPath(path)
	if err != nil {
		http.Error(w, "invalid handoff bootstrap path", http.StatusUnauthorized)
		return false, true
	}
	tokenPath := handoffTokenFilePath()
	isDefaultUploadAlias := path == defaultHandoffTokenUpload || requestedPath == defaultHandoffTokenFile
	if !isDefaultUploadAlias && requestedPath != tokenPath {
		http.Error(w, "invalid handoff bootstrap path", http.StatusUnauthorized)
		return false, true
	}
	tokenValue := strings.TrimSpace(string(req.Files[0].Data))
	if tokenValue == "" {
		http.Error(w, "empty handoff bootstrap token", http.StatusBadRequest)
		return false, true
	}
	if !validHandoffBearer(r.Header.Get("Authorization"), tokenValue) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false, true
	}
	req.Files[0].Data = []byte(tokenValue)
	req.Files[0].Path = tokenPath
	data, err = json.Marshal(req)
	if err != nil {
		http.Error(w, "invalid handoff bootstrap request", http.StatusBadRequest)
		return false, true
	}
	r.Body.Close() //nolint:errcheck
	r.Body = io.NopCloser(bytes.NewReader(data))
	return true, true
}

func handoffToken() (string, error) {
	if token := strings.TrimSpace(os.Getenv(envHandoffToken)); token != "" {
		return token, nil
	}
	data, err := os.ReadFile(handoffTokenFilePath())
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("handoff token file is empty")
	}
	return token, nil
}

func handoffTokenFilePath() string {
	path := strings.TrimSpace(os.Getenv(envHandoffTokenFile))
	if path == "" {
		path = defaultHandoffTokenFile
	}
	normalized, err := normalizeAgentPath(path)
	if err != nil {
		return defaultHandoffTokenFile
	}
	return normalized
}

func normalizeAgentPath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join("/app", value)
	}
	return filepath.Clean(value), nil
}

func validHandoffBearer(header, token string) bool {
	got, ok := bearerToken(header)
	if !ok {
		return false
	}
	if got == "" || strings.TrimSpace(token) == "" {
		return false
	}
	gotHash := sha256.Sum256([]byte(got))
	tokenHash := sha256.Sum256([]byte(token))
	return subtle.ConstantTimeCompare(gotHash[:], tokenHash[:]) == 1
}

func bearerToken(header string) (string, bool) {
	const prefix = "Bearer "
	if !strings.HasPrefix(header, prefix) {
		return "", false
	}
	return strings.TrimSpace(strings.TrimPrefix(header, prefix)), true
}

func (s *workspaceAgentServer) handleExec(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req execRequest
	if err := s.decodeJSON(r, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if len(req.Command) == 0 || strings.TrimSpace(req.Command[0]) == "" {
		http.Error(w, "command is required", http.StatusBadRequest)
		return
	}
	normalized, err := s.normalizeExecRequest(req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Detach {
		id, err := newExecID()
		if err != nil {
			http.Error(w, "failed to create execution", http.StatusInternalServerError)
			return
		}
		s.storeExecution(execResponse{ExecID: id, Running: true})
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), normalized.timeout)
			defer cancel()
			resp := s.runExec(ctx, req, normalized)
			resp.ExecID = id
			s.storeExecution(resp)
		}()
		writeJSON(w, execResponse{ExecID: id, Running: true})
		return
	}

	writeJSON(w, s.runExec(r.Context(), req, normalized))
}

func (s *workspaceAgentServer) handleExecStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	id := strings.Trim(strings.TrimPrefix(r.URL.Path, "/v1/exec/"), "/")
	if id == "" {
		http.Error(w, "execution id is required", http.StatusBadRequest)
		return
	}
	resp, ok := s.loadExecution(id)
	if !ok {
		http.Error(w, "execution not found", http.StatusNotFound)
		return
	}
	writeJSON(w, resp)
}

type normalizedExecRequest struct {
	workDir   string
	timeout   time.Duration
	maxOutput int64
}

func (s *workspaceAgentServer) normalizeExecRequest(req execRequest) (normalizedExecRequest, error) {
	workDir := strings.TrimSpace(req.WorkDir)
	if workDir == "" {
		workDir = "/workspace"
	}
	safeWorkDir, err := safePath(workDir)
	if err != nil {
		return normalizedExecRequest{}, fmt.Errorf("invalid workDir")
	}
	timeout := s.defaultCommandTimeout
	if req.TimeoutSeconds > 0 {
		timeout = time.Duration(req.TimeoutSeconds) * time.Second
	}
	maxOutput := req.MaxOutputBytes
	if maxOutput <= 0 || maxOutput > s.defaultMaxOutputBytes {
		maxOutput = s.defaultMaxOutputBytes
	}
	return normalizedExecRequest{workDir: safeWorkDir, timeout: timeout, maxOutput: maxOutput}, nil
}

func (s *workspaceAgentServer) runExec(
	ctx context.Context,
	req execRequest,
	normalized normalizedExecRequest,
) execResponse {
	ctx, cancel := context.WithTimeout(ctx, normalized.timeout)
	defer cancel()
	startedAt := time.Now().UTC()
	cmd := exec.CommandContext(ctx, req.Command[0], req.Command[1:]...)
	cmd.Dir = normalized.workDir
	cmd.Env = mergeEnv(os.Environ(), req.Env)
	if len(req.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(req.Stdin)
	}
	stdout := &boundedBuffer{limit: normalized.maxOutput}
	stderr := &boundedBuffer{limit: normalized.maxOutput}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	err := cmd.Run()
	finishedAt := time.Now().UTC()
	exitCode := 0
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			exitCode = exitErr.ExitCode()
		} else if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			exitCode = 124
		} else {
			exitCode = 1
		}
	}
	return execResponse{
		Stdout:          stdout.String(),
		Stderr:          stderr.String(),
		ExitCode:        exitCode,
		StdoutTruncated: stdout.truncated,
		StderrTruncated: stderr.truncated,
		StartedAt:       startedAt,
		FinishedAt:      finishedAt,
	}
}

func (s *workspaceAgentServer) storeExecution(resp execResponse) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictCompletedExecutionsLocked(time.Now().UTC())
	s.executions[resp.ExecID] = resp
}

func (s *workspaceAgentServer) loadExecution(id string) (execResponse, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.evictCompletedExecutionsLocked(time.Now().UTC())
	resp, ok := s.executions[id]
	if ok && !resp.Running {
		delete(s.executions, id)
	}
	return resp, ok
}

func (s *workspaceAgentServer) evictCompletedExecutionsLocked(now time.Time) {
	for id, resp := range s.executions {
		if resp.Running || resp.FinishedAt.IsZero() {
			continue
		}
		if now.Sub(resp.FinishedAt) >= completedExecutionRetention {
			delete(s.executions, id)
		}
	}
}

func newExecID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func (s *workspaceAgentServer) handleFiles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req uploadRequest
	if err := s.decodeJSON(r, &req); err != nil || len(req.Files) == 0 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	artifacts := make([]artifact, 0, len(req.Files))
	for _, file := range req.Files {
		path, err := safePath(file.Path)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			http.Error(w, "failed to create parent directory", http.StatusInternalServerError)
			return
		}
		mode := os.FileMode(file.Mode)
		if mode == 0 {
			mode = 0o644
		}
		if err := os.WriteFile(path, file.Data, mode); err != nil {
			http.Error(w, "failed to write file", http.StatusInternalServerError)
			return
		}
		if !file.ModTime.IsZero() {
			_ = os.Chtimes(path, file.ModTime, file.ModTime)
		}
		artifacts = append(artifacts, fileArtifact(path, file.Path, file.Data, uint32(mode), file.ModTime))
	}
	writeJSON(w, uploadResponse{Artifacts: artifacts})
}

func (s *workspaceAgentServer) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req downloadRequest
	if err := s.decodeJSON(r, &req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	paths := req.Paths
	if len(paths) == 0 {
		listed, err := listWorkspaceFiles("/workspace")
		if err != nil {
			http.Error(w, "failed to list files", http.StatusInternalServerError)
			return
		}
		paths = listed
	}
	artifacts := make([]downloadedArtifact, 0, len(paths))
	for _, requested := range paths {
		path, err := safePath(requested)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		data, err := os.ReadFile(path)
		if err != nil {
			http.Error(w, "failed to read file", http.StatusInternalServerError)
			return
		}
		info, _ := os.Stat(path)
		modTime := time.Time{}
		mode := uint32(0o644)
		if info != nil {
			modTime = info.ModTime()
			mode = uint32(info.Mode().Perm())
		}
		artifacts = append(artifacts, downloadedArtifact{
			Artifact: artifact{
				Path:    cleanReportedPath(requested),
				Size:    int64(len(data)),
				Digest:  digest(data),
				Mode:    mode,
				ModTime: modTime,
			},
			Data: data,
		})
	}
	writeJSON(w, downloadResponse{Artifacts: artifacts})
}

func (s *workspaceAgentServer) handleScrub(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req scrubRequest
	if err := s.decodeJSON(r, &req); err != nil || len(req.Paths) == 0 {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	req.Paths = appendUniquePath(req.Paths, handoffTokenFilePath())
	for _, requested := range req.Paths {
		path, err := safePath(requested)
		if err != nil {
			http.Error(w, "invalid path", http.StatusBadRequest)
			return
		}
		if err := os.RemoveAll(path); err != nil {
			http.Error(w, "failed to scrub path", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func appendUniquePath(paths []string, path string) []string {
	for _, existing := range paths {
		if filepath.Clean(existing) == filepath.Clean(path) {
			return paths
		}
	}
	return append(paths, path)
}

type execRequest struct {
	Command        []string          `json:"command"`
	Env            map[string]string `json:"env,omitempty"`
	WorkDir        string            `json:"workDir,omitempty"`
	Stdin          []byte            `json:"stdin,omitempty"`
	TimeoutSeconds int64             `json:"timeoutSeconds,omitempty"`
	MaxOutputBytes int64             `json:"maxOutputBytes,omitempty"`
	Detach         bool              `json:"detach,omitempty"`
}

type execResponse struct {
	ExecID          string    `json:"execId,omitempty"`
	Running         bool      `json:"running,omitempty"`
	Stdout          string    `json:"stdout"`
	Stderr          string    `json:"stderr"`
	ExitCode        int       `json:"exitCode"`
	StdoutTruncated bool      `json:"stdoutTruncated"`
	StderrTruncated bool      `json:"stderrTruncated"`
	StartedAt       time.Time `json:"startedAt"`
	FinishedAt      time.Time `json:"finishedAt"`
}

type uploadRequest struct {
	Files []uploadFile `json:"files"`
}

type uploadFile struct {
	Path    string    `json:"path"`
	Data    []byte    `json:"data"`
	Mode    uint32    `json:"mode,omitempty"`
	ModTime time.Time `json:"modTime,omitempty"`
}

type uploadResponse struct {
	Artifacts []artifact `json:"artifacts"`
}

type downloadRequest struct {
	Paths []string `json:"paths,omitempty"`
}

type downloadResponse struct {
	Artifacts []downloadedArtifact `json:"artifacts"`
}

type scrubRequest struct {
	Paths []string `json:"paths"`
}

type artifact struct {
	Path    string    `json:"path"`
	Size    int64     `json:"size"`
	Digest  string    `json:"digest"`
	Mode    uint32    `json:"mode"`
	ModTime time.Time `json:"modTime"`
}

type downloadedArtifact struct {
	Artifact
	Data []byte `json:"data"`
}

type Artifact = artifact

func (s *workspaceAgentServer) decodeJSON(r *http.Request, out any) error {
	limit := s.maxRequestBytes
	if limit <= 0 {
		limit = defaultMaxRequestBytes
	}
	return json.NewDecoder(io.LimitReader(r.Body, limit)).Decode(out)
}

func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(value); err != nil {
		slog.Error("failed to encode response", "err", err)
	}
}

func safePath(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("path is required")
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join("/app", value)
	}
	clean := filepath.Clean(value)
	for _, root := range allowedRoots {
		root = filepath.Clean(root)
		if pathWithinRoot(clean, root) {
			if err := validateResolvedPath(clean, root); err != nil {
				return "", err
			}
			return clean, nil
		}
	}
	return "", fmt.Errorf("path escapes allowed roots")
}

func validateResolvedPath(path, root string) error {
	resolved, err := resolveExistingPrefix(path)
	if err != nil {
		return fmt.Errorf("resolve path: %w", err)
	}
	resolvedRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		resolvedRoot = root
	}
	if pathWithinRoot(resolved, root) || pathWithinRoot(resolved, resolvedRoot) {
		return nil
	}
	return fmt.Errorf("path escapes allowed root")
}

func resolveExistingPrefix(path string) (string, error) {
	path = filepath.Clean(path)
	current := path
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			parts := append([]string{resolved}, suffix...)
			return filepath.Clean(filepath.Join(parts...)), nil
		}
		if !os.IsNotExist(err) {
			return "", err
		}
		if info, lstatErr := os.Lstat(current); lstatErr == nil {
			if info.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("dangling symlink %s", current)
			}
			return "", err
		} else if !os.IsNotExist(lstatErr) {
			return "", lstatErr
		}
		parent := filepath.Dir(current)
		if parent == current {
			return path, nil
		}
		suffix = append([]string{filepath.Base(current)}, suffix...)
		current = parent
	}
}

func pathWithinRoot(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	return path == root || strings.HasPrefix(path, root+string(os.PathSeparator))
}

func cleanReportedPath(path string) string {
	path = strings.TrimSpace(path)
	if filepath.IsAbs(path) {
		for _, root := range allowedRoots {
			if rel, err := filepath.Rel(root, path); err == nil && !strings.HasPrefix(rel, "..") {
				return rel
			}
		}
	}
	return strings.TrimPrefix(filepath.Clean(path), "/")
}

func listWorkspaceFiles(root string) ([]string, error) {
	root, err := safePath(root)
	if err != nil {
		return nil, err
	}
	var paths []string
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		paths = append(paths, path)
		return nil
	})
	return paths, err
}

func mergeEnv(base []string, overrides map[string]string) []string {
	out := make([]string, 0, len(base)+len(overrides))
	seen := make(map[string]struct{}, len(base)+len(overrides))
	for _, item := range base {
		name, _, ok := strings.Cut(item, "=")
		if !ok || name == "" {
			continue
		}
		if _, exists := overrides[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, item)
	}
	for name, value := range overrides {
		if name == "" {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		out = append(out, name+"="+value)
	}
	return out
}

func fileArtifact(absPath, requestedPath string, data []byte, mode uint32, modTime time.Time) artifact {
	if modTime.IsZero() {
		if info, err := os.Stat(absPath); err == nil {
			modTime = info.ModTime()
		}
	}
	return artifact{
		Path:    cleanReportedPath(requestedPath),
		Size:    int64(len(data)),
		Digest:  digest(data),
		Mode:    mode,
		ModTime: modTime,
	}
}

func digest(data []byte) string {
	sum := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(sum[:])
}

type boundedBuffer struct {
	buf       bytes.Buffer
	limit     int64
	written   int64
	truncated bool
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		b.limit = defaultMaxOutputBytes
	}
	remaining := b.limit - b.written
	if remaining > 0 {
		chunk := p
		if int64(len(chunk)) > remaining {
			chunk = chunk[:remaining]
			b.truncated = true
		}
		_, _ = b.buf.Write(chunk)
		b.written += int64(len(chunk))
	} else {
		b.truncated = true
	}
	if int64(len(p)) > remaining {
		b.truncated = true
	}
	return len(p), nil
}

func (b *boundedBuffer) String() string {
	return b.buf.String()
}

func durationEnvSeconds(name string, fallback time.Duration) time.Duration {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	seconds, err := strconv.ParseInt(value, 10, 64)
	if err != nil || seconds <= 0 {
		return fallback
	}
	return time.Duration(seconds) * time.Second
}

func int64Env(name string, fallback int64) int64 {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseInt(value, 10, 64)
	if err != nil || parsed <= 0 {
		return fallback
	}
	return parsed
}
