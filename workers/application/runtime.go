//go:build linux

package main

import (
	"errors"
	"io"
	"os"
	"strconv"
	"strings"
	"syscall"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"golang.org/x/sys/unix"
)

func runtimeConfig() (*pv.ApplicationRuntimeConfig, error) {
	profile := os.Getenv("ORKA_APPLICATION_RUNTIME_PROFILE")
	if profile == "" {
		return nil, nil
	}
	if profile != pv.ApplicationRuntimeGVisorBoundedV1 {
		return nil, errors.New("application runtime profile is not admitted")
	}
	var filesystem unix.Statfs_t
	if unix.Statfs(pv.ApplicationRuntimeQualificationPath, &filesystem) != nil || filesystem.Flags&unix.ST_RDONLY == 0 {
		return nil, errors.New("application runtime qualification must be read-only")
	}
	file, err := openProjected(pv.ApplicationRuntimeQualificationPath)
	if err != nil {
		return nil, err
	}
	config, readErr := pv.DecodeApplicationRuntimeConfig(file)
	closeErr := file.Close()
	if readErr != nil || closeErr != nil || config.Qualification.Profile != profile ||
		os.Getenv("ORKA_APPLICATION_NODE_NAME") != config.Qualification.NodeName ||
		os.Getenv("ORKA_APPLICATION_TARGET_CLUSTER_UID") != config.Qualification.TargetClusterUID {
		return nil, pv.ErrBinding
	}
	return &config, nil
}

func verifyRuntimeManifest(manifest pv.Manifest) error {
	config, err := runtimeConfig()
	if err != nil {
		return err
	}
	expected := manifest.Environment.Dependencies[pv.ApplicationRuntimeQualificationKey]
	if config == nil {
		if expected != "" {
			return pv.ErrBinding
		}
		return nil
	}
	digest, err := pv.ApplicationRuntimeQualificationDigest(config.Qualification)
	if err != nil || expected != digest {
		return pv.ErrBinding
	}
	return nil
}

func measureQualifiedRuntime(
	report pv.ApplicationBoundaryReport, config pv.ApplicationRuntimeConfig,
) (pv.ApplicationBoundaryReport, error) {
	mounts, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return report, errors.New("qualified guest cgroup mount identity is unavailable")
	}
	for _, mount := range []string{"/sys/fs/cgroup/memory", "/sys/fs/cgroup/cpu"} {
		found := false
		for line := range strings.SplitSeq(string(mounts), "\n") {
			left, right, ok := strings.Cut(line, " - ")
			fields, filesystem := strings.Fields(left), strings.Fields(right)
			found = found || ok && len(fields) > 4 && len(filesystem) > 0 && fields[4] == mount && filesystem[0] == "cgroup"
		}
		if !found {
			return report, errors.New("qualified guest limits are not served by genuine cgroup mounts")
		}
	}
	report.MemoryMax, err = readGuestCgroupLimit("/sys/fs/cgroup/memory/memory.limit_in_bytes")
	if err != nil {
		return report, err
	}
	report.CPUQuota, err = readGuestCgroupLimit("/sys/fs/cgroup/cpu/cpu.cfs_quota_us")
	if err != nil {
		return report, err
	}
	report.CPUPeriod, err = readGuestCgroupLimit("/sys/fs/cgroup/cpu/cpu.cfs_period_us")
	if err != nil {
		return report, err
	}
	var processes unix.Rlimit
	if unix.Getrlimit(unix.RLIMIT_NPROC, &processes) != nil || processes.Cur != 512 || processes.Max != 512 {
		return report, errors.New("qualified guest process limit is missing or differs from the proven inherited hard limit")
	}
	report.GuestPIDSoft, report.GuestPIDHard, report.PIDsMax = processes.Cur, processes.Max, processes.Max
	report.PIDLimitSource = "guest-rlimit-nproc"
	// No virtual memsw/pids display is treated as enforcement. No-swap is an
	// explicitly attributed, independently qualified native ancestor property.
	report.SwapMax, report.SwapLimitSource = config.Qualification.NativeSwapMax, config.Qualification.NativeSwapSource
	report.RuntimeProfile = config.Qualification.Profile
	report.RuntimeQualificationDigest, err = pv.ApplicationRuntimeQualificationDigest(config.Qualification)
	if err != nil || report.MemoryMax != config.Limits.MemoryMax ||
		report.CPUQuota != config.Limits.CPUQuota || report.CPUPeriod != config.Limits.CPUPeriod ||
		report.PIDsMax != config.Limits.PIDsMax || report.FileSizeSoft != config.Limits.FileSizeMax ||
		report.FileSizeHard != config.Limits.FileSizeMax {
		return report, errors.New("qualified guest limits do not match the exact rendered role limits")
	}
	return report, nil
}

func readGuestCgroupLimit(name string) (uint64, error) {
	file, err := openRegular(name)
	if err != nil {
		return 0, err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return 0, pv.ErrEvidence
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	var filesystem unix.Statfs_t
	if !ok || stat.Uid != 0 || info.Mode().Perm()&0022 != 0 ||
		unix.Fstatfs(int(file.Fd()), &filesystem) != nil || filesystem.Type != unix.CGROUP_SUPER_MAGIC {
		return 0, errors.New("qualified guest limits are not root-owned non-writable cgroup files")
	}
	content, err := io.ReadAll(io.LimitReader(file, 129))
	if err != nil || len(content) > 128 {
		return 0, pv.ErrEvidence
	}
	value, err := strconv.ParseUint(strings.TrimSpace(string(content)), 10, 64)
	if err != nil || value == 0 {
		return 0, errors.New("qualified guest resource limits must be finite and positive")
	}
	return value, nil
}
