package enforcement

import (
	"context"
	"errors"
	"slices"
	"testing"

	enforcementv1 "github.com/orka-agents/orka/api/enforcement/v1"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

func exampleRequest() *enforcementv1.EnsureRequest {
	return &enforcementv1.EnsureRequest{
		BindingId: "container-1",
		Requirement: &enforcementv1.Requirement{
			Format: "example.policy/v1",
			Policy: []byte(" complete policy \n"),
			RequiredCapabilities: []string{
				"filesystem/deny-write/v1", "network/egress-allow/v1",
			},
			Mode: enforcementv1.Requirement_MODE_ENFORCE,
		},
		TargetFacts: &enforcementv1.TargetFacts{
			Format: "example.container/v1",
			Value:  []byte(`{"container":"incarnation-1"}`),
		},
	}
}

func proofFor(t *testing.T, request *enforcementv1.EnsureRequest, capabilities ...string) *enforcementv1.EnsureResponse {
	t.Helper()
	requirementDigest, err := RequirementDigest(request.Requirement)
	if err != nil {
		t.Fatal(err)
	}
	targetDigest, err := TargetFactsDigest(request.TargetFacts)
	if err != nil {
		t.Fatal(err)
	}
	return &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Enforced{
		Enforced: &enforcementv1.Enforced{
			BindingId:            request.BindingId,
			RequirementDigest:    requirementDigest,
			TargetFactsDigest:    targetDigest,
			EnforcedCapabilities: capabilities,
			ResolvedTarget: &enforcementv1.TargetFacts{
				Format: "example.physical-target/v1",
				Value:  []byte(`{"boot":"boot-1","object":"physical-1"}`),
			},
			Evidence: []byte("current provider-owned kernel readback"),
		},
	}}
}

func TestIdentityMatchesPythonAndPreservesPolicyBytes(t *testing.T) {
	request := exampleRequest()
	got, err := RequirementDigest(request.Requirement)
	if err != nil {
		t.Fatal(err)
	}
	if got != "sha256:4579b629d512e51992545c0782c96074c7914c767842b059cf9df678bc13ed30" {
		t.Fatalf("cross-language requirement identity mismatch: %s", got)
	}
	facts, err := TargetFactsDigest(request.TargetFacts)
	if err != nil {
		t.Fatal(err)
	}
	if facts != "sha256:e6e7e0369ff73d0aebb869ee4c30d981ff11fdf35c4c58fdd033ce5da052c31a" {
		t.Fatalf("cross-language target identity mismatch: %s", facts)
	}
	before := slices.Clone(request.Requirement.Policy)
	if err := ValidateRequest(request); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(before, request.Requirement.Policy) {
		t.Fatal("validation rewrote submitted policy bytes")
	}
	request.Requirement.Policy = []byte("complete policy")
	changed, err := RequirementDigest(request.Requirement)
	if err != nil {
		t.Fatal(err)
	}
	if changed == got {
		t.Fatal("changed policy bytes retained execution identity")
	}
}

func TestInvalidRequirementsNeverBecomeAuthority(t *testing.T) {
	mutations := map[string]func(*enforcementv1.EnsureRequest){
		"missing binding": func(r *enforcementv1.EnsureRequest) { r.BindingId = "" },
		"missing policy":  func(r *enforcementv1.EnsureRequest) { r.Requirement.Policy = nil },
		"oversized policy": func(r *enforcementv1.EnsureRequest) {
			r.Requirement.Policy = make([]byte, MaxPolicyBytes+1)
		},
		"unspecified mode": func(r *enforcementv1.EnsureRequest) {
			r.Requirement.Mode = enforcementv1.Requirement_MODE_UNSPECIFIED
		},
		"invalid unicode": func(r *enforcementv1.EnsureRequest) { r.Requirement.Format = string([]byte{0xff}) },
		"duplicate capabilities": func(r *enforcementv1.EnsureRequest) {
			r.Requirement.RequiredCapabilities = []string{"fs/v1", "fs/v1"}
		},
		"missing capabilities": func(r *enforcementv1.EnsureRequest) { r.Requirement.RequiredCapabilities = nil },
		"missing target":       func(r *enforcementv1.EnsureRequest) { r.TargetFacts = nil },
		"oversized target": func(r *enforcementv1.EnsureRequest) {
			r.TargetFacts.Value = make([]byte, MaxFactsBytes+1)
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			request := exampleRequest()
			mutate(request)
			if ValidateRequest(request) == nil {
				t.Fatal("invalid request accepted")
			}
		})
	}
}

func TestMismatchedEvidenceNeverBecomesAuthority(t *testing.T) {
	mutations := map[string]func(*enforcementv1.Enforced){
		"binding":     func(p *enforcementv1.Enforced) { p.BindingId = "other" },
		"requirement": func(p *enforcementv1.Enforced) { p.RequirementDigest = "other" },
		"incarnation": func(p *enforcementv1.Enforced) { p.TargetFactsDigest = "other" },
		"no target":   func(p *enforcementv1.Enforced) { p.ResolvedTarget = nil },
		"no readback": func(p *enforcementv1.Enforced) { p.Evidence = nil },
		"no controls": func(p *enforcementv1.Enforced) { p.EnforcedCapabilities = nil },
		"unrequested": func(p *enforcementv1.Enforced) { p.EnforcedCapabilities = []string{"unrequested/v1"} },
		"duplicates": func(p *enforcementv1.Enforced) {
			p.EnforcedCapabilities = []string{"filesystem/deny-write/v1", "filesystem/deny-write/v1"}
		},
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			request := exampleRequest()
			response := proofFor(t, request, request.Requirement.RequiredCapabilities...)
			mutate(response.GetEnforced())
			if ValidateResponse(request, response) == nil {
				t.Fatal("mismatched evidence accepted")
			}
		})
	}
}

