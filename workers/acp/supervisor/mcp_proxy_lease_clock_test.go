package supervisor

import (
	"context"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	harnessv2 "github.com/orka-agents/orka/internal/harness/v2"
)

func TestMCPProxyEarlyCallbackRearmsAbsoluteExpiry(t *testing.T) {
	for _, bound := range []string{"authorization", "lease", "equal"} {
		t.Run(bound, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC() // Wire deadlines have no monotonic component.
				gate, cancel := context.WithCancelCause(context.Background())
				authExpiry, leaseExpiry := now.Add(3*time.Second), now.Add(3*time.Second)
				if bound != "lease" {
					authExpiry = now.Add(2 * time.Second)
				}
				if bound != "authorization" {
					leaseExpiry = now.Add(2 * time.Second)
				}
				session := &mcpProxySession{
					authorization: &harnessv2.PromptMCPAuthorization{PromptID: testPromptOneID, ExpiresAt: authExpiry},
					lease:         harnessv2.PromptLease{ExpiresAt: leaseExpiry}, state: harnessv2.RuntimeSessionStatePromptRunning,
					gateContext: gate, gateCancel: cancel,
				}
				session.mu.Lock()
				session.resetLeaseTimerLocked(now)
				version := session.leaseVersion
				session.leaseTimer.Stop()
				// Model a relative timer firing after a one-second backward
				// wall-clock step: the absolute bounds remain unchanged.
				session.leaseTimer = time.AfterFunc(time.Second, func() { session.expire(testPromptOneID, version) })
				session.mu.Unlock()
				defer session.revoke(harnessv2.RuntimeSessionStateIdle)
				time.Sleep(2*time.Second - time.Nanosecond)
				synctest.Wait()
				if gate.Err() != nil {
					t.Fatal("early relative timer revoked still-live absolute authority")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if gate.Err() == nil {
					t.Fatal("rearmed timer did not revoke at the earlier absolute expiry")
				}
				session.mu.Lock()
				defer session.mu.Unlock()
				if session.authorization != nil || session.state != harnessv2.RuntimeSessionStateCancelling {
					t.Fatal("expired MCP authority remained active")
				}
			})
		})
	}
}

func TestMCPProxyActivationUsesLockedTime(t *testing.T) {
	for _, tc := range []struct {
		name  string
		delay time.Duration
		live  bool
	}{
		{name: "live-authorization", delay: 30 * time.Second, live: true},
		{name: "authorization-at-expiry", delay: time.Minute},
		{name: "authorization-after-expiry", delay: time.Minute + time.Nanosecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				fence := harnessv2.Fence{RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3}
				auth, lease := testMCPAuthorization(t, fence, now, false)
				session := &mcpProxySession{fence: fence, configuration: auth.Configuration(), state: harnessv2.RuntimeSessionStateIdle}
				defer session.revoke(harnessv2.RuntimeSessionStateIdle)
				// Model admission waiting for locks after the handler samples now.
				time.Sleep(tc.delay)
				err := session.activate(t.Context(), auth, lease, now)
				if !tc.live {
					if err == nil {
						t.Fatal("activation admitted already-expired MCP authorization using stale request time")
					}
					session.mu.Lock()
					installed := session.authorization != nil || session.gateContext != nil || session.leaseTimer != nil
					session.mu.Unlock()
					if installed {
						t.Fatal("rejected activation installed expired authority")
					}
					return
				}
				if err != nil {
					t.Fatal(err)
				}
				if err := session.markRunning(auth.PromptID, time.Now()); err != nil {
					t.Fatal(err)
				}
				session.mu.Lock()
				gate := session.gateContext
				session.mu.Unlock()
				time.Sleep(time.Until(auth.ExpiresAt) - time.Nanosecond)
				synctest.Wait()
				if gate.Err() != nil {
					t.Fatal("admission delay revoked still-live authority before absolute expiry")
				}
				time.Sleep(time.Nanosecond)
				synctest.Wait()
				if gate.Err() == nil {
					t.Fatal("stale admission time delayed MCP revocation past absolute authorization expiry")
				}
			})
		})
	}
}

