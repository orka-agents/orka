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
	"strings"
	"testing"
)

const testToken = "fixture-token-1234"

func startForge(t *testing.T) (*httptest.Server, string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is required")
	}
	server := httptest.NewUnstartedServer(nil)
	host := server.Listener.Addr().String()
	forge, err := New(t.Context(), Config{
		Host: host, Root: t.TempDir(), Token: testToken, Repositories: []string{"e2e/source"},
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

func TestForgeServesCloneOnlyWithTheToken(t *testing.T) {
	server, _ := startForge(t)
	url := server.URL + "/e2e/source.git"

	if _, err := gitAs(t, "", "clone", url, filepath.Join(t.TempDir(), "anonymous")); err == nil {
		t.Fatal("clone without credentials succeeded")
	}
	wrong := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:wrong-token"))
	if _, err := gitAs(t, wrong, "clone", url, filepath.Join(t.TempDir(), "wrong")); err == nil {
		t.Fatal("clone with the wrong token succeeded")
	}
	basic := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+testToken))
	if output, err := gitAs(t, basic, "clone", url, filepath.Join(t.TempDir(), "clone")); err != nil {
		t.Fatalf("clone with the token failed: %v: %s", err, output)
	}
}

func TestForgeReconcilesPullRequestsForPushedBranches(t *testing.T) {
	server, host := startForge(t)
	header := "Authorization: Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+testToken))
	work := filepath.Join(t.TempDir(), "work")
	for _, args := range [][]string{
		{"clone", server.URL + "/e2e/source.git", work},
		{"-C", work, "checkout", "-b", "orka/e2e"},
		{"-C", work, "commit", "--allow-empty", "-m", "change"},
		{"-C", work, "push", "origin", "orka/e2e"},
	} {
		if output, err := gitAs(t, header, args...); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, output)
		}
	}
	pushed, err := gitAs(t, "", "-C", work, "rev-parse", "HEAD")
	if err != nil {
		t.Fatal(err)
	}

	if status, _ := api(t, server, http.MethodGet, "/api/v3/repos/e2e/source", "", nil); status != 401 {
		t.Fatalf("anonymous REST status = %d, want 401", status)
	}
	status, body := api(t, server, http.MethodGet, "/api/v3/repos/e2e/source", testToken, nil)
	var repo map[string]any
	if status != http.StatusOK || json.Unmarshal(body, &repo) != nil ||
		repo["html_url"] != "https://"+host+"/e2e/source" || repo["clone_url"] != "https://"+host+"/e2e/source.git" {
		t.Fatalf("repository = %d %s", status, body)
	}

	status, body = api(t, server, http.MethodGet, "/api/v3/repos/e2e/source/git/ref/heads/orka/e2e", testToken, nil)
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

	status, body = api(t, server, http.MethodPost, "/api/v3/repos/e2e/source/pulls", testToken, map[string]any{
		"title": "E2E", "body": "marker", "head": "e2e:orka/e2e", "base": "main",
	})
	if status != http.StatusCreated {
		t.Fatalf("create = %d %s", status, body)
	}
	status, body = api(t, server, http.MethodGet,
		"/api/v3/repos/e2e/source/pulls?state=all&head=e2e:orka/e2e&base=main", testToken, nil)
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

	status, body = api(t, server, http.MethodGet, "/fake/file?repo=e2e/source&ref=orka/e2e&path=README.md", "", nil)
	if status != http.StatusOK || !strings.HasPrefix(string(body), "# e2e/source") {
		t.Fatalf("file = %d %s", status, body)
	}
}
