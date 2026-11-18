//go:build linux

package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

func runSubject(ctx context.Context, input pv.ApplicationWorkerInput) error {
	if _, err := measureProcess(); err != nil {
		return err
	}
	if input.Artifact == nil ||
		pv.ValidateApplicationArtifact(input.Manifest, input.Binding, input.Side, *input.Artifact) != nil {
		return pv.ErrBinding
	}
	if err := verifySubjectTLS(input); err != nil {
		return err
	}
	var volume unix.Statfs_t
	if unix.Statfs(pv.ApplicationArtifactsPath, &volume) != nil || volume.Flags&unix.ST_RDONLY == 0 {
		return errors.New("application artifact volume must be read-only")
	}
	receipt, err := openRegular(filepath.Join(pv.ApplicationArtifactsPath, pv.ApplicationArtifactName))
	if err != nil {
		return err
	}
	onDisk, readErr := pv.DecodeApplicationArtifact(receipt)
	closeErr := receipt.Close()
	if readErr != nil || closeErr != nil || pv.ValidateApplicationArtifactFile(*input.Artifact, onDisk) != nil {
		return errors.New("application artifact receipt does not match the frozen build")
	}
	file, err := openRegular(filepath.Join(pv.ApplicationArtifactsPath, pv.ApplicationBinaryName))
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	digest, size, err := measureBinary(file, input)
	if err != nil || digest != input.Artifact.BinaryDigest || size != input.Artifact.BinaryBytes {
		return errors.New("application executable differs from the measured build artifact")
	}
	arguments := []string{"/artifacts/application"}
	for _, argument := range input.Manifest.Application.Runtime.Args {
		arguments = append(arguments, strings.ReplaceAll(argument, "{{namespace}}", input.Target.Namespace))
	}
	environment := []string{
		"PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
		workerHomeEnv, "TMPDIR=/work", workerLanguageEnv, workerLocaleEnv,
		"ORKA_APPLICATION_NAMESPACE=" + input.Target.Namespace,
		"POD_NAMESPACE=" + input.Target.Namespace, "POD_NAME=" + os.Getenv("ORKA_APPLICATION_POD_NAME"),
		"POD_UID=" + os.Getenv("ORKA_APPLICATION_POD_UID"),
		"GOMAXPROCS=2", "GOMEMLIMIT=1536MiB",
	}
	for _, name := range []string{
		"KUBERNETES_SERVICE_HOST", "KUBERNETES_SERVICE_PORT", "KUBERNETES_SERVICE_PORT_HTTPS",
	} {
		if value := os.Getenv(name); value != "" {
			environment = append(environment, name+"="+value)
		}
	}
	operatorEnvironment, err := pv.ApplicationSubjectEnvironmentVariables(
		input.Manifest.ApplicationPolicy, input.Target.Namespace,
	)
	if err != nil {
		return err
	}
	environment = append(environment, operatorEnvironment...)
	if err := os.Chdir(pv.ApplicationWorkPath); err != nil {
		return pv.ErrEvidence
	}
	null, err := unix.Open("/dev/null", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return pv.ErrEvidence
	}
	defer func() { _ = unix.Close(null) }()
	for _, fd := range []int{0, 1, 2} {
		if err := unix.Dup2(null, fd); err != nil {
			return pv.ErrEvidence
		}
	}
	// Execute the already measured inode, not a path that can be swapped after
	// measurement. The FD is close-on-exec and the artifact mount is read-only.
	// After exec, only the backend's persisted Job deadline/cleanup is a trusted
	// wall-clock boundary; a timer inside the candidate cannot enforce it.
	if err := ctx.Err(); err != nil {
		return err
	}
	return unix.Exec("/proc/self/fd/"+strconv.FormatUint(uint64(file.Fd()), 10), arguments, environment)
}
