//go:build linux

package enforcementprovider

import (
	"fmt"
	"net"
	"syscall"
)

func authenticateUnixPeer(conn *net.UnixConn, allowedUID uint32) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var credential *syscall.Ucred
	var socketErr error
	if err = raw.Control(func(fd uintptr) {
		credential, socketErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return err
	}
	if socketErr != nil {
		return socketErr
	}
	if credential == nil || credential.Uid != allowedUID {
		var actual uint32
		if credential != nil {
			actual = credential.Uid
		}
		return fmt.Errorf("provider peer uid %d is not trusted", actual)
	}
	return nil
}
