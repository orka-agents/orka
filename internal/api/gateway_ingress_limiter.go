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
