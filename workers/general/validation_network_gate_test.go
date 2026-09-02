/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"net"
	"slices"
	"testing"
	"time"
)

func TestWaitForValidationNetworkAccessRequiresSuccessfulProbe(t *testing.T) {
	originalDial := validationNetworkDial
	originalInterval := validationNetworkProbeInterval
	t.Cleanup(func() {
		validationNetworkDial = originalDial
		validationNetworkProbeInterval = originalInterval
	})

	validationNetworkProbeInterval = time.Millisecond
	calls := 0
	validationNetworkDial = func(context.Context, string) (net.Conn, error) {
		calls++
		if calls < 3 {
			return nil, errors.New("unreachable")
		}
		left, right := net.Pipe()
		_ = right.Close()
		return left, nil
	}

	if err := waitForValidationNetworkAccess(context.Background(), []string{"github.com:443"}); err != nil {
		t.Fatalf("waitForValidationNetworkAccess() error = %v", err)
	}
	if calls != 3 {
		t.Fatalf("probe calls = %d, want 3", calls)
	}
}

func TestWaitForValidationNetworkPolicyRequiresConsecutiveBlockedProbes(t *testing.T) {
	originalRead := validationNetworkReadGate
	originalDial := validationNetworkDial
	originalInterval := validationNetworkProbeInterval
	t.Cleanup(func() {
		validationNetworkReadGate = originalRead
		validationNetworkDial = originalDial
		validationNetworkProbeInterval = originalInterval
	})

	validationNetworkReadGate = func(string) ([]byte, error) { return []byte("true"), nil }
	validationNetworkProbeInterval = time.Millisecond
	locks := 0
	stubValidationNetworkLock(t, func(context.Context) error {
		locks++
		return nil
	})
	calls := 0
	validationNetworkDial = func(context.Context, string) (net.Conn, error) {
		calls++
		if calls == 3 {
			left, right := net.Pipe()
			_ = right.Close()
			return left, nil
		}
		return nil, errors.New("blocked")
	}

	if err := waitForValidationNetworkPolicy(context.Background(), []string{"/gate/ready", "github.com:443"}); err != nil {
		t.Fatalf("waitForValidationNetworkPolicy() error = %v", err)
	}
	if calls != 6 || locks != 1 {
		t.Fatalf("probe calls/network locks = %d/%d, want 6/1 so a successful probe resets the blocked count", calls, locks)
	}
}

func TestWaitForValidationNetworkPolicyWaitsForControllerGate(t *testing.T) {
	originalRead := validationNetworkReadGate
	originalDial := validationNetworkDial
	originalInterval := validationNetworkProbeInterval
	t.Cleanup(func() {
		validationNetworkReadGate = originalRead
		validationNetworkDial = originalDial
		validationNetworkProbeInterval = originalInterval
	})

	reads := 0
	validationNetworkReadGate = func(string) ([]byte, error) {
		reads++
		if reads < 3 {
			return []byte("false"), nil
		}
		return []byte("true"), nil
	}
	validationNetworkProbeInterval = time.Millisecond
	stubValidationNetworkLock(t, func(context.Context) error { return nil })
	dials := 0
	validationNetworkDial = func(context.Context, string) (net.Conn, error) {
		dials++
		return nil, errors.New("blocked")
	}

	if err := waitForValidationNetworkPolicy(context.Background(), []string{"/gate/ready", "github.com:443"}); err != nil {
		t.Fatalf("waitForValidationNetworkPolicy() error = %v", err)
	}
	if reads < 5 || dials != validationNetworkBlockedProbeCount {
		t.Fatalf(
			"gate reads/probes = %d/%d, want pending reads before %d blocked probes",
			reads, dials, validationNetworkBlockedProbeCount,
		)
	}
}

func TestWaitForValidationNetworkPolicyFailsClosedWhenNetworkLockFails(t *testing.T) {
	originalRead := validationNetworkReadGate
	originalDial := validationNetworkDial
	originalInterval := validationNetworkProbeInterval
	t.Cleanup(func() {
		validationNetworkReadGate = originalRead
		validationNetworkDial = originalDial
		validationNetworkProbeInterval = originalInterval
	})

	validationNetworkReadGate = func(string) ([]byte, error) { return []byte("true"), nil }
	validationNetworkDial = func(context.Context, string) (net.Conn, error) { return nil, errors.New("blocked") }
	validationNetworkProbeInterval = time.Millisecond
	lockErr := errors.New("network lock unavailable")
	stubValidationNetworkLock(t, func(context.Context) error { return lockErr })

	err := waitForValidationNetworkPolicy(
		context.Background(),
		[]string{"/gate/ready", "github.com:443"},
	)
	if !errors.Is(err, lockErr) {
		t.Fatalf("waitForValidationNetworkPolicy() error = %v, want network lock failure", err)
	}
}

func TestLockValidationNetworkInterfacesDisablesEveryNonLoopbackInterface(t *testing.T) {
	originalInterfaces := validationNetworkInterfaces
	originalSetLinkDown := validationNetworkSetLinkDown
	t.Cleanup(func() {
		validationNetworkInterfaces = originalInterfaces
		validationNetworkSetLinkDown = originalSetLinkDown
	})

	validationNetworkInterfaces = func() ([]net.Interface, error) {
		return []net.Interface{
			{Name: "lo", Flags: net.FlagLoopback | net.FlagUp},
			{Name: "eth0", Flags: net.FlagUp},
			{Name: "net1", Flags: net.FlagUp},
		}, nil
	}
	var disabled []string
	validationNetworkSetLinkDown = func(_ context.Context, name string) error {
		disabled = append(disabled, name)
		return nil
	}

	if err := lockValidationNetworkInterfaces(context.Background()); err != nil {
		t.Fatalf("lockValidationNetworkInterfaces() error = %v", err)
	}
	if !slices.Equal(disabled, []string{"eth0", "net1"}) {
		t.Fatalf("disabled interfaces = %v, want eth0 and net1", disabled)
	}
}

func stubValidationNetworkLock(t *testing.T, lock func(context.Context) error) {
	t.Helper()
	original := validationNetworkLock
	validationNetworkLock = lock
	t.Cleanup(func() { validationNetworkLock = original })
}
