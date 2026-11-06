/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/microsoft/agent-host-protocol/clients/go/ahptypes"
	"github.com/stretchr/testify/require"

	"github.com/orka-agents/orka/internal/ahphost"
)

func TestAHPWorkspacePath(t *testing.T) {
	for uri, want := range map[string]string{
		"file:///workspace/src/a/b/README.md": "/workspace/src/a/b/README.md",
		"file:///workspace":                   "/workspace",
		"file:///workspace/src/../src/x":      "/workspace/src/x",
	} {
		got, ok := ahpWorkspacePath(uri)
		require.True(t, ok, uri)
		require.Equal(t, want, got)
	}
	for _, uri := range []string{"file:///etc/passwd", "file:///workspace/../etc/passwd", "file:///workspacex/a", "https://example.com/a", "file://", "file:///workspace/a\nb"} {
		_, ok := ahpWorkspacePath(uri)
		require.False(t, ok, uri)
	}
}

// A workspace path can be a symlink out of the workspace, such as one a cloned
// repository carries to the workspace's git token: reads refuse it as not
// found, and still follow links that stay inside.
func TestAHPWorkspaceReadsStayInTheWorkspace(t *testing.T) {
	base, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	root, outside := filepath.Join(base, "workspace"), filepath.Join(base, "tmp")
	require.NoError(t, os.MkdirAll(filepath.Join(root, "repo"), 0o755))
	require.NoError(t, os.MkdirAll(outside, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(outside, "github-token"), []byte("secret"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(root, "repo", "README"), []byte("hello"), 0o600))
	require.NoError(t, os.Symlink(filepath.Join(outside, "github-token"), filepath.Join(root, "repo", "token")))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "repo", "out")))
	require.NoError(t, os.Symlink("README", filepath.Join(root, "repo", "readme-link")))
	read := ahpInWorkspaceUnder(root) + `[ -f "$p" ] || exit 3; exec cat -- "$p"`
	for name, want := range map[string]string{
		"repo/README": "hello", "repo/readme-link": "hello", "repo/token": "", "repo/out/github-token": "",
	} {
		out, err := exec.Command("/bin/sh", "-c", read, "sh", filepath.Join(root, name)).Output()
		if want == "" {
			var exit *exec.ExitError
			require.ErrorAs(t, err, &exit, name)
			require.Equal(t, 3, exit.ExitCode(), name)
			require.Empty(t, out, name)
			continue
		}
		require.NoError(t, err, name)
		require.Equal(t, want, string(out), name)
	}
}

func TestAHPMatchAny(t *testing.T) {
	require.True(t, ahpMatchAny([]string{".github/app.yml"}, ".github/app.yml"))
	require.True(t, ahpMatchAny([]string{"pkg"}, "pkg/a.go"))
	require.True(t, ahpMatchAny([]string{"*.go"}, "a.go"))
	require.False(t, ahpMatchAny([]string{"*.go"}, "pkg/a.go"))
	require.True(t, ahpMatchAny([]string{"**/*.go"}, "pkg/sub/a.go"))
	require.False(t, ahpMatchAny([]string{"**/*.md"}, "pkg/sub/a.go"))
	require.False(t, ahpMatchAny(nil, "a"))
	// A ** segment matches any number of directories wherever it is.
	for pattern, rel := range map[string]string{
		"src/**/*.go": "src/a/b/c.go", "src/**/x.go": "src/x.go", "**/test/**": "a/test/b/c", "a/**/b/**/c": "a/x/b/y/z/c",
	} {
		require.True(t, ahpMatchAny([]string{pattern}, rel), "%s %s", pattern, rel)
	}
	for pattern, rel := range map[string]string{
		"src/**/*.go": "lib/a/b.go", "src/**/*.md": "src/a/b.go", "a/**/b": "a/x/c",
	} {
		require.False(t, ahpMatchAny([]string{pattern}, rel), "%s %s", pattern, rel)
	}
	// Many ** segments against a deep path stay cheap.
	deep := strings.Repeat("d/", 63) + "e"
	require.False(t, ahpMatchAny([]string{strings.Repeat("**/", 80) + "f"}, deep))
}

