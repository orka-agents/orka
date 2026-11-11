//go:build linux

package main

import (
	"context"
	"crypto/sha256"
	"debug/buildinfo"
	"debug/elf"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	pv "github.com/orka-agents/orka/internal/patchverification"
)

var (
	errOfflineBuild = errors.New(
		"offline Go package build failed; dynamic dependencies, CGO and code generation are unsupported",
	)
	errToolchainIdentity = errors.New("application pinned Go compiler version does not match")
)

// buildCommand is deliberately not configurable beyond a validated package.
// In particular no repository build script, go generate, toolexec, CGO,
// network module resolution, workspace or inherited GOFLAGS is executed.
func buildCommand(input pv.ApplicationWorkerInput) ([]string, []string, error) {
	if pv.ValidateApplicationWorkerInput(input) != nil {
		return nil, nil, pv.ErrEvidence
	}
	platform := strings.Split(input.Manifest.Environment.Platform, "/")
	if len(platform) != 2 || platform[0] != "linux" {
		return nil, nil, pv.ErrEvidence
	}
	arguments := []string{modeBuild, "-mod=vendor", "-trimpath", "-buildvcs=false", "-p=2",
		"-o", filepath.Join(pv.ApplicationArtifactsPath, pv.ApplicationBinaryName),
		input.Manifest.Application.Build.Package}
	environment := []string{
		"PATH=/usr/local/go/bin:/usr/bin:/bin", workerHomeEnv, "TMPDIR=/work/compiler",
		workerLanguageEnv, workerLocaleEnv, "GOROOT=/usr/local/go", "GOENV=off", "GOTOOLCHAIN=local",
		"GOWORK=off", "GOPROXY=off", "GOSUMDB=off", "GONOPROXY=none", "GONOSUMDB=none",
		"GOPRIVATE=", "GOFLAGS=", "CGO_ENABLED=0", "GOOS=linux", "GOARCH=" + platform[1],
		"GOCACHE=/work/cache", "GOMODCACHE=/work/modules", "GOPATH=/work/gopath",
		"GOTELEMETRY=off", "GOMAXPROCS=2", "GOMEMLIMIT=1536MiB",
	}
	return arguments, environment, nil
}

func runBuild(ctx context.Context, input pv.ApplicationWorkerInput, output io.Writer) error {
	if _, err := measureProcess(); err != nil {
		return err
	}
	if err := noTargetCredentials(); err != nil {
		return err
	}
	if input.Artifact != nil || input.Subject != nil || input.CheckID != "" {
		return pv.ErrEvidence
	}
	if err := pv.ValidateApplicationSourceMetadata(input.Manifest); err != nil {
		return err
	}
	args, environment, err := buildCommand(input)
	if err != nil {
		return err
	}
	sourceDirectory, err := stageBuildSource(ctx, input)
	if err != nil {
		return err
	}
	versionCommand := boundedCommand(ctx, []string{"version"}, environment)
	version, err := versionCommand.Output()
	expectedVersion := "go version " + input.Manifest.Application.Build.GoVersion +
		" " + input.Manifest.Environment.Platform + "\n"
	if err != nil || string(version) != expectedVersion {
		return errToolchainIdentity
	}
	command := boundedCommand(ctx, args, environment)
	command.Dir = sourceDirectory
	command.Stdout, command.Stderr = io.Discard, io.Discard
	if err := command.Run(); err != nil {
		return errOfflineBuild
	}
	artifact, err := sealBuiltApplication(input)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return writeReport(output, pv.ApplicationBuildReport{Version: pv.ApplicationProtocolVersion,
		TaskUID: input.TaskUID, PodUID: artifact.Build.PodUID, Artifact: artifact})
}

func stageBuildSource(ctx context.Context, input pv.ApplicationWorkerInput) (string, error) {
	entries, err := os.ReadDir(pv.ApplicationWorkPath)
	if err != nil || len(entries) != 0 {
		return "", errors.New("application build workspace is not empty")
	}
	if err := pv.ValidateApplicationVolumeRoot(pv.ApplicationArtifactsPath); err != nil {
		return "", err
	}
	sourceFile, err := openRegular(pv.ApplicationSourcePath)
	if err != nil {
		return "", err
	}
	archive, readErr := io.ReadAll(io.LimitReader(sourceFile, pv.MaxApplicationSourceBytes+1))
	closeErr := sourceFile.Close()
	if readErr != nil || closeErr != nil || len(archive) > pv.MaxApplicationSourceBytes {
		return "", pv.ErrLimit
	}
	source := input.Manifest.Sources.Original
	metadata := input.Manifest.ApplicationSourceMetadata.Original
	if input.Side == pv.Patched {
		source = input.Manifest.Sources.Patched
		if input.Manifest.ApplicationSourceMetadata.Patched == nil {
			return "", pv.ErrBinding
		}
		metadata = *input.Manifest.ApplicationSourceMetadata.Patched
	}
	sourceDirectory := filepath.Join(pv.ApplicationWorkPath, "source")
	if err := pv.ExtractApplicationSource(ctx, archive, source, sourceDirectory, metadata); err != nil {
		return "", err
	}
	for _, directory := range []string{"compiler", "cache", "modules", "gopath"} {
		if err := os.Mkdir(filepath.Join(pv.ApplicationWorkPath, directory), 0700); err != nil {
			return "", errors.New("application compiler workspace could not be created")
		}
	}
	return sourceDirectory, nil
}

