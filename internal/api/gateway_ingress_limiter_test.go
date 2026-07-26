package api

import (
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func TestGatewayIngressLimiterBoundsClientsAndRequests(t *testing.T) {
	now := time.Now()
	limiter := newGatewayIngressLimiterWithLimits(rate.Limit(100), 10, rate.Limit(1), 1, 2, time.Minute)
	if !limiter.Allow("client-a", now) {
		t.Fatal("first client-a request was denied")
	}
	if limiter.Allow("client-a", now) {
		t.Fatal("second client-a request exceeded the per-client burst")
	}
	if !limiter.Allow("client-b", now) || !limiter.Allow("client-c", now) {
		t.Fatal("independent clients were unexpectedly denied")
	}
	if len(limiter.clients) > 2 {
		t.Fatalf("tracked clients = %d, want <= 2", len(limiter.clients))
	}
}
