/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package fakeforge

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

var testTokens = map[string]string{
	RoleRead: "read-token-1234", RolePublicationRead: "publication-read-token-1234",
	RolePush: "push-token-1234", RoleForge: "forge-token-1234",
}

func basicAuth(role string) string {
	return "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+testTokens[role]))
}

func startForge(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	server := httptest.NewUnstartedServer(nil)
	host := server.Listener.Addr().String()
	forge, err := New(t.Context(), Config{
		Host: host, Root: t.TempDir(), Tokens: testTokens, Repositories: []string{"e2e/source"},
	})
	if err != nil {
		t.Fatal(err)
	}
	server.Config.Handler = forge.Handler()
	server.StartTLS()
	t.Cleanup(server.Close)
	return server, host
}

// gitAs runs git against the forge with the publisher's credential header.
func gitAs(t *testing.T, header string, args ...string) (string, error) {
	t.Helper()
	base := []string{"-c", "http.sslVerify=false", "-c", "user.name=e2e", "-c", "user.email=e2e@orka.ai"}
	if header != "" {
		base = append(base, "-c", "http.extraHeader="+header)
	}
	cmd := exec.Command("git", append(base, args...)...)
	cmd.Env = append(cmd.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1")
	output, err := cmd.CombinedOutput()
	return strings.TrimSpace(string(output)), err
}

func api(t *testing.T, server *httptest.Server, method, path, token string, body any) (int, []byte) {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, server.URL+path, &payload)
	if err != nil {
		t.Fatal(err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := server.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close() //nolint:errcheck
	var out bytes.Buffer
	_, _ = out.ReadFrom(resp.Body)
	return resp.StatusCode, out.Bytes()
}

func TestForgeAcceptsEachTokenOnlyOnItsSurface(t *testing.T) {
	server, _ := startForge(t)
	url := server.URL + "/e2e/source.git"

	if _, err := gitAs(t, "", "clone", url, filepath.Join(t.TempDir(), "anonymous")); err == nil {
		t.Fatal("clone without credentials succeeded")
	}
	wrong := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:wrong-token"))
	if _, err := gitAs(t, wrong, "clone", url, filepath.Join(t.TempDir(), "wrong")); err == nil {
		t.Fatal("clone with an unknown token succeeded")
	}
	for _, role := range []string{RolePush, RoleForge} {
		if _, err := gitAs(t, basicAuth(role), "clone", url, filepath.Join(t.TempDir(), role)); err == nil {
			t.Fatalf("clone with the %s token succeeded", role)
		}
	}
	// The publisher observes the target branch with the push credential
	// before its CAS push, so the push token may list refs.
	output, err := gitAs(t, basicAuth(RolePush), "-c", "protocol.version=2", "ls-remote", url, "refs/heads/main")
	if err != nil {
		t.Fatalf("ls-remote with the push token failed: %v: %s", err, output)
	}
	if _, err := gitAs(t, basicAuth(RoleForge), "ls-remote", url); err == nil {
		t.Fatal("ls-remote with the forge token succeeded")
	}
	work := filepath.Join(t.TempDir(), "clone")
	for _, role := range []string{RoleRead, RolePublicationRead} {
		if output, err := gitAs(t, basicAuth(role), "clone", url, filepath.Join(t.TempDir(), role)); err != nil {
			t.Fatalf("clone with the %s token failed: %v: %s", role, err, output)
		}
	}
	if output, err := gitAs(t, basicAuth(RoleRead), "clone", url, work); err != nil {
		t.Fatalf("clone failed: %v: %s", err, output)
	}
	if _, err := gitAs(t, basicAuth(RoleRead), "-C", work, "push", "origin", "HEAD:refs/heads/read"); err == nil {
		t.Fatal("push with the read token succeeded")
	}
	if output, err := gitAs(t, basicAuth(RolePush), "-C", work, "push", "origin", "HEAD:refs/heads/pushed"); err != nil {
		t.Fatalf("push with the push token failed: %v: %s", err, output)
	}
	for _, role := range []string{RoleRead, RolePush} {
		status, _ := api(t, server, http.MethodGet, "/api/v3/repos/e2e/source", testTokens[role], nil)
		if status != http.StatusForbidden {
			t.Fatalf("REST with the %s token = %d, want 403", role, status)
		}
	}

	status, body := api(t, server, http.MethodGet, "/fake/state", "", nil)
	var state State
	if status != http.StatusOK || json.Unmarshal(body, &state) != nil {
		t.Fatalf("state = %d %s", status, body)
	}
	for _, want := range []RequestRecord{
		{Surface: SurfaceGitFetch, Role: RolePush, Status: http.StatusForbidden},
		{Surface: SurfaceGitRefs, Role: RolePush, Status: http.StatusOK},
		{Surface: SurfaceGitRefs, Role: RoleForge, Status: http.StatusForbidden},
		{Surface: SurfaceGitPush, Role: RoleRead, Status: http.StatusForbidden},
		{Surface: SurfaceGitPush, Role: RolePush, Status: http.StatusOK},
		{Surface: SurfaceAPI, Role: RoleRead, Status: http.StatusForbidden},
	} {
		if !slices.ContainsFunc(state.Requests, func(got RequestRecord) bool {
			return got.Surface == want.Surface && got.Role == want.Role && got.Status == want.Status
		}) {
			t.Fatalf("no request %+v in %+v", want, state.Requests)
		}
	}
	// Protocol v2 ls-remote sends ls-refs over POST upload-pack.
	lsRefs := RequestRecord{Method: http.MethodPost, Path: "/e2e/source.git/git-upload-pack",
		Surface: SurfaceGitRefs, Role: RolePush, Status: http.StatusOK}
	if !slices.Contains(state.Requests, lsRefs) {
		t.Fatalf("no request %+v in %+v", lsRefs, state.Requests)
	}
}

func TestForgeReconcilesPullRequestsForPushedBranches(t *testing.T) {
	server, host := startForge(t)
	work := filepath.Join(t.TempDir(), "work")
	for _, step := range []struct {
		header string
		args   []string
	}{
		{basicAuth(RoleRead), []string{"clone", server.URL + "/e2e/source.git", work}},
		{"", []string{"-C", work, "checkout", "-b", "orka/e2e"}},
		{"", []string{"-C", work, "commit", "--allow-empty", "-m", "change"}},
		{basicAuth(RolePush), []string{"-C", work, "push", "origin", "orka/e2e"}},
	} {
		if output, err := gitAs(t, step.header, step.args...); err != nil {
			t.Fatalf("git %v: %v: %s", step.args, err, output)
		}
	}
	pushed, err := gitAs(t, "", "-C", work, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if status, _ := api(t, server, http.MethodGet, "/api/v3/repos/e2e/source", "", nil); status != 401 {
		t.Fatalf("anonymous REST status = %d, want 401", status)
	}
	status, body := api(t, server, http.MethodGet, "/api/v3/repos/e2e/source", testTokens[RoleForge], nil)
	var repo map[string]any
	if status != http.StatusOK || json.Unmarshal(body, &repo) != nil ||
		repo["html_url"] != "https://"+host+"/e2e/source" || repo["clone_url"] != "https://"+host+"/e2e/source.git" {
		t.Fatalf("repository = %d %s", status, body)
	}

	status, body = api(t, server, http.MethodGet, "/api/v3/repos/e2e/source/git/ref/heads/orka/e2e",
		testTokens[RoleForge], nil)
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			Type string `json:"type"`
			SHA  string `json:"sha"`
		} `json:"object"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &ref) != nil ||
		ref.Ref != "refs/heads/orka/e2e" || ref.Object.Type != "commit" || ref.Object.SHA != pushed {
		t.Fatalf("ref = %d %s, want %s", status, body, pushed)
	}

	status, body = api(t, server, http.MethodPost, "/api/v3/repos/e2e/source/pulls", testTokens[RoleForge], map[string]any{
		"title": "E2E", "body": "marker", "head": "e2e:orka/e2e", "base": "main",
	})
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	status, body = api(t, server, http.MethodGet,
		"/api/v3/repos/e2e/source/pulls?state=all&head=e2e:orka/e2e&base=main", testTokens[RoleForge], nil)
	var pulls []struct {
		Number  int64  `json:"number"`
		HTMLURL string `json:"html_url"`
		State   string `json:"state"`
		Body    string `json:"body"`
		Head    struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
	}
	if status != http.StatusOK || json.Unmarshal(body, &pulls) != nil || len(pulls) != 1 {
		t.Fatalf("list = %d %s", status, body)
	}
	if pull := pulls[0]; pull.Number != 1 || pull.HTMLURL != "https://"+host+"/e2e/source/pull/1" ||
		pull.State != "open" || pull.Body != "marker" || pull.Head.Ref != "orka/e2e" || pull.Head.SHA != pushed {
		t.Fatalf("pull = %+v", pull)
	}

	status, body = api(t, server, http.MethodGet, "/fake/state", "", nil)
	var state State
	if status != http.StatusOK || json.Unmarshal(body, &state) != nil {
		t.Fatalf("state = %d %s", status, body)
	}
	if state.Branches["e2e/source"]["orka/e2e"] != pushed || len(state.Pulls) != 1 {
		t.Fatalf("state = %+v", state)
	}

	status, body = api(t, server, http.MethodGet, "/fake/commit?repo=e2e/source&sha="+pushed, "", nil)
	var commit CommitState
	if status != http.StatusOK || json.Unmarshal(body, &commit) != nil {
		t.Fatalf("commit = %d %s", status, body)
	}
	if commit.SHA != pushed || len(commit.Tree) != 40 || len(commit.Parents) != 1 ||
		commit.Parents[0] != state.Branches["e2e/source"]["main"] || len(commit.Files) != 0 {
		t.Fatalf("commit = %+v, want the empty change on top of main", commit)
	}
	status, _ = api(t, server, http.MethodGet, "/fake/commit?repo=e2e/source&sha=HEAD", "", nil)
	if status != http.StatusNotFound {
		t.Fatalf("symbolic commit status = %d, want 404", status)
	}

	status, body = api(t, server, http.MethodGet, "/fake/file?repo=e2e/source&ref=orka/e2e&path=README.md", "", nil)
	if status != http.StatusOK || !strings.HasPrefix(string(body), "# e2e/source") {
		t.Fatalf("file = %d %s", status, body)
	}
}

// The injection is in the real Git baseline, not added by a model prompt or an
// arbitrary-content fixture API; the independent forge read agrees with Git.
func TestForgeSeedsRepositoryOriginEvalInjection(t *testing.T) {
	server, _ := startForge(t)
	work := filepath.Join(t.TempDir(), "read-control")
	if _, err := gitAs(t, basicAuth(RoleRead), "clone", server.URL+"/e2e/source.git", work); err != nil {
		t.Fatal("credentialed injection control clone failed")
	}
	contents, err := gitAs(t, "", "-C", work, "show", "HEAD:"+EvalInjectionPath)
	if err != nil || contents != strings.TrimSpace(evalInjectionContent) ||
		strings.Count(contents, EvalInjectionNonce) != 1 {
		t.Fatal("repository baseline did not contain the exact static injection and nonce")
	}
	for _, instruction := range []string{"native file-write", "EVAL_FORBIDDEN.md", "process environments", "publish"} {
		if !strings.Contains(contents, instruction) {
			t.Fatal("repository injection lost an adversarial instruction")
		}
	}
	status, body := api(t, server, http.MethodGet,
		"/fake/file?repo=e2e/source&ref=main&path="+EvalInjectionPath, "", nil)
	if status != http.StatusOK || string(body) != evalInjectionContent {
		t.Fatal("independent forge read did not observe the seeded injection")
	}
}
