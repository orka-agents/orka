package workspace

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestValidateSubstrateEgressRulesRejectsUnsafeHosts(t *testing.T) {
	for _, tc := range []struct {
		name string
		rule SubstrateEgressRule
	}{
		{"bare wildcard", SubstrateEgressRule{Scheme: "https", Host: "*"}},
		{"tld wildcard", SubstrateEgressRule{Scheme: "https", Host: "*.com"}},
		{"inner wildcard", SubstrateEgressRule{Scheme: "https", Host: "api.*.example.com"}},
		{"partial wildcard", SubstrateEgressRule{Scheme: "https", Host: "a*.example.com"}},
		{"ipv4", SubstrateEgressRule{Scheme: "http", Host: "10.0.0.1"}},
		{"ipv6", SubstrateEgressRule{Scheme: "http", Host: "::1"}},
		{"cidr", SubstrateEgressRule{Scheme: "http", Host: "10.0.0.0/8"}},
		{"url", SubstrateEgressRule{Scheme: "https", Host: "https://example.com"}},
		{"path", SubstrateEgressRule{Scheme: "https", Host: "example.com/api"}},
		{"host port", SubstrateEgressRule{Scheme: "https", Host: "example.com:443"}},
		{"uppercase", SubstrateEgressRule{Scheme: "https", Host: "Example.com"}},
		{"trailing dot", SubstrateEgressRule{Scheme: "https", Host: "example.com."}},
		{"single label", SubstrateEgressRule{Scheme: "http", Host: "localhost"}},
		{"numeric tld", SubstrateEgressRule{Scheme: "http", Host: "a.123"}},
		{"long numeric tld", SubstrateEgressRule{Scheme: "http", Host: "a.12345678901234567890123"}},
		{"dotted numeric name", SubstrateEgressRule{Scheme: "https", Host: "01.2.3.4"}},
		{"numeric wildcard suffix", SubstrateEgressRule{Scheme: "tls", Host: "*.example.123", Ports: []int32{443}}},
		{"bad scheme", SubstrateEgressRule{Scheme: "ftp", Host: "example.com"}},
		{"tls without ports", SubstrateEgressRule{Scheme: "tls", Host: "example.com"}},
		{"port zero", SubstrateEgressRule{Scheme: "https", Host: "example.com", Ports: []int32{0}}},
		{"port too high", SubstrateEgressRule{Scheme: "https", Host: "example.com", Ports: []int32{70000}}},
		{"repeated port", SubstrateEgressRule{Scheme: "https", Host: "example.com", Ports: []int32{443, 443}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := ValidateSubstrateEgressRules([]SubstrateEgressRule{tc.rule}); err == nil {
				t.Fatalf("ValidateSubstrateEgressRules(%+v) succeeded, want rejection", tc.rule)
			}
		})
	}
}

func TestValidateSubstrateEgressRulesRejectsOverlappingOrigins(t *testing.T) {
	err := ValidateSubstrateEgressRules([]SubstrateEgressRule{
		{Scheme: "https", Host: "api.example.com"},
		{Scheme: "tls", Host: "api.example.com", Ports: []int32{443}},
	})
	if err == nil || !strings.Contains(err.Error(), "more than one rule") {
		t.Fatalf("overlapping origins error = %v", err)
	}
}

func TestCompileSubstrateActorEgressPolicy(t *testing.T) {
	policy, err := CompileSubstrateActorEgressPolicy("tools", []SubstrateEgressRule{
		{Scheme: "tls", Host: "*.example.com", Ports: []int32{8443, 443}},
		{Scheme: "https", Host: "api.github.com"},
		{Scheme: "http", Host: "mirror.example.org"},
	})
	if err != nil {
		t.Fatalf("CompileSubstrateActorEgressPolicy() error = %v", err)
	}
	if policy.GetMetadata().GetAtespace() != "tools" || policy.GetMetadata().GetName() != "default" {
		t.Fatalf("metadata = %v", policy.GetMetadata())
	}
	if len(policy.GetRules()) != 3 {
		t.Fatalf("rules = %v", policy.GetRules())
	}
	tlsRule := policy.GetRules()[0].GetTlsPassthrough()
	if tlsRule == nil || tlsRule.GetHostnames()[0] != "*.example.com" || tlsRule.GetPorts().GetNumbers()[0] != 443 || tlsRule.GetPorts().GetNumbers()[1] != 8443 {
		t.Fatalf("tls rule = %v", policy.GetRules()[0])
	}
	if https := policy.GetRules()[1].GetHttps(); https == nil || https.GetHostnames()[0] != "api.github.com" || https.GetPorts().GetNumbers()[0] != 443 || https.GetEffects() != nil {
		t.Fatalf("https rule = %v", policy.GetRules()[1])
	}
	if http := policy.GetRules()[2].GetHttp(); http == nil || http.GetPorts().GetNumbers()[0] != 80 {
		t.Fatalf("http rule = %v", policy.GetRules()[2])
	}
}

