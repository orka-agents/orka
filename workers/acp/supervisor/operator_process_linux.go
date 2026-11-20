//go:build linux

package supervisor

import (
	"errors"

	"golang.org/x/sys/unix"
)

// waitOperatorLeaderExit blocks until the operation leader has exited without
// reaping it, so its PID (the process group ID) stays allocated until the
// caller reaps it under the signal fence.
func waitOperatorLeaderExit(pid int) {
	var info unix.Siginfo
	for {
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); !errors.Is(err, unix.EINTR) {
			return
		}
	}
}
