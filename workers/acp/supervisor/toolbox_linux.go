//go:build linux

package supervisor

import (
	"os"
	"syscall"
)

// toolboxCheckSysProcAttr runs the check as the fixed unprivileged identity
// when the supervisor is root, so root never parses toolbox files. A
// non-root supervisor (tests) runs it as itself.
func toolboxCheckSysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 {
		// An empty Groups list clears the supervisor's supplementary groups
		// (including group 0) so the check sees exactly what an unprivileged
		// identity can read, matching the session launcher.
		attr.Credential = &syscall.Credential{Uid: toolboxCheckUID, Gid: toolboxCheckUID, Groups: []uint32{}}
	}
	return attr
}
