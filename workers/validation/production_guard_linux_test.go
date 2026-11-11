//go:build linux

package main

import (
	"archive/tar"
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

// Opt in with CGO_ENABLED=0 and ORKA_VALIDATION_LIVE_TOOL_IMAGE set to an
// already-present immutable C compiler tool image. Network-none isolates this
// guard/HTTP phase without mocking its process, capabilities, or listener.
func TestProductionHTTPGuardContainer(t *testing.T) {
	if os.Getenv("ORKA_VALIDATION_PRODUCTION_GUARD_CHILD") == "1" {
		runProductionHTTPGuard(t)
		return
	}
	image := os.Getenv("ORKA_VALIDATION_LIVE_TOOL_IMAGE")
	if image == "" {
		t.Skip("set ORKA_VALIDATION_LIVE_TOOL_IMAGE for production-shaped Docker guard conformance")
	}
	if !strings.Contains(image, "@sha256:") && !strings.HasPrefix(image, "sha256:") {
		t.Fatal("production guard conformance requires an immutable local image")
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	guard := os.Getenv("ORKA_VALIDATION_LIVE_GUARD")
	if guard == "" {
		guard = compileGuardFixture(t, "guard/guard.c")
	}
	fixture := t.TempDir()
	if os.Chmod(fixture, 0755) != nil {
		t.Fatal("conformance fixture permissions unavailable")
	}
	for name, source := range map[string]string{"guard": guard, "validation.test": executable,
		"server.c": "testdata/production_http_server.c"} {
		content, err := os.ReadFile(source)
		if err != nil || os.WriteFile(filepath.Join(fixture, name), content, 0555) != nil {
			t.Fatal("conformance fixture could not be staged")
		}
	}
	name := fmt.Sprintf("orka-guard-conformance-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = exec.CommandContext(ctx, "docker", "rm", "--force", name).Run()
	})
	args := []string{"run", "--rm", "--name", name, "--pull", "never", "--network", "none",
		"--read-only", "--user", "0:0", "--cap-drop", "ALL",
		"--cap-add", "SETUID", "--cap-add", "SETGID", "--cap-add", "CHOWN",
		"--cap-add", "KILL", "--cap-add", "FOWNER", "--security-opt", "no-new-privileges=true",
		"--cpus", "1", "--memory", "512m", "--memory-swap", "512m", "--pids-limit", "512",
		"--mount", "type=bind,src=" + fixture + ",dst=/fixture,readonly",
		"--env", "ORKA_VALIDATION_PRODUCTION_GUARD_CHILD=1", "--env", "GOMEMLIMIT=160MiB"}
	for _, volume := range []string{"runner", "input", "src", "checks", "work", "tmp"} {
		args = append(args, "--tmpfs", "/"+volume+":rw,exec,nosuid,nodev,size=64m,mode=0700")
	}
	args = append(args, "--entrypoint", "/bin/sh", image, "-ceu",
		"cp /fixture/guard /runner/orka-validation-guard; chmod 0555 /runner/orka-validation-guard; chmod 0500 /runner; "+
			"exec /fixture/validation.test -test.run=^TestProductionHTTPGuardContainer$ -test.v")
	ctx, cancel := context.WithTimeout(t.Context(), 45*time.Second)
	defer cancel()
	if output, err := exec.CommandContext(ctx, "docker", args...).CombinedOutput(); err != nil {
		t.Fatalf("production-shaped guard conformance failed: %v\n%s", err, output)
	}
}

func runProductionHTTPGuard(t *testing.T) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if err := supervisorPreflight(); err != nil {
		t.Fatalf("production supervisor preflight failed: %v", err)
	}
	var archive bytes.Buffer
	tarball := tar.NewWriter(&archive)
	content := []byte("healthy")
	header := &tar.Header{Name: "README", Mode: 0644, Typeflag: tar.TypeReg, Size: int64(len(content))}
	if err := tarball.WriteHeader(header); err != nil {
		t.Fatal(err)
	}
	if _, err := tarball.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tarball.Close(); err != nil {
		t.Fatal(err)
	}
	serverCode, err := os.ReadFile("/fixture/server.c")
	if err != nil {
		t.Fatal(err)
	}
	serverScript := []byte("#!/bin/sh\nset -eu\n" +
		"cc -std=c11 -Wall -Wextra -Werror /checks/server.c -o \"${TMPDIR:?}/report-fixture\"\n" +
		"exec \"${TMPDIR:?}/report-fixture\"\n")
	check := workerHTTPCheck(t)
	check.HTTP.ServerCommand = []string{"/checks/server.sh"}
	manifest := pv.Manifest{Version: pv.SchemaVersion, Action: pv.ValidateReport,
		Problem: "synthetic production guard conformance", Scope: []string{"actual source response"},
		Sources: pv.Sources{Repository: "https://github.com/example/fixture", Original: pv.SourceIdentity{
			Commit: strings.Repeat("1", 40), Tree: strings.Repeat("2", 40), ArchiveDigest: pv.Digest(archive.Bytes())}},
		Environment: pv.Environment{Image: "fixture/tool@" + pv.Digest(nil), ImageID: pv.Digest(nil),
			Platform: "linux/" + runtime.GOARCH, Profile: pv.LocalServices,
			Dependencies: map[string]string{"orka.kubernetes.policy": pv.KubernetesPolicyVersion}},
		Files: []pv.FrozenFile{
			{Path: "server.c", Content: serverCode, Digest: pv.Digest(serverCode)},
			{Path: "server.sh", Content: serverScript, Digest: pv.Digest(serverScript), Executable: true}},
		Checks: []pv.Check{check}}
	manifest.ReportDigest, err = pv.StableReportDigest(manifest.Problem, manifest.Scope)
	if err != nil {
		t.Fatal(err)
	}
	binding, err := pv.NewRunBinding(manifest, "production-guard", "production-guard-task", "")
	if err != nil {
		t.Fatal(err)
	}
	input := pv.PodInput{Version: pv.PodProtocolVersion, Manifest: manifest, Binding: binding,
		Side: pv.Original, CheckID: check.ID, Archive: archive.Bytes()}
	if err := pv.StagePodInput(t.Context(), input, "/src", "/checks"); err != nil {
		t.Fatal(err)
	}
	manager := newProcessManager()
	report := newReport(input, "production-guard-pod")
	server, runErr := runHTTPCheck(t.Context(), manager, input, check, &report.Evidence)
	cleanupErr := manager.completeEvidence(t.Context(), &report.Evidence, nil, nil)
	if server != nil {
		if err := captureHTTPDiagnostics(&report.Evidence, server); cleanupErr == nil {
			cleanupErr = err
		}
	}
	if runErr != nil || cleanupErr != nil {
		t.Fatalf("real supervisor/guard HTTP execution failed: execution=%v cleanup=%v", runErr, cleanupErr)
	}
	observation := report.Evidence.Observation
	if !observation.Executed || !observation.HTTPCompleted || observation.SetupError != "" ||
		observation.StdoutDigest != pv.Digest([]byte(check.Healthy.Stdout)) {
		t.Fatal("real production guard did not produce the independently observed HTTP response")
	}
}
