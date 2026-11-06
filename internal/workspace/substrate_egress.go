package workspace

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	ateapipb "github.com/orka-agents/orka/internal/substratepb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Substrate egress schemes accepted by Orka. "tls" compiles to an upstream
// tls_passthrough rule; "https" is intercepted by the egress gateway, so the
// Actor must trust the gateway CA bundle.
const (
	SubstrateEgressSchemeHTTP  = "http"
	SubstrateEgressSchemeHTTPS = "https"
	SubstrateEgressSchemeTLS   = "tls"

	// substrateEgressPolicyName is the upstream singleton name of an Actor's
	// egress policy resource.
	substrateEgressPolicyName = "default"
	// MaxSubstrateEgressRules bounds one Actor policy below the upstream limit
	// of 256 rules.
	MaxSubstrateEgressRules = 64
	maxSubstrateEgressPorts = 16
	// substrateEgressApplyAttempts bounds optimistic-concurrency retries.
	substrateEgressApplyAttempts = 5
)

// SubstrateEgressRule allows one DNS host name on explicit ports. It never
// carries credentials: Orka does not configure gateway header effects.
type SubstrateEgressRule struct {
	Scheme string
	Host   string
	Ports  []int32
}

// substrateEgressControl is the subset of the native Control API used to
// manage an Actor's egress policy. ateapipb.ControlClient satisfies it.
type substrateEgressControl interface {
	GetActorEgressPolicy(ctx context.Context, in *ateapipb.GetActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error)
	CreateActorEgressPolicy(ctx context.Context, in *ateapipb.CreateActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error)
	UpdateActorEgressPolicy(ctx context.Context, in *ateapipb.UpdateActorEgressPolicyRequest, opts ...grpc.CallOption) (*ateapipb.EgressPolicy, error)
}

// ValidateSubstrateEgressRules rejects anything other than DNS host names
// with an optional leftmost-label wildcard, explicit schemes and valid ports.
func ValidateSubstrateEgressRules(rules []SubstrateEgressRule) error {
	_, err := canonicalSubstrateEgressRules(rules)
	return err
}

// CompileSubstrateActorEgressPolicy compiles an allowlist into the Actor's
// native default policy. An empty allowlist compiles to a policy without
// rules, which denies all egress (and answers external DNS with NXDOMAIN).
func CompileSubstrateActorEgressPolicy(atespace string, rules []SubstrateEgressRule) (*ateapipb.EgressPolicy, error) {
	atespace = strings.TrimSpace(atespace)
	if len(validation.IsDNS1123Label(atespace)) != 0 {
		return nil, NewError("compile egress policy", ErrorKindInvalidArgument, "egress policy requires a DNS-label Atespace", false, nil)
	}
	canonical, err := canonicalSubstrateEgressRules(rules)
	if err != nil {
		return nil, err
	}
	policy := &ateapipb.EgressPolicy{Metadata: &ateapipb.ResourceMetadata{Atespace: atespace, Name: substrateEgressPolicyName}}
	for _, rule := range canonical {
		ports := &ateapipb.Ports{Numbers: slices.Clone(rule.Ports)}
		hostnames := []string{rule.Host}
		switch rule.Scheme {
		case SubstrateEgressSchemeHTTP:
			policy.Rules = append(policy.Rules, &ateapipb.EgressRule{Http: &ateapipb.HttpRule{Hostnames: hostnames, Ports: ports}})
		case SubstrateEgressSchemeHTTPS:
			policy.Rules = append(policy.Rules, &ateapipb.EgressRule{Https: &ateapipb.HttpsRule{Hostnames: hostnames, Ports: ports}})
		case SubstrateEgressSchemeTLS:
			policy.Rules = append(policy.Rules, &ateapipb.EgressRule{TlsPassthrough: &ateapipb.TlsPassthroughRule{Hostnames: hostnames, Ports: ports}})
		}
	}
	return policy, nil
}

// EnsureActorEgressPolicy compiles rules and installs them as the qualified
// Actor's ("name.atespace") native egress policy, then re-reads the policy
// and verifies it. Any error means the policy may not match; callers must not
// resume the Actor.
func (c *SubstrateNativeClient) EnsureActorEgressPolicy(ctx context.Context, actorID string, rules []SubstrateEgressRule) error {
	if c == nil || c.Control == nil {
		return NewError("apply egress policy", ErrorKindInvalidArgument, "Substrate native client is not configured", false, nil)
	}
	return ensureSubstrateActorEgressPolicy(ctx, c.Control, actorID, rules)
}

