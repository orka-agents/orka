//go:build linux

package supervisor

import (
	"errors"

	"golang.org/x/sys/unix"
)

func waitOperatorLeaderExit(pid int) {
	var info unix.Siginfo
	for {
		if err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil); !errors.Is(err, unix.EINTR) {
			return
		}
	}
}
