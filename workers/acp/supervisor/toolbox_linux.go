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
	attr := &syscall.SysProcAttr{Setpgid: true}
	if os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{Uid: toolboxCheckUID, Gid: toolboxCheckUID, NoSetGroups: true}
	}
	return attr
}
