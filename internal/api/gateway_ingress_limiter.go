/*
Copyright (c) 2026.

MIT License - see LICENSE file for details.
*/

package api

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

const (
	gatewayIngressGlobalRate  = rate.Limit(500)
	gatewayIngressGlobalBurst = 2000
	gatewayIngressClientRate  = rate.Limit(100)
	gatewayIngressClientBurst = 1000
	gatewayIngressMaxClients  = 1024
	gatewayIngressClientTTL   = 10 * time.Minute
)

// GitHub sign-in spends the App's GitHub rate limit, which renewal and
// invites share, typically 5,000 requests an hour. One sign-in makes at most
// four App-authenticated calls: it checks the submitted token, the token its
// refresh token yields, and the token custody already holds, and revokes a
// minted token it does not keep. The global rate allows at most 900 sign-ins,
// 3,600 such calls, an hour, plus its burst, however many clients call; a
// person signs in rarely, so one client gets a few attempts a minute. Session
// renewals, which make one such call, share the limit, keyed by the person.
const (
	githubSignInGlobalRate  = rate.Limit(0.25)
	githubSignInGlobalBurst = 30
	githubSignInClientRate  = rate.Limit(0.1)
	githubSignInClientBurst = 5
	githubSignInMaxClients  = 1024
	githubSignInClientTTL   = 10 * time.Minute
)

func newGitHubSignInLimiter() *gatewayIngressLimiter {
	return newGatewayIngressLimiterWithLimits(githubSignInGlobalRate, githubSignInGlobalBurst,
		githubSignInClientRate, githubSignInClientBurst, githubSignInMaxClients, githubSignInClientTTL)
}

type gatewayIngressClientLimiter struct {
	limiter  *rate.Limiter
	lastSeen time.Time
}

type gatewayIngressLimiter struct {
	mu         sync.Mutex
	global     *rate.Limiter
	clients    map[string]*gatewayIngressClientLimiter
	clientRate rate.Limit
	burst      int
	maxClients int
	ttl        time.Duration
}

func newGatewayIngressLimiter() *gatewayIngressLimiter {
	return newGatewayIngressLimiterWithLimits(
		gatewayIngressGlobalRate,
		gatewayIngressGlobalBurst,
		gatewayIngressClientRate,
		gatewayIngressClientBurst,
		gatewayIngressMaxClients,
		gatewayIngressClientTTL,
	)
}

func newGatewayIngressLimiterWithLimits(
	globalRate rate.Limit,
	globalBurst int,
	clientRate rate.Limit,
	clientBurst int,
	maxClients int,
	ttl time.Duration,
) *gatewayIngressLimiter {
	return &gatewayIngressLimiter{
		global:     rate.NewLimiter(globalRate, globalBurst),
		clients:    make(map[string]*gatewayIngressClientLimiter),
		clientRate: clientRate,
		burst:      clientBurst,
		maxClients: max(maxClients, 1),
		ttl:        ttl,
	}
}

func (l *gatewayIngressLimiter) Allow(key string, now time.Time) bool {
	if l == nil || !l.global.AllowN(now, 1) {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.clients[key]
	if entry == nil {
		l.evictStaleOrOldest(now)
		entry = &gatewayIngressClientLimiter{limiter: rate.NewLimiter(l.clientRate, l.burst)}
		l.clients[key] = entry
	}
	entry.lastSeen = now
	return entry.limiter.AllowN(now, 1)
}

// AllowClientFirst is Allow that spends global capacity only on a request
// its client's limit admits: a client over its own limit cannot drain what
// every other client shares. A request the global limit then refuses gives
// its client's token back.
func (l *gatewayIngressLimiter) AllowClientFirst(key string, now time.Time) bool {
	if l == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	entry := l.clients[key]
	if entry == nil {
		l.evictStaleOrOldest(now)
		entry = &gatewayIngressClientLimiter{limiter: rate.NewLimiter(l.clientRate, l.burst)}
		l.clients[key] = entry
	}
	entry.lastSeen = now
	reservation := entry.limiter.ReserveN(now, 1)
	if !reservation.OK() || reservation.DelayFrom(now) > 0 {
		reservation.CancelAt(now)
		return false
	}
	if !l.global.AllowN(now, 1) {
		reservation.CancelAt(now)
		return false
	}
	return true
}

func (l *gatewayIngressLimiter) evictStaleOrOldest(now time.Time) {
	if len(l.clients) < l.maxClients {
		return
	}
	for key, entry := range l.clients {
		if l.ttl > 0 && now.Sub(entry.lastSeen) >= l.ttl {
			delete(l.clients, key)
		}
	}
	if len(l.clients) < l.maxClients {
		return
	}
	var oldestKey string
	var oldest time.Time
	for key, entry := range l.clients {
		if oldestKey == "" || entry.lastSeen.Before(oldest) {
			oldestKey = key
			oldest = entry.lastSeen
		}
	}
	delete(l.clients, oldestKey)
}