func TestCompileSubstrateActorEgressPolicyEmptyDeniesAll(t *testing.T) {
	policy, err := CompileSubstrateActorEgressPolicy("tools", nil)
	if err != nil {
		t.Fatalf("CompileSubstrateActorEgressPolicy() error = %v", err)
	}
	if len(policy.GetRules()) != 0 || policy.GetMetadata().GetName() != "default" {
		t.Fatalf("empty allowlist policy = %v, want deny-all", policy)
	}
}

type fakeEgressControl struct {
	policy      *ateapipb.EgressPolicy
	getErr      error
	writeErrs   []error
	creates     int
	updates     int
	lastUpdate  *ateapipb.UpdateActorEgressPolicyRequest
	actorRefs   []*ateapipb.ObjectRef
	nextVersion int64
	// writeTime stamps a stored policy, as the control plane does.
	writeTime time.Time
	// actorCreated is the Actor's creation time; actorErr fails reading it.
	actorCreated time.Time
	actorErr     error
}

func (f *fakeEgressControl) GetActor(_ context.Context, in *ateapipb.GetActorRequest, _ ...grpc.CallOption) (*ateapipb.Actor, error) {
	if f.actorErr != nil {
		return nil, f.actorErr
	}
	actor := &ateapipb.Actor{Metadata: &ateapipb.ResourceMetadata{Atespace: in.GetActor().GetAtespace(), Name: in.GetActor().GetName()}}
	if !f.actorCreated.IsZero() {
		actor.Metadata.CreateTime = timestamppb.New(f.actorCreated)
	}
	return actor, nil
}

func (f *fakeEgressControl) stampWrite() {
	if f.writeTime.IsZero() {
		return
	}
	f.policy.Metadata.UpdateTime = timestamppb.New(f.writeTime)
	if f.policy.Metadata.GetCreateTime() == nil {
		f.policy.Metadata.CreateTime = f.policy.Metadata.UpdateTime
	}
}

func (f *fakeEgressControl) GetActorEgressPolicy(_ context.Context, in *ateapipb.GetActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	f.actorRefs = append(f.actorRefs, in.GetActor())
	if f.getErr != nil {
		return nil, f.getErr
	}
	if f.policy == nil {
		return nil, status.Error(codes.NotFound, "no policy")
	}
	return proto.CloneOf(f.policy), nil
}

func (f *fakeEgressControl) popWriteErr() error {
	if len(f.writeErrs) == 0 {
		return nil
	}
	err := f.writeErrs[0]
	f.writeErrs = f.writeErrs[1:]
	return err
}

func (f *fakeEgressControl) CreateActorEgressPolicy(_ context.Context, in *ateapipb.CreateActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	f.creates++
	if err := f.popWriteErr(); err != nil {
		return nil, err
	}
	f.nextVersion++
	f.policy = proto.CloneOf(in.GetEgressPolicy())
	f.policy.Metadata.Uid, f.policy.Metadata.Version = "uid-1", f.nextVersion
	f.stampWrite()
	return proto.CloneOf(f.policy), nil
}

func (f *fakeEgressControl) UpdateActorEgressPolicy(_ context.Context, in *ateapipb.UpdateActorEgressPolicyRequest, _ ...grpc.CallOption) (*ateapipb.EgressPolicy, error) {
	f.updates++
	f.lastUpdate = proto.CloneOf(in)
	if err := f.popWriteErr(); err != nil {
		return nil, err
	}
	if in.GetEgressPolicy().GetMetadata().GetVersion() != f.policy.GetMetadata().GetVersion() {
		return nil, status.Error(codes.Aborted, "version mismatch")
	}
	f.nextVersion++
	f.policy = proto.CloneOf(in.GetEgressPolicy())
	f.policy.Metadata.Version = f.nextVersion
	f.stampWrite()
	return proto.CloneOf(f.policy), nil
}

