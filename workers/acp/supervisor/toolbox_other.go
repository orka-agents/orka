//go:build !linux

package supervisor

import "syscall"

func toolboxCheckSysProcAttr() *syscall.SysProcAttr { return nil }