func TestMCPProxyRenewalCannotReviveExpiredAuthority(t *testing.T) {
	for _, tc := range []struct {
		name       string
		leaseBound bool
		staleNow   bool
		delay      time.Duration
	}{
		{name: "authorization-at-expiry"},
		{name: "authorization-after-expiry", delay: time.Nanosecond},
		{name: "authorization-stale-request-time", staleNow: true},
		{name: "lease-stale-request-time", leaseBound: true, staleNow: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				fence := harnessv2.Fence{RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3}
				auth, lease := testMCPAuthorization(t, fence, now, false)
				if tc.leaseBound {
					auth.ExpiresAt = lease.ExpiresAt
				}
				session := &mcpProxySession{fence: fence, configuration: auth.Configuration(), state: harnessv2.RuntimeSessionStateIdle}
				if err := session.activate(t.Context(), auth, lease, now); err != nil {
					t.Fatal(err)
				}
				if err := session.markRunning(auth.PromptID, now); err != nil {
					t.Fatal(err)
				}
				defer session.revoke(harnessv2.RuntimeSessionStateIdle)

				callbackReady, releaseCallback := make(chan struct{}), make(chan struct{})
				release := sync.OnceFunc(func() { close(releaseCallback) })
				defer release()
				session.mu.Lock()
				gate, version := session.gateContext, session.leaseVersion
				session.leaseTimer.Stop()
				// Hold the expiry callback before mutex acquisition so renewal
				// deterministically gets the lock first after authority expires.
				session.leaseTimer = time.AfterFunc(auth.ExpiresAt.Sub(now), func() {
					close(callbackReady)
					<-releaseCallback
					session.expire(auth.PromptID, version)
				})
				session.mu.Unlock()
				time.Sleep(auth.ExpiresAt.Sub(now) + tc.delay)
				<-callbackReady
				synctest.Wait()
				if gate.Err() != nil {
					t.Fatal("expiry callback was not held before revocation")
				}

				renewedLease := lease
				renewedLease.Generation++
				renewedLease.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
				renewedAuth := auth
				renewedAuth.LeaseGeneration = renewedLease.Generation
				renewedAuth.ExpiresAt = renewedLease.ExpiresAt
				renewalNow := time.Now().UTC()
				if tc.staleNow {
					// The handler can sample now before waiting for the MCP lock.
					renewalNow = now
				}
				if err := session.renew(renewedAuth, renewedLease, renewalNow); err == nil {
					t.Fatal("renewal revived expired MCP authority before its queued callback")
				}
				session.mu.Lock()
				unchanged := session.leaseVersion == version && session.authorization.ExpiresAt.Equal(auth.ExpiresAt)
				session.mu.Unlock()
				if !unchanged {
					t.Fatal("refused renewal replaced authority or invalidated its expiry callback")
				}
				release()
				synctest.Wait()
				if gate.Err() == nil {
					t.Fatal("queued callback did not revoke expired authority after refused renewal")
				}
			})
		})
	}
}

func TestMCPProxyCallerAheadTimeCannotLoosenAuthority(t *testing.T) {
	for _, guard := range []string{"activate", "renew", "call", "permission", "running"} {
		t.Run(guard, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				now := time.Now().UTC()
				fence := harnessv2.Fence{RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3}
				auth, lease := testMCPAuthorization(t, fence, now, false)
				session := &mcpProxySession{fence: fence, configuration: auth.Configuration(), state: harnessv2.RuntimeSessionStateIdle}
				t.Cleanup(func() { session.revoke(harnessv2.RuntimeSessionStateIdle) })
				if guard != "activate" {
					if err := session.activate(t.Context(), auth, lease, now); err != nil {
						t.Fatal(err)
					}
					if guard != "running" {
						if err := session.markRunning(auth.PromptID, now); err != nil {
							t.Fatal(err)
						}
					}
				}
				session.mu.Lock()
				originalAuth, originalLease := session.authorization, session.lease
				originalState, version := session.state, session.leaseVersion
				session.mu.Unlock()
				// Model a backward wall-clock step after the caller observed expiry.
				// The current clock is still before expiry; that must not rewind now.
				callerNow := auth.ExpiresAt
				var err error
				switch guard {
				case "activate":
					err = session.activate(t.Context(), auth, lease, callerNow)
				case "renew":
					renewedLease := lease
					renewedLease.Generation++
					renewedLease.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
					renewedAuth := auth
					renewedAuth.LeaseGeneration = renewedLease.Generation
					renewedAuth.ExpiresAt = renewedLease.ExpiresAt
					err = session.renew(renewedAuth, renewedLease, callerNow)
				case "call":
					_, _, _, err = session.authorizeCall("lookup", callerNow)
				case "permission":
					err = session.authorizePermissionTool("claude", auth.PromptID, "lookup", callerNow)
				case "running":
					err = session.markRunning(auth.PromptID, callerNow)
				}
				if err == nil {
					t.Fatal("guard rewound the caller's expired timestamp to the earlier current clock")
				}
				session.mu.Lock()
				unchanged := session.authorization == originalAuth && session.lease == originalLease && session.state == originalState && session.leaseVersion == version
				session.mu.Unlock()
				if !unchanged {
					t.Fatal("rejected caller-ahead guard changed prompt authority")
				}
			})
		})
	}
}

