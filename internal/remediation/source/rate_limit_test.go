package source

import (
	"net/http"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSourceRateLimitRetainsOnlyBoundedRetryMetadata(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, test := range []struct {
		status    int
		remaining string
		retry     string
		want      time.Duration
	}{
		{http.StatusForbidden, "0", "", 301 * time.Second},
		{http.StatusTooManyRequests, "", "600", 600 * time.Second},
		{http.StatusForbidden, "1", "", 0},
		{http.StatusUnauthorized, "0", "", 0},
	} {
		response := &http.Response{StatusCode: test.status, Header: make(http.Header)}
		response.Header.Set("X-RateLimit-Remaining", test.remaining)
		response.Header.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(5*time.Minute).Unix(), 10))
		response.Header.Set("Retry-After", test.retry)
		limited := sourceRateLimit(response, now)
		if test.want == 0 {
			require.Nil(t, limited, "denial is not a rate-limit retry")
		} else {
			require.NotNil(t, limited)
			require.Equal(t, now.Add(test.want), limited.RetryAt)
			require.Contains(t, limited.Error(), "anonymous GitHub source access is rate limited")
			require.Contains(t, limited.Error(), "HTTP "+strconv.Itoa(test.status))
		}
	}
	response := &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header)}
	response.Header.Set("X-RateLimit-Reset", "invalid")
	require.Equal(t, now.Add(time.Minute), sourceRateLimit(response, now).RetryAt)
	response.Header.Set("X-RateLimit-Reset", strconv.FormatInt(now.Add(24*time.Hour).Unix(), 10))
	require.Equal(t, now.Add(time.Hour+time.Minute), sourceRateLimit(response, now).RetryAt)
}
