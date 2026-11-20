/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package workspace

import (
	"context"
	"maps"
	"strings"
	"time"
)

func contextWithTimeout(ctx context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	if timeout <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, timeout)
}

func sleepContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func copyStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	maps.Copy(out, in)
	return out
}

func releaseMessage(reason, fallback string) string {
	if strings.TrimSpace(reason) == "" {
		return fallback
	}
	return fallback + ": " + strings.TrimSpace(reason)
}
