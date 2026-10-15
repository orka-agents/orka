package enforcementprovider

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
	"time"
)

func fixture(t *testing.T) (Descriptor, TrustProfile, Authorization, ReadBack, Proof) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	ctx := Context{RequestID: "req", AttemptID: "attempt", WorkloadID: "workload", ProviderID: "fixture/v1", Audience: "orka", Challenge: "challenge", IdempotencyKey: "key", PolicyBytes: []byte(`{"version":"agentsuite.policy/v1","denyWritePaths":["/workspace"],"requiredCapabilities":["filesystem/deny-write/v1"]}`), RequiredCapabilities: []string{"filesystem/deny-write/v1"}}
	ctx.PolicyDigest = PolicyDigest(ctx.PolicyBytes)
	a := Authorization{ID: "auth", Context: ctx, IssuedAt: now.Add(-time.Second), NotBefore: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute), Epoch: 3}
	d := Descriptor{ProviderID: ctx.ProviderID, EndpointID: "unix:test", Algorithm: "ed25519", ProtocolVersions: []string{ProtocolVersion}, Capabilities: []string{"filesystem/deny-write/v1"}, Epoch: 3, IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute), KeyID: "key-1", PublicKey: pub}
	rb := ReadBack{ActivationID: "activation", WorkloadID: ctx.WorkloadID, PolicyDigest: ctx.PolicyDigest, ConfigurationDigest: "sha256:config"}
	SignDescriptor(&d, priv)
	trust := TrustProfile{ProviderID: d.ProviderID, EndpointID: d.EndpointID, Algorithm: d.Algorithm, KeyID: d.KeyID, PublicKey: pub, MinimumEpoch: d.Epoch}
	p := Proof{ID: "proof", AuthorizationID: a.ID, ActivationID: rb.ActivationID, WorkloadID: ctx.WorkloadID, ProviderID: ctx.ProviderID, Audience: ctx.Audience, Challenge: ctx.Challenge, PolicyDigest: ctx.PolicyDigest, ReadBackDigest: readbackDigest(rb), KeyID: d.KeyID, Epoch: d.Epoch, IssuedAt: now.Add(-time.Second), ExpiresAt: now.Add(time.Minute)}
	SignProof(&p, priv)
	return d, trust, a, rb, p
}
func TestProofAndReadback(t *testing.T) {
	d, trust, a, rb, p := fixture(t)
	if e := VerifyProof(p, a, d, trust, rb.ActivationID, rb, time.Now()); e != nil {
		t.Fatal(e)
	}
	rb.ConfigurationDigest = "changed"
	if VerifyProof(p, a, d, trust, rb.ActivationID, rb, time.Now()) == nil {
		t.Fatal("accepted mismatched readback")
	}
}
func TestNegotiationFailsClosed(t *testing.T) {
	d, trust, _, _, _ := fixture(t)
	if Negotiate(d, trust, []string{"missing/v1"}, time.Now()) == nil {
		t.Fatal("accepted unsupported capability")
	}
	d.ExpiresAt = time.Now().Add(-time.Second)
	if Negotiate(d, trust, []string{"filesystem/deny-write/v1"}, time.Now()) == nil {
		t.Fatal("accepted stale descriptor")
	}
}
func TestProofBinding(t *testing.T) {
	d, trust, a, rb, p := fixture(t)
	p.Audience = "other"
	if VerifyProof(p, a, d, trust, rb.ActivationID, rb, time.Now()) == nil {
		t.Fatal("accepted altered proof")
	}
}

func TestDescriptorTrustRejectsSubstitution(t *testing.T) {
	d, trust, _, _, _ := fixture(t)
	_, attacker, _ := ed25519.GenerateKey(rand.Reader)
	d.PublicKey = attacker.Public().(ed25519.PublicKey)
	SignDescriptor(&d, attacker)
	if AuthenticateDescriptor(d, trust, time.Now()) == nil {
		t.Fatal("accepted substituted self-signed descriptor")
	}
}
func TestDescriptorTrustRejectsWrongEndpointAndAlgorithm(t *testing.T) {
	d, trust, _, _, _ := fixture(t)
	d.EndpointID = "unix:other"
	if AuthenticateDescriptor(d, trust, time.Now()) == nil {
		t.Fatal("accepted wrong endpoint identity")
	}
	d, trust, _, _, _ = fixture(t)
	d.Algorithm = "other"
	if AuthenticateDescriptor(d, trust, time.Now()) == nil {
		t.Fatal("accepted wrong algorithm")
	}
}

func TestAuthorizationRejectsNilEmptyAndInvalidCapabilities(t *testing.T) {
	_, _, authorization, _, _ := fixture(t)
	cases := map[string][]string{
		"nil":        nil,
		"empty":      {},
		"blank":      {""},
		"whitespace": {" filesystem/deny-write/v1"},
		"duplicate":  {"filesystem/deny-write/v1", "filesystem/deny-write/v1"},
	}
	for name, required := range cases {
		t.Run(name, func(t *testing.T) {
			changed := authorization
			changed.Context.RequiredCapabilities = required
			if ValidateAuthorization(changed, time.Now()) == nil {
				t.Fatal("accepted invalid required capability contract")
			}
		})
	}
}

func TestNegotiationRejectsNilAndEmptyCapabilities(t *testing.T) {
	descriptor, trust, _, _, _ := fixture(t)
	for name, required := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			if Negotiate(descriptor, trust, required, time.Now()) == nil {
				t.Fatal("accepted authorization without mandatory capabilities")
			}
		})
	}
}

func TestAuthorizationCapabilitiesMustMatchKnownPolicyContract(t *testing.T) {
	_, _, authorization, _, _ := fixture(t)
	cases := map[string]func(*Authorization){
		"declared differs": func(a *Authorization) { a.Context.RequiredCapabilities = []string{"other/v1"} },
		"policy omits mandatory": func(a *Authorization) {
			a.Context.PolicyBytes = []byte(`{"version":"agentsuite.policy/v1","denyWritePaths":["/workspace"],"requiredCapabilities":[]}`)
			a.Context.PolicyDigest = PolicyDigest(a.Context.PolicyBytes)
		},
		"unknown policy version": func(a *Authorization) {
			a.Context.PolicyBytes = []byte(`{"version":"future/v2","denyWritePaths":["/workspace"],"requiredCapabilities":["filesystem/deny-write/v1"]}`)
			a.Context.PolicyDigest = PolicyDigest(a.Context.PolicyBytes)
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			changed := authorization
			mutate(&changed)
			if ValidateAuthorization(changed, time.Now()) == nil {
				t.Fatal("accepted capability set divergent from known policy contract")
			}
		})
	}
}
