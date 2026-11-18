package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"syscall"
	"testing"

	pv "github.com/orka-agents/orka/internal/patchverification"
	"github.com/stretchr/testify/require"
)

func TestTransientTransportFailuresNeverBecomeBindingFailures(t *testing.T) {
	for _, err := range []error{io.ErrUnexpectedEOF, io.ErrClosedPipe, net.ErrClosed, syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE} {
		require.True(t, IsTransientError(fmt.Errorf("transport: %w", err)))
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded, pv.ErrBinding, pv.ErrEvidence, errors.New("invalid report")} {
		require.False(t, IsTransientError(err))
	}
}