func TestMCPProxyAuthorizeCallUsesLockedTime(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "expired"}[expired], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				session, auth, now := newMCPProxyGuardClockSession(t, true)
				if expired {
					time.Sleep(time.Until(auth.ExpiresAt))
				} else {
					time.Sleep(30 * time.Second)
				}
				gate, admitted, _, err := session.authorizeCall("lookup", now)
				if expired {
					if err == nil || gate != nil || admitted.PromptID != "" {
						t.Fatal("tool-call guard admitted expired authority using stale caller time")
					}
				} else if err != nil || gate == nil || gate.Err() != nil || admitted.PromptID != auth.PromptID {
					t.Fatalf("tool-call guard rejected still-live authority: %v", err)
				}
			})
		})
	}
}

func TestMCPProxyAuthorizePermissionToolUsesLockedTime(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "expired"}[expired], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				session, auth, now := newMCPProxyGuardClockSession(t, true)
				if expired {
					time.Sleep(time.Until(auth.ExpiresAt))
				} else {
					time.Sleep(30 * time.Second)
				}
				err := session.authorizePermissionTool("claude", auth.PromptID, "lookup", now)
				if expired && err == nil {
					t.Fatal("permission guard admitted expired authority using stale caller time")
				}
				if !expired && err != nil {
					t.Fatalf("permission guard rejected still-live authority: %v", err)
				}
			})
		})
	}
}

func TestMCPProxyMarkRunningUsesLockedTime(t *testing.T) {
	for _, expired := range []bool{false, true} {
		t.Run(map[bool]string{false: "live", true: "expired"}[expired], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				session, auth, now := newMCPProxyGuardClockSession(t, false)
				if expired {
					time.Sleep(time.Until(auth.ExpiresAt))
				} else {
					time.Sleep(30 * time.Second)
				}
				err := session.markRunning(auth.PromptID, now)
				session.mu.Lock()
				state := session.state
				session.mu.Unlock()
				if expired {
					if err == nil || state != harnessv2.RuntimeSessionStateIdle {
						t.Fatal("running guard activated expired authority using stale caller time")
					}
				} else if err != nil || state != harnessv2.RuntimeSessionStatePromptRunning {
					t.Fatalf("running guard rejected still-live authority: %v", err)
				}
			})
		})
	}
}

func newMCPProxyGuardClockSession(t *testing.T, running bool) (*mcpProxySession, harnessv2.PromptMCPAuthorization, time.Time) {
	t.Helper()
	now := time.Now().UTC()
	fence := harnessv2.Fence{RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3}
	auth, lease := testMCPAuthorization(t, fence, now, false)
	session := &mcpProxySession{fence: fence, configuration: auth.Configuration(), state: harnessv2.RuntimeSessionStateIdle}
	if err := session.activate(t.Context(), auth, lease, now); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { session.revoke(harnessv2.RuntimeSessionStateIdle) })
	if running {
		if err := session.markRunning(auth.PromptID, now); err != nil {
			t.Fatal(err)
		}
	}
	// Model an expiry callback that has not yet acquired the session lock.
	session.mu.Lock()
	session.leaseTimer.Stop()
	session.mu.Unlock()
	return session, auth, now
}

