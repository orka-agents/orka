//go:build linux

package supervisor

import (
	"os"
	"syscall"
)

func toolboxCheckSysProcAttr() *syscall.SysProcAttr {
	attr := &syscall.SysProcAttr{Setpgid: true, Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{Uid: toolboxCheckUID, Gid: toolboxCheckUID, Groups: []uint32{}}
	}
	return attr
}
