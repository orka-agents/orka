//go:build linux

package main

import (
	"errors"
	"os"
	"strconv"
	"strings"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

func openRegular(name string) (*os.File, error) {
	fd, err := unix.Open(name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, errors.New("application input is unavailable or symlinked")
	}
	file := os.NewFile(uintptr(fd), name)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, errors.New("application input must be a regular file")
	}
	return file, nil
}

// Kubernetes projected files are symlinks beneath a trusted read-only mount.
// Their path is fixed by the caller, never supplied by a plan or repository.
func openProjected(name string) (*os.File, error) {
	file, err := os.Open(name)
	if err != nil {
		return nil, errors.New("application projected input is unavailable")
	}
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, pv.ErrEvidence
	}
	return file, nil
}

func measureProcess() (pv.ApplicationBoundaryReport, error) {
	return measureProcessWithLeader(true)
}

func measureProcessWithLeader(leader bool) (pv.ApplicationBoundaryReport, error) {
	report := pv.ApplicationBoundaryReport{
		UID: os.Getuid(), GID: os.Getgid(), PID: os.Getpid(), Capabilities: map[string]string{},
	}
	if report.UID != 65532 || os.Geteuid() != 65532 || report.GID != 65532 ||
		os.Getegid() != 65532 || leader && report.PID != 1 {
		return report, errors.New("application worker must be PID 1 with the fixed unprivileged identity")
	}
	var fileSize unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_FSIZE, &fileSize) != nil || fileSize.Cur != fileSize.Max ||
		(fileSize.Max != 16<<20 && fileSize.Max != 512<<20) {
		return report, errors.New("application worker lacks a fixed inherited file-size hard limit")
	}
	report.FileSizeSoft, report.FileSizeHard = fileSize.Cur, fileSize.Max
	if err := measureProcessSecurity(&report); err != nil {
		return report, err
	}
	config, err := runtimeConfig()
	if err != nil {
		return report, err
	}
	if config != nil {
		return measureQualifiedRuntime(report, *config)
	}
	return measureNativeCgroup(report)
}

func measureProcessSecurity(report *pv.ApplicationBoundaryReport) error {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return errors.New("application process controls could not be measured")
	}
	fields := map[string]string{}
	for line := range strings.SplitSeq(string(status), "\n") {
		if key, value, ok := strings.Cut(line, ":"); ok {
			fields[key] = strings.TrimSpace(value)
		}
	}
	for _, name := range []string{"CapEff", "CapPrm", "CapBnd", "CapInh", "CapAmb"} {
		if fields[name] != "0000000000000000" {
			return errors.New("application worker retains Linux capabilities")
		}
		report.Capabilities[name] = fields[name]
	}
	report.NoNewPrivileges = fields["NoNewPrivs"] == "1"
	report.SeccompMode, _ = strconv.Atoi(fields["Seccomp"])
	var root unix.Statfs_t
	if unix.Statfs("/", &root) != nil {
		return pv.ErrEvidence
	}
	report.ReadOnlyRoot = root.Flags&unix.ST_RDONLY != 0
	if !report.NoNewPrivileges || report.SeccompMode != 2 || !report.ReadOnlyRoot {
		return errors.New("application process security or root filesystem is not restricted")
	}
	return nil
}

func measureNativeCgroup(report pv.ApplicationBoundaryReport) (pv.ApplicationBoundaryReport, error) {
	values := []struct {
		name        string
		destination *uint64
	}{
		{"memory.max", &report.MemoryMax}, {"pids.max", &report.PIDsMax}, {"memory.swap.max", &report.SwapMax},
	}
	for _, value := range values {
		content, err := os.ReadFile("/sys/fs/cgroup/" + value.name)
		if err != nil {
			return report, errors.New("application cgroup controls are unavailable")
		}
		*value.destination, err = strconv.ParseUint(strings.TrimSpace(string(content)), 10, 64)
		if err != nil {
			return report, errors.New("application cgroup must have finite bounds")
		}
	}
	cpu, err := os.ReadFile("/sys/fs/cgroup/cpu.max")
	parts := strings.Fields(string(cpu))
	if err != nil || len(parts) != 2 {
		return report, pv.ErrEvidence
	}
	report.CPUQuota, err = strconv.ParseUint(parts[0], 10, 64)
	if err != nil {
		return report, pv.ErrEvidence
	}
	report.CPUPeriod, err = strconv.ParseUint(parts[1], 10, 64)
	if err != nil || report.MemoryMax == 0 || report.MemoryMax > 2<<30 ||
		report.PIDsMax == 0 || report.PIDsMax > 512 || report.SwapMax != 0 ||
		report.CPUQuota == 0 || report.CPUPeriod == 0 || report.CPUPeriod > 1000000 ||
		report.CPUQuota > 2*report.CPUPeriod {
		return report, errors.New("application cgroup controls exceed fixed operator limits")
	}
	return report, nil
}

func noTargetCredentials() error {
	for _, path := range []string{targetTokenPath, "/var/run/secrets/kubernetes.io/serviceaccount/token", "/actors"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			return errors.New("this application worker mode must not receive Kubernetes credentials")
		}
	}
	return nil
}