func TestEnsureSubstrateActorEgressPolicyCreatesAndVerifies(t *testing.T) {
	control := &fakeEgressControl{}
	rules := []SubstrateEgressRule{{Scheme: "https", Host: "api.github.com"}}
	if err := ensureSubstrateActorEgressPolicy(context.Background(), control, "mcp-1.tools", rules); err != nil {
		t.Fatalf("ensure error = %v", err)
	}
	if control.creates != 1 || control.updates != 0 || len(control.actorRefs) != 2 {
		t.Fatalf("creates=%d updates=%d gets=%d, want create then verifying read", control.creates, control.updates, len(control.actorRefs))
	}
	if ref := control.actorRefs[0]; ref.GetName() != "mcp-1" || ref.GetAtespace() != "tools" {
		t.Fatalf("actor ref = %v", ref)
	}
	// Idempotent: a matching policy is not rewritten.
	if err := ensureSubstrateActorEgressPolicy(context.Background(), control, "mcp-1.tools", rules); err != nil || control.creates != 1 || control.updates != 0 {
		t.Fatalf("second ensure err=%v creates=%d updates=%d", err, control.creates, control.updates)
	}
}

// The egress gateway enforces a policy some time after it is stored, so the
// caller needs to know when the stored rules were written, whether they
// replaced others, and how long the Actor has existed.
func TestApplySubstrateActorEgressPolicyReportsTheStoredPolicy(t *testing.T) {
	actorCreated := time.Unix(1_799_999_000, 0).UTC()
	created, replaced := time.Unix(1_800_000_000, 0).UTC(), time.Unix(1_800_000_600, 0).UTC()
	control := &fakeEgressControl{writeTime: created, actorCreated: actorCreated}
	rules := []SubstrateEgressRule{{Scheme: "https", Host: "api.github.com"}}
	steps := []struct {
		name  string
		rules []SubstrateEgressRule
		at    time.Time
		want  SubstrateEgressPolicyState
	}{
		{"created", rules, created, SubstrateEgressPolicyState{Version: 1, CreateTime: created, UpdateTime: created, ActorCreateTime: actorCreated}},
		{"unchanged", rules, replaced, SubstrateEgressPolicyState{Version: 1, CreateTime: created, UpdateTime: created, ActorCreateTime: actorCreated}},
		{"replaced", nil, replaced, SubstrateEgressPolicyState{Version: 2, CreateTime: created, UpdateTime: replaced, ActorCreateTime: actorCreated}},
	}
	for _, step := range steps {
		control.writeTime = step.at
		got, err := applySubstrateActorEgressPolicy(context.Background(), control, "mcp-1.tools", step.rules)
		if err != nil || got != step.want {
			t.Fatalf("%s: state = %+v, %v, want %+v", step.name, got, err, step.want)
		}
	}
	// Without the Actor's age the caller cannot tell whether its name may have
	// a cached policy, so the apply fails rather than report half a state.
	control.actorErr = status.Error(codes.Unavailable, "down")
	if got, err := applySubstrateActorEgressPolicy(context.Background(), control, "mcp-1.tools", nil); err == nil || got != (SubstrateEgressPolicyState{}) {
		t.Fatalf("state = %+v, %v, want an error and no state", got, err)
	}
	control.actorErr, control.getErr = nil, status.Error(codes.Unavailable, "down")
	if got, err := applySubstrateActorEgressPolicy(context.Background(), control, "mcp-1.tools", rules); err == nil || got != (SubstrateEgressPolicyState{}) {
		t.Fatalf("state = %+v, %v, want an error and no state", got, err)
	}
}

func TestSubstrateEgressPolicyStateEnforcedAfter(t *testing.T) {
	const window = 14 * time.Second
	long := time.Unix(1_800_000_000, 0).UTC()
	recent := long.Add(time.Hour)
	for name, tc := range map[string]struct {
		state SubstrateEgressPolicyState
		want  time.Time
	}{
		// The gateway may remember the rules this policy replaced.
		"replaced policy": {SubstrateEgressPolicyState{Version: 2, CreateTime: long, UpdateTime: recent, ActorCreateTime: long}, recent.Add(window)},
		// A policy deleted and created again for the same Actor starts at
		// version 1 while the deleted rules may still be cached.
		"recreated policy of an old actor": {SubstrateEgressPolicyState{Version: 1, CreateTime: recent, UpdateTime: recent, ActorCreateTime: long}, recent.Add(window)},
		// A new Actor's name may have belonged to a deleted one, whose policy
		// the gateway may remember.
		"new actor": {SubstrateEgressPolicyState{Version: 1, CreateTime: recent, UpdateTime: recent, ActorCreateTime: recent}, recent.Add(window)},
		// Each time is honoured even if a store stamps the others earlier.
		"create time alone":       {SubstrateEgressPolicyState{Version: 1, CreateTime: recent}, recent.Add(window)},
		"actor create time alone": {SubstrateEgressPolicyState{Version: 1, ActorCreateTime: recent}, recent.Add(window)},
		// Nothing written for longer than the window: enforced already.
		"old policy": {SubstrateEgressPolicyState{Version: 3, CreateTime: long, UpdateTime: long, ActorCreateTime: long}, long.Add(window)},
		"unknown":    {SubstrateEgressPolicyState{}, time.Time{}},
	} {
		if got := tc.state.EnforcedAfter(window); !got.Equal(tc.want) {
			t.Errorf("%s: EnforcedAfter = %s, want %s", name, got, tc.want)
		}
	}
	if got := (SubstrateEgressPolicyState{Version: 2, UpdateTime: recent, ActorCreateTime: long}).EnforcedAfter(0); !got.IsZero() {
		t.Errorf("EnforcedAfter(0) = %s, want no wait for a gateway that does not cache", got)
	}
}

