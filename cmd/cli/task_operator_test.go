package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/orka-agents/orka/internal/operatoraccess"
)

func TestTaskExecAcceptsKubectlStyleTTYFlags(t *testing.T) {
	root := newRootCmd()
	cmd, _, err := root.Find([]string{"task", "exec"})
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.ParseFlags([]string{"-it", "--token", "abc", "-n", "team"}); err != nil {
		t.Fatalf("parse -it: %v", err)
	}
	tty, _ := cmd.Flags().GetBool("tty")
	stdin, _ := cmd.Flags().GetBool("stdin")
	token, _ := cmd.Flags().GetString("token")
	namespace, _ := cmd.Flags().GetString("namespace")
	if !tty || !stdin || token != "abc" || namespace != "team" {
		t.Fatalf("tty=%v stdin=%v token=%q namespace=%q", tty, stdin, token, namespace)
	}
}

func TestTaskExecRequiresDashSeparator(t *testing.T) {
	root := newRootCmd()
	root.SetArgs([]string{"task", "exec", "my-task", "ls"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	if err := root.Execute(); err == nil {
		t.Fatal("exec without -- must be refused")
	}
}

func TestOperatorCopyTargetParsing(t *testing.T) {
	for arg, want := range map[string][2]string{
		"my-task:src/main.go": {"my-task", "src/main.go"},
		"my-task:/abs":        {"my-task", "/abs"},
		"t.1:":                {"t.1", ""},
	} {
		task, path, ok := operatorCopyTarget(arg)
		if !ok || task != want[0] || path != want[1] {
			t.Fatalf("operatorCopyTarget(%q) = %q %q %v", arg, task, path, ok)
		}
	}
	for _, arg := range []string{"./local:file", "local", "C:\\file", "Upper:x", ":path", "-"} {
		if _, _, ok := operatorCopyTarget(arg); ok {
			t.Fatalf("operatorCopyTarget(%q) must stay local", arg)
		}
	}
}

func TestOperatorStatusExitCodes(t *testing.T) {
	for _, tc := range []struct {
		status operatoraccess.Status
		code   int
	}{
		{operatoraccess.Status{Reason: operatoraccess.ExitReasonExited, ExitCode: 0}, 0},
		{operatoraccess.Status{Reason: operatoraccess.ExitReasonExited, ExitCode: 3}, 3},
		{operatoraccess.Status{Reason: operatoraccess.ExitReasonSignaled, ExitCode: 130}, 130},
		{operatoraccess.Status{Reason: operatoraccess.ExitReasonTimeout, ExitCode: -1}, 124},
		{operatoraccess.Status{Reason: operatoraccess.ExitReasonOutputLimit, ExitCode: -1}, 125},
		{operatoraccess.Status{Reason: operatoraccess.ExitReasonError, ExitCode: -1, Message: "runtime supervisor: frozen"}, 126},
	} {
		err := operatorStatusError(tc.status)
		if tc.code == 0 {
			if err != nil {
				t.Fatalf("%+v: unexpected error %v", tc.status, err)
			}
			continue
		}
		var exitErr *operatorExitError
		if !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.code {
			t.Fatalf("%+v: error %v, want exit %d", tc.status, err, tc.code)
		}
	}
}

func TestCopyOutNeverClobbersOnFailure(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "main.go")
	if err := os.WriteFile(local, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	failing := func(_ context.Context, _, _ string, _ int64, w io.Writer) (int64, error) {
		_, _ = w.Write([]byte("partial"))
		return 7, errors.New("copy ended after 7 of 100 bytes")
	}
	if err := copyOutOfWorkspace(context.Background(), cmd, failing, "t", "main.go", local, 0); err == nil {
		t.Fatal("a failed copy must report an error")
	}
	if data, _ := os.ReadFile(local); string(data) != "keep" {
		t.Fatalf("failed copy clobbered the local file: %q", data)
	}
	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Fatalf("temporary files left behind: %v", entries)
	}
	succeeding := func(_ context.Context, _, _ string, _ int64, w io.Writer) (int64, error) {
		n, err := w.Write([]byte("fresh"))
		return int64(n), err
	}
	// A directory destination receives the workspace file's base name.
	if err := copyOutOfWorkspace(context.Background(), cmd, succeeding, "t", "src/new.go", dir, 0); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(filepath.Join(dir, "new.go")); string(data) != "fresh" {
		t.Fatalf("copy wrote %q", data)
	}
}

func TestCopyInSendsExactSize(t *testing.T) {
	dir := t.TempDir()
	local := filepath.Join(dir, "patch")
	if err := os.WriteFile(local, []byte("0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	var gotSize int64
	var gotBody bytes.Buffer
	copyTo := func(_ context.Context, task, path string, r io.Reader, size int64) (int64, error) {
		gotSize = size
		n, err := io.Copy(&gotBody, r)
		return n, err
	}
	if err := copyIntoWorkspace(context.Background(), cmd, copyTo, local, "t", "patch"); err != nil {
		t.Fatal(err)
	}
	if gotSize != 10 || gotBody.String() != "0123456789" {
		t.Fatalf("size=%d body=%q", gotSize, gotBody.String())
	}
	if err := copyIntoWorkspace(context.Background(), cmd, copyTo, dir, "t", "patch"); err == nil {
		t.Fatal("directories must be refused")
	}
}

func TestOperatorExecTimeoutSeconds(t *testing.T) {
	for _, tc := range []struct {
		timeout time.Duration
		want    int64
		ok      bool
	}{
		{500 * time.Millisecond, 1, true}, // never truncated to 0, the server default
		{time.Second, 1, true},
		{1500 * time.Millisecond, 2, true},
		{operatoraccess.MaxExecTimeout, int64(operatoraccess.MaxExecTimeout / time.Second), true},
		{0, 0, false},
		{-time.Second, 0, false},
		{operatoraccess.MaxExecTimeout + time.Nanosecond, 0, false},
		{time.Duration(math.MaxInt64), 0, false},
	} {
		got, err := operatorExecTimeoutSeconds(tc.timeout)
		if (err == nil) != tc.ok || got != tc.want {
			t.Errorf("operatorExecTimeoutSeconds(%s) = %d, %v; want %d ok=%v", tc.timeout, got, err, tc.want, tc.ok)
		}
	}
}

func TestTaskExecRejectsAnInvalidTimeoutBeforeConnecting(t *testing.T) {
	for _, timeout := range []string{"0s", "-5s", "2h"} {
		root := newRootCmd()
		root.SetArgs([]string{"task", "exec", "--quiet", "--server", "http://127.0.0.1:1", "--timeout", timeout, "my-task", "--", "ls"})
		root.SetOut(io.Discard)
		root.SetErr(io.Discard)
		err := root.Execute()
		if err == nil || !strings.Contains(err.Error(), "--timeout") {
			t.Fatalf("--timeout %s = %v, want a timeout validation error", timeout, err)
		}
	}
}

// Exit statuses below 124 belong to the remote command. A refusal that arrives
// before the stream opens (operator access disabled, a missing permission, a
// class without the opt-in) ran nothing, so it must not exit 1.
func TestTaskExecRefusalBeforeTheStreamExitsWith126(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"message":"operator workspace access is disabled"}}`)
	}))
	defer api.Close()
	root := newRootCmd()
	root.SetArgs([]string{"task", "exec", "--quiet", "--server", api.URL, "my-task", "--", "false"})
	root.SetOut(io.Discard)
	root.SetErr(io.Discard)
	err := root.Execute()
	var exitErr *operatorExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 126 {
		t.Fatalf("refused exec = %v, want exit 126", err)
	}
	if !strings.Contains(exitErr.Error(), "HTTP 403") || !strings.Contains(exitErr.Error(), "disabled") {
		t.Fatalf("refusal message = %q, want the API's reason", exitErr.Error())
	}
}

func TestOperatorExecFailureExitCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		code int
	}{
		{context.Canceled, 130},
		{errors.New("operator exec stream closed before the command finished"), 126},
		{errors.New("write command stdout: no space left on device"), 126},
	} {
		var exitErr *operatorExitError
		if err := operatorExecFailure(tc.err); !errors.As(err, &exitErr) || exitErr.ExitCode() != tc.code {
			t.Fatalf("operatorExecFailure(%v) = %v, want exit %d", tc.err, err, tc.code)
		}
	}
}

// The copy lands in a temporary file that is renamed over the destination, and
// the temporary file is created 0600. Replacing a local script must not make it
// non-executable, or a shared file unreadable to its group.
func TestCopyOutKeepsAnExistingFilesPermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no Unix permission bits")
	}
	dir := t.TempDir()
	cmd := &cobra.Command{}
	cmd.SetErr(io.Discard)
	fresh := func(_ context.Context, _, _ string, _ int64, w io.Writer) (int64, error) {
		n, err := w.Write([]byte("#!/bin/sh\necho new\n"))
		return int64(n), err
	}
	for name, mode := range map[string]os.FileMode{"run.sh": 0o755, "shared.txt": 0o640} {
		local := filepath.Join(dir, name)
		if err := os.WriteFile(local, []byte("old"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(local, mode); err != nil {
			t.Fatal(err)
		}
		if err := copyOutOfWorkspace(context.Background(), cmd, fresh, "t", name, local, 0); err != nil {
			t.Fatal(err)
		}
		info, err := os.Stat(local)
		if err != nil {
			t.Fatal(err)
		}
		if data, _ := os.ReadFile(local); !strings.Contains(string(data), "echo new") || info.Mode().Perm() != mode {
			t.Fatalf("%s after copy: mode %o content %q, want mode %o and the new content", name, info.Mode().Perm(), data, mode)
		}
	}
	created := filepath.Join(dir, "created.txt")
	if err := copyOutOfWorkspace(context.Background(), cmd, fresh, "t", "created.txt", created, 0); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(created); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("a new local file must stay private: %v %v", info, err)
	}
}
