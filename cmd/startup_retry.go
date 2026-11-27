/*
Copyright (c) 2026.
MIT License - see LICENSE file for details.
*/
package main

import (
	"context"
	"errors"
	"net"
	"syscall"
	"time"

	"github.com/go-logr/logr"

	publisherservice "github.com/orka-agents/orka/internal/publisher/service"
)

const startupRetryBudget = 90 * time.Second

type startupRetry struct {
	log          logr.Logger
	deadline     time.Time
	initialDelay time.Duration
	maxDelay     time.Duration
}

func newStartupRetry(log logr.Logger, now time.Time) startupRetry {
	return startupRetry{
		log: log, deadline: now.Add(startupRetryBudget), initialDelay: time.Second, maxDelay: 8 * time.Second,
	}
}

func retryStartupStep[T any](
	ctx context.Context,
	retry startupRetry,
	dependency string,
	isTransient func(error) bool,
	step func() (T, error),
) (T, error) {
	start := time.Now()
	delay := retry.initialDelay
	for attempt := 1; ; attempt++ {
		result, err := step()
		if err == nil {
			if attempt > 1 {
				retry.log.Info("Startup dependency is reachable", "dependency", dependency,
					"attempts", attempt, "elapsed", time.Since(start).Round(time.Millisecond).String())
			}
			return result, nil
		}
		if !isTransient(err) || ctx.Err() != nil || time.Now().Add(delay).After(retry.deadline) {
			return result, err
		}
		retry.log.Info("Startup dependency is not reachable yet; retrying", "dependency", dependency,
			"attempt", attempt, "retryAfter", delay.String(), "error", err.Error())
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return result, err
		case <-timer.C:
		}
		if ctx.Err() != nil || time.Now().After(retry.deadline) {
			return result, err
		}
		delay = min(2*delay, retry.maxDelay)
	}
}

func isTransientConnectError(err error) bool {
	if err == nil {
		return false
	}
	for _, errno := range []syscall.Errno{
		syscall.ECONNREFUSED, syscall.ECONNRESET, syscall.EHOSTUNREACH, syscall.ENETUNREACH,
	} {
		if errors.Is(err, errno) {
			return true
		}
	}
	if dnsErr, ok := errors.AsType[*net.DNSError](err); ok {
		return dnsErr.IsTimeout || dnsErr.IsTemporary
	}
	netErr, ok := errors.AsType[net.Error](err)
	return ok && netErr.Timeout()
}

func isTransientPublisherError(err error) bool {
	return isTransientConnectError(publisherservice.TransportCause(err))
}
