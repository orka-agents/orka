//go:build unix && !linux

package acp

import "golang.org/x/sys/unix"

func hardenSupervisorPlatform() (HardeningStatus, error) {
	if err := unix.Setrlimit(unix.RLIMIT_CORE, &unix.Rlimit{Cur: 0, Max: 0}); err != nil {
		return HardeningStatus{}, err
	}
	unix.Umask(0o077)
	return HardeningStatus{Subreaper: false, CoreDumpsDisabled: true, PrivateUmask: true}, nil
}
