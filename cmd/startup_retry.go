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

// startupRetryBudget is how long after the first API server attempt startup
// keeps starting new attempts against in-cluster dependencies that come up
// with the node. After a node restart, kube-proxy can take tens of seconds to
// route Service addresses. An attempt already running can end later: a dial
// to an unrouted address blocks for client-go's 30s dial timeout.
const startupRetryBudget = 90 * time.Second

// startupRetry is shared by the startup steps that reach the API server or
// the Workspace/Publisher, so their retries together end at one deadline.
type startupRetry struct {
	log logr.Logger
	// deadline is when the last attempt may start. The zero value allows only
	// the first attempt.
	deadline     time.Time
	initialDelay time.Duration
	maxDelay     time.Duration
}

func newStartupRetry(log logr.Logger, now time.Time) startupRetry {
	return startupRetry{
		log: log, deadline: now.Add(startupRetryBudget), initialDelay: time.Second, maxDelay: 8 * time.Second,
	}
}

// retryStartupStep calls step until it succeeds or fails for a reason
// isTransient rejects. A cancelled ctx or the retry deadline returns the last
// error unchanged, so startup then fails as it would without the retry.
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
		// Logging or scheduling can delay the wake-up past the deadline.
		if ctx.Err() != nil || time.Now().After(retry.deadline) {
			return result, err
		}
		delay = min(2*delay, retry.maxDelay)
	}
}

// isTransientConnectError reports whether err shows that no connection could
// be made: a timeout, a refused or reset connection, an unreachable host or
// network, or a temporary DNS failure. Everything else, including API
// responses such as 401 and 403, TLS verification failures and configuration
// errors, is not transient.
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

// isTransientPublisherError applies isTransientConnectError to the cause of a
// failed Workspace/Publisher request. Responses from the Publisher, including
// authorization failures, are not transient.
func isTransientPublisherError(err error) bool {
	return isTransientConnectError(publisherservice.TransportCause(err))
}
