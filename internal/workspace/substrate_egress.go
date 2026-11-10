package workspace

import (
	"context"
	"encoding/hex"
	"fmt"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"time"

	workspacev1alpha1 "github.com/orka-agents/orka-workspace/api/v1alpha1"
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
// carries credential values: an optional Credential names a Kubernetes Secret
// key that the egress gateway resolves and injects at request time.
type SubstrateEgressRule struct {
	Scheme     string
	Host       string
	Ports      []int32
	Credential *SubstrateEgressCredential
}

// SubstrateEgressCredential replaces one request header with a Kubernetes
// Secret value through the gateway's credential provider. Provider is the
// credential provider class the gateway was installed with; an empty
// Provider means injection is not configured and the rule fails closed.
type SubstrateEgressCredential struct {
	Header    string
	Prefix    string
	Provider  string
	Namespace string
	Name      string
	Key       string
}

// SubstrateKubernetesSecretsCredentialProvider is the upstream reference
// credential provider class for ate-secret://k8s.io/default/<ns>/<name>/<key>.
const SubstrateKubernetesSecretsCredentialProvider = "k8s.io"

// credentialURI returns the upstream reference to the Secret key.
func (c SubstrateEgressCredential) credentialURI() string {
	return fmt.Sprintf("ate-secret://%s/default/%s/%s/%s", c.Provider, c.Namespace, c.Name, c.Key)
}

func validateSubstrateEgressCredential(scheme, host string, credential *SubstrateEgressCredential) error {
	if credential == nil {
		return nil
	}
	if credential.Provider == "" {
		return fmt.Errorf("egress credential injection is not configured (--substrate-egress-credential-provider)")
	}
	if credential.Provider != SubstrateKubernetesSecretsCredentialProvider {
		return fmt.Errorf("egress credential provider %q is unsupported", credential.Provider)
	}
	if len(validation.IsDNS1123Label(credential.Namespace)) != 0 {
		return fmt.Errorf("egress credential Secret namespace is invalid")
	}
	protocol := workspacev1alpha1.RuntimeEgressProtocol("")
	switch scheme {
	case SubstrateEgressSchemeHTTP:
		protocol = workspacev1alpha1.RuntimeEgressHTTP
	case SubstrateEgressSchemeHTTPS:
		protocol = workspacev1alpha1.RuntimeEgressHTTPS
	default:
		protocol = workspacev1alpha1.RuntimeEgressTLSPassthrough
	}
	return workspacev1alpha1.ValidateRuntimeEgressCredentialHeaders(protocol, host, []workspacev1alpha1.RuntimeEgressCredentialHeader{{
		Header: credential.Header, Prefix: credential.Prefix,
		SecretKeyRef: workspacev1alpha1.RuntimeEgressSecretKeyReference{Namespace: credential.Namespace, Name: credential.Name, Key: credential.Key},
	}})
}

func compileSubstrateEgressEffects(credential *SubstrateEgressCredential) *ateapipb.HttpRuleEffects {
	if credential == nil {
		return nil
	}
	return &ateapipb.HttpRuleEffects{ReplaceHeaders: []*ateapipb.CredentialHeader{{Header: credential.Header, Prefix: credential.Prefix, CredentialUri: credential.credentialURI()}}}
}

// substrateEgressControl is the subset of the native Control API used to
// manage an Actor's egress policy. ateapipb.ControlClient satisfies it.
type substrateEgressControl interface {
	GetActor(ctx context.Context, in *ateapipb.GetActorRequest, opts ...grpc.CallOption) (*ateapipb.Actor, error)
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
			policy.Rules = append(policy.Rules, &ateapipb.EgressRule{Http: &ateapipb.HttpRule{Hostnames: hostnames, Ports: ports, Effects: compileSubstrateEgressEffects(rule.Credential)}})
		case SubstrateEgressSchemeHTTPS:
			policy.Rules = append(policy.Rules, &ateapipb.EgressRule{Https: &ateapipb.HttpsRule{Hostnames: hostnames, Ports: ports, Effects: compileSubstrateEgressEffects(rule.Credential)}})
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
	_, err := c.ApplyActorEgressPolicy(ctx, actorID, rules)
	return err
}

// SubstrateEgressPolicyState describes an Actor's stored egress policy.
//
// Storing a policy does not make it effective at once. The Substrate egress
// gateway caches each Actor's policy by Actor name (--egress-policy-cache-ttl,
// 10s by default, plus a fetch of up to 4s that may have read earlier rules)
// and nothing invalidates an entry.
type SubstrateEgressPolicyState struct {
	// Version is 1 for a policy that was created and never replaced.
	Version int64
	// CreateTime is when the control plane created the policy.
	CreateTime time.Time
	// UpdateTime is when the control plane stored the current rules.
	UpdateTime time.Time
	// ActorCreateTime is when the control plane created the Actor.
	ActorCreateTime time.Time
}

// EnforcedAfter returns the time from which the gateway can only apply the
// stored rules, given how long it may keep a cached policy: window after the
// latest of these, each of which can leave other rules cached under this
// Actor's name.
//
//   - UpdateTime: the rules this write replaced.
//   - CreateTime: a policy deleted and created again starts at version 1, and
//     the deleted one's rules may still be cached. A new policy and a
//     recreated one cannot be told apart, so every new policy waits.
//   - ActorCreateTime: deleting an Actor deletes its policy, and a new Actor
//     of the same name starts without one.
//
// The control plane stamps a new policy's UpdateTime with its CreateTime and
// creates a policy only for an existing Actor, so UpdateTime alone is the
// latest. All three are compared so that no store's stamping is relied on.
// Only a policy that has not been written for a window is enforced at once.
func (s SubstrateEgressPolicyState) EnforcedAfter(window time.Duration) time.Time {
	changed := s.UpdateTime
	for _, candidate := range []time.Time{s.CreateTime, s.ActorCreateTime} {
		if candidate.After(changed) {
			changed = candidate
		}
	}
	if changed.IsZero() || window <= 0 {
		return time.Time{}
	}
	return changed.Add(window)
}

// ApplyActorEgressPolicy is EnsureActorEgressPolicy that also reports the
// stored policy's state, so the caller can wait for it to take effect.
func (c *SubstrateNativeClient) ApplyActorEgressPolicy(ctx context.Context, actorID string, rules []SubstrateEgressRule) (SubstrateEgressPolicyState, error) {
	if c == nil || c.Control == nil {
		return SubstrateEgressPolicyState{}, NewError("apply egress policy", ErrorKindInvalidArgument, "Substrate native client is not configured", false, nil)
	}
	return applySubstrateActorEgressPolicy(ctx, c.Control, actorID, rules)
}

func ensureSubstrateActorEgressPolicy(ctx context.Context, control substrateEgressControl, actorID string, rules []SubstrateEgressRule) error {
	_, err := applySubstrateActorEgressPolicy(ctx, control, actorID, rules)
	return err
}

func applySubstrateActorEgressPolicy(ctx context.Context, control substrateEgressControl, actorID string, rules []SubstrateEgressRule) (SubstrateEgressPolicyState, error) {
	fail := func(err error) (SubstrateEgressPolicyState, error) { return SubstrateEgressPolicyState{}, err }
	if !strings.Contains(strings.TrimSpace(actorID), ".") {
		return fail(NewError("apply egress policy", ErrorKindInvalidArgument, "egress policy requires a qualified Actor ID (name.atespace)", false, nil))
	}
	ref, err := substrateObjectRef(actorID, "")
	if err != nil {
		return fail(err)
	}
	desired, err := CompileSubstrateActorEgressPolicy(ref.GetAtespace(), rules)
	if err != nil {
		return fail(err)
	}
	for range substrateEgressApplyAttempts {
		if err := ctx.Err(); err != nil {
			return fail(substrateControlError("apply egress policy", err))
		}
		existing, getErr := control.GetActorEgressPolicy(ctx, &ateapipb.GetActorEgressPolicyRequest{Actor: proto.CloneOf(ref), Name: substrateEgressPolicyName})
		switch {
		case status.Code(getErr) == codes.NotFound:
			_, err = control.CreateActorEgressPolicy(ctx, &ateapipb.CreateActorEgressPolicyRequest{Actor: proto.CloneOf(ref), EgressPolicy: proto.CloneOf(desired)})
		case getErr != nil:
			return fail(substrateControlError("get egress policy", getErr))
		case substrateEgressRulesEqual(existing.GetRules(), desired.GetRules()):
			// Verified by a fresh read after any write.
			actor, actorErr := control.GetActor(ctx, &ateapipb.GetActorRequest{Actor: proto.CloneOf(ref)})
			if actorErr != nil {
				return fail(substrateControlError("get actor", actorErr))
			}
			metadata := existing.GetMetadata()
			state := SubstrateEgressPolicyState{Version: metadata.GetVersion()}
			if created := metadata.GetCreateTime(); created != nil {
				state.CreateTime = created.AsTime()
			}
			if updated := metadata.GetUpdateTime(); updated != nil {
				state.UpdateTime = updated.AsTime()
			}
			if created := actor.GetMetadata().GetCreateTime(); created != nil {
				state.ActorCreateTime = created.AsTime()
			}
			return state, nil
		default:
			metadata := existing.GetMetadata()
			if metadata.GetUid() == "" || metadata.GetVersion() == 0 {
				return fail(NewError("apply egress policy", ErrorKindFailedPrecondition, "existing egress policy has no UID/version precondition", false, nil))
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
			return fail(substrateControlError("write egress policy", err))
		}
	}
	return fail(NewError("apply egress policy", ErrorKindFailedPrecondition, "actor egress policy did not converge after concurrent updates", true, nil))
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
		if err := validateSubstrateEgressCredential(scheme, host, rule.Credential); err != nil {
			return nil, egressRuleError(fmt.Sprintf("egress rule %d: %v", i, err))
		}
		var credential *SubstrateEgressCredential
		if rule.Credential != nil {
			copied := *rule.Credential
			credential = &copied
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
		out = append(out, SubstrateEgressRule{Scheme: scheme, Host: host, Ports: ports, Credential: credential})
	}
	if err := rejectSubstrateEgressOverrides(out); err != nil {
		return nil, err
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

// rejectSubstrateEgressOverrides refuses an exact host that a wildcard rule on
// the same port also matches with a different scheme. The gateway prefers the
// exact rule ("*.example.com" matches exactly one extra label), so the overlap
// would replace the wildcard's scheme for that host instead of adding access.
func rejectSubstrateEgressOverrides(rules []SubstrateEgressRule) error {
	for _, wildcard := range rules {
		suffix, ok := strings.CutPrefix(wildcard.Host, "*")
		if !ok {
			continue
		}
		for _, exact := range rules {
			label, matches := strings.CutSuffix(exact.Host, suffix)
			if exact.Scheme == wildcard.Scheme || !matches || label == "" || strings.ContainsAny(label, ".*") {
				continue
			}
			for _, port := range exact.Ports {
				if slices.Contains(wildcard.Ports, port) {
					return egressRuleError(fmt.Sprintf("egress host %q port %d uses %s, which overrides %s from %q", exact.Host, port, exact.Scheme, wildcard.Scheme, wildcard.Host))
				}
			}
		}
	}
	return nil
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
	// The gateway rejects any all-numeric final label, however long.
	if strings.Trim(labels[len(labels)-1], "0123456789") == "" {
		return "", fmt.Errorf("host %q must not end in a numeric label", raw)
	}
	return host, nil
}

func egressRuleError(message string) error {
	return NewError("validate egress policy", ErrorKindInvalidArgument, message, false, nil)
}

// substrateEgressRulesEqual compares rule sets independent of order,
// including each rule's exact gateway effects, so a changed or foreign
// effect makes the policy unequal and Orka replaces it.
func substrateEgressRulesEqual(a, b []*ateapipb.EgressRule) bool {
	return slices.Equal(substrateEgressRuleKeys(a), substrateEgressRuleKeys(b))
}

func substrateEgressRuleKeys(rules []*ateapipb.EgressRule) []string {
	keys := make([]string, 0, len(rules))
	for _, rule := range rules {
		kind := "unknown"
		var hostnames []string
		var ports *ateapipb.Ports
		var effects *ateapipb.HttpRuleEffects
		switch {
		case rule.GetHttp() != nil:
			kind, hostnames, ports = "http", rule.GetHttp().GetHostnames(), rule.GetHttp().GetPorts()
			effects = rule.GetHttp().GetEffects()
		case rule.GetHttps() != nil:
			kind, hostnames, ports = "https", rule.GetHttps().GetHostnames(), rule.GetHttps().GetPorts()
			effects = rule.GetHttps().GetEffects()
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
		keys = append(keys, fmt.Sprintf("%s|%s|%s|%s", kind, strings.Join(hostnames, ","), portText, substrateEgressEffectsKey(effects)))
	}
	slices.Sort(keys)
	return keys
}

// substrateEgressEffectsKey canonically encodes a rule's gateway effects.
func substrateEgressEffectsKey(effects *ateapipb.HttpRuleEffects) string {
	if effects == nil {
		return "none"
	}
	data, err := (proto.MarshalOptions{Deterministic: true}).Marshal(effects)
	if err != nil {
		return "unencodable"
	}
	return hex.EncodeToString(data)
}
