package source

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// RateLimitError contains only scheduling metadata, never response content,
// credentials or a requested path. The caller retains its exact source intent.
type RateLimitError struct {
	RetryAt    time.Time
	StatusCode int
}

func (e *RateLimitError) Error() string {
	if e.StatusCode == http.StatusForbidden || e.StatusCode == http.StatusTooManyRequests {
		return fmt.Sprintf("anonymous GitHub source access is rate limited (HTTP %d)", e.StatusCode)
	}
	return "anonymous GitHub source access is rate limited"
}

func sourceRateLimit(response *http.Response, now time.Time) *RateLimitError {
	if response.StatusCode != http.StatusTooManyRequests &&
		(response.StatusCode != http.StatusForbidden || response.Header.Get("X-RateLimit-Remaining") != "0") {
		return nil
	}
	retryAt := now.Add(time.Minute)
	if epoch, err := strconv.ParseInt(response.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		retryAt = time.Unix(epoch, 0).Add(time.Second)
	}
	if seconds, err := strconv.ParseInt(response.Header.Get("Retry-After"), 10, 64); err == nil && seconds > 0 && seconds <= 3600 {
		if delay := now.Add(time.Duration(seconds) * time.Second); delay.After(retryAt) {
			retryAt = delay
		}
	}
	if retryAt.Before(now.Add(time.Second)) {
		retryAt = now.Add(time.Minute)
	}
	if retryAt.After(now.Add(time.Hour + time.Minute)) {
		retryAt = now.Add(time.Hour + time.Minute)
	}
	return &RateLimitError{RetryAt: retryAt.UTC(), StatusCode: response.StatusCode}
}
