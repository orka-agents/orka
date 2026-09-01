/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"errors"
	"net"
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
	if calls != 6 {
		t.Fatalf("probe calls = %d, want 6 so a successful probe resets the blocked count", calls)
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