type recordingProvider struct {
	ensure  func(*enforcementv1.EnsureRequest) (*enforcementv1.EnsureResponse, error)
	retire  *enforcementv1.RetireResponse
	calls   int
	lastReq *enforcementv1.EnsureRequest
}

func (p *recordingProvider) EnsureEnforced(_ context.Context, request *enforcementv1.EnsureRequest, _ ...grpc.CallOption) (*enforcementv1.EnsureResponse, error) {
	p.calls++
	p.lastReq = proto.Clone(request).(*enforcementv1.EnsureRequest)
	return p.ensure(request)
}

func (p *recordingProvider) RetireEnforcement(_ context.Context, _ *enforcementv1.RetireRequest, _ ...grpc.CallOption) (*enforcementv1.RetireResponse, error) {
	return p.retire, nil
}

func TestMandatoryProvidersComposeAND(t *testing.T) {
	request := exampleRequest()
	filesystem := &recordingProvider{ensure: func(r *enforcementv1.EnsureRequest) (*enforcementv1.EnsureResponse, error) {
		return proofFor(t, r, "filesystem/deny-write/v1"), nil
	}}
	network := &recordingProvider{ensure: func(r *enforcementv1.EnsureRequest) (*enforcementv1.EnsureResponse, error) {
		return proofFor(t, r, "network/egress-allow/v1"), nil
	}}
	providers := []*Client{{name: "filesystem", rpc: filesystem}, {name: "network", rpc: network}}
	proofs, unsettled, err := EnsureAll(context.Background(), providers, request)
	if err != nil || unsettled != nil || len(proofs) != 2 {
		t.Fatalf("complete mandatory composition failed: %v %v", unsettled, err)
	}
	if !proto.Equal(filesystem.lastReq, request) || !proto.Equal(network.lastReq, request) {
		t.Fatal("providers did not receive the same complete unchanged request")
	}
	proofs, unsettled, err = EnsureAll(context.Background(), providers[:1], request)
	if err != nil || len(proofs) != 0 || unsettled.GetRefused() == nil ||
		!slices.Contains(unsettled.GetRefused().Needs, "mandatory-capability-coverage") {
		t.Fatal("partial capability coverage did not explicitly refuse admission with actionable needs")
	}
	if _, _, err := EnsureAll(context.Background(), nil, request); err == nil {
		t.Fatal("absent providers authorized execution")
	}
}

func TestPendingRefusalAndUncertaintyNeverYieldProofs(t *testing.T) {
	for _, state := range []string{"pending", "refused", "rpc error", "malformed"} {
		t.Run(state, func(t *testing.T) {
			first := &recordingProvider{ensure: func(r *enforcementv1.EnsureRequest) (*enforcementv1.EnsureResponse, error) {
				return proofFor(t, r, r.Requirement.RequiredCapabilities...), nil
			}}
			second := &recordingProvider{ensure: func(*enforcementv1.EnsureRequest) (*enforcementv1.EnsureResponse, error) {
				switch state {
				case "pending":
					return &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Pending{
						Pending: &enforcementv1.Pending{Reason: "target is still preparing"},
					}}, nil
				case "refused":
					return &enforcementv1.EnsureResponse{Result: &enforcementv1.EnsureResponse_Refused{
						Refused: &enforcementv1.Refused{Reason: "unsupported target"},
					}}, nil
				case "rpc error":
					return nil, errors.New("connection lost after activation")
				default:
					return &enforcementv1.EnsureResponse{}, nil
				}
			}}
			proofs, unsettled, err := EnsureAll(context.Background(),
				[]*Client{{name: "first", rpc: first}, {name: "second", rpc: second}}, exampleRequest())
			if len(proofs) != 0 {
				t.Fatal("incomplete mandatory enforcement yielded execution evidence")
			}
			if state == "pending" || state == "refused" {
				if unsettled == nil || err != nil {
					t.Fatal("explicit provider result lost")
				}
			} else if err == nil {
				t.Fatal("uncertainty or malformed result accepted")
			}
		})
	}
}

func TestRetirementMustMatchExactBinding(t *testing.T) {
	rpc := &recordingProvider{retire: &enforcementv1.RetireResponse{Result: &enforcementv1.RetireResponse_Retired{
		Retired: &enforcementv1.Retired{BindingId: "other-container"},
	}}}
	client := &Client{name: "provider", rpc: rpc}
	if _, err := client.Retire(context.Background(), "container-1"); err == nil {
		t.Fatal("retirement for another binding accepted")
	}
	rpc.retire.GetRetired().BindingId = "container-1"
	if _, err := client.Retire(context.Background(), "container-1"); err != nil {
		t.Fatal(err)
	}
}
