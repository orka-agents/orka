//go:build linux

package skill

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"

	"github.com/orka-agents/orka/internal/acp/toolbox"
)

const validMarkdown = "---\nname: code-review\ndescription: Review a change for correctness.\nlicense: MIT\nmetadata:\n  owner: platform\n---\n\n# Code review\n\nRun `make test` before you answer.\n"

func writeSkill(t *testing.T, files map[string]string) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "code-review")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	for name, content := range files {
		target := filepath.Join(root, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			t.Fatal(err)
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(name, ".sh") {
			mode = 0o755
		}
		if err := os.WriteFile(target, []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func requireReason(t *testing.T, err error, reason string) {
	t.Helper()
	var failure *toolbox.Failure
	if !errors.As(err, &failure) || failure.Reason != reason {
		t.Fatalf("error = %v, want reason %s", err, reason)
	}
}

func TestLoadAndWriteSkill(t *testing.T) {
	root := writeSkill(t, map[string]string{
		"SKILL.md":            validMarkdown,
		"references/guide.md": "Prefer small diffs.\n",
		"scripts/check.sh":    "#!/bin/sh\nexit 0\n",
	})
	tree, err := Load(root, "code-review", DefaultLimits())
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(tree.Files) != 3 || strings.Join(tree.Dirs, ",") != "references,scripts" {
		t.Fatalf("tree = %#v", tree)
	}
	destination := filepath.Join(t.TempDir(), "code-review")
	if err := tree.WriteTo(destination); err != nil {
		t.Fatalf("WriteTo: %v", err)
	}
	for name, wantMode := range map[string]os.FileMode{
		"":                    0o755 | os.ModeDir,
		"references":          0o755 | os.ModeDir,
		"SKILL.md":            0o644,
		"references/guide.md": 0o644,
		"scripts/check.sh":    0o755,
	} {
		info, err := os.Lstat(filepath.Join(destination, filepath.FromSlash(name)))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode() != wantMode {
			t.Errorf("%q mode = %v, want %v", name, info.Mode(), wantMode)
		}
	}
	data, err := os.ReadFile(filepath.Join(destination, "SKILL.md"))
	if err != nil || string(data) != validMarkdown {
		t.Fatalf("SKILL.md = %q, %v", data, err)
	}
	if err := tree.WriteTo(destination); err == nil {
		t.Fatal("WriteTo must refuse an existing destination")
	}
}

func TestWriteToDoesNotFollowSymlinkedDestination(t *testing.T) {
	root := writeSkill(t, map[string]string{"SKILL.md": validMarkdown})
	tree, err := Load(root, "code-review", DefaultLimits())
	if err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	parent := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(parent, "code-review")); err != nil {
		t.Fatal(err)
	}
	if err := tree.WriteTo(filepath.Join(parent, "code-review")); err == nil {
		t.Fatal("WriteTo must not write through a symlinked destination")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("WriteTo wrote outside its destination: %v", entries)
	}
}

func TestLoadRejectsUnsafeLayouts(t *testing.T) {
	cases := map[string]struct {
		setup  func(t *testing.T, root string)
		reason string
	}{
		"symlink escaping the skill": {setup: func(t *testing.T, root string) {
			if err := os.Symlink("/etc/passwd", filepath.Join(root, "passwd")); err != nil {
				t.Fatal(err)
			}
		}, reason: ReasonUnsupportedFileType},
		"symlink inside the skill": {setup: func(t *testing.T, root string) {
			if err := os.Symlink("SKILL.md", filepath.Join(root, "alias.md")); err != nil {
				t.Fatal(err)
			}
		}, reason: ReasonUnsupportedFileType},
		"symlinked folder traversal": {setup: func(t *testing.T, root string) {
			if err := os.Symlink("..", filepath.Join(root, "up")); err != nil {
				t.Fatal(err)
			}
		}, reason: ReasonUnsupportedFileType},
		"fifo": {setup: func(t *testing.T, root string) {
			if err := syscall.Mkfifo(filepath.Join(root, "pipe"), 0o644); err != nil {
				t.Fatal(err)
			}
		}, reason: ReasonUnsupportedFileType},
		"control character in a name": {setup: func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "bad\nname"), nil, 0o644); err != nil {
				t.Fatal(err)
			}
		}, reason: ReasonInvalidFileName},
		"file too large": {setup: func(t *testing.T, root string) {
			if err := os.WriteFile(filepath.Join(root, "big.bin"), make([]byte, DefaultLimits().MaxFileBytes+1), 0o644); err != nil {
				t.Fatal(err)
			}
		}, reason: ReasonFileTooLarge},
		"too large in total": {setup: func(t *testing.T, root string) {
			for i := range 5 {
				name := filepath.Join(root, "part"+string(rune('a'+i)))
				if err := os.WriteFile(name, make([]byte, DefaultLimits().MaxFileBytes), 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}, reason: ReasonTooLarge},
		"too many entries": {setup: func(t *testing.T, root string) {
			for i := range DefaultLimits().MaxEntries {
				if err := os.WriteFile(filepath.Join(root, "f"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('a'+i/26))), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
		}, reason: ReasonTooManyEntries},
		"too deep": {setup: func(t *testing.T, root string) {
			deep := root
			for range DefaultLimits().MaxDepth + 1 {
				deep = filepath.Join(deep, "d")
			}
			if err := os.MkdirAll(deep, 0o755); err != nil {
				t.Fatal(err)
			}
		}, reason: ReasonTooDeep},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := writeSkill(t, map[string]string{"SKILL.md": validMarkdown})
			tc.setup(t, root)
			_, err := Load(root, "code-review", DefaultLimits())
			requireReason(t, err, tc.reason)
		})
	}
}