func TestMCPProxyRearmedCallbackCannotRevokeRenewal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		now := time.Now().UTC()
		auth, lease := testMCPAuthorization(t, harnessv2.Fence{RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3}, now, false)
		session := &mcpProxySession{fence: harnessv2.Fence{RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3}, configuration: auth.Configuration(), state: harnessv2.RuntimeSessionStateIdle}
		if err := session.activate(t.Context(), auth, lease, now); err != nil {
			t.Fatal(err)
		}
		if err := session.markRunning(auth.PromptID, now); err != nil {
			t.Fatal(err)
		}
		defer session.revoke(harnessv2.RuntimeSessionStateIdle)
		session.mu.Lock()
		gate := session.gateContext
		version := session.leaseVersion
		session.leaseTimer.Stop()
		session.mu.Unlock()
		session.expire(auth.PromptID, version) // Early callback must rearm, not revoke.
		session.mu.Lock()
		rearmedVersion := session.leaseVersion
		session.mu.Unlock()
		renewedLease := lease
		renewedLease.Generation++
		renewedLease.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
		renewedAuth := auth
		renewedAuth.LeaseGeneration = renewedLease.Generation
		renewedAuth.ExpiresAt = renewedLease.ExpiresAt
		if err := session.renew(renewedAuth, renewedLease, now); err != nil {
			t.Fatalf("early callback lost renewable authority: %v", err)
		}
		session.expire(auth.PromptID, version)
		session.expire(auth.PromptID, rearmedVersion)
		if gate.Err() != nil {
			t.Fatal("stale rearmed callback revoked renewed gate")
		}
		time.Sleep(2*time.Minute + time.Nanosecond)
		if gate.Err() != nil {
			t.Fatal("old expiry revoked renewed gate")
		}
		time.Sleep(time.Minute - time.Nanosecond)
		synctest.Wait()
		if gate.Err() == nil {
			t.Fatal("renewed absolute expiry did not revoke gate")
		}
	})
}

func TestMCPProxyRenewRequiresLiveCurrentAuthority(t *testing.T) {
	for _, bound := range []string{"authorization", "equal", "incoming"} {
		for _, offset := range []struct {
			name         string
			delta        time.Duration
			wantRejected bool
		}{
			{name: "before", delta: -time.Nanosecond},
			{name: "at", wantRejected: true},
			{name: "after", delta: time.Nanosecond, wantRejected: true},
		} {
			t.Run(bound+"/"+offset.name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					now := time.Now().UTC()
					fence := harnessv2.Fence{RuntimeSessionUID: "session-uid", RuntimeSessionGeneration: 3}
					auth, lease := testMCPAuthorization(t, fence, now, false)
					if bound == "equal" {
						auth.ExpiresAt = lease.ExpiresAt
					}
					session := &mcpProxySession{fence: fence, configuration: auth.Configuration(), state: harnessv2.RuntimeSessionStateIdle}
					if err := session.activate(t.Context(), auth, lease, now); err != nil {
						t.Fatal(err)
					}
					if err := session.markRunning(auth.PromptID, now); err != nil {
						t.Fatal(err)
					}
					defer session.revoke(harnessv2.RuntimeSessionStateIdle)
					session.mu.Lock()
					// Suppress the callback to model renewal committing first
					// after a delay, including waiting for the session mutex.
					session.leaseTimer.Stop()
					priorAuthorization, priorVersion := session.authorization, session.leaseVersion
					gate := session.gateContext
					session.mu.Unlock()
					renewedLease := lease
					renewedLease.Generation++
					renewedLease.ExpiresAt = lease.ExpiresAt.Add(time.Minute)
					renewedAuth := auth
					renewedAuth.LeaseGeneration = renewedLease.Generation
					renewedAuth.ExpiresAt = renewedLease.ExpiresAt
					deadline := auth.ExpiresAt
					if bound == "incoming" {
						renewedAuth.ExpiresAt = now.Add(30 * time.Second)
						deadline = renewedAuth.ExpiresAt
					}
					renewalNow := deadline.Add(offset.delta)
					renewedLease.IssuedAt = renewalNow
					time.Sleep(renewalNow.Sub(now))
					// Pass the request's stale timestamp; commit must use the
					// actual clock for both expiry checks and timer scheduling.
					err := session.renew(renewedAuth, renewedLease, now)
					if offset.wantRejected {
						if err == nil {
							t.Fatal("renewal revived authority at or after its absolute expiry")
						}
						session.mu.Lock()
						defer session.mu.Unlock()
						if session.authorization != priorAuthorization || session.lease != lease || session.leaseVersion != priorVersion {
							t.Fatal("rejected renewal replaced authority or invalidated its expiry callback")
						}
					} else {
						if err != nil {
							t.Fatalf("live authority was not renewable: %v", err)
						}
						if gate.Err() != nil {
							t.Fatal("live renewal cancelled the active gate")
						}
						time.Sleep(time.Until(renewedAuth.ExpiresAt) - time.Nanosecond)
						synctest.Wait()
						if gate.Err() != nil {
							t.Fatal("renewed authority expired before its absolute deadline")
						}
						time.Sleep(time.Nanosecond)
						synctest.Wait()
						if gate.Err() == nil {
							t.Fatal("renewal scheduled expiry from the stale request timestamp")
						}
					}
				})
			})
		}
	}
}
