//go:build windows

package common

import "syscall"

func gitCommandSysProcAttr() *syscall.SysProcAttr { return nil }
