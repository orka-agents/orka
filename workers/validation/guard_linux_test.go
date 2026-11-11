//go:build linux

package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func compileGuardFixture(t *testing.T, source string) string {
	t.Helper()
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Fatal("C compiler is required to validate the process sandbox")
	}
	target := filepath.Join(t.TempDir(), "guard-probe")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, compiler, "-std=c11", "-O2", "-Wall", "-Wextra", "-Werror",
		"-static", "-fstack-protector-strong", "-D_FORTIFY_SOURCE=2", source, "-o", target)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("C sandbox fixture failed to compile: %v\n%s", err, output)
	}
	return target
}

func TestGuardPolicies(t *testing.T) {
	probe := compileGuardFixture(t, "testdata/guard_probe.c")
	t.Run("offline-and-exec-inheritance", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, probe, "offline")
		command.Env = []string{"PATH=/usr/bin:/bin"}
		output, err := command.CombinedOutput()
		if err != nil || string(output) != "guard-inherited\n" {
			t.Fatalf("offline guard did not survive exec or reject escape syscalls: %v (%q)", err, output)
		}
	})
	t.Run("local-subject", func(t *testing.T) {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, probe, "subject", strconv.Itoa(listener.Addr().(*net.TCPAddr).Port))
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("local subject guard failed: %v (%q)", err, output)
		}
	})
	t.Run("local-fixture", func(t *testing.T) {
		listener, err := preopenedListener(0)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		address, err := unix.Getsockname(int(listener.Fd()))
		if err != nil {
			t.Fatal(err)
		}
		port := address.(*unix.SockaddrInet6).Port
		exchange := make(chan error, 1)
		go func() {
			connection, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), time.Second)
			if err != nil {
				exchange <- err
				return
			}
			defer func() { _ = connection.Close() }()
			_ = connection.SetDeadline(time.Now().Add(2 * time.Second))
			if _, err := connection.Write([]byte("x")); err != nil {
				exchange <- err
				return
			}
			var response [1]byte
			_, err = io.ReadFull(connection, response[:])
			if err == nil && response[0] != 'y' {
				err = errors.New("fixture did not respond through the activated listener")
			}
			exchange <- err
		}()
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, probe, "fixture", strconv.Itoa(port))
		command.ExtraFiles = []*os.File{listener}
		output, commandErr := command.CombinedOutput()
		networkErr := <-exchange
		if commandErr != nil || networkErr != nil {
			t.Fatalf("fixture cannot accept solely on FD3: process=%v exchange=%v (%q)", commandErr, networkErr, output)
		}
	})
	t.Run("architecture-check", func(t *testing.T) {
		if runtime.GOARCH != "amd64" {
			t.Skip("x32 applies only to amd64; native audit architecture is checked on both supported architectures")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		command := exec.CommandContext(ctx, probe, "x32")
		err := command.Run()
		var exit *exec.ExitError
		if !errors.As(err, &exit) {
			t.Fatal("x32 ABI was not killed by the architecture filter")
		}
		status, ok := exit.Sys().(syscall.WaitStatus)
		if !ok || !status.Signaled() || status.Signal() != syscall.SIGSYS {
			t.Fatal("x32 ABI was not killed by the architecture filter")
		}
	})
}

func TestGuardClosesInheritedDescriptors(t *testing.T) {
	probe := compileGuardFixture(t, "testdata/guard_probe.c")
	reader, control, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close(); _ = control.Close() }()
	extra, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = extra.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, probe, "fds")
	command.ExtraFiles = []*os.File{control, extra, extra, extra}
	output, err := command.CombinedOutput()
	if err != nil || string(output) != "closed\n" {
		t.Fatalf("guard leaked an inherited descriptor across exec: %v (%q)", err, output)
	}
}

func TestGuardToolchains(t *testing.T) {
	probe := compileGuardFixture(t, "testdata/guard_probe.c")
	compiler, err := exec.LookPath("cc")
	if err != nil {
		t.Fatal(err)
	}
	goRoot, err := exec.Command("go", "env", "GOROOT").Output()
	if err != nil {
		t.Fatal("Go toolchain is required to validate the process sandbox")
	}
	for _, tool := range []struct {
		name    string
		command string
		source  string
		script  string
	}{
		{"c", compiler, "#include <stdio.h>\nint main(void) { puts(\"compiled\"); return 0; }\n",
			`cp "$1/input.c" "$1/work.c"; "$2" -Wall -Wextra -Werror "$1/work.c" -o "$1/program"; exec "$1/program"`},
		{"go", filepath.Join(strings.TrimSpace(string(goRoot)), "bin", "go"),
			"package main\nimport \"fmt\"\nfunc main() { fmt.Println(\"compiled\") }\n",
			`cp "$1/input.go" "$1/work.go"; "$2" build -buildvcs=false -o "$1/program" "$1/work.go"; exec "$1/program"`},
	} {
		t.Run(tool.name, func(t *testing.T) {
			work := t.TempDir()
			if err := os.WriteFile(filepath.Join(work, "input."+tool.name), []byte(tool.source), 0444); err != nil {
				t.Fatal(err)
			}
			environment, err := childEnvironment(map[string]string{"GOMAXPROCS": "2"}, work, false)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			command := exec.CommandContext(ctx, probe, "exec", "/bin/sh", "-ec", tool.script, "check", work, tool.command)
			command.Dir = work
			command.Env = append(environment, "GO111MODULE=off")
			command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
			command.Cancel = func() error { return unix.Kill(-command.Process.Pid, unix.SIGKILL) }
			command.WaitDelay = 2 * time.Second
			output, err := command.CombinedOutput()
			if err != nil || string(output) != "compiled\n" {
				t.Fatalf("offline %s build/check failed under the real guard: %v (%q)", tool.name, err, output)
			}
		})
	}
}

func TestGuardFailsClosedBeforeExec(t *testing.T) {
	guard := compileGuardFixture(t, "guard/guard.c")
	for _, uid := range []string{"0", "19999", "60001", "20000"} {
		t.Run(uid, func(t *testing.T) {
			if uid == "20000" && os.Geteuid() == 0 {
				t.Skip("the rootless-identity rejection probe requires an unprivileged test process")
			}
			reader, control, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reader.Close(); _ = control.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, guard, uid, "offline", "subject", "0", "--",
				"/bin/sh", "-c", "printf should-not-execute")
			command.ExtraFiles = []*os.File{control}
			output, err := command.CombinedOutput()
			_ = control.Close()
			status, readErr := io.ReadAll(reader)
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != 125 || readErr != nil ||
				!bytes.Equal(status, []byte("E")) || len(output) != 0 {
				t.Fatal("guard ran untrusted code or leaked diagnostics after sandbox setup failure")
			}
		})
	}
}