func ensureSubstrateActorEgressPolicy(ctx context.Context, control substrateEgressControl, actorID string, rules []SubstrateEgressRule) error {
	if !strings.Contains(strings.TrimSpace(actorID), ".") {
		return NewError("apply egress policy", ErrorKindInvalidArgument, "egress policy requires a qualified Actor ID (name.atespace)", false, nil)
	}
	ref, err := substrateObjectRef(actorID, "")
	if err != nil {
		return err
	}
	desired, err := CompileSubstrateActorEgressPolicy(ref.GetAtespace(), rules)
	if err != nil {
		return err
	}
	for range substrateEgressApplyAttempts {
		if err := ctx.Err(); err != nil {
			return substrateControlError("apply egress policy", err)
		}
		existing, getErr := control.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{Actor: proto.CloneOf(ref), Name: substrateEgressPolicyName})
		switch {
		case status.Code(getErr) == codes.NotFound:
			_, err = control.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{Actor: proto.CloneOf(ref), EgressPolicy: proto.CloneOf(desired)})
		case getErr != nil:
			return substrateControlError("get egress policy", getErr)
		case substrateEgressRulesEqual(existing.GetRules(), desired.GetRules()):
			// Verified by a fresh read after any write.
			return nil
		default:
			metadata := existing.GetMetadata()
			if metadata.GetUid() == "" || metadata.GetVersion() == 0 {
				return NewError("apply egress policy", ErrorKindFailedPrecondition, "existing egress policy has no UID/version precondition", false, nil)
			}
			update := proto.CloneOf(desired)
			update.Metadata = proto.CloneOf(metadata)
			_, err = control.UpdateActorEgressPolicy(ctx, &ateapipb.UpdateActorEgressPolicyRequest{Actor: proto.CloneOf(ref), EgressPolicy: update})
		}
		switch status.Code(err) {
		case codes.OK, codes.Aborted, codes.AlreadyExists:
			// Re-read: verify our write or reload after a concurrent one.
			continue
		default:
			return substrateControlError("write egress policy", err)
		}
	}
	return NewError("apply egress policy", ErrorKindFailedPrecondition, "actor egress policy did not converge after concurrent updates", true, nil)
}

func canonicalSubstrateEgressRules(rules []SubstrateEgressRule) ([]SubstrateEgressRule, error) {
	if len(rules) > MaxSubstrateEgressRules {
		return nil, egressRuleError(fmt.Sprintf("at most %d egress rules are allowed", MaxSubstrateEgressRules))
	}
	out := make([]SubstrateEgressRule, 0, len(rules))
	claimed := map[string]string{}
	for i, rule := range rules {
		scheme := strings.TrimSpace(rule.Scheme)
		switch scheme {
		case SubstrateEgressSchemeHTTP, SubstrateEgressSchemeHTTPS, SubstrateEgressSchemeTLS:
		default:
			return nil, egressRuleError(fmt.Sprintf("egress rule %d scheme must be http, https or tls", i))
		}
		host, err := validateSubstrateEgressHost(rule.Host)
		if err != nil {
			return nil, egressRuleError(fmt.Sprintf("egress rule %d: %v", i, err))
		}
		ports := slices.Clone(rule.Ports)
		if len(ports) == 0 {
			switch scheme {
			case SubstrateEgressSchemeHTTP:
				ports = []int32{80}
			case SubstrateEgressSchemeHTTPS:
				ports = []int32{443}
			default:
				return nil, egressRuleError(fmt.Sprintf("egress rule %d: tls egress requires explicit ports", i))
			}
		}
		if len(ports) > maxSubstrateEgressPorts {
			return nil, egressRuleError(fmt.Sprintf("egress rule %d allows at most %d ports", i, maxSubstrateEgressPorts))
		}
		slices.Sort(ports)
		for j, port := range ports {
			if port < 1 || port > 65535 {
				return nil, egressRuleError(fmt.Sprintf("egress rule %d port %d is out of range", i, port))
			}
			if j > 0 && ports[j-1] == port {
				return nil, egressRuleError(fmt.Sprintf("egress rule %d repeats port %d", i, port))
			}
			// Upstream picks exactly one rule per name and port; a tie is an
			// admission error, and two schemes on one origin are ambiguous.
			key := host + ":" + strconv.Itoa(int(port))
			if previous, ok := claimed[key]; ok {
				return nil, egressRuleError(fmt.Sprintf("egress host %q port %d is allowed by more than one rule (%s and %s)", host, port, previous, scheme))
			}
			claimed[key] = scheme
		}
		out = append(out, SubstrateEgressRule{Scheme: scheme, Host: host, Ports: ports})
	}
	slices.SortFunc(out, func(a, b SubstrateEgressRule) int {
		if c := strings.Compare(a.Host, b.Host); c != 0 {
			return c
		}
		if c := strings.Compare(a.Scheme, b.Scheme); c != 0 {
			return c
		}
		return slices.Compare(a.Ports, b.Ports)
	})
	return out, nil
}

