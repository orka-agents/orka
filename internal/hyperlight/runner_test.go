//go:build !windows

/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package hyperlight

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeHluk stands in for hluk: it logs each invocation to $HOME/calls.log
// (HOME is the Runner's cache dir) and runs the "guest" program with sh.
const fakeHluk = `#!/bin/sh
echo "$*" >> "$HOME/calls.log"
case "$1 $2" in
"snapshot key") echo "k0123abcd-c4"; exit 0 ;;
"snapshot save")
	[ -f "$HOME/fail-save" ] && exit 1
	while [ $# -gt 0 ]; do [ "$1" = "--output" ] && out="$2"; shift; done
	mkdir "$out" && touch "$out/index.json"
	exit 0 ;;
"snapshot run") script="$4" ;;
*) script="$6" ;;
esac
. "$script"
`

type fixture struct {
	runner *Runner
	cache  string
	rootfs string
	device string
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	dir := t.TempDir()
	binary := filepath.Join(dir, "hluk")
	if err := os.WriteFile(binary, []byte(fakeHluk), 0o755); err != nil {
		t.Fatal(err)
	}
	rootfs := filepath.Join(dir, "rootfs")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rootfs, "bash.cpio"), []byte("cpio"), 0o644); err != nil {
		t.Fatal(err)
	}
	device := filepath.Join(dir, "kvm")
	if err := os.WriteFile(device, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	cache := filepath.Join(dir, "cache")
	if err := os.MkdirAll(cache, 0o700); err != nil {
		t.Fatal(err)
	}
	return fixture{
		runner: NewRunner(Config{Binary: binary, RootfsDir: rootfs, CacheDir: cache, DevicePaths: []string{device}}),
		cache:  cache,
		rootfs: rootfs,
		device: device,
	}
}

func (f fixture) calls(t *testing.T) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(f.cache, "calls.log"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(data)), "\n")
}

func run(t *testing.T, r *Runner, ctx context.Context, req Request) (Result, string, string, error) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	req.Stdout, req.Stderr = &stdout, &stderr
	if req.Runtime == "" {
		req.Runtime = "bash"
	}
	result, err := r.Run(ctx, req)
	return result, stdout.String(), stderr.String(), err
}

