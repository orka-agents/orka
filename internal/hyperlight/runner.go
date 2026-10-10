/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

// Package hyperlight runs a program in a Hyperlight micro-VM through the hluk
// CLI (hyperlight-unikraft). Each run boots a fresh guest from a warm snapshot
// of its runtime image, saved once per runtime into a writable cache, and the
// guest reaches no network and no host files unless the request grants them.
package hyperlight

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
)

// Environment variables that configure a Runner (see ConfigFromEnv).
const (
	EnvBinary    = "ORKA_HYPERLIGHT_BINARY"
	EnvRootfsDir = "ORKA_HYPERLIGHT_ROOTFS_DIR"
	EnvCacheDir  = "ORKA_HYPERLIGHT_CACHE_DIR"
	EnvScratchMB = "ORKA_HYPERLIGHT_SCRATCH_MB"
)

const (
	defaultBinary    = "hluk"
	defaultRootfsDir = "/opt/orka/hyperlight/rootfs"
	defaultScratchMB = 256
	safePath         = "/usr/local/bin:/usr/bin:/bin"
)

// defaultDevicePaths are the hypervisor devices hluk drives on Linux: KVM, or
// the Microsoft Hypervisor.
var defaultDevicePaths = []string{"/dev/kvm", "/dev/mshv"}

// runtimeScratchMB is the guest memory each published runtime image is
// tested with; others get defaultScratchMB.
var runtimeScratchMB = map[string]int{
	"agent":        1536,
	"bash":         128,
	"node":         512,
	"python":       256,
	"python-shell": 256,
}

var runtimeNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

// ErrUnavailable marks a run that never reached a guest: no hypervisor device,
// no hluk binary, or no image for the runtime.
var ErrUnavailable = errors.New("hyperlight unavailable")

// Config says where hluk and the runtime images are.
type Config struct {
	// Binary is the hluk executable, a path or a name on PATH.
	Binary string
	// RootfsDir holds one <runtime>.cpio per runtime image.
	RootfsDir string
	// CacheDir is writable and keeps the warm snapshots. Without one every
	// run boots its guest cold.
	CacheDir string
	// ScratchMB is the guest memory; 0 picks the runtime's default.
	ScratchMB int
	// DevicePaths are the hypervisor devices of which one must exist.
	DevicePaths []string
}

// ConfigFromEnv reads a Config from the ORKA_HYPERLIGHT_* variables, with the
// defaults of the Orka worker images.
func ConfigFromEnv() Config {
	cfg := Config{
		Binary:    strings.TrimSpace(os.Getenv(EnvBinary)),
		RootfsDir: strings.TrimSpace(os.Getenv(EnvRootfsDir)),
		CacheDir:  strings.TrimSpace(os.Getenv(EnvCacheDir)),
	}
	if cfg.Binary == "" {
		cfg.Binary = defaultBinary
	}
	if cfg.RootfsDir == "" {
		cfg.RootfsDir = defaultRootfsDir
	}
	if cfg.CacheDir == "" {
		cfg.CacheDir = filepath.Join(os.TempDir(), "orka-hyperlight")
	}
	if mb, err := strconv.Atoi(strings.TrimSpace(os.Getenv(EnvScratchMB))); err == nil && mb > 0 {
		cfg.ScratchMB = mb
	}
	return cfg
}

// Mount gives the guest a host directory.
type Mount struct {
	Host     string
	Guest    string
	ReadOnly bool
}

// Request is one program to run in a fresh guest. The context bounds it.
type Request struct {
	// Runtime names the image: python, node, bash, or another <name>.cpio
	// in the RootfsDir.
	Runtime string
	// Script is the program text, run by the runtime's interpreter.
	Script string
	// Mounts are the host directories the guest may reach; none by default.
	Mounts []Mount
	// NetAllow are the hosts the guest may connect to; none by default.
	NetAllow []string
	// Env are KEY=VALUE variables for the guest.
	Env []string
	// Stdout and Stderr receive the guest's output; nil discards it.
	Stdout io.Writer
	Stderr io.Writer
	// Credential runs hluk as another user, which then owns the script.
	Credential *Credential
}

// Credential is a user to run hluk as: the owner of the files the guest is
// given, with the hypervisor device's group among its Groups.
type Credential struct {
	UID    uint32
	GID    uint32
	Groups []uint32
}

// Result is how a run ended.
type Result struct {
	ExitCode int
	TimedOut bool
	// Warm reports a run restored from a warm snapshot rather than booted.
	Warm bool
}

// Runner runs requests with one Config.
type Runner struct {
	cfg Config
}