func sealBuiltApplication(input pv.ApplicationWorkerInput) (pv.ApplicationArtifact, error) {
	binaryPath := filepath.Join(pv.ApplicationArtifactsPath, pv.ApplicationBinaryName)
	file, err := openRegular(binaryPath)
	if err != nil {
		return pv.ApplicationArtifact{}, err
	}
	defer func() { _ = file.Close() }()
	digest, size, err := measureBinary(file, input)
	if err != nil {
		return pv.ApplicationArtifact{}, err
	}
	if err := os.Chmod(binaryPath, 0555); err != nil {
		return pv.ApplicationArtifact{}, err
	}
	build := input.Worker
	build.PodName, build.PodUID = os.Getenv("ORKA_APPLICATION_POD_NAME"), os.Getenv("ORKA_APPLICATION_POD_UID")
	build.Namespace, build.NamespaceUID = input.Target.Namespace, input.Target.NamespaceUID
	build.ImageID = input.Manifest.Environment.ImageID
	if build.ContainerName == "" {
		build.ContainerName = os.Getenv("ORKA_APPLICATION_CONTAINER_NAME")
	}
	artifact, err := pv.NewApplicationArtifact(input, digest, size, build)
	if err != nil {
		return pv.ApplicationArtifact{}, err
	}
	content, err := json.Marshal(artifact)
	if err != nil {
		return pv.ApplicationArtifact{}, pv.ErrEvidence
	}
	receiptPath := filepath.Join(pv.ApplicationArtifactsPath, pv.ApplicationArtifactName)
	if err := writeNewFile(receiptPath, content, 0444); err != nil {
		return pv.ApplicationArtifact{}, err
	}
	return artifact, nil
}

func boundedCommand(ctx context.Context, arguments, environment []string) *exec.Cmd {
	command := exec.CommandContext(ctx, "/usr/local/go/bin/go", arguments...)
	command.Env = environment
	command.Stderr = io.Discard
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error {
		err := syscall.Kill(-command.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	command.WaitDelay = time.Second
	return command
}

func measureBinary(file *os.File, input pv.ApplicationWorkerInput) (string, int64, error) {
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > pv.MaxApplicationBinaryBytes {
		return "", 0, pv.ErrLimit
	}
	header, err := elf.NewFile(file)
	if err != nil {
		return "", 0, errors.New("application package did not produce an executable ELF binary")
	}
	defer func() { _ = header.Close() }()
	machine := elf.EM_X86_64
	if input.Manifest.Environment.Platform == "linux/arm64" {
		machine = elf.EM_AARCH64
	}
	if header.Class != elf.ELFCLASS64 || header.Type != elf.ET_EXEC || header.Machine != machine {
		return "", 0, pv.ErrEvidence
	}
	for _, program := range header.Progs {
		if program.Type == elf.PT_INTERP {
			return "", 0, errors.New("dynamic application executables are unsupported")
		}
	}
	build, err := buildinfo.Read(file)
	if err != nil || build.GoVersion != input.Manifest.Application.Build.GoVersion {
		return "", 0, errors.New("application binary compiler identity differs from the frozen toolchain")
	}
	settings := map[string]string{}
	for _, setting := range build.Settings {
		settings[setting.Key] = setting.Value
	}
	platform := strings.Split(input.Manifest.Environment.Platform, "/")
	if settings["CGO_ENABLED"] != "0" || settings["GOOS"] != platform[0] || settings["GOARCH"] != platform[1] {
		return "", 0, errors.New("application binary does not match the fixed offline build profile")
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	hasher := sha256.New()
	n, err := io.Copy(hasher, io.LimitReader(file, pv.MaxApplicationBinaryBytes+1))
	if err != nil || n != info.Size() {
		return "", 0, errors.New("application binary measurement was incomplete")
	}
	return "sha256:" + hex.EncodeToString(hasher.Sum(nil)), n, nil
}

func writeNewFile(name string, content []byte, mode os.FileMode) error {
	file, err := os.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	n, writeErr := file.Write(content)
	syncErr := file.Sync()
	closeErr := file.Close()
	if n != len(content) {
		writeErr = io.ErrShortWrite
	}
	return errors.Join(writeErr, syncErr, closeErr)
}