func TestAHPResolveSessionConfig(t *testing.T) {
	env := newAHPTestEnv(t)
	c := env.connect("alice")
	env.setUpSession(c, "manual")
	alice := ahphost.Principal{UserKey: ahphost.UserKey("https://issuer.example", "alice"), Namespace: ahpTestNamespace}
	dir := ahptypes.URI(ahpTestDir)
	result, err := env.backend.ResolveSessionConfig(ahpCtx(t), alice, ahptypes.ResolveSessionConfigParams{
		WorkingDirectory: &dir, Config: map[string]json.RawMessage{"approvalMode": json.RawMessage(`"assisted"`), "branch": json.RawMessage(`"feature/x"`)},
	})
	require.NoError(t, err)
	require.JSONEq(t, `"manual"`, string(result.Values["approvalMode"]), "assisted becomes manual: Orka asks the person")
	require.JSONEq(t, `"main"`, string(result.Values["baseBranch"]), "the project's default branch")
	require.JSONEq(t, `"feature/x"`, string(result.Values["branch"]))
	require.Contains(t, result.Schema.Properties, "approvalMode")
	require.Contains(t, result.Schema.Properties, "baseBranch")

	_, err = env.backend.ResolveSessionConfig(ahpCtx(t), alice, ahptypes.ResolveSessionConfigParams{Config: map[string]json.RawMessage{"approvalMode": json.RawMessage(`"yolo"`)}})
	require.Error(t, err)
}

// A changeset whose files together exceed one read shows the rest as too
// large instead of failing.
func TestAHPSelectBlobsBoundsTheTotalRead(t *testing.T) {
	objects := make([]string, 0, 62)
	objects = append(objects, "huge", "missing")
	sizes := map[string]int{"huge": ahpChangeContentMax + 1}
	for i := range 60 {
		name := fmt.Sprintf("file-%d", i)
		objects = append(objects, name)
		sizes[name] = ahpChangeContentMax
	}
	small, large := ahpSelectBlobs(objects, sizes)
	total := 0
	for _, object := range small {
		total += sizes[object]
	}
	require.LessOrEqual(t, total, ahpChangesContentTotal)
	require.Len(t, small, ahpChangesContentTotal/ahpChangeContentMax)
	require.Contains(t, large, "huge")
	require.NotContains(t, append(small, large...), "missing", "an object that is not a blob has no entry")
	require.Len(t, large, len(objects)-1-len(small))
}

// Watch stamps keep stat's sub-second modification time, so a file rewritten
// at the same size within a second still changes; names keep their spaces
// and slashes.
func TestAHPParseStamps(t *testing.T) {
	raw := strings.Join([]string{
		"12/2026-10-28 12:00:00.123456789 +0000//workspace/src/a b.go",
		"12/2026-10-28 12:00:00.987654321 +0000//workspace/src/c.go",
		"7/2026-10-28 12:00:01.000000000 +0000//workspace/src/d.go",
		"malformed",
		"3/no-name/",
	}, "\n")
	require.Equal(t, map[string]string{
		"/workspace/src/a b.go": "12 2026-10-28 12:00:00.123456789 +0000",
		"/workspace/src/c.go":   "12 2026-10-28 12:00:00.987654321 +0000",
		"/workspace/src/d.go":   "7 2026-10-28 12:00:01.000000000 +0000",
	}, ahpParseStamps([]byte(raw)))
}

// Where stat takes -c, as in workspace images, a same-size rewrite within one
// second changes the file's stamp.
func TestAHPStampsSeeASameSecondRewrite(t *testing.T) {
	file := filepath.Join(t.TempDir(), "a b.txt")
	stamp := func() string {
		out, err := exec.Command("stat", "-c", "%s/%y/%n", "--", file).Output()
		if err != nil {
			t.Skip("stat does not take -c here")
		}
		stamps := ahpParseStamps(out)
		require.Contains(t, stamps, file)
		return stamps[file]
	}
	require.NoError(t, os.WriteFile(file, []byte("one"), 0o600))
	second := time.Date(2026, time.October, 28, 12, 0, 0, 0, time.UTC)
	require.NoError(t, os.Chtimes(file, second, second.Add(100*time.Millisecond)))
	first := stamp()
	require.NoError(t, os.WriteFile(file, []byte("two"), 0o600))
	require.NoError(t, os.Chtimes(file, second, second.Add(500*time.Millisecond)))
	require.NotEqual(t, first, stamp())
}

