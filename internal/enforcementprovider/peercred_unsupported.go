//go:build !linux

package enforcementprovider

import (
	"errors"
	"net"
)

func authenticateUnixPeer(*net.UnixConn, uint32) error {
	return errors.New("Unix peer credentials are unsupported on this platform")
}
