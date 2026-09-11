//go:build unix

package main

import "syscall"

func setPrivateUmask() {
	_ = syscall.Umask(0o077)
}