// NewRunner returns a Runner for cfg.
func NewRunner(cfg Config) *Runner {
	if len(cfg.DevicePaths) == 0 {
		cfg.DevicePaths = defaultDevicePaths
	}
	return &Runner{cfg: cfg}
}

// ScratchMB is the guest memory a run of runtime gets.
func (r *Runner) ScratchMB(runtime string) int {
	if r.cfg.ScratchMB > 0 {
		return r.cfg.ScratchMB
	}
	if mb, ok := runtimeScratchMB[runtime]; ok {
		return mb
	}
	return defaultScratchMB
}

// Run boots a guest for req and waits for its program to end. A non-nil
// error means no guest ran (ErrUnavailable) or hluk could not be started;
// the program's own failure is a non-zero Result.ExitCode.
func (r *Runner) Run(ctx context.Context, req Request) (Result, error) {
	rootfs, err := r.preflight(req)
	if err != nil {
		return Result{ExitCode: -1}, err
	}

	scriptDir, err := os.MkdirTemp("", "orka-hyperlight-*")
	if err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("create script dir: %w", err)
	}
	defer os.RemoveAll(scriptDir) //nolint:errcheck
	script := filepath.Join(scriptDir, "main"+scriptExtension(req.Runtime))
	if err := os.WriteFile(script, []byte(req.Script), 0o600); err != nil {
		return Result{ExitCode: -1}, fmt.Errorf("write script: %w", err)
	}
	if req.Credential != nil {
		for _, path := range []string{scriptDir, script} {
			if err := os.Chown(path, int(req.Credential.UID), int(req.Credential.GID)); err != nil {
				return Result{ExitCode: -1}, fmt.Errorf("hand the script to uid %d: %w", req.Credential.UID, err)
			}
		}
	}

	var args []string
	snapshot, warm := r.warmSnapshot(ctx, req.Runtime, rootfs)
	if warm {
		args = []string{"snapshot", "run", snapshot, script}
	} else {
		args = []string{"run", "--initrd", rootfs, "--scratch-mb", strconv.Itoa(r.ScratchMB(req.Runtime)), script}
	}
	args = append(args, capabilityArgs(req)...)

	cmd := exec.CommandContext(ctx, r.cfg.Binary, args...)
	cmd.Env = r.env()
	cmd.Stdout = writerOrDiscard(req.Stdout)
	cmd.Stderr = &nulFilter{w: writerOrDiscard(req.Stderr)}
	killProcessGroupOnCancel(cmd)
	if err := runAs(cmd, req.Credential); err != nil {
		return Result{ExitCode: -1}, err
	}

	runErr := cmd.Run()
	result := Result{Warm: warm}
	if ctx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		result.ExitCode = -1
		return result, nil
	}
	var exitErr *exec.ExitError
	switch {
	case runErr == nil:
		result.ExitCode = 0
	case errors.As(runErr, &exitErr):
		result.ExitCode = exitErr.ExitCode()
	default:
		result.ExitCode = -1
		return result, fmt.Errorf("run hluk: %w", runErr)
	}
	return result, nil
}

// preflight checks what a run needs before any process starts, and returns
// the runtime's image.
func (r *Runner) preflight(req Request) (string, error) {
	if !runtimeNamePattern.MatchString(req.Runtime) {
		return "", fmt.Errorf("%w: invalid runtime %q", ErrUnavailable, req.Runtime)
	}
	if !r.hasDevice() {
		return "", fmt.Errorf("%w: no hypervisor device (%s) in this pod", ErrUnavailable, strings.Join(r.cfg.DevicePaths, " or "))
	}
	if _, err := exec.LookPath(r.cfg.Binary); err != nil {
		return "", fmt.Errorf("%w: hluk binary %q: %v", ErrUnavailable, r.cfg.Binary, err)
	}
	rootfs := filepath.Join(r.cfg.RootfsDir, req.Runtime+".cpio")
	if _, err := os.Stat(rootfs); err != nil {
		return "", fmt.Errorf("%w: no image for runtime %q: %v", ErrUnavailable, req.Runtime, err)
	}
	for _, m := range req.Mounts {
		if err := validateMount(m); err != nil {
			return "", err
		}
	}
	return rootfs, nil
}

