//go:build !windows

package common

import "syscall"

// Git commands inherit the worker identity. ACP runtime children perform their
// own UID/GID isolation in the v2 supervisor and never route through workers/common.
func gitCommandSysProcAttr() *syscall.SysProcAttr { return nil }
