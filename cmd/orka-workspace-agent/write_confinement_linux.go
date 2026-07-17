//go:build linux

package main

import (
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

const minimumLandlockWriteABI = 3

func commandWriteConfinementSupported() bool {
	abi, err := landlockABIVersion()
	return err == nil && abi >= minimumLandlockWriteABI
}

func runWriteConfinedCommand(command []string) error {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if len(command) == 0 || command[0] == "" {
		return fmt.Errorf("command is required")
	}
	executable, err := exec.LookPath(command[0])
	if err != nil {
		return fmt.Errorf("resolve command: %w", err)
	}
	if err := applyCommandWriteConfinement(commandWritableRoots); err != nil {
		return err
	}
	return unix.Exec(executable, command, os.Environ())
}

func applyCommandWriteConfinement(roots []string) error {
	abi, err := landlockABIVersion()
	if err != nil {
		return fmt.Errorf("Landlock is unavailable: %w", err)
	}
	if abi < minimumLandlockWriteABI {
		return fmt.Errorf("Landlock ABI %d lacks truncate confinement", abi)
	}
	access := landlockWriteAccess(abi)
	ruleset := unix.LandlockRulesetAttr{Access_fs: access}
	rulesetFD, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		uintptr(unsafe.Pointer(&ruleset)),
		unsafe.Sizeof(ruleset),
		0,
	)
	if errno != 0 {
		return fmt.Errorf("create Landlock ruleset: %w", errno)
	}
	defer unix.Close(int(rulesetFD)) //nolint:errcheck

	for _, root := range roots {
		pathFD, err := unix.Open(root, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			return fmt.Errorf("open confined write root: %w", err)
		}
		rule := unix.LandlockPathBeneathAttr{
			Allowed_access: access,
			Parent_fd:      int32(pathFD), //nolint:gosec // file descriptors are bounded ints
		}
		_, _, errno = unix.Syscall6(
			unix.SYS_LANDLOCK_ADD_RULE,
			rulesetFD,
			uintptr(unix.LANDLOCK_RULE_PATH_BENEATH),
			uintptr(unsafe.Pointer(&rule)),
			0,
			0,
			0,
		)
		_ = unix.Close(pathFD)
		if errno != 0 {
			return fmt.Errorf("add Landlock path rule: %w", errno)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("set no-new-privileges: %w", err)
	}
	_, _, errno = unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, rulesetFD, 0, 0)
	if errno != 0 {
		return fmt.Errorf("apply Landlock ruleset: %w", errno)
	}
	return nil
}

func landlockABIVersion() (int, error) {
	version, _, errno := unix.Syscall(
		unix.SYS_LANDLOCK_CREATE_RULESET,
		0,
		0,
		uintptr(unix.LANDLOCK_CREATE_RULESET_VERSION),
	)
	if errno != 0 {
		return 0, errno
	}
	if version < 1 {
		return 0, fmt.Errorf("invalid Landlock ABI version %d", version)
	}
	return int(version), nil
}

func landlockWriteAccess(abi int) uint64 {
	access := uint64(
		unix.LANDLOCK_ACCESS_FS_WRITE_FILE |
			unix.LANDLOCK_ACCESS_FS_REMOVE_DIR |
			unix.LANDLOCK_ACCESS_FS_REMOVE_FILE |
			unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
			unix.LANDLOCK_ACCESS_FS_MAKE_DIR |
			unix.LANDLOCK_ACCESS_FS_MAKE_REG |
			unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
			unix.LANDLOCK_ACCESS_FS_MAKE_FIFO |
			unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK |
			unix.LANDLOCK_ACCESS_FS_MAKE_SYM,
	)
	if abi >= 2 {
		access |= unix.LANDLOCK_ACCESS_FS_REFER
	}
	if abi >= 3 {
		access |= unix.LANDLOCK_ACCESS_FS_TRUNCATE
	}
	return access
}
