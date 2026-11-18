//go:build linux

package main

import (
	"context"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"golang.org/x/sys/unix"
)

func TestGuardValidatesKernelListenerShape(t *testing.T) {
	probe := compileGuardFixture(t, "testdata/guard_probe.c")
	for _, test := range []struct {
		name           string
		address        [16]byte
		requestV6Only  int
		observedV6Only int
		valid          bool
	}{
		{name: "wildcard dual stack", observedV6Only: 0, valid: true},
		{name: "loopback becomes IPv6 only", address: [16]byte{15: 1}, observedV6Only: 1, valid: true},
		{name: "wildcard IPv6-only is not the fixture contract", requestV6Only: 1, observedV6Only: 1},
		{name: "mapped IPv4 is not an assigned listener",
			address: [16]byte{10: 0xff, 11: 0xff, 12: 127, 15: 1}, observedV6Only: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			fd, err := unix.Socket(unix.AF_INET6, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, unix.IPPROTO_TCP)
			require.NoError(t, err)
			listener := os.NewFile(uintptr(fd), "listener")
			t.Cleanup(func() { require.NoError(t, listener.Close()) })
			require.NoError(t, unix.SetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY, test.requestV6Only))
			require.NoError(t, unix.Bind(fd, &unix.SockaddrInet6{Addr: test.address}))
			require.NoError(t, unix.Listen(fd, 1))
			mode, err := unix.GetsockoptInt(fd, unix.IPPROTO_IPV6, unix.IPV6_V6ONLY)
			require.NoError(t, err)
			require.Equal(t, test.observedV6Only, mode)
			address, err := unix.Getsockname(fd)
			require.NoError(t, err)
			port := address.(*unix.SockaddrInet6).Port
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			command := exec.CommandContext(ctx, probe, "listener-check", strconv.Itoa(port))
			command.ExtraFiles = []*os.File{listener}
			err = command.Run()
			if test.valid {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}
