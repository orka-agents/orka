/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package fakeforge serves the subset of GitHub the clean-room workspace
// publisher uses: real Git smart HTTP for clone, fetch, and push (via
// `git http-backend`), plus the REST endpoints for pull request
// reconciliation. Its credentials are fixture values, not GitHub tokens.
// Never expose this fixture outside a test cluster.
//
//nolint:goconst // Keep the fixed JSON wire shapes readable.
package fakeforge

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/http/cgi"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
)

const (
	apiPrefix     = "/api/v3"
	defaultBranch = "main"
)

var (
	repositoryPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9-]*$`)
	gitPathPattern    = regexp.MustCompile(`^/([a-z0-9][a-z0-9-]*/[a-z0-9][a-z0-9-]*)\.git(/.*)$`)
)

// Config describes one fake forge.
type Config struct {
	// Host is the canonical host in repository URLs (no scheme or port 443).
	Host string
	// Root holds the bare repositories as <owner>/<name>.git.
	Root string
	// Token is the only accepted credential, for Git (Basic password or
	// Bearer) and the REST API (Bearer).
	Token string
	// Repositories are seeded as <owner>/<name> with one commit on main.
	Repositories []string
	// Git is the git binary; defaults to "git" on PATH.
	Git string
}

// Server is an in-memory fake forge over real bare repositories.
type Server struct {
	config Config
	git    string

	mu       sync.Mutex
	repos    map[string]int64
	pulls    []*pullRequest
	requests []RequestRecord
}

// RequestRecord is one request the forge accepted or rejected.
type RequestRecord struct {
	Method string `json:"method"`
	Path   string `json:"path"`
	Auth   string `json:"auth"`
	Status int    `json:"status"`
}

type pullRequest struct {
	Number     int64
	Title      string
	Body       string
	BaseRepo   string
	BaseBranch string
	HeadRepo   string
	HeadBranch string
}

// New seeds the configured repositories and returns the forge.
func New(ctx context.Context, config Config) (*Server, error) {
	if config.Host == "" || config.Root == "" || len(config.Token) < 8 {
		return nil, errors.New("fake forge requires a host, a root, and a token of at least 8 bytes")
	}
	git := config.Git
	if git == "" {
		git = "git"
	}
	// The CGI handler runs http-backend by absolute path.
	git, err := exec.LookPath(git)
	if err != nil {
		return nil, fmt.Errorf("find git: %w", err)
	}
	s := &Server{config: config, git: git, repos: map[string]int64{}}
	for i, repository := range config.Repositories {
		if !repositoryPattern.MatchString(repository) {
			return nil, fmt.Errorf("invalid repository %q", repository)
		}
		if err := s.seed(ctx, repository); err != nil {
			return nil, fmt.Errorf("seed %s: %w", repository, err)
		}
		s.repos[repository] = int64(1001 + i)
	}
	return s, nil
}

func (s *Server) seed(ctx context.Context, repository string) error {
	bare := s.bare(repository)
	if _, err := os.Stat(bare); err == nil {
		return nil
	}
	work, err := os.MkdirTemp("", "fakeforge-seed-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(work) //nolint:errcheck
	readme := "# " + repository + "\n\nSeeded by the Orka E2E fake forge.\n"
	if err := os.WriteFile(filepath.Join(work, "README.md"), []byte(readme), 0o600); err != nil {
		return err
	}
	for _, args := range [][]string{
		{"init", "--quiet", "--initial-branch=" + defaultBranch, work},
		{"-C", work, "add", "README.md"},
		{"-C", work, "-c", "user.name=Orka E2E", "-c", "user.email=e2e@orka.ai", "commit", "--quiet", "-m", "Initial commit"},
		{"init", "--quiet", "--bare", "--initial-branch=" + defaultBranch, bare},
		{"-C", work, "push", "--quiet", bare, defaultBranch},
		// The publisher fetches exact commits and pushes over smart HTTP.
		{"--git-dir", bare, "config", "http.receivepack", "true"},
		{"--git-dir", bare, "config", "uploadpack.allowAnySHA1InWant", "true"},
	} {
		if output, err := s.run(ctx, args...); err != nil {
			return fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, output)
		}
	}
	return nil
}

func (s *Server) bare(repository string) string {
	return filepath.Join(s.config.Root, repository+".git")
}

func (s *Server) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, s.git, args...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "HOME="+os.TempDir())
	return cmd.CombinedOutput()
}

// Handler routes Git smart HTTP, the REST API, and fixture state.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		switch {
		case r.URL.Path == "/healthz":
			recorder.WriteHeader(http.StatusOK)
			return
		case r.URL.Path == "/fake/state":
			s.serveState(recorder)
			return
		case r.URL.Path == "/fake/file":
			s.serveFile(recorder, r)
			return
		case strings.HasPrefix(r.URL.Path, apiPrefix+"/"):
			s.serveAPI(recorder, r)
		default:
			s.serveGit(recorder, r)
		}
		s.record(r, recorder.status)
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (s *Server) record(r *http.Request, status int) {
	scheme, _, _ := strings.Cut(r.Header.Get("Authorization"), " ")
	record := RequestRecord{Method: r.Method, Path: r.URL.Path, Auth: strings.ToLower(scheme), Status: status}
	log.Printf("%s %s auth=%q status=%d", record.Method, record.Path, record.Auth, record.Status)
	s.mu.Lock()
	defer s.mu.Unlock()
	s.requests = append(s.requests, record)
}

func (s *Server) authorized(r *http.Request, allowBasic bool) bool {
	scheme, value, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok {
		return false
	}
	switch strings.ToLower(scheme) {
	case "bearer":
		return subtle.ConstantTimeCompare([]byte(value), []byte(s.config.Token)) == 1
	case "basic":
		decoded, err := base64.StdEncoding.DecodeString(value)
		if !allowBasic || err != nil {
			return false
		}
		_, password, found := strings.Cut(string(decoded), ":")
		return found && subtle.ConstantTimeCompare([]byte(password), []byte(s.config.Token)) == 1
	}
	return false
}

func (s *Server) serveGit(w http.ResponseWriter, r *http.Request) {
	match := gitPathPattern.FindStringSubmatch(r.URL.Path)
	if match == nil || !s.known(match[1]) {
		http.NotFound(w, r)
		return
	}
	if !s.authorized(r, true) {
		w.Header().Set("WWW-Authenticate", `Basic realm="fakeforge"`)
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	handler := &cgi.Handler{
		Path:       s.git,
		Root:       "/",
		Args:       []string{"http-backend"},
		InheritEnv: []string{"PATH"},
		Env: []string{
			"GIT_PROJECT_ROOT=" + s.config.Root,
			"GIT_HTTP_EXPORT_ALL=1",
			"GIT_CONFIG_NOSYSTEM=1",
			"HOME=" + os.TempDir(),
			// http-backend enables receive-pack only for authenticated users.
			"REMOTE_USER=e2e",
		},
	}
	handler.ServeHTTP(w, r)
}

func (s *Server) known(repository string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.repos[repository]
	return ok
}

func (s *Server) serveAPI(w http.ResponseWriter, r *http.Request) {
	if !s.authorized(r, false) {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, apiPrefix+"/"), "/")
	if len(parts) < 3 || parts[0] != "repos" || !s.known(parts[1]+"/"+parts[2]) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	repository := parts[1] + "/" + parts[2]
	rest := parts[3:]
	switch {
	case len(rest) == 0 && r.Method == http.MethodGet:
		writeJSON(w, http.StatusOK, s.repository(repository))
	case len(rest) == 1 && rest[0] == "pulls" && r.Method == http.MethodGet:
		s.listPulls(w, r, repository)
	case len(rest) == 1 && rest[0] == "pulls" && r.Method == http.MethodPost:
		s.createPull(w, r, repository)
	case len(rest) >= 4 && rest[0] == "git" && rest[1] == "ref" && rest[2] == "heads" && r.Method == http.MethodGet:
		branch := strings.Join(rest[3:], "/")
		sha, err := s.branchSHA(r.Context(), repository, branch)
		if err != nil {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ref": "refs/heads/" + branch, "object": map[string]string{"type": "commit", "sha": sha},
		})
	default:
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
	}
}

func (s *Server) repository(repository string) map[string]any {
	s.mu.Lock()
	id := s.repos[repository]
	s.mu.Unlock()
	htmlURL := "https://" + s.config.Host + "/" + repository
	return map[string]any{
		"id": id, "node_id": "R_fakeforge_" + strconv.FormatInt(id, 10), "full_name": repository,
		"html_url": htmlURL, "clone_url": htmlURL + ".git",
	}
}

func (s *Server) branchSHA(ctx context.Context, repository, branch string) (string, error) {
	output, err := s.run(ctx, "--git-dir", s.bare(repository),
		"rev-parse", "--verify", "--quiet", "refs/heads/"+branch+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("branch %s not found in %s", branch, repository)
	}
	return strings.TrimSpace(string(output)), nil
}

func (s *Server) listPulls(w http.ResponseWriter, r *http.Request, repository string) {
	head := r.URL.Query().Get("head")
	base := r.URL.Query().Get("base")
	s.mu.Lock()
	var matches []pullRequest
	for _, pull := range s.pulls {
		owner, _, _ := strings.Cut(pull.HeadRepo, "/")
		if pull.BaseRepo == repository && (head == "" || head == owner+":"+pull.HeadBranch) &&
			(base == "" || base == pull.BaseBranch) {
			matches = append(matches, *pull)
		}
	}
	s.mu.Unlock()
	response := make([]map[string]any, 0, len(matches))
	for i := range matches {
		rendered, err := s.renderPull(r.Context(), &matches[i])
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		response = append(response, rendered)
	}
	writeJSON(w, http.StatusOK, response)
}

func (s *Server) createPull(w http.ResponseWriter, r *http.Request, repository string) {
	var request struct {
		Title    string `json:"title"`
		Body     string `json:"body"`
		Head     string `json:"head"`
		HeadRepo string `json:"head_repo"`
		Base     string `json:"base"`
	}
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid request"})
		return
	}
	headOwner, headBranch, ok := strings.Cut(request.Head, ":")
	headRepo := repository
	if request.HeadRepo != "" {
		headRepo = headOwner + "/" + request.HeadRepo
	}
	if !ok || headBranch == "" || request.Base == "" || !s.known(headRepo) {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "invalid head or base"})
		return
	}
	for _, check := range []struct{ repo, branch string }{{headRepo, headBranch}, {repository, request.Base}} {
		if _, err := s.branchSHA(r.Context(), check.repo, check.branch); err != nil {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": err.Error()})
			return
		}
	}
	s.mu.Lock()
	pull := &pullRequest{
		Number: int64(len(s.pulls) + 1), Title: request.Title, Body: request.Body,
		BaseRepo: repository, BaseBranch: request.Base, HeadRepo: headRepo, HeadBranch: headBranch,
	}
	s.pulls = append(s.pulls, pull)
	created := *pull
	s.mu.Unlock()
	rendered, err := s.renderPull(r.Context(), &created)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
		return
	}
	writeJSON(w, http.StatusCreated, rendered)
}

func (s *Server) renderPull(ctx context.Context, pull *pullRequest) (map[string]any, error) {
	headSHA, err := s.branchSHA(ctx, pull.HeadRepo, pull.HeadBranch)
	if err != nil {
		return nil, err
	}
	baseSHA, err := s.branchSHA(ctx, pull.BaseRepo, pull.BaseBranch)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"number":    pull.Number,
		"html_url":  "https://" + s.config.Host + "/" + pull.BaseRepo + "/pull/" + strconv.FormatInt(pull.Number, 10),
		"state":     "open",
		"merged_at": nil,
		"title":     pull.Title,
		"body":      pull.Body,
		"head":      map[string]any{"ref": pull.HeadBranch, "sha": headSHA, "repo": s.repository(pull.HeadRepo)},
		"base":      map[string]any{"ref": pull.BaseBranch, "sha": baseSHA, "repo": s.repository(pull.BaseRepo)},
	}, nil
}

// State is the fixture's observable state for test assertions.
type State struct {
	Branches map[string]map[string]string `json:"branches"`
	Pulls    []PullState                  `json:"pulls"`
	Requests []RequestRecord              `json:"requests"`
}

// PullState is one pull request as tests observe it.
type PullState struct {
	Number     int64  `json:"number"`
	Title      string `json:"title"`
	Body       string `json:"body"`
	BaseRepo   string `json:"baseRepo"`
	BaseBranch string `json:"baseBranch"`
	HeadRepo   string `json:"headRepo"`
	HeadBranch string `json:"headBranch"`
}

func (s *Server) serveState(w http.ResponseWriter) {
	ctx := context.Background()
	s.mu.Lock()
	repositories := make([]string, 0, len(s.repos))
	for repository := range s.repos {
		repositories = append(repositories, repository)
	}
	state := State{Branches: map[string]map[string]string{}, Requests: append([]RequestRecord(nil), s.requests...)}
	for _, pull := range s.pulls {
		state.Pulls = append(state.Pulls, PullState(*pull))
	}
	s.mu.Unlock()
	sort.Strings(repositories)
	for _, repository := range repositories {
		output, err := s.run(ctx, "--git-dir", s.bare(repository),
			"for-each-ref", "--format=%(refname:lstrip=2) %(objectname)", "refs/heads")
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]string{"message": err.Error()})
			return
		}
		branches := map[string]string{}
		for line := range strings.SplitSeq(strings.TrimSpace(string(output)), "\n") {
			if name, sha, ok := strings.Cut(line, " "); ok {
				branches[name] = sha
			}
		}
		state.Branches[repository] = branches
	}
	writeJSON(w, http.StatusOK, state)
}

// serveFile returns one file at a branch so tests can check published content.
func (s *Server) serveFile(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	repository, branch, file := query.Get("repo"), query.Get("ref"), query.Get("path")
	if !s.known(repository) || branch == "" || file == "" || strings.HasPrefix(branch, "-") {
		http.NotFound(w, r)
		return
	}
	output, err := s.run(r.Context(), "--git-dir", s.bare(repository), "show", "refs/heads/"+branch+":"+file)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(output)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	var body bytes.Buffer
	_ = json.NewEncoder(&body).Encode(value)
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_, _ = w.Write(body.Bytes())
}