func TestRunKeepsStreamsAndExitCode(t *testing.T) {
	f := newFixture(t)
	result, stdout, stderr, err := run(t, f.runner, context.Background(), Request{
		Script: "echo out; printf 'err\\000\\n' >&2; exit 3",
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if stdout != "out\n" || stderr != "err\n" {
		t.Fatalf("stdout=%q stderr=%q, want %q and %q (NULs dropped)", stdout, stderr, "out\n", "err\n")
	}
	if result.ExitCode != 3 || result.TimedOut {
		t.Fatalf("result = %+v, want exit 3", result)
	}
}

func TestRunSavesOneWarmSnapshotPerRuntime(t *testing.T) {
	f := newFixture(t)
	for i := 0; i < 2; i++ {
		result, stdout, _, err := run(t, f.runner, context.Background(), Request{Script: "echo hi"})
		if err != nil || stdout != "hi\n" || !result.Warm {
			t.Fatalf("run %d: result=%+v stdout=%q err=%v, want a warm run printing hi", i, result, stdout, err)
		}
	}
	var saves, runs int
	for _, call := range f.calls(t) {
		switch {
		case strings.HasPrefix(call, "snapshot save"):
			saves++
			if !strings.Contains(call, "--scratch-mb 128") || !strings.Contains(call, filepath.Join(f.rootfs, "bash.cpio")) {
				t.Errorf("save = %q, want the bash image at its 128 MiB", call)
			}
		case strings.HasPrefix(call, "snapshot run "+filepath.Join(f.cache, "snapshots", "bash-128mb-k0123abcd-c4")):
			runs++
		}
	}
	if saves != 1 || runs != 2 {
		t.Fatalf("saves=%d runs=%d, want 1 save and 2 restores; calls: %q", saves, runs, f.calls(t))
	}
}

func TestRunBootsColdWhenTheSnapshotCannotBeSaved(t *testing.T) {
	f := newFixture(t)
	if err := os.WriteFile(filepath.Join(f.cache, "fail-save"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	result, stdout, _, err := run(t, f.runner, context.Background(), Request{Script: "echo cold"})
	if err != nil || stdout != "cold\n" || result.Warm {
		t.Fatalf("result=%+v stdout=%q err=%v, want a cold run printing cold", result, stdout, err)
	}
	calls := f.calls(t)
	if last := calls[len(calls)-1]; !strings.HasPrefix(last, "run --initrd "+filepath.Join(f.rootfs, "bash.cpio")+" --scratch-mb 128 ") {
		t.Fatalf("last call = %q, want a cold run of the bash image", last)
	}
	if entries, _ := filepath.Glob(filepath.Join(f.cache, "snapshots", "*")); len(entries) != 0 {
		t.Fatalf("a failed save left %q behind", entries)
	}
}

func TestRunGrantsOnlyWhatTheRequestAsks(t *testing.T) {
	f := newFixture(t)
	t.Setenv("ORKA_HYPERLIGHT_TEST_SECRET", "leak")
	_, stdout, _, err := run(t, f.runner, context.Background(), Request{
		Script:   "env",
		Mounts:   []Mount{{Host: "/work", Guest: "/workspace"}, {Host: "/data", Guest: "/data", ReadOnly: true}},
		NetAllow: []string{"pypi.org", " "},
		Env:      []string{"A=1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout, "ORKA_HYPERLIGHT_TEST_SECRET") {
		t.Fatalf("the caller's environment reached hluk:\n%s", stdout)
	}
	calls := f.calls(t)
	want := "--mount /work:/workspace --mount /data:/data:ro --net-allow pypi.org --env A=1"
	if last := calls[len(calls)-1]; !strings.HasSuffix(last, want) {
		t.Fatalf("last call = %q, want it to end with %q", last, want)
	}

	_, _, _, err = run(t, f.runner, context.Background(), Request{Script: "env"})
	if err != nil {
		t.Fatal(err)
	}
	calls = f.calls(t)
	if last := calls[len(calls)-1]; strings.Contains(last, "--mount") || strings.Contains(last, "--net") || strings.Contains(last, "--env") {
		t.Fatalf("a request with no grants ran with %q", last)
	}
}

func TestRunTimesOut(t *testing.T) {
	f := newFixture(t)
	// Warm the snapshot first, so the deadline only covers the run.
	if _, _, _, err := run(t, f.runner, context.Background(), Request{Script: "true"}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	result, _, _, err := run(t, f.runner, ctx, Request{Script: "sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	if !result.TimedOut || result.ExitCode != -1 {
		t.Fatalf("result = %+v, want a timeout", result)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("the run took %s after its deadline", elapsed)
	}
}

func TestRunRefusesWhatCannotRun(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*fixture, *Request)
	}{
		{"no hypervisor device", func(f *fixture, _ *Request) { _ = os.Remove(f.device) }},
		{"no image", func(_ *fixture, req *Request) { req.Runtime = "node" }},
		{"invalid runtime", func(_ *fixture, req *Request) { req.Runtime = "../bash" }},
		{"no hluk", func(f *fixture, _ *Request) { f.runner.cfg.Binary = filepath.Join(f.cache, "missing") }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			req := Request{Runtime: "bash", Script: "echo ran"}
			tc.mutate(&f, &req)
			result, stdout, _, err := run(t, f.runner, context.Background(), req)
			if !errors.Is(err, ErrUnavailable) || result.ExitCode != -1 || stdout != "" {
				t.Fatalf("result=%+v stdout=%q err=%v, want ErrUnavailable and no run", result, stdout, err)
			}
		})
	}

	f := newFixture(t)
	_, _, _, err := run(t, f.runner, context.Background(), Request{Script: "true", Mounts: []Mount{{Host: "relative", Guest: "/x"}}})
	if err == nil || !strings.Contains(err.Error(), "invalid mount") {
		t.Fatalf("err = %v, want an invalid mount", err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(EnvBinary, "/opt/hluk")
	t.Setenv(EnvRootfsDir, "/images")
	t.Setenv(EnvCacheDir, "/cache")
	t.Setenv(EnvScratchMB, "512")
	cfg := ConfigFromEnv()
	if cfg.Binary != "/opt/hluk" || cfg.RootfsDir != "/images" || cfg.CacheDir != "/cache" || cfg.ScratchMB != 512 {
		t.Fatalf("cfg = %+v", cfg)
	}
	if mb := NewRunner(cfg).ScratchMB("node"); mb != 512 {
		t.Fatalf("ScratchMB = %d, want the configured 512", mb)
	}

	t.Setenv(EnvBinary, "")
	t.Setenv(EnvRootfsDir, "")
	t.Setenv(EnvScratchMB, "x")
	cfg = ConfigFromEnv()
	if cfg.Binary != defaultBinary || cfg.RootfsDir != defaultRootfsDir || cfg.ScratchMB != 0 {
		t.Fatalf("defaults = %+v", cfg)
	}
	if mb := NewRunner(cfg).ScratchMB("node"); mb != 512 {
		t.Fatalf("node ScratchMB = %d, want its default 512", mb)
	}
}
