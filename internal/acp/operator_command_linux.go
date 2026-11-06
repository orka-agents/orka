//go:build linux

package acp

import (
	"fmt"
	"os"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

func operatorTTYSysProcAttr(uid, gid int) *syscall.SysProcAttr {
	// Setsid makes the helper its own process-group leader, which satisfies
	// the exec helper's process-group fence exactly like Setpgid does.
	attr := &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0, Pdeathsig: syscall.SIGKILL}
	if os.Geteuid() == 0 {
		attr.Credential = &syscall.Credential{Uid: uint32(uid), Gid: uint32(gid), Groups: []uint32{}}
	}
	return attr
}

// OpenOperatorPTY allocates a pseudo-terminal whose slave is owned by the
// operator command identity.
func OpenOperatorPTY(uid, gid int, rows, cols uint16) (*os.File, *os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("open pseudo-terminal: %w", err)
	}
	fail := func(err error) (*os.File, *os.File, error) {
		_ = master.Close()
		return nil, nil, err
	}
	conn, err := master.SyscallConn()
	if err != nil {
		return fail(err)
	}
	var index int
	var ioctlErr error
	if err := conn.Control(func(fd uintptr) {
		if ioctlErr = unix.IoctlSetPointerInt(int(fd), unix.TIOCSPTLCK, 0); ioctlErr != nil {
			return
		}
		index, ioctlErr = unix.IoctlGetInt(int(fd), unix.TIOCGPTN)
	}); err != nil {
		return fail(err)
	}
	if ioctlErr != nil {
		return fail(fmt.Errorf("unlock pseudo-terminal: %w", ioctlErr))
	}
	slave, err := os.OpenFile("/dev/pts/"+strconv.Itoa(index), os.O_RDWR|syscall.O_NOCTTY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fail(fmt.Errorf("open pseudo-terminal slave: %w", err))
	}
	if os.Geteuid() == 0 {
		if err := slave.Chown(uid, gid); err != nil {
			_ = slave.Close()
			return fail(fmt.Errorf("assign pseudo-terminal ownership: %w", err))
		}
	}
	if rows > 0 && cols > 0 {
		if err := ResizeOperatorPTY(master, rows, cols); err != nil {
			_ = slave.Close()
			return fail(err)
		}
	}
	return master, slave, nil
}

// ResizeOperatorPTY applies a terminal window size.
func ResizeOperatorPTY(master *os.File, rows, cols uint16) error {
	conn, err := master.SyscallConn()
	if err != nil {
		return err
	}
	var ioctlErr error
	if err := conn.Control(func(fd uintptr) {
		ioctlErr = unix.IoctlSetWinsize(int(fd), unix.TIOCSWINSZ, &unix.Winsize{Row: rows, Col: cols})
	}); err != nil {
		return err
	}
	return ioctlErr
}

// OperatorSignal maps an allowed operator signal name.
func OperatorSignal(name string) (os.Signal, bool) {
	switch name {
	case "INT":
		return syscall.SIGINT, true
	case "TERM":
		return syscall.SIGTERM, true
	case "HUP":
		return syscall.SIGHUP, true
	case "QUIT":
		return syscall.SIGQUIT, true
	case "KILL":
		return syscall.SIGKILL, true
	default:
		return nil, false
	}
}
