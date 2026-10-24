package apierror

import (
	"errors"
	"testing"
	"time"
)

func TestError(t *testing.T) {
	cause := errors.New("internal")
	err := (&Error{Status: 503, Reason: "MEMORY_BACKEND_UNAVAILABLE", Message: "memory backend unavailable", Cause: cause}).WithRetryAfter(3 * time.Second)
	if err.Error() != "memory backend unavailable" || !errors.Is(err, cause) || err.RetryAfter != 3*time.Second {
		t.Fatalf("unexpected error: %#v", err)
	}
}