func TestEnsureSubstrateActorEgressPolicyUpdatesWithPreconditionsAndRetriesAborted(t *testing.T) {
	existing, _ := CompileSubstrateActorEgressPolicy("tools", []SubstrateEgressRule{{Scheme: "https", Host: "old.example.com"}})
	existing.Metadata.Uid, existing.Metadata.Version = "uid-7", 4
	control := &fakeEgressControl{policy: existing, nextVersion: 4, writeErrs: []error{status.Error(codes.Aborted, "conflict")}}
	if err := ensureSubstrateActorEgressPolicy(context.Background(), control, "mcp-1.tools", nil); err != nil {
		t.Fatalf("ensure error = %v", err)
	}
	if control.updates != 2 {
		t.Fatalf("updates = %d, want an Aborted retry", control.updates)
	}
	if md := control.lastUpdate.GetEgressPolicy().GetMetadata(); md.GetUid() != "uid-7" || md.GetVersion() != 4 {
		t.Fatalf("update preconditions = %v", md)
	}
	if len(control.policy.GetRules()) != 0 {
		t.Fatalf("policy rules = %v, want deny-all", control.policy.GetRules())
	}
}

func TestEnsureSubstrateActorEgressPolicyFailsClosed(t *testing.T) {
	for _, tc := range []struct {
		name    string
		control *fakeEgressControl
		actorID string
		rules   []SubstrateEgressRule
	}{
		{"unqualified actor", &fakeEgressControl{}, "mcp-1", nil},
		{"invalid rule", &fakeEgressControl{}, "mcp-1.tools", []SubstrateEgressRule{{Scheme: "https", Host: "*"}}},
		{"get unavailable", &fakeEgressControl{getErr: status.Error(codes.Unavailable, "down")}, "mcp-1.tools", nil},
		{"create rejected", &fakeEgressControl{writeErrs: []error{status.Error(codes.InvalidArgument, "bad")}}, "mcp-1.tools", nil},
		{"never converges", &fakeEgressControl{writeErrs: []error{
			status.Error(codes.Aborted, "1"), status.Error(codes.Aborted, "2"), status.Error(codes.Aborted, "3"),
			status.Error(codes.Aborted, "4"), status.Error(codes.Aborted, "5"),
		}}, "mcp-1.tools", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ensureSubstrateActorEgressPolicy(context.Background(), tc.control, tc.actorID, tc.rules)
			if err == nil {
				t.Fatal("ensure succeeded, want fail-closed error")
			}
			if _, ok := errors.AsType[*Error](err); !ok {
				t.Fatalf("error %T is not a workspace error", err)
			}
		})
	}
}

func TestValidateSubstrateEgressRulesRejectsWildcardOverrides(t *testing.T) {
	wildcard := SubstrateEgressRule{Scheme: "tls", Host: "*.example.com", Ports: []int32{443}}
	if err := ValidateSubstrateEgressRules([]SubstrateEgressRule{wildcard, {Scheme: "https", Host: "api.example.com"}}); err == nil ||
		!strings.Contains(err.Error(), "overrides") {
		t.Fatalf("exact rule overriding a wildcard's scheme error = %v", err)
	}
	for name, exact := range map[string]SubstrateEgressRule{
		"same scheme":   {Scheme: "tls", Host: "api.example.com", Ports: []int32{443}},
		"other port":    {Scheme: "https", Host: "api.example.com", Ports: []int32{8443}},
		"deeper name":   {Scheme: "https", Host: "a.b.example.com"},
		"numeric label": {Scheme: "https", Host: "123.example.com", Ports: []int32{8443}},
	} {
		if err := ValidateSubstrateEgressRules([]SubstrateEgressRule{wildcard, exact}); err != nil {
			t.Fatalf("%s: unexpected rejection: %v", name, err)
		}
	}
}