// A watched symlink inside the workspace is followed to its checked target,
// and its files are named under the watched path; a symlink below it that
// leaves the workspace is not followed.
func TestAHPStampsFollowAWatchedSymlink(t *testing.T) {
	if _, err := exec.LookPath("find"); err != nil {
		t.Skip("find is not installed")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	require.NoError(t, err)
	outside := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(outside, "secret"), []byte("s"), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "real", "dir"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(root, "real", "dir", "a.txt"), []byte("a"), 0o600))
	require.NoError(t, os.Symlink(outside, filepath.Join(root, "real", "dir", "out")))
	link := filepath.Join(root, "link")
	require.NoError(t, os.Symlink(filepath.Join(root, "real", "dir"), link))
	out, err := exec.Command("/bin/sh", "-c", ahpInWorkspaceUnder(root)+ahpStampsScript, "sh", link, "64", "10").CombinedOutput()
	if err != nil && strings.Contains(string(out), "stat") {
		t.Skip("stat does not take -c here")
	}
	require.NoError(t, err, string(out))
	stamps := ahpParseStamps(out)
	require.Contains(t, stamps, filepath.Join(link, "a.txt"), string(out))
	require.Len(t, stamps, 1, "only the watched target's own file: %s", out)
}

// A file read from the workspace is redacted for the person's git tokens as
// terminal output is, to its very end: a shell could otherwise copy a token
// into a file and read it back here.
func TestAHPRedactWholeTokens(t *testing.T) {
	const token = "ghu_0123456789abcdefghijklmnopqrstuvwx"
	tokens := []string{"ghu_olderolderolderolder", token}
	for name, tc := range map[string]struct{ data, want string }{
		"no token":          {data: "package main\n", want: "package main\n"},
		"whole token":       {data: "t=" + token + "\n", want: "t=[REDACTED]\n"},
		"older token":       {data: "ghu_olderolderolderolder", want: "[REDACTED]"},
		"secret alone":      {data: token[4:], want: "[REDACTED]"},
		"cut short in file": {data: "x " + token[:20] + " y", want: "x [REDACTED] y"},
		"cut short at end":  {data: "x " + token[:12], want: "x ghu_[REDACTED]"},
		// Fewer bytes than are redacted anywhere else stay, at the end too.
		"few bytes at end": {data: "x ghu_0123", want: "x ghu_0123"},
		"ends like one":    {data: "ab0123", want: "ab0123"},
	} {
		require.Equal(t, tc.want, string(ahpRedactWholeTokens([]byte(tc.data), tokens)), name)
	}
	require.Equal(t, "x", string(ahpRedactWholeTokens([]byte("x"), nil)), "nothing to redact without tokens")
	binary := append([]byte{0xff, 0xfe}, token...)
	require.NotContains(t, string(ahpRedactWholeTokens(binary, tokens)), token[4:], "a binary file is redacted too")
}

// A branch's committed files, which the changeset shows, are redacted too.
func TestAHPRedactGitChanges(t *testing.T) {
	const token = "ghu_0123456789abcdefghijklmnopqrstuvwx"
	before, after := "old\n", "t="+token+"\n"
	changed := map[string]ahpGitChange{
		"/workspace/a": {Before: &before, After: &after},
		"/workspace/b": {After: &after},
		"/workspace/c": {Before: &after},
	}
	ahpRedactGitChanges(changed, []string{token})
	require.Equal(t, "old\n", *changed["/workspace/a"].Before)
	require.Equal(t, "t=[REDACTED]\n", *changed["/workspace/a"].After)
	require.Nil(t, changed["/workspace/b"].Before, "an added file keeps no before side")
	require.Equal(t, "t=[REDACTED]\n", *changed["/workspace/b"].After)
	require.Nil(t, changed["/workspace/c"].After, "a deleted file keeps no after side")
	require.Equal(t, "t=[REDACTED]\n", *changed["/workspace/c"].Before)
	require.Equal(t, "t="+token+"\n", after, "the branch's own text is not changed in place")
}
