package kube

import (
	"context"
	"errors"
	"io"
	"net"
	"syscall"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// IsTransientError allows observing the same accepted workload again. It never
// authorizes a new execution and excludes integrity and expired-budget errors.
func IsTransientError(err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsServiceUnavailable(err) ||
		apierrors.IsTooManyRequests(err) || apierrors.IsConflict(err) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) ||
		errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var status interface{ Status() metav1.Status }
	if errors.As(err, &status) && status.Status().Code >= 500 && status.Status().Code <= 599 {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}
