package enforcementprovider

import (
	"testing"
	"time"
)

func TestControllerRequiresStartupAndRecoversRelease(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	path := secureStorePath(t, "controller.json")
	first, err := NewController(path, provider, trust, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = first.Hold(authorization); err == nil {
		t.Fatal("controller accepted work before recovery")
	}
	if err = first.Start(); err != nil {
		t.Fatal(err)
	}
	held, err := first.Hold(authorization)
	if err != nil {
		t.Fatal(err)
	}
	consumed, err := first.ConsumeProof(authorization.Context.AttemptID, held.ProofID)
	if err != nil {
		t.Fatal(err)
	}
	if consumed.Phase != PhaseProofConsumed {
		t.Fatalf("phase = %s", consumed.Phase)
	}

	// Simulate a crash before the releaser runs. A new controller executes the
	// durable release obligation during Start and becomes ready only afterwards.
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewController(path, provider, trust, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.Start(); err != nil {
		t.Fatal(err)
	}
	recovered, err := restarted.Attempt(authorization.Context.AttemptID)
	if err != nil {
		t.Fatal(err)
	}
	if recovered.Phase != PhaseReleased || recovered.ReleaseID != consumed.ReleaseID {
		t.Fatalf("recovery lost release identity: %#v", recovered)
	}
}

func TestControllerStartupFailurePreventsReadiness(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	path := secureStorePath(t, "controller.json")
	first, _ := NewController(path, provider, trust, time.Now)
	if err := first.Start(); err != nil {
		t.Fatal(err)
	}
	held, err := first.Hold(authorization)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = first.ConsumeProof(authorization.Context.AttemptID, held.ProofID); err != nil {
		t.Fatal(err)
	}
	provider.releaseFailures = 1
	if err = first.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, err := NewController(path, provider, trust, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.Start(); err == nil {
		t.Fatal("controller became ready despite failed recovery")
	}
	if _, err = restarted.Release(authorization.Context.AttemptID); err == nil {
		t.Fatal("unready controller released work")
	}
}

func TestControllerRetriesExactAdmittedAuthorizationAfterExpiry(t *testing.T) {
	provider, trust, authorization := newSynthetic()
	admittedAt := time.Now().UTC()
	authorization.IssuedAt = admittedAt.Add(-time.Second)
	authorization.NotBefore = admittedAt.Add(-time.Second)
	authorization.ExpiresAt = admittedAt.Add(time.Second)
	provider.a = authorization
	provider.d.ExpiresAt = admittedAt.Add(time.Hour)
	SignDescriptor(&provider.d, provider.priv)
	provider.p.ExpiresAt = admittedAt.Add(time.Hour)
	provider.p.AuthorizationDigest = authorizationDigest(authorization)
	SignProof(&provider.p, provider.priv)
	now := admittedAt
	controller, err := NewController(secureStorePath(t, "controller.json"), provider, trust, func() time.Time { return now })
	if err != nil {
		t.Fatal(err)
	}
	if err = controller.Start(); err != nil {
		t.Fatal(err)
	}
	first, err := controller.Hold(authorization)
	if err != nil {
		t.Fatal(err)
	}
	now = admittedAt.Add(2 * time.Second)
	retried, err := controller.Hold(authorization)
	if err != nil {
		t.Fatal(err)
	}
	if retried.Phase != PhaseProofIssued || retried.Version != first.Version {
		t.Fatalf("retry changed admitted attempt: first=%#v retry=%#v", first, retried)
	}
}