func TestLoadRejectsSymlinkedOrMissingRoot(t *testing.T) {
	root := writeSkill(t, map[string]string{"SKILL.md": validMarkdown})
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(root, link); err != nil {
		t.Fatal(err)
	}
	_, err := Load(link, "code-review", DefaultLimits())
	requireReason(t, err, ReasonMissingMount)
	_, err = Load(filepath.Join(t.TempDir(), "absent"), "code-review", DefaultLimits())
	requireReason(t, err, ReasonMissingMount)
	_, err = Load("relative/path", "code-review", DefaultLimits())
	requireReason(t, err, ReasonMissingMount)
}

func TestLoadValidatesSkillMarkdown(t *testing.T) {
	frontmatter := func(fields string) string {
		return "---\n" + fields + "\n---\nBody.\n"
	}
	cases := map[string]struct {
		files  map[string]string
		reason string
	}{
		"missing SKILL.md":          {files: map[string]string{"README.md": "hi"}, reason: ReasonMissingMarkdown},
		"SKILL.md in a subfolder":   {files: map[string]string{"nested/README.md": validMarkdown}, reason: ReasonMissingMarkdown},
		"no frontmatter":            {files: map[string]string{"SKILL.md": "# Title\n"}, reason: ReasonInvalidFrontmatter},
		"unclosed frontmatter":      {files: map[string]string{"SKILL.md": "---\nname: code-review\n"}, reason: ReasonInvalidFrontmatter},
		"name mismatch":             {files: map[string]string{"SKILL.md": frontmatter("name: other\ndescription: d")}, reason: ReasonInvalidFrontmatter},
		"missing description":       {files: map[string]string{"SKILL.md": frontmatter("name: code-review")}, reason: ReasonInvalidFrontmatter},
		"allowed-tools":             {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d\nallowed-tools: Bash(*)")}, reason: ReasonInvalidFrontmatter},
		"hooks":                     {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d\nhooks:\n  PreToolUse: []")}, reason: ReasonInvalidFrontmatter},
		"model override":            {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d\nmodel: other")}, reason: ReasonInvalidFrontmatter},
		"forked sub-agent":          {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d\ncontext: fork\nagent: Explore")}, reason: ReasonInvalidFrontmatter},
		"duplicate key":             {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d\ndescription: e")}, reason: ReasonInvalidFrontmatter},
		"non-string metadata":       {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d\nmetadata:\n  a: [1]")}, reason: ReasonInvalidFrontmatter},
		"shell preprocessing":       {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d") + "Status: !`git status`\n"}, reason: ReasonUnsafeContent},
		"shell preprocessing fence": {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d") + "```!\nid\n```\n"}, reason: ReasonUnsafeContent},
		"secret":                    {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d") + "Use OPENAI_API_KEY=sk-proj-abcdefghijklmnopqrstuvwxyz0123456789\n"}, reason: ReasonUnsafeContent},
		"invalid UTF-8":             {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d") + "\xff\xfe"}, reason: ReasonUnsafeContent},
		"NUL byte":                  {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d") + "\x00"}, reason: ReasonUnsafeContent},
		"nested SKILL.md":           {files: map[string]string{"SKILL.md": validMarkdown, "examples/other/SKILL.md": "---\nname: other\ndescription: d\n---\n"}, reason: ReasonUnsafeContent},
		"nested skill.md any case":  {files: map[string]string{"SKILL.md": validMarkdown, "examples/Skill.MD": "x"}, reason: ReasonUnsafeContent},
		"codex agents/openai.yaml":  {files: map[string]string{"SKILL.md": validMarkdown, "agents/openai.yaml": "dependencies:\n  tools: []\n"}, reason: ReasonUnsafeContent},
		"SKILL.md too large":        {files: map[string]string{"SKILL.md": frontmatter("name: code-review\ndescription: d") + strings.Repeat("a", int(DefaultLimits().MaxMarkdownBytes))}, reason: ReasonFileTooLarge},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := writeSkill(t, tc.files)
			_, err := Load(root, "code-review", DefaultLimits())
			requireReason(t, err, tc.reason)
		})
	}
	crlf := strings.ReplaceAll(validMarkdown, "\n", "\r\n")
	if _, err := Load(writeSkill(t, map[string]string{"SKILL.md": crlf}), "code-review", DefaultLimits()); err != nil {
		t.Fatalf("CRLF SKILL.md: %v", err)
	}
	if _, err := Load(writeSkill(t, map[string]string{"SKILL.md": validMarkdown}), "Bad", DefaultLimits()); err == nil {
		t.Fatal("an invalid declared name must be rejected")
	}
}

// TestFailuresWithholdCredentialShapedDiagnostics keeps image-controlled file
// names and frontmatter keys that look like credentials out of the failure
// line, which reaches stderr, the termination log, and resource status.
func TestFailuresWithholdCredentialShapedDiagnostics(t *testing.T) {
	// Split so the literal is not itself a credential-shaped string.
	token := "gh" + "p_0123456789abcdefghijABCDEFGHIJ012345"
	frontmatter := func(fields string) string {
		return "---\n" + fields + "\n---\nBody.\n"
	}
	cases := map[string]struct {
		files  map[string]string
		reason string
	}{
		"file name": {files: map[string]string{
			"SKILL.md":                     validMarkdown,
			"references/" + token + ".bin": strings.Repeat("a", int(DefaultLimits().MaxFileBytes)+1),
		}, reason: ReasonFileTooLarge},
		"nested SKILL.md folder name": {files: map[string]string{
			"SKILL.md":          validMarkdown,
			token + "/SKILL.md": validMarkdown,
		}, reason: ReasonUnsafeContent},
		"frontmatter key": {files: map[string]string{
			"SKILL.md": frontmatter("name: code-review\ndescription: d\n" + token + ": x"),
		}, reason: ReasonInvalidFrontmatter},
		"metadata key": {files: map[string]string{
			"SKILL.md": frontmatter("name: code-review\ndescription: d\nmetadata:\n  " + token + ": [1]"),
		}, reason: ReasonInvalidFrontmatter},
		"duplicate key": {files: map[string]string{
			"SKILL.md": frontmatter("name: code-review\ndescription: d\n" + token + ": a\n" + token + ": b"),
		}, reason: ReasonInvalidFrontmatter},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Load(writeSkill(t, tc.files), "code-review", DefaultLimits())
			requireReason(t, err, tc.reason)
			var failure *toolbox.Failure
			errors.As(err, &failure)
			line := toolbox.FormatFailureLine(failure)
			if strings.Contains(line, token) || strings.Contains(err.Error(), token) {
				t.Fatalf("failure echoes the credential-shaped value: %s", line)
			}
			if failure.Message != withheldDiagnostic {
				t.Fatalf("message = %q, want the withheld diagnostic", failure.Message)
			}
		})
	}
	// Ordinary diagnostics keep their detail.
	_, err := Load(writeSkill(t, map[string]string{
		"SKILL.md": frontmatter("name: code-review\ndescription: d\nallowed-tools: Bash(*)"),
	}), "code-review", DefaultLimits())
	requireReason(t, err, ReasonInvalidFrontmatter)
	if !strings.Contains(err.Error(), `"allowed-tools"`) {
		t.Fatalf("a benign diagnostic must keep its detail: %v", err)
	}
}

func requireCloseOnExec(t *testing.T, what string, fd int) {
	t.Helper()
	flags, err := unix.FcntlInt(uintptr(fd), unix.F_GETFD, 0)
	if err != nil {
		t.Fatalf("%s: F_GETFD: %v", what, err)
	}
	if flags&unix.FD_CLOEXEC == 0 {
		t.Fatalf("%s: descriptor %d is inheritable; an adapter started meanwhile would keep it", what, fd)
	}
}

// TestDuplicatedSkillFolderHandlesAreCloseOnExec covers the two places the
// loader duplicates a folder handle. Another session can start an adapter at
// any moment, so a duplicate must never be inheritable, even briefly.
func TestDuplicatedSkillFolderHandlesAreCloseOnExec(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "nested"), 0o755); err != nil {
		t.Fatal(err)
	}
	rootFd, err := unix.Open(root, openDirFlags, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(rootFd) //nolint:errcheck

	dir, err := openDirectoryStream(rootFd)
	if err != nil {
		t.Fatal(err)
	}
	requireCloseOnExec(t, "folder stream", int(dir.Fd()))
	if err := dir.Close(); err != nil {
		t.Fatal(err)
	}
	if names, err := readDirectoryNames(rootFd, 10); err != nil || len(names) != 1 || names[0] != "nested" {
		t.Fatalf("readDirectoryNames = %v, %v", names, err)
	}

	for _, relative := range []string{"file.md", "nested/file.md"} {
		calls := 0
		if err := withParent(rootFd, relative, func(fd int, name string) error {
			calls++
			requireCloseOnExec(t, "parent of "+relative, fd)
			return nil
		}); err != nil || calls != 1 {
			t.Fatalf("withParent(%s) = %v after %d calls", relative, err, calls)
		}
	}
}