func validateSubstrateEgressHost(raw string) (string, error) {
	host := strings.TrimSpace(raw)
	if host == "" {
		return "", fmt.Errorf("host is required")
	}
	if host != raw || host != strings.ToLower(host) {
		return "", fmt.Errorf("host %q must be a lowercase DNS name without surrounding spaces", raw)
	}
	if strings.ContainsAny(host, ":/@?#[]% ") {
		return "", fmt.Errorf("host %q must be a bare DNS name, not a URL, CIDR, path or host:port", raw)
	}
	base, wildcard := strings.CutPrefix(host, "*.")
	if host == "*" || strings.Contains(base, "*") {
		return "", fmt.Errorf("host %q may use \"*\" only as the complete leftmost label of a domain", raw)
	}
	if _, err := netip.ParseAddr(base); err == nil {
		return "", fmt.Errorf("host %q must be a DNS name, not an IP address", raw)
	}
	if strings.HasSuffix(base, ".") || len(validation.IsDNS1123Subdomain(base)) != 0 {
		return "", fmt.Errorf("host %q is not a valid DNS name", raw)
	}
	labels := strings.Split(base, ".")
	if len(labels) < 2 {
		if wildcard {
			return "", fmt.Errorf("host %q must not wildcard a top-level domain", raw)
		}
		return "", fmt.Errorf("host %q must be a fully qualified DNS name", raw)
	}
	for _, label := range labels {
		if len(label) > 63 {
			return "", fmt.Errorf("host %q has a DNS label longer than 63 characters", raw)
		}
	}
	if _, err := strconv.ParseUint(labels[len(labels)-1], 10, 64); err == nil {
		return "", fmt.Errorf("host %q must not end in a numeric label", raw)
	}
	return host, nil
}

func egressRuleError(message string) error {
	return NewError("validate egress policy", ErrorKindInvalidArgument, message, false, nil)
}

// substrateEgressRulesEqual compares rule sets independent of order. Any
// effect on an existing rule makes it unequal, so Orka replaces it.
func substrateEgressRulesEqual(a, b []*ateapipb.EgressRule) bool {
	return slices.Equal(substrateEgressRuleKeys(a), substrateEgressRuleKeys(b))
}

func substrateEgressRuleKeys(rules []*ateapipb.EgressRule) []string {
	keys := make([]string, 0, len(rules))
	for _, rule := range rules {
		kind := "unknown"
		var hostnames []string
		var ports *ateapipb.Ports
		effects := false
		switch {
		case rule.GetHttp() != nil:
			kind, hostnames, ports = "http", rule.GetHttp().GetHostnames(), rule.GetHttp().GetPorts()
			effects = rule.GetHttp().GetEffects() != nil
		case rule.GetHttps() != nil:
			kind, hostnames, ports = "https", rule.GetHttps().GetHostnames(), rule.GetHttps().GetPorts()
			effects = rule.GetHttps().GetEffects() != nil
		case rule.GetTlsPassthrough() != nil:
			kind, hostnames, ports = "tls", rule.GetTlsPassthrough().GetHostnames(), rule.GetTlsPassthrough().GetPorts()
		}
		hostnames = slices.Clone(hostnames)
		slices.Sort(hostnames)
		portText := "default"
		switch {
		case ports.GetAll() != nil:
			portText = "all"
		case len(ports.GetNumbers()) > 0:
			numbers := slices.Clone(ports.GetNumbers())
			slices.Sort(numbers)
			parts := make([]string, len(numbers))
			for i, n := range numbers {
				parts[i] = strconv.Itoa(int(n))
			}
			portText = strings.Join(parts, ",")
		}
		keys = append(keys, fmt.Sprintf("%s|%s|%s|%t", kind, strings.Join(hostnames, ","), portText, effects))
	}
	slices.Sort(keys)
	return keys
}
