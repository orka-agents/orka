//go:build linux

package supervisor

import (
	"fmt"
	"os"
	"runtime"

	"golang.org/x/sys/unix"
)

// asSessionChild runs fn on a thread whose filesystem identity is the
// session child's, so the kernel checks fn's file operations exactly as if
// the child made them. The supervisor holds CHOWN, SETUID and SETGID but not
// DAC_OVERRIDE, so as itself it cannot create files in the child's 0700
// directories at all. The thread is never unlocked: the runtime retires it
// when the goroutine exits instead of reusing its changed identity.
//
// Only this thread changes identity, so fn must do its work on it: use only
// os.Root operations on a handle opened beforehand, and start no goroutines
// or processes, which would run as the supervisor.
func asSessionChild(uid, gid int, fn func() error) error {
	result := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		// Drop the supervisor's supplementary groups on this thread, as
		// session children are started without any. The raw call changes
		// only this thread; syscall.Setgroups would change every thread.
		// Only an unprivileged process acting as itself, which changes no
		// identity at all, may keep its own groups; any other refusal
		// fails closed.
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
		// setfsuid and setfsgid report the previous value rather than
		// failing a refused change; an invalid ID reads the current one.
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
