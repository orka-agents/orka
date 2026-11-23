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

// A client over its own sign-in limit spends none of what every client
// shares, so it cannot lock others out; a request the shared limit refuses
// gives its client's token back.
func TestAllowClientFirstSparesTheSharedLimit(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	limiter := newGatewayIngressLimiterWithLimits(rate.Limit(0.0001), 3, rate.Limit(0.0001), 1, 16, time.Minute)
	if !limiter.AllowClientFirst("blocked", now) {
		t.Fatal("the client's first request is refused")
	}
	for range 50 {
		if limiter.AllowClientFirst("blocked", now) {
			t.Fatal("a client over its limit is admitted")
		}
	}
	for _, client := range []string{"second", "third"} {
		if !limiter.AllowClientFirst(client, now) {
			t.Fatalf("%s is refused after another client's refused requests", client)
		}
	}
	// The shared limit is spent: the fourth client is refused, and keeps its
	// token for when the shared limit refills.
	if limiter.AllowClientFirst("fourth", now) {
		t.Fatal("a request past the shared limit is admitted")
	}
	limiter.global = rate.NewLimiter(rate.Limit(0.0001), 1)
	if !limiter.AllowClientFirst("fourth", now) {
		t.Fatal("a request the shared limit refused spent its client's token")
	}
}