func (r *Runner) hasDevice() bool {
	for _, path := range r.cfg.DevicePaths {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// env is hluk's own environment: nothing of the caller's reaches it, and its
// cache is the Runner's.
func (r *Runner) env() []string {
	env := []string{"PATH=" + safePath}
	if r.cfg.CacheDir != "" {
		env = append(env, "HOME="+r.cfg.CacheDir, "HLUK_CACHE_DIR="+filepath.Join(r.cfg.CacheDir, "hluk"))
	} else {
		env = append(env, "HOME="+os.TempDir())
	}
	return env
}

// snapshotLocks serializes the save of each snapshot directory across the
// Runners of a process.
var snapshotLocks sync.Map

// warmSnapshot returns the warm snapshot of runtime, saving it first when the
// cache has none. Without a cache, or when the save fails, the run boots cold.
func (r *Runner) warmSnapshot(ctx context.Context, runtime, rootfs string) (string, bool) {
	if r.cfg.CacheDir == "" {
		return "", false
	}
	dir := filepath.Join(r.cfg.CacheDir, "snapshots", runtime+"-"+strconv.Itoa(r.ScratchMB(runtime))+"mb-"+r.snapshotKey(ctx))
	lockValue, _ := snapshotLocks.LoadOrStore(dir, &sync.Mutex{})
	lock := lockValue.(*sync.Mutex)
	lock.Lock()
	defer lock.Unlock()

	if info, err := os.Stat(dir); err == nil && info.IsDir() {
		return dir, true
	}
	// A snapshot is a freshly booted runtime image, nothing of any run: it
	// stays readable by every user a Credential may run as.
	if err := os.MkdirAll(filepath.Dir(dir), 0o755); err != nil {
		return "", false
	}
	partial, err := os.MkdirTemp(filepath.Dir(dir), filepath.Base(dir)+".part-*")
	if err != nil {
		return "", false
	}
	defer os.RemoveAll(partial) //nolint:errcheck
	// hluk writes the layout itself; it wants a directory that is not there.
	if err := os.Remove(partial); err != nil {
		return "", false
	}
	cmd := exec.CommandContext(ctx, r.cfg.Binary, "snapshot", "save",
		"--initrd", rootfs, "--scratch-mb", strconv.Itoa(r.ScratchMB(runtime)), "--output", partial)
	cmd.Env = r.env()
	killProcessGroupOnCancel(cmd)
	if err := cmd.Run(); err != nil {
		return "", false
	}
	if err := makeReadable(partial); err != nil {
		return "", false
	}
	if err := os.Rename(partial, dir); err != nil {
		return "", false
	}
	return dir, true
}

func makeReadable(root string) error {
	return filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if entry.IsDir() {
			mode = 0o755
		}
		return os.Chmod(path, mode)
	})
}

// snapshotKeys caches each hluk binary's snapshot key.
var snapshotKeys sync.Map

// snapshotKey names what a snapshot depends on (the embedded kernel and the
// host contract), so a new hluk never restores an old snapshot.
func (r *Runner) snapshotKey(ctx context.Context) string {
	if key, ok := snapshotKeys.Load(r.cfg.Binary); ok {
		return key.(string)
	}
	cmd := exec.CommandContext(ctx, r.cfg.Binary, "snapshot", "key")
	cmd.Env = r.env()
	out, err := cmd.Output()
	key := strings.TrimSpace(string(out))
	if err != nil || !runtimeNamePattern.MatchString(key) {
		return "unknown"
	}
	snapshotKeys.Store(r.cfg.Binary, key)
	return key
}

func capabilityArgs(req Request) []string {
	var args []string
	for _, m := range req.Mounts {
		spec := m.Host + ":" + m.Guest
		if m.ReadOnly {
			spec += ":ro"
		}
		args = append(args, "--mount", spec)
	}
	for _, host := range req.NetAllow {
		if host = strings.TrimSpace(host); host != "" {
			args = append(args, "--net-allow", host)
		}
	}
	for _, kv := range req.Env {
		args = append(args, "--env", kv)
	}
	return args
}

func validateMount(m Mount) error {
	for _, path := range []string{m.Host, m.Guest} {
		if !filepath.IsAbs(path) || strings.Contains(path, ":") {
			return fmt.Errorf("invalid mount %q:%q: paths must be absolute and contain no ':'", m.Host, m.Guest)
		}
	}
	return nil
}

func scriptExtension(runtime string) string {
	switch runtime {
	case "node", "quickjs":
		return ".js"
	case "bash":
		return ".sh"
	default:
		if strings.HasPrefix(runtime, "python") || runtime == "agent" {
			return ".py"
		}
		return ""
	}
}

func writerOrDiscard(w io.Writer) io.Writer {
	if w == nil {
		return io.Discard
	}
	return w
}

// nulFilter drops the NUL bytes the guest kernel puts in its messages.
type nulFilter struct {
	w io.Writer
}

func (f *nulFilter) Write(p []byte) (int, error) {
	if bytes.IndexByte(p, 0) < 0 {
		return f.w.Write(p)
	}
	if _, err := f.w.Write(bytes.ReplaceAll(p, []byte{0}, nil)); err != nil {
		return 0, err
	}
	return len(p), nil
}
