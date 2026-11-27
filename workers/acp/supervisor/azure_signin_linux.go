//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

func asSessionChild(uid, gid int, fn func() error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		if _, _, errno := unix.RawSyscall(unix.SYS_SETGROUPS, 0, 0, 0); errno != 0 {
			if errno != unix.EPERM || os.Geteuid() == 0 || uid != os.Geteuid() || gid != os.Getegid() {
				result <- fmt.Errorf("drop the supervisor's groups: %w", errno)
				return
			}
		} else if groups, err := unix.Getgroups(); err != nil || len(groups) != 0 {
			result <- fmt.Errorf("the thread kept supplementary groups %v: %v", groups, err)
			return
		}
		if _, err := unix.SetfsgidRetGid(gid); err != nil {
			result <- fmt.Errorf("take the session's group: %w", err)
			return
		}
		if _, err := unix.SetfsuidRetUid(uid); err != nil {
			result <- fmt.Errorf("take the session's user: %w", err)
			return
		}
		if current, _ := unix.SetfsuidRetUid(-1); current != uid {
			result <- fmt.Errorf("the thread kept filesystem user %d, not the session's %d", current, uid)
			return
		}
		if current, _ := unix.SetfsgidRetGid(-1); current != gid {
			result <- fmt.Errorf("the thread kept filesystem group %d, not the session's %d", current, gid)
			return
		}
		result <- fn()
	}()
	return <-result
}
