package supervisor

import (
	"context"
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
