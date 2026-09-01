/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

const validationNetworkBlockedProbeCount = 3

var (
	validationNetworkProbeInterval = time.Second
	validationNetworkReadGate      = os.ReadFile
	validationNetworkDial          = func(ctx context.Context, address string) (net.Conn, error) {
		dialer := net.Dialer{Timeout: time.Second}
		return dialer.DialContext(ctx, "tcp", address)
	}
)

// waitForValidationNetworkPolicy blocks the application container until the
// controller has created a deny-all NetworkPolicy and that policy is observable
// from the Pod's own network namespace. The workspace clone just reached the
// probe address from this Pod, so consecutive connection failures after the
// gate flips are evidence that the dataplane changed rather than an API-only
// observation.
func waitForValidationNetworkPolicy(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("validation network gate requires a gate file and probe address")
	}
	gateFile := strings.TrimSpace(args[0])
	probeAddress := strings.TrimSpace(args[1])
	if gateFile == "" || probeAddress == "" {
		return fmt.Errorf("validation network gate file and probe address must not be empty")
	}

	blocked := 0
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		state, err := validationNetworkReadGate(gateFile)
		if err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("read validation network gate: %w", err)
		}
		if strings.TrimSpace(string(state)) != "true" {
			blocked = 0
			if err := waitForValidationNetworkProbe(ctx); err != nil {
				return err
			}
			continue
		}

		connection, dialErr := validationNetworkDial(ctx, probeAddress)
		if connection != nil {
			_ = connection.Close()
		}
		if dialErr == nil {
			blocked = 0
		} else {
			if err := ctx.Err(); err != nil {
				return err
			}
			blocked++
			if blocked >= validationNetworkBlockedProbeCount {
				return nil
			}
		}
		if err := waitForValidationNetworkProbe(ctx); err != nil {
			return err
		}
	}
}

func waitForValidationNetworkProbe(ctx context.Context) error {
	timer := time.NewTimer(validationNetworkProbeInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
